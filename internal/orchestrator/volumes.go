package orchestrator

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/store"
)

// The §6.1 Volume block: create / list / inspect / patch / delete + the two direct
// content endpoints.
//
// Volumes are addressed by opaque ID and NOTHING here exposes a host path (§6.2):
// the backend resolves storage internally, so the wire carries an ID, a byte count
// and a root-relative path, never a location on the host.
//
// import/export need NO sandbox, and that is the point rather than a convenience:
// together with create they are how "a volume's lifecycle is decoupled from any
// sandbox" (§5.C) becomes true in substance rather than only in the record layer —
// seeding a volume before any sandbox exists, and reading one after the last sandbox
// is gone, have no other route.

type volumeCreateReq struct {
	// Size is the capacity cap in bytes. REQUIRED while the disk axis is metered
	// (§3.4 / §6.2 conditionally-required-per-axis): an uncapped volume would
	// contribute nothing to the node budget while free to consume all of it, so a
	// missing size is a 400 rather than a silently-defaulted value. Omitted / 0 =
	// uncapped, legal only on an unmetered axis.
	Size int64 `json:"size"`
	// There is deliberately NO access_mode field. It is a property of the MOUNT ROW,
	// not of the volume — one volume can be shared-rw to one sandbox and shared-ro to
	// another at the same time — so it appears only in a sandbox's `volumes` binding
	// (§5.C / §6.1).
}

type volumeResp struct {
	VolumeID string `json:"volume_id"`
	// Size is the cap FROM THE RECORD, which is its only authority: the storage
	// layer's read-back cannot distinguish "never capped" from "deliberately
	// uncapped" (volume.md D7.3), so reporting the record is what keeps inspect
	// honest about what the engine PROMISED — the same number the admission gate
	// counts.
	Size    int64  `json:"size"`
	Backend string `json:"backend,omitempty"`
	// UsedByte is the backend's own statement of consumed bytes. Omitted when it
	// could not be read, rather than reported as 0 — a zero would read as "empty",
	// and the caller uses this number to choose a new cap that must be strictly
	// above it.
	//
	// A POINTER, and that is what makes the sentence above TRUE rather than merely
	// intended: with `int64` plus omitempty, a genuine 0 also serializes as absent,
	// so "empty" and "unreadable" become one answer — the collision this key's whole
	// rationale exists to prevent. Not hypothetical: the plain-directory arm reports
	// exactly 0 for a fresh volume, and on the §6.1 SANDBOX side a fresh uncapped
	// upper+work walk is 0 in the common case.
	UsedByte *int64 `json:"used_byte,omitempty"`
}

// registerVolumeRoutes mounts the Volume block on the control-plane mux.
func (e *Engine) registerVolumeRoutes(mux *http.ServeMux) {
	// POST /v1/volumes ; GET /v1/volumes
	mux.HandleFunc("/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			e.handleCreateVolume(w, r)
		case http.MethodGet:
			e.handleListVolumes(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// /v1/volumes/{id}[/import|/export]
	mux.HandleFunc("/v1/volumes/", e.handleVolumeSub)
}

func (e *Engine) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Size < 0 {
		http.Error(w, "size must be non-negative (0 = uncapped)", http.StatusBadRequest)
		return
	}
	id, err := e.Mediator.CreateVolume(r.Context(), req.Size)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, volumeResp{VolumeID: string(id), Size: req.Size})
}

