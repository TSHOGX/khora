package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

// The control-plane API Volume block: create / list / inspect / patch / delete + the two direct
// content endpoints.
//
// Volumes are addressed by opaque ID and NOTHING here exposes a host path (the API error contract):
// the backend resolves storage internally, so the wire carries an ID, a byte count
// and a root-relative path, never a location on the host.
//
// import/export need NO sandbox, and that is the point rather than a convenience:
// together with create they are how "a volume's lifecycle is decoupled from any
// sandbox" (the Volume subsystem) becomes true in substance rather than only in the record layer —
// seeding a volume before any sandbox exists, and reading one after the last sandbox
// is gone, have no other route.

type volumeCreateReq struct {
	// Size is the capacity cap in bytes. REQUIRED while the disk axis is metered
	// (resource admission / the API error contract conditionally-required-per-axis): an uncapped volume would
	// contribute nothing to the node budget while free to consume all of it, so a
	// missing size is a 400 rather than a silently-defaulted value. Omitted / 0 =
	// uncapped, legal only on an unmetered axis.
	Size int64 `json:"size"`
	// There is deliberately NO access_mode field. It is a property of the MOUNT ROW,
	// not of the volume — one volume can be shared-rw to one sandbox and shared-ro to
	// another at the same time — so it appears only in a sandbox's `volumes` binding
	// (the Volume subsystem / the control-plane API).
}

