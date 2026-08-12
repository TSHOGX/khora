package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/rootfs"
	"warren/internal/store"
	"warren/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// HTTPHandler exposes the minimal §6.1 sandbox-centric API — ID-addressed, no
// host paths / placement exposed (§6.2): sandbox lifecycle
// (create/get/pause/resume/destroy/list) + process.Start + fs read/write, all
// proxied by sandbox ID (the orchestrator resolves the current endpoint
// internally via the mediator, so placement never leaves the engine).
//
// This is a deliberately small net/http router (no framework). The outer-edge
// protocol split is settled (transport D5): the CONTROL plane (sandbox
// lifecycle) is REST/JSON on this router; the process/fs DATA plane is
// Connect-RPC (the SAME transport.proto as the inner edge, re-proxied by
// sandbox_id — D5.2). The two mount on one mux: Connect services own their
// /engine.transport.v1.*/ paths, control-plane REST owns /v1/... . The whole
// thing is wrapped in h2c so the data-plane server-streams work over cleartext
// HTTP/2 (D4.2). The data plane is Connect-only, no REST/NDJSON facade (D5.3).
func (e *Engine) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// --- data plane: Connect-RPC re-proxy (transport D5) ---
	mux.Handle(transportv1connect.NewProcessServiceHandler(&processProxy{e: e}))
	mux.Handle(transportv1connect.NewFsServiceHandler(&fsProxy{e: e}))

	// --- operational surface: /healthz, /readyz, /metrics (health.go) ---
	// Deliberately NOT under /v1: the §6.1 contract is the sandbox-centric API, and
	// an operational endpoint is not part of it.
	e.registerHealthRoutes(mux)

	// --- control plane: §6.1 REST/JSON ---
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

	// GET /v1/events?after=&limit= — the §4.4-C event-stream replay (pull only).
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		e.handleEvents(w, r)
	})

	// --- Template lifecycle: §6.1 REST/JSON (immutable object, §3.1/§4.3) ---
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
	// DELETE /v1/templates/{id} (referenced → 409 template in use, §4.3).
	mux.HandleFunc("/v1/templates/", e.handleTemplateSub)

	// --- Volume lifecycle: §6.1 REST/JSON (§5.C, decoupled from every sandbox) ---
	// create/list/inspect/patch/delete + the two direct content endpoints, which
	// need no sandbox at all (see volumes.go).
	e.registerVolumeRoutes(mux)

	// h2c so Connect server-streams work over cleartext HTTP/2 (D4.2).
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
	// IdleTimeout / Retention are the optional TTL Binding inputs (§6.1
	// idle_timeout?/retention?). Accepted as Go duration strings ("30m", "24h")
	// so the unit is explicit on the wire; omitted/"" → engine-wide default
	// (materialized by the mediator at create).
	IdleTimeout string `json:"idle_timeout"`
	Retention   string `json:"retention"`
	// Resource-limit Binding inputs (§6.1 / §3.4). Omitted / 0 = unlimited on that
	// axis. Three separate fields, never one blended "size" — the axes and why they
	// are not interchangeable are documented on model.Limits. There is deliberately
	// no io_* field: the disk I/O rate axis joins in the same shape once it has a
	// consumer (§3.4 / §5.A), and its one open question — where the whole-disk
	// MAJ:MIN comes from — is settled (provider-derived, never on the seam).
	CPUMillis  int64 `json:"cpu_millis"`
	MemoryByte int64 `json:"memory_byte"`
	DiskByte   int64 `json:"disk_byte"`
	// Volumes is the §6.1 `volumes?` binding: the COMPLETE set of volume mounts this
	// sandbox is created with. `access` is given HERE and not on the volume record,
	// because it is a property of the MOUNT (§5.C).
	Volumes []volumeBindingItem `json:"volumes"`
	// Network is the §6.1 `network?` declaration (§5.D). A POINTER because omitting
	// the whole object is LEGAL — it normalizes to an explicit {deny, [], [], []} at
	// the write point — while giving the object and leaving out `egress.default` is
	// §6.2's first 400 class. The two share a code path in most implementations, and
	// whichever is handled first eats the other, so the distinction is carried rather
	// than collapsed.
	Network *networkWire `json:"network"`
}