func (e *Engine) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	list, err := e.Mediator.ListVolumes(r.Context())
	if err != nil {
		statusFor(w, err)
		return
	}
	// List reports the RECORD only, with no per-volume usage read: usage costs one
	// backend round trip each, and a list is the one place where that multiplies by
	// the number of volumes. A caller who needs usage inspects the volume.
	out := make([]volumeResp, 0, len(list))
	for _, v := range list {
		out = append(out, volumeResp{VolumeID: string(v.ID), Size: v.CapByte, Backend: v.Backend})
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleVolumeSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/volumes/")
	parts := strings.SplitN(rest, "/", 2)
	id := model.VolumeID(parts[0])
	if id == "" {
		http.Error(w, "missing volume id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		e.handleInspectVolume(w, r, id)
	case sub == "" && r.Method == http.MethodPatch:
		e.handlePatchVolume(w, r, id)
	case sub == "" && r.Method == http.MethodDelete:
		e.handleDeleteVolume(w, r, id)
	case sub == "snapshot" && r.Method == http.MethodPost:
		e.handleDeriveVolume(w, r, id, false)
	case sub == "clone" && r.Method == http.MethodPost:
		e.handleDeriveVolume(w, r, id, true)
	case sub == "import" && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		e.handleImportVolume(w, r, id)
	case sub == "export" && r.Method == http.MethodGet:
		e.handleExportVolume(w, r, id)
	// §6.2's general rule, mirroring handleSandboxSub: a wrong method on a route that
	// EXISTS is 405. It has to enumerate the known subs rather than blanket the default
	// arm, because the two verdicts stay distinguishable — an unrecognized sub-path is
	// still 404 below, and answering it 405 would assert a route the engine does not have.
	case sub == "", sub == "snapshot", sub == "clone", sub == "import", sub == "export":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (e *Engine) handleInspectVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	info, err := e.Mediator.GetVolume(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, volumeResp{
		VolumeID: string(info.Volume.ID),
		Size:     info.Volume.CapByte,
		Backend:  info.Volume.Backend,
		UsedByte: info.UsedByte,
	})
}

// volumePatchReq is the §6.1 PATCH body. Size is a POINTER so "omitted" is
// distinguishable from "set to 0", which matters because 0 is itself an instruction
// here (remove the cap) rather than an absence.
type volumePatchReq struct {
	Size *int64 `json:"size"`
}

// handlePatchVolume is PATCH /v1/volumes/{id} {size} — the cap's only write point
// besides create (§6.1).
//
// It works whether or not anything has the volume mounted, which is the exact
// OPPOSITE of a sandbox cap (editable only while `paused`), so there is deliberately
// no "pause it first" 409 here. The reason is mechanical: a sandbox cap
// re-materializes at the next placement, while a volume has no placement of its own
// — "defer to the next placement" would mean never for an unmounted volume (§5.C).
//
// A raise re-clears admission and may be 507; lowering below current usage is 400
// (strict inequality — see lifecycle.ErrVolumeCapBelowUsage for why the engine
// refuses what the substrate would accept). It emits no event: this is not a §4.1
// transition.
func (e *Engine) handlePatchVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	var req volumePatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Size == nil {
		http.Error(w, "size is required (it is the only editable field)", http.StatusBadRequest)
		return
	}
	if *req.Size < 0 {
		http.Error(w, "size must be non-negative (0 = uncapped)", http.StatusBadRequest)
		return
	}
	if err := e.Mediator.SetVolumeCapacity(r.Context(), id, *req.Size); err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, volumeResp{VolumeID: string(id), Size: *req.Size})
}

func (e *Engine) handleDeleteVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	if err := e.Mediator.DeleteVolume(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeriveVolume is POST /v1/volumes/{id}/snapshot and .../clone (§6.1).
//
// BOTH TAKE `size` AND NEITHER INHERITS, for two independent reasons: the substrate has
// nothing to inherit (a snapshot gets a brand-new UNCAPPED quota row — measured, and
// verified by writing 3× the source's cap into one), and inheriting would silently
// DOUBLE the node's declared disk claim at every snapshot without the caller asking.
// §3.4 counts the DECLARED cap precisely because the real bytes are metadata-level.
//
// So `size` is conditionally required exactly as on create — the same §6.2 rule
// appearing at its second and third endpoint, not a new rule. A backend that does not
// support the operation is 400, and the capability is asked BEFORE any bytes move.
//
// The result is an ORDINARY WRITABLE volume carrying `parent`; there is no read-only
// snapshot type (§5.C / volume.md D6.1a). Immutability is expressed by a shared-ro
// mount row.
func (e *Engine) handleDeriveVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID, isClone bool) {
	var req volumeCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Size < 0 {
		http.Error(w, "size must be non-negative (0 = uncapped)", http.StatusBadRequest)
		return
	}
	var newID model.VolumeID
	var err error
	if isClone {
		newID, err = e.Mediator.CloneVolume(r.Context(), id, req.Size)
	} else {
		newID, err = e.Mediator.SnapshotVolume(r.Context(), id, req.Size)
	}
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, volumeResp{VolumeID: string(newID), Size: req.Size})
}

type importResp struct {
	VolumeID     string `json:"volume_id"`
	Path         string `json:"path"`
	BytesWritten int64  `json:"bytes_written"`
}

// handleImportVolume is POST /v1/volumes/{id}/import?path= (§6.1 direct write).
//
// The body STREAMS to the backend through a fixed buffer — it is never buffered
// whole. That is the same reason the sandbox large-file route is REST rather than
// RPC (§6.1): a file must not become one message, nor sit in memory at either end.
//
// `path` is relative to the VOLUME ROOT and out-of-root targets are unreachable —
// the accessor is root-confined by construction (§5.C / volume.md D5.1), so a
// traversal clamps inside the volume rather than being rejected by a check here.
// Validating the string would be the weaker design: it would have to be right, where
// the accessor cannot be wrong.
func (e *Engine) handleImportVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required (relative to the volume root)", http.StatusBadRequest)
		return
	}
	n, err := e.Mediator.ImportVolume(r.Context(), id, path, r.Body)
	if err != nil {
		// `path` is echoed: it is the caller's OWN parameter, so handing it back tells
		// them nothing about the host. Everything else path-shaped in the error was
		// authored below the API and is redacted (see sanitizeErr). Note the accessor is
		// root-confined, so the string the mediator reports here can differ from what
		// was sent — a traversal clamps — and only the exact token given is echoed.
		statusFor(w, err, path)
		return
	}
	writeJSON(w, http.StatusOK, importResp{VolumeID: string(id), Path: path, BytesWritten: n})
}

