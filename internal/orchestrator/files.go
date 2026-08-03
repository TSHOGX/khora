package orchestrator

import (
	"io"
	"log"
	"net/http"
	"strconv"

	"connectrpc.com/connect"

	"engine/internal/model"
)

// --- large-file bypass, outer edge (§5.E fs / transport D4.4) -----------------
//
// GET|PUT /v1/sandboxes/{id}/files?path=…  streams bytes to/from the guest.
//
// This is the one part of the data plane that is REST rather than Connect-RPC, and
// the reason is the payload, not taste: a large file must not become a single
// protobuf message, and neither end should hold it in memory. Everything else
// about the §6.2 constraints still holds — the sandbox is addressed by its opaque
// ID in the path, and the guest endpoint (placement) never appears on the wire.
//
// The engine picks no size threshold. Small payloads are cheaper as an fs RPC,
// large ones must stream; only the caller knows which it has, so both paths exist
// and the caller chooses.

// maxUploadBytes bounds a single upload. It is a DEFENSIVE cap on the outer edge,
// not a capacity decision: the real limit on how much a sandbox can store is the
// §3.4 disk cap, enforced by the filesystem quota, and a full quota surfaces as
// 507 from the guest. This exists so a runaway or malformed request cannot stream
// unbounded bytes through the engine before the quota notices.
const maxUploadBytes = 8 << 30 // 8 GiB

// fileParams pulls the guest path off the query string. Guest-absolute paths, no
// confinement — same contract as the fs RPCs (see cmd/sandboxd/fs.go for why).
func fileParams(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "missing ?path=", http.StatusBadRequest)
		return "", false
	}
	return p, true
}

// bypassStatus maps a transport-client error to an HTTP status. The inner client
// already normalized the guest's status into a Connect code, so this is the same
// classification the RPC edge reports — one contract, two encodings.
func bypassStatus(w http.ResponseWriter, err error) {
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		http.Error(w, err.Error(), http.StatusNotFound)
	case connect.CodePermissionDenied:
		http.Error(w, err.Error(), http.StatusForbidden)
	case connect.CodeResourceExhausted:
		// The §3.4 disk cap: valid request, no room. The sandbox stays alive (§2.3).
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case connect.CodeInvalidArgument:
		http.Error(w, err.Error(), http.StatusBadRequest)
	case connect.CodeUnavailable:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (e *Engine) handleFileDownload(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	path, ok := fileParams(w, r)
	if !ok {
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	body, err := cl.OpenRead(r.Context(), path)
	if err != nil {
		bypassStatus(w, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	// No Content-Length: it would require asking the guest for a size first, and
	// the file could change between that answer and this transfer. Chunked instead
	// — an honest "unknown length" beats a length that might be wrong.
	if _, err := io.Copy(w, body); err != nil {
		// Headers are already sent, so this cannot become a status. Log it: the
		// usual cause is the caller hanging up mid-download.
		log.Printf("files: download %s from %s interrupted: %v", path, id, err)
	}
}

func (e *Engine) handleFileUpload(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	path, ok := fileParams(w, r)
	if !ok {
		return
	}
	// Check the declared length BEFORE reading a byte, so an oversized upload is
	// refused up front rather than after the engine has already streamed it.
	if r.ContentLength > maxUploadBytes {
		http.Error(w, "upload exceeds the engine's per-request limit", http.StatusRequestEntityTooLarge)
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// And cap the actual stream too: ContentLength is a claim, and a chunked body
	// does not set it at all.
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	n, err := cl.WriteStream(r.Context(), path, body)
	if err != nil {
		bypassStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"path":          path,
		"bytes_written": strconv.FormatInt(n, 10),
	})
}
