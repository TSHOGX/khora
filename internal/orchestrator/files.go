package orchestrator

import (
	"fmt"
	"io"
	"log"
	"net/http"

	"connectrpc.com/connect"

	"warren/internal/model"
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

// uploadResp is the §6.1 upload response. `bytes_written` is typed to match the
// volume import response's field of the same name, so one name means one wire type.
type uploadResp struct {
	Path         string `json:"path"`
	BytesWritten int64  `json:"bytes_written"`
}

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

// bypassCode maps a transport-client error to an HTTP status. The inner client
// already normalized the guest's status into a Connect code, so this is the same
// classification the RPC edge reports — one contract, two encodings.
//
// It returns a CODE and writes nothing, mirroring statusCodeFor: the body is the
// funnel's business (writeErr). That split matters more here than on the control
// paths, because the errors reaching this one are built by the inner transport
// client, which dials the netns veth IP — client.go's `hc.Do` failures carry
// `10.x.y.z:port` in their text, and §6.2 bars placement from the wire exactly as it
// bars host paths. So this REST half sanitizes through the funnel, the same
// guarantee the RPC half gets from proxyErr.
func bypassCode(err error) int {
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		// The §3.4 disk cap: valid request, no room. The sandbox stays alive (§2.3).
		return http.StatusInsufficientStorage
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (e *Engine) handleFileDownload(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	path, ok := fileParams(w, r)
	if !ok {
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	body, err := cl.OpenRead(r.Context(), path)
	if err != nil {
		// The path is stated HERE rather than relied upon from the inner error, and the
		// echo list carries it so the funnel lets it through. Both halves are needed: a
		// transport-level failure's text is net/http's (`Get "http://10.x.y.z:9000/files?
		// path=%2Fetc%2Fx": dial tcp …`), which names the file only inside a URL that is
		// redacted WHOLE — so without this wrap the caller would get a sanitized 5xx that
		// does not say which download failed.
		writeErr(w, bypassCode(err), fmt.Errorf("download %s: %w", path, err), path)
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
		writeErr(w, http.StatusNotFound, err)
		return
	}
	// Barrier-gated like the mutating fs RPCs (§4.2 / D4.4): a streaming upload is
	// the LONGEST engine-issued write there is, so it is the one a pause is most
	// likely to interrupt. The guest's temp-file-then-rename means an interrupted
	// upload cannot leave a partial file, but the drain still matters — it lets the
	// upload FINISH rather than be discarded wholesale.
	release, ok := e.Mediator.Enter(id, "files.upload "+path)
	if !ok {
		http.Error(w, "sandbox is pausing; retry after it settles", http.StatusServiceUnavailable)
		return
	}
	defer release()
	// And cap the actual stream too: ContentLength is a claim, and a chunked body
	// does not set it at all.
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	n, err := cl.WriteStream(r.Context(), path, body)
	if err != nil {
		writeErr(w, bypassCode(err), fmt.Errorf("upload %s: %w", path, err), path)
		return
	}
	// bytes_written is a NUMBER, matching the volume import response's field of the
	// same name (§6.1). One field name must not carry two wire types across two
	// endpoints — a client that parses one and not the other is a defect we would be
	// authoring into the contract.
	writeJSON(w, http.StatusOK, uploadResp{Path: path, BytesWritten: n})
}
