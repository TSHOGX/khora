package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

// HTTPHandler exposes the sandbox-centric control-plane API: ID-addressed, with no
// host paths / placement exposed (the API error contract): sandbox lifecycle
// (create/get/pause/resume/destroy/list) + process.Start + fs read/write, all
// proxied by sandbox ID (the orchestrator resolves the current endpoint
// internally via the mediator, so placement never leaves the engine).
//
// A deliberately small net/http router (no framework). The outer-edge protocol
// split is settled (the outer-edge transport split): the CONTROL plane (sandbox lifecycle) is
// REST/JSON here; the process/fs DATA plane is Connect-RPC (the SAME
// transport.proto as the inner edge, re-proxied by sandbox ID), with no REST or
// NDJSON facade. The two surfaces mount on one mux: Connect
// services own their /khora.transport.v1.*/ paths, control-plane REST owns
// /v1/... .
func (e *Engine) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// --- data plane: Connect-RPC re-proxy (the outer-edge transport split) ---
	mux.Handle(transportv1connect.NewProcessServiceHandler(&processProxy{e: e}))
	mux.Handle(transportv1connect.NewFsServiceHandler(&fsProxy{e: e}))

	// --- operational surface: /healthz, /readyz, /metrics (health.go) ---
	e.registerHealthRoutes(mux)

	// --- control plane: the control-plane API REST/JSON ---
	// POST /v1/sandboxes ; GET /v1/sandboxes
	mux.HandleFunc("/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			e.handleCreate(w, r)
		case http.MethodGet:
			e.handleList(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /v1/sandboxes/{id}[/pause|/resume]  (data-plane verbs are Connect, above)
	mux.HandleFunc("/v1/sandboxes/", e.handleSandboxSub)
	mux.HandleFunc("/v1/processes/", e.handleProcessSub)

	// GET /v1/events?after=&limit= — the transactional outbox event-stream replay (pull only).
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		e.handleEvents(w, r)
	})

	// --- Template lifecycle: the control-plane API REST/JSON (immutable object, the Template and Overlay model/health and reclamation) ---
	// GET /v1/templates (list) ; POST /v1/templates (Promote).
	mux.HandleFunc("/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			e.handleListTemplates(w, r)
		case http.MethodPost:
			e.handlePromote(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// DELETE /v1/templates/{id} (referenced → 409 template in use, health and reclamation).
	mux.HandleFunc("/v1/templates/", e.handleTemplateSub)

	// --- Volume lifecycle: the control-plane API REST/JSON (the Volume subsystem, decoupled from every sandbox) ---
	// create/list/inspect/patch/delete + the two direct content endpoints, which
	// need no sandbox at all (see volumes.go).
	e.registerVolumeRoutes(mux)

	// h2c so Connect server-streams work over cleartext HTTP/2 (the transport contract).
	h := h2c.NewHandler(mux, &http2.Server{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.httpHandlers.enter() {
			http.Error(w, "server is draining", http.StatusServiceUnavailable)
			return
		}
		defer e.httpHandlers.leave()
		h.ServeHTTP(w, r)
	})
}

type createReq struct {
	TemplateID string `json:"template_id"`
	// IdleTimeout / Retention are the optional TTL Binding inputs (the control-plane API
	// idle_timeout?/retention?). Accepted as Go duration strings ("30m", "24h")
	// so the unit is explicit on the wire; omitted/"" → engine-wide default
	// (materialized by the mediator at create).
	IdleTimeout string `json:"idle_timeout"`
	Retention   string `json:"retention"`
	// Resource-limit Binding inputs (the control-plane API / resource admission). Omitted / 0 = unlimited on that
	// axis. Three separate fields, never one blended "size" — the axes and why they
	// are not interchangeable are documented on model.Limits. No io_* field: the disk
	// I/O rate axis joins in the same shape once it has a consumer (resource admission / the runtime provider contract).
	CPUMillis  int64 `json:"cpu_millis"`
	MemoryByte int64 `json:"memory_byte"`
	DiskByte   int64 `json:"disk_byte"`
	// Volumes is the control-plane API `volumes?` binding: the COMPLETE set of volume mounts this
	// sandbox is created with. `access` is given HERE and not on the volume record,
	// because it is a property of the MOUNT (the Volume subsystem).
	Volumes []volumeBindingItem `json:"volumes"`
	// Network is the control-plane API `network?` declaration (the network declaration). A POINTER because omitting
	// the whole object is LEGAL — it normalizes to an explicit {deny, [], [], []} at
	// the write point — while giving the object and leaving out `egress.default` is
	// ClassDefaultMissing. The two share a code path in most implementations, and
	// whichever is handled first eats the other, so the distinction is carried rather
	// than collapsed.
	Network *networkWire `json:"network"`
}

// volumeBindingItem is one entry of a `volumes` binding, on create and on PATCH
// (the control-plane API). One type for both, because the API error contract makes PATCH swap a whole binding for a whole
// binding — the two carry the identical shape, and a second type would let them drift.
type volumeBindingItem struct {
	VolumeID string `json:"volume_id"`
	Path     string `json:"path"`
	Access   string `json:"access"`
}

// parseVolumeBinding converts wire items into mount rows, validating `access` at the
// boundary so an unknown value is a plain 400 rather than a store verdict.
//
// The Sandbox field is deliberately left empty: on create the ID does not exist yet,
// and on PATCH the mediator already knows it — accepting one from the caller could
// only be wrong.
func parseVolumeBinding(items []volumeBindingItem) ([]model.Mount, error) {
	out := make([]model.Mount, 0, len(items))
	for _, it := range items {
		if it.VolumeID == "" {
			return nil, errors.New("volumes: volume_id is required")
		}
		if it.Path == "" {
			return nil, errors.New("volumes: path is required")
		}
		access := model.AccessMode(it.Access)
		if it.Access == "" {
			// No default is invented: guessing would silently pick either an exclusive
			// lease the caller did not ask for or a shared writer they did not intend.
			return nil, errors.New("volumes: access is required (exclusive / shared-ro / shared-rw)")
		}
		if !access.Valid() {
			return nil, errors.New("volumes: access must be one of exclusive / shared-ro / shared-rw, got " + it.Access)
		}
		out = append(out, model.Mount{
			Volume: model.VolumeID(it.VolumeID),
			Path:   it.Path,
			Access: access,
		})
	}
	return out, nil
}

// parseTTL is THE spelling of a wire TTL for both write points — create and PATCH (resource admission
// idle_timeout / retention). One function because the defect it closes is the two write points
// disagreeing: create parsed without a sign check while PATCH refused a negative, so the SAME
// field was legal on one and a 400 on the other.
//
// A NEGATIVE DURATION IS REFUSED because its effect is SILENT: both consumers read a
// non-positive TTL as an OPT-OUT (the reaper returns early on `Retention <= 0`, idle
// classification fires only on `IdleTimeout > 0`), so `-5m` produces NEVER EVICT / NEVER
// IDLE-PAUSE with nothing reporting that the value was not honored. `0` remains legal —
// it is the DOCUMENTED opt-out spelling (the control-plane API `retention: 0` = never evict).
//
// Returns a POINTER because both call sites need one — create's CreateSpec and PATCH's
// store.Binding both distinguish "not given" from "given".
func parseTTL(field, raw string) (*time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	if d < 0 {
		// Through the funnel even though `field` is one of two engine-authored literals:
		// the API error contract guard cannot see that, and an exemption for "this one is obviously
		// fine" is how the rule stops being structural.
		return nil, fmt.Errorf("%s must be non-negative (0 = no deadline; a negative value would read as never-evict / never-idle-pause at every consumer)", field)
	}
	return &d, nil
}

type createResp struct {
	SandboxID string `json:"sandbox_id"`
}

func (e *Engine) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	// UNKNOWN FIELDS REFUSED — a misspelled `network` would otherwise be a silent `deny`
	// sandbox (see strictDecode).
	if err := strictDecode(r.Body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.CPUMillis < 0 || req.MemoryByte < 0 || req.DiskByte < 0 {
		http.Error(w, "resource limits must be non-negative (0 = unlimited)", http.StatusBadRequest)
		return
	}
	binding, err := parseVolumeBinding(req.Volumes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The declaration is parsed and VALIDATED before the create verb runs, so the API error contract's
	// "validation precedes the admission gate" holds: an invalid declaration is
	// refused before any byte is touched or any budget recorded, and 400 and 507 are
	// therefore never both true.
	var decl *model.NetworkDecl
	if req.Network != nil {
		got, derr := req.Network.decode()
		if derr != nil {
			statusFor(w, derr)
			return
		}
		if verr := e.Mediator.ValidateDeclaration(got, true); verr != nil {
			statusFor(w, verr)
			return
		}
		decl = &got
	}
	spec := lifecycle.CreateSpec{
		Template:       model.TemplateID(req.TemplateID),
		IdempotencyKey: model.IdempotencyKey(strings.TrimSpace(r.Header.Get("Idempotency-Key"))),
		Volumes:        binding,
		Network:        decl,
		Limits: model.Limits{
			CPUMillis:  req.CPUMillis,
			MemoryByte: req.MemoryByte,
			DiskByte:   req.DiskByte,
		},
	}
	if req.IdleTimeout != "" {
		d, terr := parseTTL("idle_timeout", req.IdleTimeout)
		if terr != nil {
			writeErr(w, http.StatusBadRequest, terr)
			return
		}
		spec.IdleTimeout = d
	}
	if req.Retention != "" {
		d, terr := parseTTL("retention", req.Retention)
		if terr != nil {
			writeErr(w, http.StatusBadRequest, terr)
			return
		}
		spec.Retention = d
	}
	sid, err := e.Mediator.Create(r.Context(), spec)
	if err != nil {
		// route through statusFor so a bad template_id (store.ErrTemplateNotFound,
		// the health and reclamation reference side) surfaces as 404, not a raw 500.
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createResp{SandboxID: string(sid)})
}

func (e *Engine) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := e.Mediator.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Same shape as the single-sandbox read, death_cause included — see
	// sandboxResp for why List carries it rather than forcing a GET per ID.
	out := make([]sandboxResp, 0, len(list))
	for _, s := range list {
		resp := sandboxResp{
			SandboxID:  string(s.ID),
			State:      string(s.State),
			DeathCause: string(s.DeathCause),
		}
		if placement, err := e.Store.GetPlacement(r.Context(), s.ID); err == nil {
			resp.PlacementEpoch = uint64(placement.Epoch)
			resp.PlacementState = string(placement.State)
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type promoteReq struct {
	FromSandboxID string                  `json:"from_sandbox_id"`
	Manifest      *model.TemplateManifest `json:"manifest,omitempty"`
}
type promoteResp struct {
	TemplateID string `json:"template_id"`
}

// handlePromote is POST /v1/templates {from_sandbox_id} → {template_id} (the control-plane API
// Promote): freeze the sandbox's current Overlay into a new Template (the Template and Overlay model).
func (e *Engine) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req promoteReq
	if err := strictDecode(r.Body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FromSandboxID == "" {
		http.Error(w, "from_sandbox_id is required", http.StatusBadRequest)
		return
	}
	tid, err := e.Mediator.PromoteWithManifest(r.Context(), model.SandboxID(req.FromSandboxID), req.Manifest)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, promoteResp{TemplateID: string(tid)})
}

func (e *Engine) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := e.Mediator.ListTemplates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]templateResp, 0, len(list))
	for _, t := range list {
		out = append(out, templateResp{
			TemplateID: string(t.ID),
			Parent:     string(t.Parent),
			Builtin:    t.Builtin,
			Manifest:   t.Manifest,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// templateResp is one entry of GET /v1/templates (the control-plane API). A NAMED type rather than an
// inline map, because the generated OpenAPI document derives this operation's schema by
// reflection (openapi.go) and a map is invisible to that.
type templateResp struct {
	TemplateID string `json:"template_id"`
	// Parent is the immediate parent Template in the immutable lineage, "" for a builtin.
	Parent   string                 `json:"parent"`
	Builtin  bool                   `json:"builtin"`
	Manifest model.TemplateManifest `json:"manifest"`
}

func (e *Engine) handleTemplateSub(w http.ResponseWriter, r *http.Request) {
	// path: /v1/templates/{id}
	id := model.TemplateID(strings.TrimPrefix(r.URL.Path, "/v1/templates/"))
	if id == "" {
		http.Error(w, "missing template id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		tm, err := e.Mediator.GetTemplate(r.Context(), id)
		if err != nil {
			statusFor(w, err)
			return
		}
		writeJSON(w, http.StatusOK, templateResp{TemplateID: string(tm.ID), Parent: string(tm.Parent), Builtin: tm.Builtin, Manifest: tm.Manifest})
	case http.MethodDelete:
		if err := e.Mediator.DeleteTemplate(r.Context(), id); err != nil {
			statusFor(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (e *Engine) handleSandboxSub(w http.ResponseWriter, r *http.Request) {
	// path: /v1/sandboxes/{id}[/{verb}...]
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
	parts := strings.SplitN(rest, "/", 2)
	id := model.SandboxID(parts[0])
	if id == "" {
		http.Error(w, "missing sandbox id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		e.handleGet(w, r, id)
	case sub == "" && r.Method == http.MethodPatch:
		e.handlePatch(w, r, id)
	case sub == "" && r.Method == http.MethodDelete:
		e.handleDestroy(w, r, id)
	case sub == "pause" && r.Method == http.MethodPost:
		e.doVerb(w, r, id, e.Mediator.Pause)
	case sub == "resume" && r.Method == http.MethodPost:
		e.doVerb(w, r, id, e.Mediator.Resume)
	// The large-file bypass (the transport data plane fs / fs transport). REST rather than
	// Connect-RPC because its whole purpose is to STREAM bytes: a file must not
	// become one protobuf message, nor sit in memory at either end.
	case sub == "files" && r.Method == http.MethodGet:
		e.handleFileDownload(w, r, id)
	case sub == "files" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		e.handleFileUpload(w, r, id)
	// the control-plane API's Network block. `network` is GET-only and a wrong method there is a 405
	// by the API error contract general rule, NOT a 404 landing in the default arm below: the
	// collection exists, the method does not apply to it.
	case sub == "network" && r.Method == http.MethodGet:
		e.handleNetworkGet(w, r, id)
	case sub == "network":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case sub == "publish" && r.Method == http.MethodPost:
		e.handlePublish(w, r, id)
	case sub == "publish":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case sub == "activity" && r.Method == http.MethodPost:
		e.handleActivity(w, r, id)
	case sub == "telemetry" && r.Method == http.MethodGet:
		e.handleTelemetry(w, r, id)
	case sub == "snapshot" && r.Method == http.MethodGet:
		e.handleSandboxSnapshot(w, r, id)
	case sub == "placement" && r.Method == http.MethodGet:
		e.handlePlacementSnapshot(w, r, id)
	case sub == "processes" && r.Method == http.MethodGet:
		e.handleSandboxProcesses(w, r, id)
	case sub == "activity":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	// The same the API error contract general rule for the older routes. They landed in `default` as 404,
	// which contradicts the API error contract ("a wrong method on ANY collection/object route → 405")
	// and was inconsistent with the two arms above for no reason anyone chose: the
	// route exists, the method does not apply to it.
	case sub == "files", sub == "pause", sub == "resume", sub == "telemetry", sub == "snapshot", sub == "placement":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		// process/fs data plane is Connect-RPC (the outer-edge transport split), not REST here.
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (e *Engine) handleSandboxSnapshot(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	s, err := e.Mediator.SandboxSnapshot(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (e *Engine) handlePlacementSnapshot(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	s, err := e.Mediator.PlacementSnapshot(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (e *Engine) handleSandboxProcesses(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	// Both Store backends return an empty slice for an unknown scope. Check the
	// authoritative Sandbox row first so an unknown object is a deterministic
	// 404 rather than a misleading successful empty collection.
	if _, err := e.Store.GetSandbox(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	list, err := e.Store.ListProcesses(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (e *Engine) handleProcessSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := model.ProcessID(strings.TrimPrefix(r.URL.Path, "/v1/processes/"))
	if id == "" {
		http.Error(w, "missing process id", http.StatusBadRequest)
		return
	}
	s, err := e.Mediator.ProcessSnapshot(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (e *Engine) handleTelemetry(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	after, err := parseIntQuery(r, "after", 0)
	if err != nil || after < 0 {
		if err == nil {
			err = model.NewContractError(model.ErrorInvalidArgument, "after must be non-negative")
		}
		statusFor(w, err)
		return
	}
	limit, err := parseIntQuery(r, "limit", 100)
	if err != nil || limit <= 0 {
		if err == nil {
			err = model.NewContractError(model.ErrorInvalidArgument, "limit must be positive")
		}
		statusFor(w, err)
		return
	}
	if limit > 1000 {
		limit = 1000
	}
	samples, earliest, latest, err := e.Mediator.Telemetry(r.Context(), id, uint64(after), int(limit))
	if errors.Is(err, model.ErrCursorExpired) {
		writeJSON(w, http.StatusGone, struct {
			Samples  []model.TelemetrySample `json:"samples"`
			Earliest uint64                  `json:"earliest"`
			Latest   uint64                  `json:"latest"`
		}{[]model.TelemetrySample{}, earliest, latest})
		return
	}
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Samples  []model.TelemetrySample `json:"samples"`
		Earliest uint64                  `json:"earliest"`
		Latest   uint64                  `json:"latest"`
	}{samples, earliest, latest})
}

// patchReq is the control-plane API PATCH body — rewrite a sandbox's Binding (resource admission). Every
// field is a POINTER so "omitted" is distinguishable from "set to zero", which
// matters because zero is itself meaningful on these axes (`0 = unlimited` for a
// cap, `retention: 0` = never evict). A value type could not express "leave this
// one alone".
type patchReq struct {
	CPUMillis   *int64  `json:"cpu_millis"`
	MemoryByte  *int64  `json:"memory_byte"`
	DiskByte    *int64  `json:"disk_byte"`
	IdleTimeout *string `json:"idle_timeout"`
	Retention   *string `json:"retention"`
	// Volumes is the mount table's COMPLETE binding (the control-plane API) — an empty array means
	// DETACH EVERYTHING, which is why the pointer's "omitted vs given as empty"
	// distinction carries weight here. On PATCH rather than separate attach/detach
	// endpoints because the API error contract defines PATCH as THE rebinding point: a sandbox holds one
	// complete binding at all times, and PATCH swaps one whole binding for another.
	Volumes *[]volumeBindingItem `json:"volumes"`
	// Network is the network declaration, swapped WHOLE for WHOLE (the API error contract — no field-level
	// merge, no pending value). The pointer carries real weight: the zero declaration is
	// not "no change", it is `{deny, [], [], []}` — the most restrictive there is — so a
	// value type would make every unrelated PATCH silently reset the sandbox to
	// deny-everything.
	//
	// `applies_at: next-resume`, and editable only from `paused` (the mediator enforces
	// the state gate): the provider materializes it at placement, so accepting the edit
	// on `running` would leave the record and the live rule set disagreeing indefinitely.
	//
	// PUBLISH DOES NOT COME THROUGH HERE — it has its own hot-add endpoint — but it CAN
	// be removed through here, since there is no un-publish endpoint. the control-plane API makes the
	// read-modify-write REQUIRED rather than advisory for that reason: a binding built
	// from a stale GET silently drops a publish added in between, and the reverse
	// (writing back an already-removed publish) reopens an inbound port.
	Network *networkWire `json:"network"`
}

type patchResp struct {
	SandboxID string `json:"sandbox_id"`
	State     string `json:"state"`
	// AppliesAt tells the caller WHEN the change takes effect rather than leaving them to
	// infer it: "now" for TTL fields (the reaper re-reads the record every sweep) vs
	// "next-resume" for resource caps (the providers materialize them at placement, resource admission).
	AppliesAt string `json:"applies_at"`
	// LeaseReleased reports that a detach took effect IMMEDIATELY, which `applies_at`
	// alone cannot express (the control-plane API).
	//
	// A mount change SPLITS INTO TWO EFFECT TIMES: the guest TOPOLOGY changes at
	// next-resume (runsc takes mounts at startup and does not hot-plug), while a DETACH
	// RELEASES THE EXCLUSIVE LEASE AT ONCE — the lease is the row's existence, so deleting
	// the row releases it now. A caller waiting to attach that volume elsewhere can
	// proceed immediately even though the detaching sandbox still shows the old topology.
	LeaseReleased bool `json:"lease_released,omitempty"`
}

func (e *Engine) handlePatch(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	var req patchReq
	if err := strictDecode(r.Body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if (req.CPUMillis != nil && *req.CPUMillis < 0) ||
		(req.MemoryByte != nil && *req.MemoryByte < 0) ||
		(req.DiskByte != nil && *req.DiskByte < 0) {
		http.Error(w, "resource limits must be non-negative (0 = unlimited)", http.StatusBadRequest)
		return
	}
	b := store.Binding{CPUMillis: req.CPUMillis, MemoryByte: req.MemoryByte, DiskByte: req.DiskByte}
	for _, f := range []struct {
		raw  *string
		dst  **time.Duration
		name string
	}{
		{req.IdleTimeout, &b.IdleTimeout, "idle_timeout"},
		{req.Retention, &b.Retention, "retention"},
	} {
		if f.raw == nil {
			continue
		}
		d, terr := parseTTL(f.name, *f.raw)
		if terr != nil {
			writeErr(w, http.StatusBadRequest, terr)
			return
		}
		*f.dst = d
	}

	// The mount binding, if given. Parsed before the write so a malformed one is a 400
	// that changed nothing.
	var mounts *[]model.Mount
	if req.Volumes != nil {
		binding, perr := parseVolumeBinding(*req.Volumes)
		if perr != nil {
			writeErr(w, http.StatusBadRequest, perr)
			return
		}
		mounts = &binding
	}

	// DECODED here, VALIDATED below the seam — the lifecycle ownership line: the decode is a
	// wire concern (this package owns the JSON shape and its 400s), while the API error contract's
	// declaration classes are a WRITE-POINT obligation the mediator applies for all three
	// write points — create, PATCH, publish — so a validation living in one caller is not
	// a validation the next caller lacks. Rebind runs it ahead of even its state gate, so
	// "validation precedes the admission gate" still holds.
	if req.Network != nil {
		got, derr := req.Network.decode()
		if derr != nil {
			statusFor(w, derr)
			return
		}
		b.Network = &got
	}

	// ONE mediator call for the WHOLE request. per-sandbox transition serialization requires PATCH to hold this
	// sandbox's transition lock for its whole duration, which three separately-locking
	// calls cannot do: between them a resume or an evict fits entirely, so
	// `{retention, network}` on a `paused` sandbox committed the retention and then
	// answered 409 `state=running` — the caller told "nothing changed" while the store
	// disagreed. Rebind evaluates every gate under one lock and orders the
	// store-arbitrated mount commit first, so every 4xx from here really did change
	// nothing.
	released, err := e.Mediator.Rebind(r.Context(), id, lifecycle.RebindSpec{
		Binding: b,
		Mounts:  mounts,
	})
	if err != nil {
		statusFor(w, err)
		return
	}
	stt, err := e.Mediator.State(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	appliesAt := "now"
	if b.CPUMillis != nil || b.MemoryByte != nil || b.DiskByte != nil || req.Volumes != nil || req.Network != nil {
		// A mount change materializes at the next placement, exactly like a resource cap
		// — runsc takes mounts at startup, so there is no hot-plug path (the Volume subsystem).
		appliesAt = "next-resume"
	}
	writeJSON(w, http.StatusOK, patchResp{
		SandboxID: string(id), State: string(stt), AppliesAt: appliesAt,
		LeaseReleased: released,
	})
}

// Event-stream replay limits (the control-plane API GET /v1/events).
const (
	// eventsDefaultLimit is the page size for a request that omits `limit`.
	eventsDefaultLimit = 100

	// eventsMaxLimit is a HARD server-side cap, not merely a default: a `limit` above it
	// is clamped down, because the caller controls the parameter. Exactly ONE cap, shared
	// by every read path — the audited failure was two paths with mismatched caps (one at
	// 10k, a second uncapped), so `after=0` on the uncapped one materialized the entire
	// retained backlog (plus a same-sized channel) while holding a lock.
	//
	// The direction of the argument runs opposite to the intuitive one: because we never
	// trim (the transactional outbox), the retained window is ALL history, so `after=0` must page through
	// everything that ever happened. Not trimming AGGRAVATES the unbounded-backlog hazard
	// rather than relieving it.
	eventsMaxLimit = 1000

	// eventsMaxWait is the HARD ceiling on `wait`, caller-controlled like `limit` and
	// clamped the same way (a caller asking longer gets the longest allowed, and re-polls).
	//
	// 20s, and the binding constraint is THIS PROCESS'S OWN DRAIN, not a proxy in front of
	// it. --shutdown-timeout defaults to 30s and bounds the whole teardown graph;
	// Server.Shutdown deliberately does NOT cancel an in-flight handler's context (so a
	// mid-flight resume finishes its cold-start chain), so a hold longer than that deadline
	// is guaranteed to reach the force-close path and report the drain as having timed out —
	// on a request that was doing nothing but waiting. It also stays clear of the idle-read
	// timeouts common in proxies (65-75s), which would turn a longer hold into a 504 the
	// subscriber must interpret rather than the empty 200 it would have received.
	//
	// A deployment that lowers --shutdown-timeout below this reintroduces the overlap; the
	// cost is one force-closed idle poll per drain, which is reconnectable (the cursor did
	// not advance) and is why this is a constant rather than a knob.
	eventsMaxWait = 20 * time.Second

	// eventsWaitPoll is the re-read cadence while waiting. the transactional outbox settles the mechanism —
	// "a presentation-layer adapter that server-polls the same pull query", which holds no
	// state and shares the same `seq` — so this deliberately does NOT add an
	// append-notification channel to the store contract: that would be per-subscriber state
	// in the one place the section rules it out, and every backend would owe the same wakeup
	// semantics. The cost is one COUNT-free indexed read per waiting request per tick,
	// against a read the subscriber was already making.
	eventsWaitPoll = 250 * time.Millisecond
)

type eventItem struct {
	// Seq is the cursor: pass the last one back as `after` to resume. It is the
	// ordering key — clients must not order by occurred_at, which has no tie-break
	// at equal nanos and can move backwards if the wall clock is stepped.
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	// PlacementAddress is the guest-visible address established by created/resumed.
	// It is historical event data, not a lookup of the sandbox's current placement.
	PlacementAddress string `json:"placement_address,omitempty"`
	PlacementEpoch   uint64 `json:"placement_epoch,omitempty"`
	// Cause is WHY, present only on a Kind reachable by more than one route (the transactional outbox),
	// omitted otherwise. Today: `destroyed` carries `requested` (the Destroy verb) or
	// `retention-expired` (the reaper's evict of a paused sandbox past `retention`).
	//
	// For `destroyed` this is the fact's ONLY carrier: the record is deleted in the same
	// commit, so a follow-up GET is a contractual 404 and the route is unrecoverable
	// afterwards. NOT the same field as a sandbox read's `death_cause` (why an isolation
	// UNIT was found gone) — different question, deliberately not merged.
	Cause      string `json:"cause,omitempty"`
	OccurredAt string `json:"occurred_at"`
}

type eventsResp struct {
	Events []eventItem `json:"events"`
	// Earliest / Latest bound the retained window (0/0 when the outbox is empty). They
	// are what makes the 410 actionable: a caller whose cursor fell below the window
	// restarts from `earliest` rather than guessing. Reachable only when a retention
	// window is configured (the transactional outbox) — with trimming off `earliest` never advances.
	Earliest int64 `json:"earliest"`
	Latest   int64 `json:"latest"`
}

// readEvents is the control-plane API / the transactional outbox replay read plus the optional `wait` hold; wait == 0 is
// one read. PULL ONLY — the engine tracks no subscribers (the extension boundary), so there is no state here
// to leak or clean up, and the stream is engine-level (neither per-sandbox nor per-pid), so
// the data plane has nowhere to address it from. The wait loop re-runs THE SAME query
// rather than waiting on a notification (see eventsWaitPoll).
//
// Two properties the shape has to preserve:
//
//   - A NON-EMPTY first read never waits. Waiting after finding something would add
//     latency to the case that already has its answer.
//   - The window bounds come from the SAME read as the page, every iteration — never
//     from the first one. `earliest` can advance mid-wait (a trim), and pairing a fresh
//     page with stale bounds is what would let a caller compute a 410 against a window
//     that no longer exists.
func (e *Engine) readEvents(ctx context.Context, after int64, limit int, wait time.Duration) ([]store.Event, int64, int64, error) {
	evs, earliest, latest, err := e.Mediator.Events(ctx, after, limit)
	if err != nil || len(evs) > 0 || wait <= 0 {
		return evs, earliest, latest, err
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(eventsWaitPoll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			// The client hung up, or a shutdown began. The ERROR rather than the empty page:
			// on the drain path an empty page would falsely tell a subscriber it is caught up.
			return nil, 0, 0, ctx.Err()
		case <-deadline.C:
			// Expired with nothing new: the same empty 200 an immediate call returns.
			// Bounds are re-read so they are not stale by the length of the wait.
			return e.Mediator.Events(ctx, after, limit)
		case <-tick.C:
			evs, earliest, latest, err = e.Mediator.Events(ctx, after, limit)
			if err != nil || len(evs) > 0 {
				return evs, earliest, latest, err
			}
		}
	}
}

func (e *Engine) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	after := int64(0)
	if raw := q.Get("after"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "after: must be an integer sequence", http.StatusBadRequest)
			return
		}
		if v < 0 {
			http.Error(w, "after must be non-negative (0 = from the start of the retained window)", http.StatusBadRequest)
			return
		}
		after = v
	}

	limit := eventsDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "limit: must be an integer", http.StatusBadRequest)
			return
		}
		if v <= 0 {
			http.Error(w, "limit must be positive", http.StatusBadRequest)
			return
		}
		limit = v
	}
	if limit > eventsMaxLimit {
		limit = eventsMaxLimit
	}

	// `wait` (optional): hold an EMPTY page open for up to this long, so a subscriber's
	// detection latency stops being its poll interval. It changes NO cursor semantics —
	// the page is still `seq > after` in `seq` order, and an expired wait returns the
	// identical empty 200 an immediate call would have.
	var wait time.Duration
	if raw := q.Get("wait"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			http.Error(w, "wait: must be a duration (e.g. 30s)", http.StatusBadRequest)
			return
		}
		if d < 0 {
			http.Error(w, "wait must be non-negative (omit it, or 0, to return immediately)", http.StatusBadRequest)
			return
		}
		if d > eventsMaxWait {
			d = eventsMaxWait
		}
		wait = d
	}

	evs, earliest, latest, err := e.readEvents(r.Context(), after, limit, wait)
	if err != nil {
		// A canceled request is the client hanging up mid-wait — the NORMAL end of a long
		// poll: nobody to write a status to, and statusFor would log an error per disconnect.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		statusFor(w, err)
		return
	}
	// 410: the cursor is BELOW the retained window, so the events between it and
	// `earliest` are gone and cannot be served. Reachable only once trimming is
	// configured (the transactional outbox).
	//
	// A REFUSAL rather than a short page: the transactional outbox's "losing one loses an invoice" means a
	// silently-truncated page would let a subscriber conclude it is caught up while having
	// skipped events. {earliest, latest} lets the caller restart deliberately at the
	// window's real lower bound.
	//
	// `after > 0` is load-bearing, not defensive: 0 is contractually "from the start of
	// the retained window", so a bare `after < earliest` would 410 every cold-start cursor
	// on any trimmed outbox and break the documented way to begin reading.
	//
	// The comparison is `after+1 < earliest`, NOT `after < earliest`, because `after` is
	// EXCLUSIVE — the page is `seq > after`, so the first event this cursor still needs is
	// `after+1`. A cursor at exactly `earliest-1` has therefore consumed the trimmed prefix
	// and nothing else; refusing it would tell a caught-up subscriber it lost data, and the
	// remedy this response documents ("restart from `earliest`") would then skip the event
	// AT `earliest` — manufacturing the gap this refusal exists to prevent.
	// Compare without computing after+1: after is an int64 cursor and may be
	// MaxInt64.  Wrapping that addition would turn a valid tail read into a
	// false 410.  Because after is exclusive, expiry is exactly after <
	// earliest-1 when earliest is at least two.
	if after > 0 && earliest > 1 && after < earliest-1 {
		writeJSON(w, http.StatusGone, eventsResp{
			Events:   []eventItem{},
			Earliest: earliest,
			Latest:   latest,
		})
		return
	}
	out := eventsResp{Events: make([]eventItem, 0, len(evs)), Earliest: earliest, Latest: latest}
	for _, ev := range evs {
		out.Events = append(out.Events, eventItem{
			Seq:              ev.Seq,
			ID:               ev.ID,
			SandboxID:        string(ev.Sandbox),
			Kind:             string(ev.Kind),
			State:            string(ev.State),
			PlacementAddress: ev.PlacementAddress,
			PlacementEpoch:   uint64(ev.PlacementEpoch),
			Cause:            string(ev.Cause),
			OccurredAt:       ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// sandboxResp is the control-plane API read shape for GET /v1/sandboxes/{id} and each element
// of GET /v1/sandboxes.
//
// ONE TYPE, TWO SURFACES THAT DIFFER DELIBERATELY: the single read populates UsedByte, the
// LIST leaves it nil so it is omitted — usage costs a backend round trip the list would pay
// once per sandbox (the control-plane API, the same rule the volume list follows).
//
// DeathCause shape:
//
//   - On the READ PATH because health and reclamation puts the recovery decision on the caller:
//     dead-recoverable → resume, resource-terminated → do NOT. Otherwise it is reachable only
//     as the text of a 409, forcing the caller to attempt the wrong thing to learn it should
//     not.
//   - `death_cause`, matching the model field and store column exactly (the engine vocabulary boundary). Values are the
//     abstract health and reclamation enum, never an errno or cgroup path, which would not survive a remote
//     provider.
//   - OMITTED WHEN EMPTY: CauseNone is the ABSENCE of a death, so `"death_cause": ""` would
//     put a falsy-but-present field on the healthy response and invite `if (resp.death_cause)`
//     as the liveness test. No `null` either — two spellings of absence is one too many.
//   - List gets it too: without it a caller seeing several `paused` sandboxes must fan out one
//     GET per ID to learn which are resumable, turning one list into N+1 requests.
type sandboxResp struct {
	SandboxID      string `json:"sandbox_id"`
	State          string `json:"state"`
	PlacementEpoch uint64 `json:"placement_epoch,omitempty"`
	PlacementState string `json:"placement_state,omitempty"`
	// DeathCause is why the isolation unit was last found dead (health and reclamation). Absent =
	// never observed dead. See the type doc for why absent rather than "".
	DeathCause string `json:"death_cause,omitempty"`
	// UsedByte is what this sandbox's Overlay unit consumes (upper + work) as the rootfs
	// provider's backend reports it. Absent when it could not be read, never reported as
	// 0 — the volume side's rule word for word (the control-plane API), because it answers the same
	// question for the same consumer: `PATCH disk_byte` refuses a cap that is not
	// STRICTLY greater than this, and the only other route to the number is parsing it
	// out of that 400's message text.
	//
	// A POINTER, so a genuine 0 survives: a fresh uncapped sandbox's upper+work IS 0, so
	// the collision between "empty" and "unreadable" is the common case here.
	//
	// Present while `paused`, unlike HostEndpoint: the unit is readable AT REST, and
	// `paused` is the only state a disk cap may be edited from (resource admission) — omitting it there
	// would omit it from the one state its consumer works in.
	UsedByte *int64 `json:"used_byte,omitempty"`
}

func (e *Engine) handleGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	// Inspect rather than Get: the single read carries usage (see sandboxResp).
	info, err := e.Mediator.Inspect(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	resp := sandboxResp{
		SandboxID:  string(info.Sandbox.ID),
		State:      string(info.Sandbox.State),
		DeathCause: string(info.Sandbox.DeathCause),
		UsedByte:   info.UsedByte,
	}
	if info.Placement != nil {
		resp.PlacementEpoch = uint64(info.Placement.Epoch)
		resp.PlacementState = string(info.Placement.State)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) doVerb(w http.ResponseWriter, r *http.Request, id model.SandboxID, verb func(context.Context, model.SandboxID) error) {
	if err := verb(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	stt, _ := e.Mediator.State(r.Context(), id)
	writeJSON(w, http.StatusOK, verbResp{SandboxID: string(id), State: string(stt)})
}

// verbResp is what pause and resume answer with (the control-plane API). Named for the same reason as
// templateResp: an inline map cannot be reflected into the published contract.
type verbResp struct {
	SandboxID string `json:"sandbox_id"`
	// State is read back AFTER the verb, so the caller sees the achieved state rather
	// than the one it asked for.
	State string `json:"state"`
}

// activityResp is the control-plane API's POST /v1/sandboxes/{id}/activity body. One field, and it
// is a READING rather than an acknowledgement: the idle duration measured in the
// guest AFTER the stamp landed, so a caller gets positive confirmation the clock
// moved instead of an opaque 204.
type activityResp struct {
	IdleMillis int64 `json:"idle_millis"`
}

// handleActivity records one activity now (the control-plane API). It exists because the only other
// repeatable way to move the clock is writing to a process's stdin, which couples
// "keep this sandbox from being swept" to whatever protocol the guest program
// speaks (the activity signal, "An external caller may stamp it explicitly").
//
// THE STAMP HAPPENS IN THE GUEST, never here (guest-side activity measurement) — a host-side timestamp
// would be a second, differently-clocked source for the one signal the classifier reads.
// Three consequences, all CONTRACT:
//
//   - running only. A paused sandbox has no clock to stamp, so this is 409 rather
//     than a resume-by-side-effect — the engine will not start a sandbox because
//     somebody said "still busy" about it.
//   - 503 when L2 is unreachable, exactly like any probe of a transport-down
//     sandbox. A caller cannot hold a sandbox looking active while the very
//     channel that would prove it is dead.
//   - no drain barrier (FS-first pause and resume). A stamp truncates nothing, and gating it on a
//     pause-in-progress would only turn a 409-or-nothing answer into a slower one:
//     pause is preemptive, so the freeze does not wait for a claim of activity.
func (e *Engine) handleActivity(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	stt, err := e.Mediator.State(r.Context(), id)
	if err != nil {
		// 404 for an unknown id, through the funnel like every other lookup.
		statusFor(w, err)
		return
	}
	if stt != model.Running {
		// Through the funnel rather than http.Error, because the message NAMES THE STATE
		// so the caller knows whether to resume or give up — and a composed message is
		// exactly what the API error contract requires to be sanitized, even when (as here) the only
		// interpolated value is one of four the lifecycle state machine spellings.
		writeErr(w, http.StatusConflict,
			fmt.Errorf("sandbox is not running (state %s); activity can only be recorded on a running sandbox", stt))
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		// Running per the record but no live placement: this process does not hold the
		// channel. 503, not 404 — the sandbox exists and the answer may differ later.
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	st, err := cl.RecordActivity(r.Context())
	if err != nil {
		// bypassCode maps the guest's own classification; an unreachable L2 arrives as
		// CodeUnavailable → 503, which is the case this endpoint most has to get right.
		writeErr(w, bypassCode(err), fmt.Errorf("record activity: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, activityResp{IdleMillis: st.Idle.Milliseconds()})
}

func (e *Engine) handleDestroy(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	if err := e.Mediator.Destroy(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- the control plane's error edge (the API error contract) ------------------------------------
//
// THREE FUNCTIONS: `statusCodeFor` decides WHICH code, `sanitizeErr` decides WHAT TEXT
// the caller may see, and `writeErr` is the ONLY place in the control plane where an
// `error` value becomes a response body.
//
// The alternative shape — `http.Error(w, err.Error(), code)` in every arm — leaks by
// construction: the backend errors arriving here interpolate ABSOLUTE HOST PATHS
// (`btrfs: create subvolume /srv/state/volumes/vol_x: …`), so a failing
// `POST /v1/volumes` would hand the caller the engine's storage layout. the API error contract admits
// opaque IDs on the wire and NOTHING about host paths or placement.
//
// Same move the DATA plane already made (dataplane.go proxyErr): log the real cause
// engine-side, hand the caller a placement-free message. The control plane cannot copy
// proxyErr's ONE constant message, because the Volume subsystem verdicts must stay DISTINGUISHABLE —
// a 409 naming the wrong object sends the caller to the wrong remedy — so it redacts
// rather than replaces.

// statusFor classifies err and answers with a sanitized body. `echo` names strings
// the caller ALREADY KNOWS (their own request parameters), which are therefore safe
// to hand back — see sanitizeErr.
func statusFor(w http.ResponseWriter, err error, echo ...string) {
	writeErr(w, statusCodeFor(err), err, echo...)
}

// writeErr is THE funnel: every error-shaped response in the control plane goes through here,
// so sanitization is structural rather than remembered. `arch`-style enforcement lives in
// edge_test.go — it fails if any production file in this package calls `.Error()` outside
// sanitizeErr, or passes http.Error a message it did not spell out as a literal.
//
// The FULL error is logged, unredacted: operators need the path, it being the only thing that
// says WHICH unit failed. Host detail to the log, verdict to the caller.
func writeErr(w http.ResponseWriter, code int, err error, echo ...string) {
	log.Printf("control plane: responding %d (full error, sanitized on the wire): %v", code, err)
	http.Error(w, sanitizeErr(err, echo...), code)
}

// redacted stands in for a removed token. VISIBLE rather than silently dropped: a caller
// reading "create subvolume [redacted]: permission denied" can tell the engine withheld
// something and quote the message to an operator who can find it in the log, where an
// elided token would read as a mangled message.
const redacted = "[redacted]"

// placementShaped matches an IPv4 address with an optional port — the inner
// transport endpoint's shape: a netns veth IP. Placement is as private as a host
// path, so the error funnel strips both.
var placementShaped = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d+)?$`)

// sanitizeErr renders err for the wire with host-shaped detail removed.
//
// The rule is an ALLOWLIST OF WHAT THE CALLER ALREADY KNOWS, not a blocklist of
// known-bad strings: every whitespace-delimited token that looks like an absolute
// host path or a placement endpoint is redacted UNLESS it is one of `echo` — a
// value this request itself carried in (the `?path=` of a files download, say).
// Echoing a caller's own parameter back tells them nothing they did not type; every
// other path-shaped token was authored somewhere below the API and is exactly the
// class the API error contract excludes.
//
// SENTINEL MEANING SURVIVES INTACT, which is why redaction beats replacement here:
// every sentinel's own text ("store: volume in use by a mount", "lifecycle: volume
// has a live read-write mount") is path-free prose, so the verdict — and the object
// it names, by opaque ID — passes through untouched.
//
// Deliberately INDISCRIMINATE about guest paths it cannot attribute: a mount destination
// inside somebody else's sandbox reads exactly like a host path from here, so it is
// redacted too. That costs a little prose in a 409 that still names the holding sandbox
// by ID.
func sanitizeErr(err error, echo ...string) string {
	fields := strings.Fields(err.Error())
	for i, f := range fields {
		lead, tok, trail := trimPunct(f)
		if tok == "" || !hostShaped(tok) {
			continue
		}
		if slices.Contains(echo, tok) {
			continue
		}
		fields[i] = lead + redact(tok) + trail
	}
	return strings.Join(fields, " ")
}

// wireIDPrefixes are the object-ID prefixes the API error contract ADMITS on the wire: sandbox,
// template and volume. Overlay (`ov_`) is deliberately absent — the ID-addressed Overlay model keeps the
// Overlay artifact ID-addressed INTERNALLY, and a caller can act on neither the ID
// nor its location, so it is redacted with the rest of the path.
var wireIDPrefixes = []string{"vol_", "sbx_", "tmpl_"}

// redact replaces a host-shaped token, KEEPING a trailing engine-minted object ID if
// the token ends in one.
//
// The engine composes its storage locations as `filepath.Join(<configured root>, <object
// id>)`, so the ID is very often present in the error ONLY as that path's last segment —
// `btrfs: set quota limit 4096 on /srv/volumes/vol_x: …` names the volume nowhere else.
// Redacting the whole token there produces a 409/500 saying a cap write failed on
// SOMETHING, which is the "wrong remedy" failure the API error contract's per-object sentinels prevent.
// Keeping the last segment discloses nothing: the ID is the wire identity the caller
// supplied in the URL, and the withheld prefix is what carried the host location.
func redact(tok string) string {
	if i := strings.LastIndexByte(tok, '/'); i >= 0 {
		if last := tok[i+1:]; last != "" {
			for _, p := range wireIDPrefixes {
				if strings.HasPrefix(last, p) {
					return redacted + "/" + last
				}
			}
		}
	}
	return redacted
}

// hostShaped reports whether tok is an absolute host path, a placement endpoint, or a
// URL carrying one.
//
// "Absolute" is the whole test for the path half, and it is sufficient because the leak is
// always a location the engine composed from its own configured roots —
// `filepath.Join(VolumesDir, volumeID)` and its siblings — absolute by construction.
//
// THE URL ARM IS NOT DEFENSIVE, IT IS THE DOMINANT REAL CASE on the bypass route: a failed
// request from net/http reports itself as `Get "http://<host>:<port>/…": dial tcp
// <host>:<port>: …`, so the endpoint appears TWICE and only the second occurrence looks
// like a bare address. Matching the bare form alone redacts the visible half and leaves
// the URL intact — it reads as sanitized while still naming the placement.
func hostShaped(tok string) bool {
	return (len(tok) > 1 && tok[0] == '/') ||
		placementShaped.MatchString(tok) ||
		strings.Contains(tok, "://")
}

// trimPunct splits a token into leading punctuation, the token proper, and trailing
// punctuation, so `"/srv/x":` redacts to `"[redacted]":` rather than surviving
// because a quote or a comma made it stop looking like a path. Error prose glues
// punctuation onto paths constantly — `%s:` is the dominant wrapping idiom — so this
// is the ordinary case, not an edge one.
func trimPunct(f string) (lead, tok, trail string) {
	const leadSet = `"'` + "`" + `([<`
	const trailSet = `"'` + "`" + `)]>,;:.…`
	i, j := 0, len(f)
	for i < j && strings.ContainsRune(leadSet, rune(f[i])) {
		i++
	}
	for j > i && strings.ContainsRune(trailSet, rune(f[j-1])) {
		j--
	}
	return f[:i], f[i:j], f[j:]
}

// statusCodeFor is the API error contract verdict → status mapping, and NOTHING ELSE: it writes
// no response, so it cannot smuggle a body past the funnel.
func statusCodeFor(err error) int {
	// Protocol-neutral state-core classes are handled before subsystem-specific
	// sentinels so every HTTP edge shares the same idempotency/epoch semantics.
	switch {
	case errors.Is(err, model.ErrInvalidArgument), errors.Is(err, model.ErrUnsupported):
		return http.StatusBadRequest
	case errors.Is(err, model.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, model.ErrIdempotencyConflict), errors.Is(err, model.ErrStaleEpoch),
		errors.Is(err, model.ErrConflict), errors.Is(err, model.ErrInProgress), errors.Is(err, model.ErrCursorGap):
		return http.StatusConflict
	case errors.Is(err, model.ErrCursorExpired):
		return http.StatusGone
	case errors.Is(err, model.ErrUnavailable):
		return http.StatusServiceUnavailable
	}
	// The Volume subsystem verdicts first: they are 400s that would otherwise be caught by
	// a broader arm below and reported as something the caller could retry.
	if code, ok := volumeStatus(err); ok {
		return code
	}
	// the API error contract's declaration refusals, likewise ahead of the broader arms: every one is a
	// property of the request that no retry can change, so a 409 or 507 here would
	// send the caller to wait for something that will never happen.
	if code, ok := networkStatus(err); ok {
		return code
	}
	switch {
	case errors.Is(err, lifecycle.ErrCapRequired):
		// resource admission: the node meters this axis, so a cap is required. 400, NOT 507 —
		// nothing is full; the request is invalid for this engine's configuration.
		// Checked before ErrInsufficientCapacity below because the two are distinct
		// verdicts and conflating them would tell the caller to wait for capacity
		// that is not the problem.
		return http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrUncappedDiskNotCappable):
		// resource admission `0 → N` on the disk axis: the request is well-formed and the node may have
		// plenty of room, but the promise is UNMATERIALIZABLE — an uncapped sandbox's Overlay
		// unit carries no quota row, so the cap could never be enforced and the Rootfs subsystem would refuse
		// every future placement. 400, NOT 507 (nothing is full) and NOT 409 (the remedy is a
		// new sandbox, not a retry).
		return http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrLineageTooDeep):
		// the Rootfs subsystem: the Template's lower stack is deeper than the rootfs provider can mount
		// (rootfs provider portability). 400 — the remedy is a shallower parent, and lineage is
		// immutable once built, so there is no state to wait for.
		//
		// The one path that reaches this WITHOUT naming a Template is resume, and only after
		// a deployment moved its state/layer roots to longer paths, shrinking the ceiling
		// under an existing lineage. That is a deployment change rather than a caller error,
		// so the 400 misattributes it; no separate sentinel because the extension boundary refuses a
		// speculative one, and the response body names depth and limit either way.
		return http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrDiskCapBelowUsage):
		// resource admission `N → M` on the disk axis, where M ≤ current Overlay usage. 400 for the same
		// reasons as the volume twin (ErrVolumeCapBelowUsage) — the caller must pick a larger
		// size, or 0. Checked BEFORE ErrInsufficientCapacity so a full node cannot turn this
		// into a 507 telling the caller to wait for capacity that would not help.
		return http.StatusBadRequest
	case errors.Is(err, rootfs.ErrCapBelowUsage):
		// Binding effective points's PLACEMENT-side twin of the refusal above: a stored cap that is no longer
		// strictly above the unit's usage, refused when it would be re-materialized rather
		// than wedging the unit. 409, NOT 400: resume/adopt do not CARRY a cap, so a 400 would
		// send the caller to fix something this request did not send. The remedy is state
		// elsewhere — PATCH disk_byte strictly above usage (or to 0), then resume. Create
		// reaches the same substrate condition while carrying `disk_byte`, and the mediator
		// translates it there to the 400 sentinel above.
		return http.StatusConflict
	case errors.Is(err, lifecycle.ErrInsufficientCapacity):
		// resource-admission refusal: nothing is wrong with the request or the sandbox — the NODE
		// cannot honor another promise. 507 Insufficient Storage says "capacity", where 409
		// would wrongly imply a state conflict the caller could resolve by retrying.
		return http.StatusInsufficientStorage
	case errors.Is(err, lifecycle.ErrResourceTerminated):
		// health and reclamation resource-terminated: a state conflict the caller resolves by raising
		// the cap or destroying the sandbox — 409, never a raw 500.
		return http.StatusConflict
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound),
		errors.Is(err, store.ErrVolumeNotFound), errors.Is(err, store.ErrPlacementNotFound),
		errors.Is(err, store.ErrProcessNotFound):
		// Volume not-found has its own sentinel so the 404 names the right object; a
		// 404 mentioning the wrong one sends the caller looking in the wrong place.
		// Note it is NOT folded into success on delete (the API error contract): the reference-protection
		// refusals arrive on that same path, and a delete that swallows not-found
		// would swallow those too.
		return http.StatusNotFound
	case errors.Is(err, store.ErrInUse):
		// health and reclamation reference protection: a referenced Template cannot be deleted →
		// 409 template in use (the control-plane API DELETE), never a raw 500.
		return http.StatusConflict
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
