package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// HTTPHandler exposes the minimal §6.1 sandbox-centric API — ID-addressed, no
// host paths / placement exposed (§6.2): sandbox lifecycle
// (create/get/pause/resume/destroy/list) + process.Start + fs read/write, all
// proxied by sandbox ID (the orchestrator resolves the current endpoint
// internally via the mediator, so placement never leaves the engine).
//
// This is a deliberately small net/http router (no framework). The outer-edge
// protocol split is settled (transport D6): the CONTROL plane (sandbox
// lifecycle) is REST/JSON on this router; the process/fs DATA plane is
// Connect-RPC (the SAME transport.proto as the inner edge, re-proxied by
// sandbox_id — D6.2). The two mount on one mux: Connect services own their
// /engine.transport.v1.*/ paths, control-plane REST owns /v1/... . The whole
// thing is wrapped in h2c so the data-plane server-streams work over cleartext
// HTTP/2 (D4.2). The data plane is Connect-only, no REST/NDJSON facade (D6.3).
func (e *Engine) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// --- data plane: Connect-RPC re-proxy (transport D6) ---
	mux.Handle(transportv1connect.NewProcessServiceHandler(&processProxy{e: e}))
	mux.Handle(transportv1connect.NewFsServiceHandler(&fsProxy{e: e}))

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
	return h2c.NewHandler(mux, &http2.Server{})
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
}
type createResp struct {
	SandboxID string `json:"sandbox_id"`
}

func (e *Engine) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.CPUMillis < 0 || req.MemoryByte < 0 || req.DiskByte < 0 {
		http.Error(w, "resource limits must be non-negative (0 = unlimited)", http.StatusBadRequest)
		return
	}
	spec := lifecycle.CreateSpec{
		Template: model.TemplateID(req.TemplateID),
		Limits: model.Limits{
			CPUMillis:  req.CPUMillis,
			MemoryByte: req.MemoryByte,
			DiskByte:   req.DiskByte,
		},
	}
	if req.IdleTimeout != "" {
		d, err := time.ParseDuration(req.IdleTimeout)
		if err != nil {
			http.Error(w, "idle_timeout: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec.IdleTimeout = &d
	}
	if req.Retention != "" {
		d, err := time.ParseDuration(req.Retention)
		if err != nil {
			http.Error(w, "retention: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec.Retention = &d
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	default:
		// process/fs data plane is Connect-RPC (transport D6), not REST here.
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
}

type patchResp struct {
	SandboxID string `json:"sandbox_id"`
	State     string `json:"state"`
	// AppliesAt tells the caller WHEN the change takes effect rather than leaving
	// them to infer it: "now" for TTL fields (the reaper re-reads the record every
	// sweep) vs "next-resume" for resource caps (the providers materialize them at
	// placement, §3.4). Making the deferral explicit is the point.
	AppliesAt string `json:"applies_at"`
}

func (e *Engine) handlePatch(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	var req patchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		d, err := time.ParseDuration(*f.raw)
		if err != nil {
			http.Error(w, f.name+": "+err.Error(), http.StatusBadRequest)
			return
		}
		if d < 0 {
			http.Error(w, f.name+" must be non-negative (0 = no deadline)", http.StatusBadRequest)
			return
		}
		*f.dst = &d
	}

	if err := e.Mediator.UpdateBinding(r.Context(), id, b); err != nil {
		statusFor(w, err)
		return
	}
	stt, err := e.Mediator.State(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	appliesAt := "now"
	if b.CPUMillis != nil || b.MemoryByte != nil || b.DiskByte != nil {
		appliesAt = "next-resume"
	}
	writeJSON(w, http.StatusOK, patchResp{SandboxID: string(id), State: string(stt), AppliesAt: appliesAt})
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
	// They are reported even though nothing is trimmed today — the cost is zero
	// and it fixes the response SHAPE now, so introducing trimming later needs no
	// change to the caller contract. With no trimming `earliest` never advances,
	// which is exactly why 410 is unreachable today and deliberately not
	// implemented (documented as reserved semantics in api-contract.md §6.1).
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
	out := eventsResp{Events: make([]eventItem, 0, len(evs)), Earliest: earliest, Latest: latest}
	for _, ev := range evs {
		out.Events = append(out.Events, eventItem{
			Seq:        ev.Seq,
			ID:         ev.ID,
			SandboxID:  string(ev.Sandbox),
			Kind:       ev.Kind,
			State:      string(ev.State),
			OccurredAt: ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// sandboxResp is the §6.1 read shape for GET /v1/sandboxes/{id} and each element
// of GET /v1/sandboxes.
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
}

func (e *Engine) handleGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	rec, err := e.Mediator.Get(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxResp{
		SandboxID:  string(rec.ID),
		State:      string(rec.State),
		DeathCause: string(rec.DeathCause),
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

func statusFor(w http.ResponseWriter, err error) {
	// The §5.C Volume verdicts first: they are 400s that would otherwise be caught by
	// a broader arm below and reported as something the caller could retry.
	if code, ok := volumeStatus(err); ok {
		http.Error(w, err.Error(), code)
		return
	}
	switch {
	case errors.Is(err, lifecycle.ErrCapRequired):
		// §3.4: the node meters this axis, so a cap is required. 400, NOT 507 —
		// nothing is full; the request is invalid for this engine's configuration.
		// Checked before ErrInsufficientCapacity below because the two are distinct
		// verdicts and conflating them would tell the caller to wait for capacity
		// that is not the problem.
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, lifecycle.ErrInsufficientCapacity):
		// §3.4 admission refusal: nothing is wrong with the request or the sandbox —
		// the NODE cannot honor another promise. 507 Insufficient Storage says
		// "capacity", where 409 would wrongly imply a state conflict the caller could
		// resolve by retrying.
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case errors.Is(err, lifecycle.ErrResourceTerminated):
		// §4.3 resource-terminated: a state conflict the caller resolves by raising
		// the cap or destroying the sandbox — 409, never a raw 500.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound),
		errors.Is(err, store.ErrVolumeNotFound):
		// Volume not-found has its own sentinel so the 404 names the right object; a
		// 404 mentioning the wrong one sends the caller looking in the wrong place.
		// Note it is NOT folded into success on delete (§6.2): the reference-protection
		// refusals arrive on that same path, and a delete that swallows not-found
		// would swallow those too.
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrInUse):
		// §4.3 reference protection: a referenced Template cannot be deleted →
		// 409 template in use (§6.1 DELETE), never a raw 500.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
