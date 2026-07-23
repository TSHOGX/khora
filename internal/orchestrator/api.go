package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	spec := lifecycle.CreateSpec{
		Template: model.TemplateID(req.TemplateID),
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
	out := make([]map[string]string, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]string{"sandbox_id": string(s.ID), "state": string(s.State)})
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
	case sub == "" && r.Method == http.MethodDelete:
		e.handleDestroy(w, r, id)
	case sub == "pause" && r.Method == http.MethodPost:
		e.doVerb(w, r, id, e.Mediator.Pause)
	case sub == "resume" && r.Method == http.MethodPost:
		e.doVerb(w, r, id, e.Mediator.Resume)
	default:
		// process/fs data plane is Connect-RPC (transport D6), not REST here.
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (e *Engine) handleGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	stt, err := e.Mediator.State(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": string(id), "state": string(stt)})
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
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound):
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
