package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/TSHOGX/khora/internal/guestport"
)

// --- large-file bypass (the transport data plane fs / fs transport) ----------------------------
//
// Byte-carrying fs calls have two paths on purpose. The RPC (FsService.Read /
// Write) is bounded by the message size and buffers the whole payload; this HTTP
// route STREAMS, so a file is never a single protobuf message and never has to
// fit in memory at either end.
//
// It rides the same listener and port as the Connect handlers — sandboxd already
// serves a plain ServeMux, so nothing new is exposed. No auth here for the same
// reason the RPCs have none: the endpoint is netns-internal and only the host can
// reach it (guest TCP transport trust model).
//
// The threshold is the CALLER's choice, not a number the engine picks: small payloads are
// cheaper as one RPC, large ones must stream, and only the caller knows which it has.

// filesPath extracts and validates the ?path= query parameter. Guest-absolute and
// unconfined, exactly like the fs RPCs (see fs.go for why).
func filesPath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", errors.New("missing ?path=")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute in the guest namespace: " + p)
	}
	return p, nil
}

// httpFsErr maps a filesystem error to an HTTP status using the SAME classification as the RPC
// path's fsErr — one rule, two encodings, so a caller cannot observe a file as not-found over
// one path and permission-denied over the other.
//
// Fewer errnos than fsErr, only because fewer are reachable: this route does open / create /
// rename-within-a-directory, so ENOTEMPTY, EXDEV and ELOOP cannot arise. If it ever grows a verb
// that can produce one, ADD THE ROW rather than relying on the 500 fallback, which the host
// turns into `internal` and reads as an engine bug.
func httpFsErr(w http.ResponseWriter, op, path string, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		status = http.StatusNotFound
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		status = http.StatusForbidden
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		// The resource admission capacity cap. 507 rather than 500: the request was valid, the
		// sandbox simply has no room, and the sandbox itself stays alive (the data-plane exposure boundary).
		status = http.StatusInsufficientStorage
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		status = http.StatusBadRequest
	}
	http.Error(w, op+" "+path+": "+err.Error(), status)
}

// handleFilesGet streams a guest file out. io.Copy, never ReadAll: the point of
// this route is that neither end holds the whole file.
func handleFilesGet(w http.ResponseWriter, r *http.Request) {
	p, err := filesPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		httpFsErr(w, "download", p, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		httpFsErr(w, "download", p, err)
		return
	}
	if fi.IsDir() {
		http.Error(w, "download "+p+": is a directory", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	// A write error here means the CLIENT went away mid-transfer. Nothing to
	// report — the status line is already sent — so it is logged and dropped.
	if _, err := io.Copy(w, f); err != nil {
		logf("files: download %s interrupted: %v", p, err)
	}
}

// handleFilesPut streams a request body into a guest file. Accepts either a raw body
// (Content-Type absent or octet-stream) or one multipart part, since a streaming client and a
// form-posting client want different framings.
//
// Parent directories are created, matching MakeDir's mkdir -p and the fs RPC Write's tolerance —
// failing only because a directory is missing would force every caller into a two-call dance.
func handleFilesPut(w http.ResponseWriter, r *http.Request) {
	p, err := filesPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, err := uploadReader(r)
	if err != nil {
		http.Error(w, "upload "+p+": "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	// Write to a temp file in the SAME directory, then rename: a rename within one
	// directory is atomic, so an interrupted upload cannot leave a truncated file
	// where the caller expects a whole one. This is also why the streaming path
	// does not share fs.Write's truncate-then-write hazard.
	//
	// It must be the same directory, not /tmp: rename(2) cannot cross filesystems
	// (EXDEV), and /tmp is a separate tmpfs mount. The cost is that a crash mid-
	// upload strands a dotfile next to the caller's target, so the prefix below is
	// matched by rootfs.promoteExcludedPrefixes — otherwise a Promote would freeze
	// that debris into a Template. Renaming this prefix means changing it there too.
	tmp, err := os.CreateTemp(filepath.Dir(p), guestport.UploadTempPrefix+"*")
	if err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename below succeeded
	}()

	n, err := io.Copy(tmp, body)
	if err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	if err := tmp.Close(); err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	if err := os.Rename(tmpName, p); err != nil {
		httpFsErr(w, "upload", p, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"bytes_written":` + strconv.FormatInt(n, 10) + `}`))
}

// uploadReader picks the body framing. Returns a reader that must not outlive the
// request.
func uploadReader(r *http.Request) (io.Reader, error) {
	ct := r.Header.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		return r.Body, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		// Not multipart either — treat it as a raw body rather than refusing, since
		// the caller's intent (these bytes, at this path) is unambiguous.
		return r.Body, nil
	}
	part, err := mr.NextPart()
	if err != nil {
		return nil, errors.New("multipart body has no part")
	}
	return part, nil
}

// registerFileRoutes wires the bypass onto the guest mux.
func registerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleFilesGet(w, r)
		case http.MethodPut, http.MethodPost:
			handleFilesPut(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