type volumeResp struct {
	VolumeID string `json:"volume_id"`
	// Size is the cap FROM THE RECORD, which is its only authority: the storage
	// layer's read-back cannot distinguish "never capped" from "deliberately
	// uncapped" (volume cap enforcement), so reporting the record is what keeps inspect
	// honest about what the engine PROMISED — the same number the admission gate
	// counts.
	Size    int64  `json:"size"`
	Backend string `json:"backend,omitempty"`
	// UsedByte is the backend's own statement of consumed bytes. Omitted when it could not
	// be read, rather than reported as 0 — a zero would read as "empty", and the caller
	// uses this number to choose a new cap that must be strictly above it.
	//
	// A POINTER, which is what makes that TRUE rather than merely intended: with `int64`
	// plus omitempty a genuine 0 also serializes as absent, collapsing "empty" and
	// "unreadable" into one answer. Not hypothetical — the plain-directory arm reports
	// exactly 0 for a fresh volume.
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
	id, err := e.Mediator.CreateVolumeWithKey(r.Context(), req.Size, model.IdempotencyKey(strings.TrimSpace(r.Header.Get("Idempotency-Key"))))
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
	// The RECORD only, no per-volume usage read: usage costs one backend round trip each,
	// multiplied here by the volume count. A caller who needs it inspects the volume.
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
	case sub == "stat" && r.Method == http.MethodGet:
		e.handleStatVolume(w, r, id)
	case sub == "lstat" && r.Method == http.MethodGet:
		e.handleLstatVolume(w, r, id)
	case sub == "list" && r.Method == http.MethodGet:
		e.handleListVolumePath(w, r, id)
	case sub == "read" && r.Method == http.MethodGet:
		e.handleReadVolume(w, r, id)
	case sub == "write" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		e.handleWriteVolume(w, r, id)
	case sub == "mkdir" && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		e.handleMkdirVolume(w, r, id)
	case sub == "move" && r.Method == http.MethodPost:
		e.handleMoveVolume(w, r, id)
	case sub == "remove" && (r.Method == http.MethodDelete || r.Method == http.MethodPost):
		e.handleRemoveVolume(w, r, id)
	// the API error contract's general rule, mirroring handleSandboxSub: a wrong method on a route that
	// EXISTS is 405. It enumerates the known subs rather than blanketing the default arm —
	// an unrecognized sub-path is still 404 below, and answering it 405 would assert a
	// route the engine does not have.
	case sub == "", sub == "snapshot", sub == "clone", sub == "import", sub == "export", sub == "stat", sub == "lstat", sub == "list", sub == "read", sub == "write", sub == "mkdir", sub == "move", sub == "remove":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

type volumeFileResp struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Mode          uint32 `json:"mode"`
	IsDir         bool   `json:"is_dir"`
	Type          string `json:"type"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	ModTime       string `json:"mod_time"`
}

func volumeFileResponse(fi volume.FileInfo) volumeFileResp {
	return volumeFileResp{Name: fi.Name, Size: fi.Size, Mode: uint32(fi.Mode), IsDir: fi.IsDir, Type: string(fi.Type), SymlinkTarget: fi.SymlinkTarget, ModTime: fi.ModTime.UTC().Format(time.RFC3339Nano)}
}

func volumePathQuery(r *http.Request) (string, error) {
	name := r.URL.Query().Get("path")
	if name == "" {
		return "", errors.New("path is required (relative to the volume root)")
	}
	return name, nil
}

func (e *Engine) handleStatVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	fi, err := e.Mediator.StatVolume(r.Context(), id, name)
	if err != nil {
		statusFor(w, err, name)
		return
	}
	writeJSON(w, http.StatusOK, volumeFileResponse(fi))
}

func (e *Engine) handleLstatVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	fi, err := e.Mediator.LstatVolume(r.Context(), id, name)
	if err != nil {
		statusFor(w, err, name)
		return
	}
	writeJSON(w, http.StatusOK, volumeFileResponse(fi))
}

func (e *Engine) handleListVolumePath(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name := r.URL.Query().Get("path")
	if name == "" {
		name = "."
	}
	list, err := e.Mediator.ListVolume(r.Context(), id, name)
	if err != nil {
		statusFor(w, err, name)
		return
	}
	out := make([]volumeFileResp, 0, len(list))
	for _, fi := range list {
		out = append(out, volumeFileResponse(fi))
	}
	writeJSON(w, http.StatusOK, out)
}

func parseIntQuery(r *http.Request, key string, def int64) (int64, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}

func (e *Engine) handleReadVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	offset, err := parseIntQuery(r, "offset", 0)
	if err != nil {
		statusFor(w, err)
		return
	}
	length, err := parseIntQuery(r, "length", 0)
	if err != nil {
		statusFor(w, err)
		return
	}
	data, err := e.Mediator.ReadVolume(r.Context(), id, name, offset, length)
	if err != nil {
		statusFor(w, err, name)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (e *Engine) handleWriteVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	offset, err := parseIntQuery(r, "offset", 0)
	if err != nil {
		statusFor(w, err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil {
		statusFor(w, err)
		return
	}
	if len(data) > 64<<20 {
		http.Error(w, "write body exceeds 64 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	n, err := e.Mediator.WriteVolume(r.Context(), id, name, offset, data)
	if err != nil {
		statusFor(w, err, name)
		return
	}
	writeJSON(w, http.StatusOK, importResp{VolumeID: string(id), Path: name, BytesWritten: n})
}

func (e *Engine) handleMkdirVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	perm, err := parseIntQuery(r, "mode", 0o755)
	if err != nil || perm < 0 {
		if err == nil {
			err = errors.New("mode must be non-negative")
		}
		statusFor(w, err)
		return
	}
	fi, err := e.Mediator.MkdirVolume(r.Context(), id, name, fs.FileMode(perm))
	if err != nil {
		statusFor(w, err, name)
		return
	}
	writeJSON(w, http.StatusCreated, volumeFileResponse(fi))
}

type volumeMoveReq struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

func (e *Engine) handleMoveVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	var req volumeMoveReq
	if err := strictDecode(r.Body, &req); err != nil {
		statusFor(w, err)
		return
	}
	if req.Source == "" || req.Destination == "" {
		statusFor(w, errors.New("source and destination are required"))
		return
	}
	fi, err := e.Mediator.MoveVolume(r.Context(), id, req.Source, req.Destination)
	if err != nil {
		statusFor(w, err, req.Source, req.Destination)
		return
	}
	writeJSON(w, http.StatusOK, volumeFileResponse(fi))
}

func (e *Engine) handleRemoveVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	name, err := volumePathQuery(r)
	if err != nil {
		statusFor(w, err)
		return
	}
	if err := e.Mediator.RemoveVolumePath(r.Context(), id, name); err != nil {
		statusFor(w, err, name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// volumePatchReq is the control-plane API PATCH body. Size is a POINTER so "omitted" is
// distinguishable from "set to 0", which matters because 0 is itself an instruction
// here (remove the cap) rather than an absence.
type volumePatchReq struct {
	Size *int64 `json:"size"`
}

// handlePatchVolume is PATCH /v1/volumes/{id} {size} — the cap's only write point
// besides create (the control-plane API).
//
// It works whether or not anything has the volume mounted — the exact OPPOSITE of a
// sandbox cap (editable only while `paused`), so no "pause it first" 409 here. The reason
// is mechanical: a sandbox cap re-materializes at the next placement, while a volume has
// no placement of its own, so "defer to the next placement" would mean never for an
// unmounted volume (the Volume subsystem).
//
// A raise re-clears admission and may be 507; lowering below current usage is 400
// (strict inequality — see lifecycle.ErrVolumeCapBelowUsage for why the engine
// refuses what the substrate would accept). It emits no event: this is not a lifecycle state machine
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

// handleDeriveVolume is POST /v1/volumes/{id}/snapshot and .../clone (the control-plane API).
//
// BOTH TAKE `size` AND NEITHER INHERITS, for two independent reasons: the substrate has
// nothing to inherit (a snapshot gets a brand-new UNCAPPED quota row — measured, and
// verified by writing 3× the source's cap into one), and inheriting would silently
// DOUBLE the node's declared disk claim at every snapshot without the caller asking.
// resource admission counts the DECLARED cap precisely because the real bytes are metadata-level.
//
// So `size` is conditionally required exactly as on create — the same the API error contract rule
// appearing at its second and third endpoint, not a new rule. A backend that does not
// support the operation is 400, and the capability is asked BEFORE any bytes move.
//
// The result is an ORDINARY WRITABLE volume carrying `parent`; there is no read-only
// snapshot type (the Volume subsystem / snapshot lineage). Immutability is expressed by a shared-ro
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
		newID, err = e.Mediator.CloneVolumeWithKey(r.Context(), id, req.Size, model.IdempotencyKey(strings.TrimSpace(r.Header.Get("Idempotency-Key"))))
	} else {
		newID, err = e.Mediator.SnapshotVolumeWithKey(r.Context(), id, req.Size, model.IdempotencyKey(strings.TrimSpace(r.Header.Get("Idempotency-Key"))))
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

// handleImportVolume is POST /v1/volumes/{id}/import?path= (the control-plane API direct write).
//
// The body STREAMS to the backend through a fixed buffer, never buffered whole — the same
// reason the sandbox large-file route is REST rather than RPC (the control-plane API).
//
// `path` is relative to the VOLUME ROOT and out-of-root targets are unreachable — the
// accessor is root-confined by construction, so a traversal clamps
// inside the volume rather than being rejected by a check here. Validating the string would
// have to be right, where the accessor cannot be wrong.
func (e *Engine) handleImportVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required (relative to the volume root)", http.StatusBadRequest)
		return
	}
	n, err := e.Mediator.ImportVolume(r.Context(), id, path, r.Body)
	if err != nil {
		// `path` is echoed: it is the caller's OWN parameter (see sanitizeErr). Only the
		// exact token given — the accessor is root-confined, so a clamped traversal makes the
		// mediator's reported string differ from what was sent.
		statusFor(w, err, path)
		return
	}
	writeJSON(w, http.StatusOK, importResp{VolumeID: string(id), Path: path, BytesWritten: n})
}

// handleExportVolume is GET /v1/volumes/{id}/export?path= (the control-plane API direct read),
// streaming for the same reason import does.
func (e *Engine) handleExportVolume(w http.ResponseWriter, r *http.Request, id model.VolumeID) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required (relative to the volume root)", http.StatusBadRequest)
		return
	}
	// The status line is committed before the first byte, so a mid-stream failure cannot
	// become a 4xx/5xx — only a truncation. The error is logged rather than written into
	// the body, where it would be indistinguishable from volume content.
	w.Header().Set("Content-Type", "application/octet-stream")
	n, err := e.Mediator.ExportVolume(r.Context(), id, path, w)
	if err != nil && n == 0 {
		// Nothing written yet: a real status code is still possible. `path` is echoed for
		// the same reason as on import — it is the caller's own parameter.
		statusFor(w, err, path)
		return
	}
	if err != nil {
		// Truncated. Already 200 with partial bytes, so the log is the only honest signal left.
		log.Printf("volumes: export %s from %s truncated after %d bytes: %v", path, id, n, err)
	}
}

// volumeStatus maps the Volume subsystem / the API error contract Volume verdicts onto status codes. Called from
// statusFor so the mapping lives beside the endpoints that produce it.
//
// ALL FOUR STORE VERDICTS ARE MAPPED EXPLICITLY and must not be folded into a shared "in
// use" arm: the API error contract requires them to stay distinguishable, because a 409 naming the wrong
// object sends the caller to look at something unrelated. Before these arms existed, every
// one of them reached statusFor's default and became a 500.
func volumeStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, true
	case errors.Is(err, fs.ErrPermission):
		return http.StatusForbidden, true
	case errors.Is(err, fs.ErrExist):
		return http.StatusConflict, true
	case errors.Is(err, volume.ErrInvalidProvider):
		// Invalid assembly/configuration is a request/configuration error, not
		// an internal backend failure.
		return http.StatusBadRequest, true
	case errors.Is(err, volume.ErrUnknownProvider), errors.Is(err, volume.ErrProviderUnavailable):
		// A durable row whose provider is not available cannot be safely
		// redirected.  Surface that as unavailable so a caller/operator can
		// repair assembly and retry.
		return http.StatusServiceUnavailable, true
	case errors.Is(err, store.ErrVolumeHasDescendants):
		// The FOURTH FK path: a live snapshot or clone names this volume as its parent.
		// 409, and DISTINCT from ErrVolumeInUse because the remedy differs — delete the
		// descendant rather than detach a mount (the control-plane API).
		return http.StatusConflict, true
	case errors.Is(err, store.ErrVolumeInUse):
		//  delete a volume a live mount row holds. A state conflict the caller resolves
		// by detaching or destroying the holder.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrSandboxHasMounts):
		//  delete a sandbox a live mount row holds. Reachable only if a caller bypasses
		// the mediator's destroy (which deletes the rows first), so it is a genuine
		// 409 rather than an internal error.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrMountLeaseConflict):
		//  attach lost the exclusive lease — RETRYABLE once the holder releases.
		return http.StatusConflict, true
	case errors.Is(err, store.ErrMountBindingInvalid):
		// 400, NOT one of the four 409s (the API error contract): a single submitted binding contradicting
		// itself is decidable without reading another row, and no retry or state change
		// can make it legal. the API error contract's four 409s do not grow.
		return http.StatusBadRequest, true
	}
	switch {
	case errors.Is(err, lifecycle.ErrVolumeCapBelowUsage):
		// 400, NOT 507: nothing is full, and no retry or state change makes this legal.
		// The engine refuses it even though the substrate would accept it, because
		// accepting manufactures a state the control plane cannot leave (the Volume subsystem /
		// volume cap enforcement).
		return http.StatusBadRequest, true
	case errors.Is(err, lifecycle.ErrVolumeLiveWriter):
		// the API error contract's FOURTH Volume 409: the write regime refused a direct change because a
		// live read-write mount holds the volume. 409 rather than 400 because it is
		// RETRYABLE — pausing the holder makes the same request succeed, which is the
		// documented remedy (and a `paused` sandbox is deliberately NOT a live writer).
		return http.StatusConflict, true
	case errors.Is(err, lifecycle.ErrVolumeDirectWriteInFlight):
		// The SAME the API error contract category as ErrVolumeLiveWriter, refused from the other side: a cold
		// start would mount read-write a volume with a direct write in flight (the Volume subsystem's
		// symmetric write regime). 409 for the same reason — retryable, the remedy being to
		// wait for that import to finish.
		return http.StatusConflict, true
	case errors.Is(err, lifecycle.ErrVolumeCapUnsupported):
		// 400: the backend genuinely cannot enforce a cap (the API error contract capability query).
		// Refused rather than silently run uncapped, which would leave every the data-plane exposure boundary
		// blast-radius assertion trusting a limit that is not there.
		return http.StatusBadRequest, true
	}
	return 0, false
}