// handleExportVolume is GET /v1/volumes/{id}/export?path= (§6.1 direct read),
// streaming for the same reason import does.
func (e *Engine) handleExportVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required (relative to the volume root)", http.StatusBadRequest)
		return
	}
	// The status line has to be committed before the first byte, so a mid-stream
	// failure cannot become a 4xx/5xx — it can only truncate the response. Content-Type
	// is set accordingly, and the error is logged rather than written into the body,
	// where it would be indistinguishable from volume content.
	w.Header().Set("Content-Type", "application/octet-stream")
	n, err := e.Mediator.ExportVolume(r.Context(), id, path, w)
	if err != nil && n == 0 {
		// Nothing written yet: a real status code is still possible. `path` is echoed for
		// the same reason as on import — it is the caller's own parameter.
		statusFor(w, err, path)
		return
	}
	if err != nil {
		// Truncated. The response is already 200 with partial bytes, so the only honest
		// signal left is the log — inventing a trailer the caller does not read, or
		// appending an error message into the byte stream, would be worse.
		log.Printf("volumes: export %s from %s truncated after %d bytes: %v", path, id, n, err)
	}
}

// volumeStatus maps the §5.C / §6.2 Volume verdicts onto status codes. Called from
// statusFor so the mapping lives beside the endpoints that produce it.
//
// ALL FOUR STORE VERDICTS ARE MAPPED HERE EXPLICITLY, and they must not be folded
// into a shared "in use" arm: §6.2 requires them to stay distinguishable, and the
// whole reason each has its own sentinel is that a 409 naming the wrong object sends
// the caller to look at something unrelated. Before these arms existed, every one of
// them reached statusFor's default and became a 500 — which is exactly the defect the
// mounts.sandbox translation point was added to fix, reappearing one layer up.
func volumeStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, store.ErrVolumeHasDescendants):
		// The FOURTH FK path: a live snapshot or clone names this volume as its parent.
		// 409, and DISTINCT from ErrVolumeInUse because the remedy differs — delete the
		// descendant rather than detach a mount (§6.1).
		return http.StatusConflict, true
	case errors.Is(err, store.ErrVolumeInUse):
		// ① delete a volume a live mount row holds. A state conflict the caller resolves
		// by detaching or destroying the holder.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrSandboxHasMounts):
		// ② delete a sandbox a live mount row holds. Reachable only if a caller bypasses
		// the mediator's destroy (which deletes the rows first), so it is a genuine
		// 409 rather than an internal error.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrMountLeaseConflict):
		// ③ attach lost the exclusive lease — RETRYABLE once the holder releases.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrMountBindingInvalid):
		// 400, NOT one of the four 409s (§6.2): a single submitted binding contradicting
		// itself is decidable without reading another row, and no retry or state change
		// can make it legal. §6.2's four 409s do not grow.
		return http.StatusBadRequest, true
	}
	switch {
	case errors.Is(err, lifecycle.ErrVolumeCapBelowUsage):
		// 400, NOT 507: nothing is full, and no retry or state change makes this legal.
		// The engine refuses it even though the substrate would accept it, because
		// accepting manufactures a state the control plane cannot leave (§5.C /
		// volume.md D7.3).
		return http.StatusBadRequest, true
	case errors.Is(err, lifecycle.ErrVolumeLiveWriter):
		// §6.2's FOURTH Volume 409: the write regime refused a direct change because a
		// live read-write mount holds the volume. 409 rather than 400 because it is
		// RETRYABLE — pausing the holder makes the same request succeed, which is the
		// documented remedy (and a `paused` sandbox is deliberately NOT a live writer).
		return http.StatusConflict, true
	case errors.Is(err, lifecycle.ErrVolumeDirectWriteInFlight):
		// The SAME §6.2 category as ErrVolumeLiveWriter, refused from the other side: a
		// cold start would mount read-write a volume with a direct write in flight
		// (§5.C's symmetric write regime). 409 for the same reason — it is retryable,
		// and the remedy is waiting for that import to finish. Without this arm the
		// refusal reaches the default arm as a 500, reporting a correctly-refused verb
		// as an engine fault.
		return http.StatusConflict, true
	case errors.Is(err, lifecycle.ErrVolumeCapUnsupported):
		// 400: the backend genuinely cannot enforce a cap (§6.2 capability query).
		// Refused rather than silently run uncapped, which would leave every §2.3
		// blast-radius assertion trusting a limit that is not there.
		return http.StatusBadRequest, true
	}
	return 0, false
}