// volumeBindingItem is one entry of a `volumes` binding, on create and on PATCH
// (§6.1). One type for both, because §6.2 makes PATCH swap a whole binding for a whole
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
			// No default is invented. A caller who does not say how a volume is mounted has
			// not expressed a binding, and guessing would silently pick either an exclusive
			// lease they did not ask for or a shared writer they did not intend.
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

// parseTTL is THE spelling of a wire TTL for both write points — create and PATCH
// (§3.4 idle_timeout / retention). One function rather than two blocks, because the
// defect it closes is precisely the two write points disagreeing: create parsed
// without a sign check while PATCH refused a negative, so the SAME field was legal on
// one and a 400 on the other.
//
// A NEGATIVE DURATION IS REFUSED, and the reason is that its effect is SILENT rather
// than wrong-but-visible. Both consumers read a non-positive TTL as an OPT-OUT — the
// reaper returns early on `Retention <= 0`, and idle classification only fires on
// `IdleTimeout > 0` — so `-5m` does not produce an early deadline or an error; it
// produces NEVER EVICT / NEVER IDLE-PAUSE. A caller who typed a duration they believed
// was a deadline gets a sandbox that outlives every deadline, with nothing anywhere
// reporting that their value was not honored. Refusing at the boundary is the only
// place the mistake stays visible.
//
// `0` remains legal on both, and that is not an inconsistency: zero is the DOCUMENTED
// opt-out spelling (§6.1 `retention: 0` = never evict), i.e. a chosen value, whereas a
// negative is a value nothing in the engine can mean.
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
		// the §6.2 guard cannot see that, and an exemption for "this one is obviously
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
	// UNKNOWN FIELDS REFUSED (see strictDecode): without it a caller who typed
	// `"netwrok"` gets a 201 and believes their declaration took effect, while the
	// engine recorded the omitted-network normalization — a `deny` sandbox. Both
	// outcomes are legal, which is exactly why the misspelling must be an error.
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
	// The declaration is parsed and VALIDATED before the create verb runs, so §6.2's
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
		Template: model.TemplateID(req.TemplateID),
		Volumes:  binding,
		Network:  decl,
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
		// the §4.3 reference side) surfaces as 404, not a raw 500.
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
		out = append(out, sandboxResp{
			SandboxID:  string(s.ID),
			State:      string(s.State),
			DeathCause: string(s.DeathCause),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type promoteReq struct {
	FromSandboxID string `json:"from_sandbox_id"`
}
type promoteResp struct {
	TemplateID string `json:"template_id"`
}

// handlePromote is POST /v1/templates {from_sandbox_id} → {template_id} (§6.1
// Promote): freeze the sandbox's current Overlay into a new Template (§3.1).
func (e *Engine) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req promoteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.FromSandboxID == "" {
		http.Error(w, "from_sandbox_id is required", http.StatusBadRequest)
		return
	}
	tid, err := e.Mediator.Promote(r.Context(), model.SandboxID(req.FromSandboxID))
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
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"template_id": string(t.ID),
			"parent":      string(t.Parent),
			"builtin":     t.Builtin,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleTemplateSub(w http.ResponseWriter, r *http.Request) {
	// path: /v1/templates/{id}
	id := model.TemplateID(strings.TrimPrefix(r.URL.Path, "/v1/templates/"))
	if id == "" {
		http.Error(w, "missing template id", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := e.Mediator.DeleteTemplate(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// The large-file bypass (§5.E fs / transport D4.4). REST rather than
	// Connect-RPC because its whole purpose is to STREAM bytes: a file must not
	// become one protobuf message, nor sit in memory at either end.
	case sub == "files" && r.Method == http.MethodGet:
		e.handleFileDownload(w, r, id)
	case sub == "files" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		e.handleFileUpload(w, r, id)
	// §6.1's Network block. `network` is GET-only and a wrong method there is a 405
	// by the §6.2 general rule, NOT a 404 landing in the default arm below: the
	// collection exists, the method does not apply to it.
	case sub == "network" && r.Method == http.MethodGet:
		e.handleNetworkGet(w, r, id)
	case sub == "network":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case sub == "publish" && r.Method == http.MethodPost:
		e.handlePublish(w, r, id)
	case sub == "publish":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	// The same §6.2 general rule for the older routes. They landed in `default` as 404,
	// which contradicts §6.2 ("a wrong method on ANY collection/object route → 405")
	// and was inconsistent with the two arms above for no reason anyone chose: the
	// route exists, the method does not apply to it.
	case sub == "files", sub == "pause", sub == "resume":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		// process/fs data plane is Connect-RPC (transport D5), not REST here.
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// patchReq is the §6.1 PATCH body — rewrite a sandbox's Binding (§3.4). Every
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
	// Volumes is the mount table's COMPLETE binding (§6.1). A POINTER slice so
	// "omitted" is distinguishable from "given as empty" — and here that distinction
	// carries real weight: an empty array means DETACH EVERYTHING, which is a legitimate
	// request and must not be confused with "leave the mounts alone".
	//
	// It is on PATCH rather than on separate attach/detach endpoints because §6.2
	// already defines PATCH as THE rebinding point: a sandbox holds one complete
	// binding at all times, and PATCH swaps one whole binding for another.
	Volumes *[]volumeBindingItem `json:"volumes"`
	// Network is the §5.D declaration, swapped WHOLE for WHOLE (§6.2 — no field-level
	// merge, no pending value). A pointer for the usual reason, which carries real
	// weight here: the zero declaration is not "no change", it is
	// `{deny, [], [], []}` — the most restrictive declaration there is — so a value
	// type would make every unrelated PATCH silently reset the sandbox to
	// deny-everything.
	//
	// `applies_at: next-resume`, and editable only from `paused` (the mediator
	// enforces the state gate): the provider materializes it at placement, so
	// accepting the edit on `running` would leave the record and the live rule set
	// disagreeing indefinitely.
	//
	// PUBLISH DOES NOT COME THROUGH HERE — it has its own hot-add endpoint — but it
	// CAN be removed through here, since there is no un-publish endpoint. §6.1 makes
	// the read-modify-write REQUIRED rather than advisory for exactly that reason: a
	// binding built from a stale GET silently drops a publish added in between, and
	// the reverse (writing back an already-removed publish) reopens an inbound port.
	Network *networkWire `json:"network"`
}

type patchResp struct {
	SandboxID string `json:"sandbox_id"`
	State     string `json:"state"`
	// AppliesAt tells the caller WHEN the change takes effect rather than leaving
	// them to infer it: "now" for TTL fields (the reaper re-reads the record every
	// sweep) vs "next-resume" for resource caps (the providers materialize them at
	// placement, §3.4). Making the deferral explicit is the point.
	AppliesAt string `json:"applies_at"`
	// LeaseReleased reports that a detach took effect IMMEDIATELY, which
	// `applies_at` alone cannot express (§6.1).
	//
	// A mount change SPLITS INTO TWO EFFECT TIMES, and that is a design fact rather
	// than a detail to fold away: the guest TOPOLOGY changes at next-resume (runsc takes
	// mounts at startup and does not hot-plug), while a DETACH RELEASES THE EXCLUSIVE
	// LEASE AT ONCE — the lease is the row's existence, so deleting the row releases it
	// now. A caller waiting to attach that volume elsewhere can proceed immediately even
	// though the detaching sandbox still shows the old topology until it resumes.
	// Omitted when nothing was released.
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

	// The declaration, whole for whole (§6.2 — no field-level merge, no pending value).
	//
	// DECODED here, VALIDATED below the seam. The split is the §4 ownership line: the
	// decode is a wire concern (this package owns the JSON shape and its 400s), while
	// §6.2's declaration classes are a WRITE-POINT obligation the mediator applies for
	// all three write points — create, PATCH, publish — so that a validation living in
	// one caller is not a validation the next caller lacks. Rebind runs it first, ahead
	// of even its state gate, so "validation precedes the admission gate" still holds.
	if req.Network != nil {
		got, derr := req.Network.decode()
		if derr != nil {
			statusFor(w, derr)
			return
		}
		b.Network = &got
	}

	// ONE mediator call for the WHOLE request, and the shape is load-bearing rather than
	// tidy. §4.4-B requires PATCH to hold this sandbox's transition lock for its whole
	// duration, which three separately-locking calls cannot do: between them a resume or
	// an evict fits entirely, so `{retention, network}` on a `paused` sandbox committed
	// the retention and then answered 409 `state=running` — the caller told "nothing
	// changed" while the store disagreed. Rebind evaluates every gate under one lock and
	// orders the store-arbitrated mount commit first, so every 4xx from here really did
	// change nothing.
	released, err := e.Mediator.Rebind(r.Context(), id, lifecycle.RebindSpec{
		Binding: b,
		// A POINTER, so "detach everything" (`"volumes": []`) stays distinguishable from
		// "leave the mounts alone" (field omitted) — §6.2 makes the empty array a real
		// request, not a no-op.
		Mounts: mounts,
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
		// — runsc takes mounts at startup, so there is no hot-plug path (§5.C).
		appliesAt = "next-resume"
	}
	writeJSON(w, http.StatusOK, patchResp{
		SandboxID: string(id), State: string(stt), AppliesAt: appliesAt,
		LeaseReleased: released,
	})
}

// Event-stream replay limits (§6.1 GET /v1/events).
const (
	// eventsDefaultLimit is the page size for a request that omits `limit`.
	eventsDefaultLimit = 100

	// eventsMaxLimit is a HARD server-side cap, not merely a default: a `limit`
	// above it is clamped down, so no caller can ask for a bigger page. A default
	// alone would be worthless here because the caller controls the parameter.
	//
	// There must be exactly ONE cap and every read path must share it. The audited
	// failure was precisely two paths with mismatched caps — one capped at 10k, a
	// second uncapped — so `after=0` on the uncapped one materialized the entire
	// retained backlog (plus a same-sized channel) while holding a lock.
	//
	// Note the direction of the argument, which runs opposite to the intuitive
	// one: because we never trim (see §4.4-C), the retained window is ALL history,
	// so `after=0` must page through everything that ever happened and the row
	// count only grows. Not trimming AGGRAVATES the unbounded-backlog hazard
	// rather than relieving it, which is what makes this cap mandatory rather than
	// a nicety.
	eventsMaxLimit = 1000
)

type eventItem struct {
	// Seq is the cursor: pass the last one back as `after` to resume. It is the
	// ordering key — clients must not order by occurred_at, which has no tie-break
	// at equal nanos and can move backwards if the wall clock is stepped.
	Seq        int64  `json:"seq"`
	ID         string `json:"id"`
	SandboxID  string `json:"sandbox_id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	OccurredAt string `json:"occurred_at"`
}

type eventsResp struct {
	Events []eventItem `json:"events"`
	// Earliest / Latest bound the retained window (0/0 when the outbox is empty).
	//
	// They are what makes the 410 actionable: a caller whose cursor fell below the
	// window restarts from `earliest` rather than guessing. Reachable only when a
	// retention window is configured (§4.4-C) — with trimming off `earliest` never
	// advances, so a deployment that sets nothing never sees a 410 and the shape
	// costs it nothing.
	Earliest int64 `json:"earliest"`
	Latest   int64 `json:"latest"`
}

// handleEvents is GET /v1/events?after=<seq>&limit=<n> (§6.1 / §4.4-C): the
// engine-level lifecycle event stream, replayed by cursor. PULL ONLY — the engine
// tracks no subscribers (§1.3), so there is no state here to leak or clean up.
//
// A cursor-paged GET is ordinary CRUD, which is why this belongs on the control
// plane: the stream is engine-level, neither per-sandbox nor per-pid, so the data
// plane has nowhere to address it from.
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
	// The hard cap: clamp rather than reject, so a caller asking for more simply
	// gets a full page and pages again. Rejecting would give the same protection
	// but break a caller for asking a reasonable question.
	if limit > eventsMaxLimit {
		limit = eventsMaxLimit
	}

	evs, earliest, latest, err := e.Mediator.Events(r.Context(), after, limit)
	if err != nil {
		statusFor(w, err)
		return
	}
	// 410: the cursor is BELOW the retained window, so the events between it and
	// `earliest` are gone and cannot be served. Reachable only once trimming is
	// configured (§4.4-C) — with no trimming `earliest` never advances, which is why this
	// was written as reserved semantics and left unimplemented.
	//
	// It is a REFUSAL rather than a short page on purpose: §4.4-C's "losing one loses an
	// invoice" means a silently-truncated page would let a subscriber conclude it is
	// caught up while having skipped events. The response carries {earliest, latest} so
	// the caller can restart deliberately at the window's real lower bound. (The field
	// approach elsewhere is to skip to the end silently; that is exactly what this
	// refuses to do.)
	//
	// `after > 0` is load-bearing, not defensive: 0 is contractually "from the start of
	// the retained window", i.e. an explicit request for whatever is retained, so a bare
	// `after < earliest` would 410 every cold-start cursor on any trimmed outbox and
	// break the documented way to begin reading.
	//
	// The comparison is `after+1 < earliest`, NOT `after < earliest`, because `after` is
	// EXCLUSIVE — the page is `seq > after`, so the first event this cursor still needs is
	// `after+1`. A cursor at exactly `earliest-1` has therefore consumed the trimmed prefix
	// and nothing else; refusing it would tell a caught-up subscriber it lost data, and the
	// remedy this response documents ("restart from `earliest`") would then skip the event
	// AT `earliest` — manufacturing the gap this refusal exists to prevent.
	if after > 0 && earliest > 0 && after+1 < earliest {
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
			Seq:        ev.Seq,
			ID:         ev.ID,
			SandboxID:  string(ev.Sandbox),
			Kind:       string(ev.Kind),
			State:      string(ev.State),
			OccurredAt: ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// sandboxResp is the §6.1 read shape for GET /v1/sandboxes/{id} and each element
// of GET /v1/sandboxes.
//
// ONE TYPE, TWO SURFACES THAT DIFFER DELIBERATELY: the single read populates UsedByte,
// the LIST leaves it nil so it is omitted. That is not an oversight to tidy up — usage
// costs a backend round trip, and the list would pay it once per sandbox (§6.1, the same
// rule the volume list follows). If you find yourself filling it in handleList, that is
// the decision being reversed, not a gap being closed.
//
// DeathCause shape, argued here because neither reference implementation offers a
// precedent to copy (one models no terminal state at all, the other has a failed
// state but no cause field anywhere):
//
//   - It is on the READ PATH because §4.3 puts the recovery decision on the
//     caller: dead-recoverable → resume, resource-terminated → do NOT resume
//     (resuming re-runs the same workload into the same cap, i.e. a crash loop).
//     A caller who owns that decision needs the fact it decides on. Today it is
//     reachable only as the text of a 409 from a resume that was already refused,
//     which forces the caller to attempt the wrong thing in order to learn it
//     should not.
//   - `death_cause`, matching the model field and the store column exactly. It
//     names a property of the sandbox atom, not a product concept, so it is clear
//     of the §1.1 vocabulary boundary. The values are the abstract §4.3 enum
//     (`gone` / `resource-exceeded` / `unknown`), never an errno or a cgroup
//     path — an errno would be a host primitive on the wire and would not survive
//     a remote provider.
//   - OMITTED WHEN EMPTY (`omitempty`). CauseNone is the ABSENCE of a death
//     rather than a kind of death, so serializing `"death_cause": ""` would put a
//     falsy-but-present field on the overwhelmingly common healthy response and
//     invite `if (resp.death_cause)` to be written as the liveness test. Absent
//     means "not observed dead"; present always means "there is a cause, and it
//     is this one". No `null`, for the same reason: two spellings of absence is
//     one more than needed.
//   - List gets it too, and this is not just symmetry. §6.2 is ID-addressing:
//     a caller that finds several sandboxes `paused` in a list would otherwise
//     have to fan out one GET per ID purely to learn which of them are resumable,
//     turning a single list into N+1 requests to answer a question the list
//     already holds. Keeping the two shapes identical also removes the chance of
//     them drifting apart.
type sandboxResp struct {
	SandboxID string `json:"sandbox_id"`
	State     string `json:"state"`
	// DeathCause is why the isolation unit was last found dead (§4.3). Absent =
	// never observed dead. See the type doc for why absent rather than "".
	DeathCause string `json:"death_cause,omitempty"`
	// UsedByte is what this sandbox's Overlay unit consumes (upper + work) as the
	// rootfs provider's backend reports it. Absent when it could not be read, never
	// reported as 0 — the volume side's rule word for word (§6.1), because it answers
	// the same question for the same consumer: `PATCH disk_byte` refuses a cap that is
	// not STRICTLY greater than this, and the only other route to the number is parsing
	// it out of that 400's message text.
	//
	// A POINTER, so a genuine 0 survives: a fresh uncapped sandbox's upper+work IS 0,
	// so here the collision between "empty" and "unreadable" is the common case rather
	// than an edge one.
	//
	// Present while `paused`, unlike HostEndpoint: the unit is readable AT REST, and
	// `paused` is the only state a disk cap may be edited from (§3.4) — omitting it
	// there would omit it from the one state its consumer works in.
	UsedByte *int64 `json:"used_byte,omitempty"`
}

func (e *Engine) handleGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	// Inspect rather than Get: the single read carries usage, which costs one backend
	// round trip. The LIST deliberately does not (see handleList) — one round trip per
	// sandbox multiplied by the sandbox count, the same reason the volume list omits it.
	info, err := e.Mediator.Inspect(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxResp{
		SandboxID:  string(info.Sandbox.ID),
		State:      string(info.Sandbox.State),
		DeathCause: string(info.Sandbox.DeathCause),
		UsedByte:   info.UsedByte,
	})
}

func (e *Engine) doVerb(w http.ResponseWriter, r *http.Request, id model.SandboxID, verb func(context.Context, model.SandboxID) error) {
	if err := verb(r.Context(), id); err != nil {
		statusFor(w, err)
		return
	}
	stt, _ := e.Mediator.State(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": string(id), "state": string(stt)})
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

// --- the control plane's error edge (§6.2) ------------------------------------
//
// THREE FUNCTIONS, AND THE SPLIT IS THE WHOLE POINT: `statusCodeFor` decides WHICH
// code, `sanitizeErr` decides WHAT TEXT the caller may see, and `writeErr` is the
// ONLY place in the control plane where an `error` value becomes a response body.
//
// The alternative shape — `http.Error(w, err.Error(), code)` in every arm — leaks by
// construction: the backend errors arriving here interpolate ABSOLUTE HOST PATHS
// (`btrfs: create subvolume /srv/state/volumes/vol_x: …`), so a failing
// `POST /v1/volumes` would hand the caller the engine's storage layout. §6.2 admits
// opaque IDs on the wire and NOTHING about host paths or placement. Ten arms each
// doing their own write is ten chances to forget, and a new arm forgets by default.
//
// This is the same move the DATA plane already made (dataplane.go proxyErr): log the
// real cause engine-side, hand the caller a placement-free message. The control
// plane cannot copy proxyErr's ONE constant message, because the §5.C verdicts must
// stay DISTINGUISHABLE — a 409 naming the wrong object sends the caller to the wrong
// remedy — so it redacts rather than replaces.

// statusFor classifies err and answers with a sanitized body. `echo` names strings
// the caller ALREADY KNOWS (their own request parameters), which are therefore safe
// to hand back — see sanitizeErr.
func statusFor(w http.ResponseWriter, err error, echo ...string) {
	writeErr(w, statusCodeFor(err), err, echo...)
}

// writeErr is THE funnel: every error-shaped response in the control plane goes
// through here, so sanitization is structural rather than remembered. `arch`-style
// enforcement lives in edge_test.go — it fails if any production file in this
// package calls `.Error()` outside sanitizeErr, or passes http.Error a message it
// did not spell out as a literal. A new endpoint therefore cannot leak by omission;
// it can only leak by editing the guard.
//
// The FULL error is logged, unredacted. Operators need the path — it is the only
// thing that says WHICH unit failed — and the asymmetry is the design: the host
// detail goes to the log, the verdict goes to the caller.
func writeErr(w http.ResponseWriter, code int, err error, echo ...string) {
	log.Printf("control plane: responding %d (full error, sanitized on the wire): %v", code, err)
	http.Error(w, sanitizeErr(err, echo...), code)
}

// redacted stands in for a removed token. It is deliberately VISIBLE rather than
// silently dropped: a caller reading "create subvolume [redacted]: permission
// denied" can tell the engine withheld something and quote the message to an
// operator who can find it in the log, where an elided token would read as a
// mangled message.
const redacted = "[redacted]"

// placementShaped matches an IPv4 address with an optional port — the inner
// transport endpoint's shape (a netns veth IP, §2.4 / D5.1). §6.2 bars placement
// from the wire on the same footing as host paths, so the funnel strips both.
var placementShaped = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d+)?$`)

// sanitizeErr renders err for the wire with host-shaped detail removed.
//
// The rule is an ALLOWLIST OF WHAT THE CALLER ALREADY KNOWS, not a blocklist of
// known-bad strings: every whitespace-delimited token that looks like an absolute
// host path or a placement endpoint is redacted UNLESS it is one of `echo` — a
// value this request itself carried in (the `?path=` of a files download, say).
// Echoing a caller's own parameter back tells them nothing they did not type; every
// other path-shaped token was authored somewhere below the API and is exactly the
// class §6.2 excludes.
//
// SENTINEL MEANING SURVIVES INTACT, which is why redaction beats replacement here:
// every sentinel's own text ("store: volume in use by a mount", "lifecycle: volume
// has a live read-write mount") is path-free prose, so the verdict — and the object
// it names, by opaque ID — passes through untouched while only the substrate's
// coordinates are dropped.
//
// It is deliberately INDISCRIMINATE about guest paths it cannot attribute: a mount
// destination inside somebody else's sandbox reads exactly like a host path from
// here, so it is redacted too. That costs a little prose in a 409 that still names
// the holding sandbox by ID, and it is the right direction to fail — the funnel
// cannot be wrong about a path it never lets through.
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

// wireIDPrefixes are the object-ID prefixes §6.2 ADMITS on the wire: sandbox,
// template and volume. Overlay (`ov_`) is deliberately absent — §3.1.1 keeps the
// Overlay artifact ID-addressed INTERNALLY, and a caller can act on neither the ID
// nor its location, so it is redacted with the rest of the path.
var wireIDPrefixes = []string{"vol_", "sbx_", "tmpl_"}

// redact replaces a host-shaped token, KEEPING a trailing engine-minted object ID if
// the token ends in one.
//
// This is not a softening of the rule; it is what makes the rule survivable. The
// engine composes its storage locations as `filepath.Join(<configured root>, <object
// id>)`, so the ID is very often present in the error ONLY as that path's last
// segment — `btrfs: set quota limit 4096 on /srv/volumes/vol_x: …` names the volume
// nowhere else. Redacting the whole token there produces a 409/500 that says a cap
// write failed on SOMETHING, which is precisely the "sends the caller to the wrong
// remedy" failure §6.2's per-object sentinels exist to prevent.
//
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
// "Absolute" is the whole test for the path half, and it is sufficient because the
// leak is always a filesystem location the engine composed from its own configured
// roots — `filepath.Join(VolumesDir, volumeID)` and its siblings — and those are
// absolute by construction. A relative token cannot name a location on the host
// without the root that is being withheld.
//
// THE URL ARM IS NOT DEFENSIVE, IT IS THE DOMINANT REAL CASE on the bypass route: a
// failed request from net/http reports itself as `Get "http://<host>:<port>/…":
// dial tcp <host>:<port>: …`, so the endpoint appears TWICE and only the second
// occurrence looks like a bare address. Matching the bare form alone redacts the
// visible half and leaves the URL intact, which is the worst outcome — it reads as
// sanitized while still naming the placement.
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

// statusCodeFor is the §6.2 verdict → status mapping, and NOTHING ELSE: it writes
// no response, so it cannot smuggle a body past the funnel.
func statusCodeFor(err error) int {
	// The §5.C Volume verdicts first: they are 400s that would otherwise be caught by
	// a broader arm below and reported as something the caller could retry.
	if code, ok := volumeStatus(err); ok {
		return code
	}
	// §6.2's declaration refusals, likewise ahead of the broader arms: every one is a
	// property of the request that no retry can change, so a 409 or 507 here would
	// send the caller to wait for something that will never happen.
	if code, ok := networkStatus(err); ok {
		return code
	}
	switch {
	case errors.Is(err, lifecycle.ErrCapRequired):
		// §3.4: the node meters this axis, so a cap is required. 400, NOT 507 —
		// nothing is full; the request is invalid for this engine's configuration.
		// Checked before ErrInsufficientCapacity below because the two are distinct
		// verdicts and conflating them would tell the caller to wait for capacity
		// that is not the problem.
		return http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrUncappedDiskNotCappable):
		// §3.4 `0 → N` on the disk axis: the request is well-formed and the node may
		// have plenty of room, but the promise is UNMATERIALIZABLE — an uncapped
		// sandbox's Overlay unit carries no quota row, so the cap could never be
		// enforced and §5.B would refuse every future placement. 400 (the request
		// cannot be honored as asked), NOT 507 (nothing is full) and NOT 409 (no
		// retry or state change makes it legal — the remedy is a new sandbox).
		return http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrDiskCapBelowUsage):
		// §3.4 `N → M` on the disk axis, where M ≤ current Overlay usage. 400 for the same
		// reasons as the volume twin (ErrVolumeCapBelowUsage): the request is well-formed
		// but cannot be honored, nothing is full (so not 507), and no retry or state change
		// makes it legal — the caller must pick a larger size, or 0. Checked BEFORE
		// ErrInsufficientCapacity so a full node cannot turn this into a 507 that tells the
		// caller to wait for capacity that would not help.
		return http.StatusBadRequest
	case errors.Is(err, rootfs.ErrCapBelowUsage):
		// §3.4.5's PLACEMENT-side twin of the refusal above: a stored cap that is no longer
		// strictly above the unit's usage, refused when it would be re-materialized rather
		// than wedging the unit. 409, NOT 400, and the split is the same one §6.1 already
		// makes for the second-hop uid: resume/adopt do not CARRY a cap, so a 400 would send
		// the caller to fix something this request did not send. The remedy is state
		// elsewhere — PATCH disk_byte strictly above usage (or to 0), then resume. Create
		// reaches the same substrate condition while carrying `disk_byte`, and the mediator
		// translates it there to the 400 sentinel above.
		return http.StatusConflict
	case errors.Is(err, lifecycle.ErrInsufficientCapacity):
		// §3.4 admission refusal: nothing is wrong with the request or the sandbox —
		// the NODE cannot honor another promise. 507 Insufficient Storage says
		// "capacity", where 409 would wrongly imply a state conflict the caller could
		// resolve by retrying.
		return http.StatusInsufficientStorage
	case errors.Is(err, lifecycle.ErrResourceTerminated):
		// §4.3 resource-terminated: a state conflict the caller resolves by raising
		// the cap or destroying the sandbox — 409, never a raw 500.
		return http.StatusConflict
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound),
		errors.Is(err, store.ErrVolumeNotFound):
		// Volume not-found has its own sentinel so the 404 names the right object; a
		// 404 mentioning the wrong one sends the caller looking in the wrong place.
		// Note it is NOT folded into success on delete (§6.2): the reference-protection
		// refusals arrive on that same path, and a delete that swallows not-found
		// would swallow those too.
		return http.StatusNotFound
	case errors.Is(err, store.ErrInUse):
		// §4.3 reference protection: a referenced Template cannot be deleted →
		// 409 template in use (§6.1 DELETE), never a raw 500.
		return http.StatusConflict
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
