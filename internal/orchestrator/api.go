package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// HTTPHandler exposes the minimal §6.1 sandbox-centric API — ID-addressed, no
// host paths / placement exposed (§6.2). M1 scope: sandbox lifecycle
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
// HTTP/2 (D4.2). The old NDJSON facade is gone (D6.3).
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

	// h2c so Connect server-streams work over cleartext HTTP/2 (D4.2).
	return h2c.NewHandler(mux, &http2.Server{})
}

type createReq struct {
	TemplateID string `json:"template_id"`
	GuestUser  string `json:"guest_user"`
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
	sid, err := e.Mediator.Create(r.Context(), lifecycle.CreateSpec{
		Template: model.TemplateID(req.TemplateID), GuestUser: req.GuestUser,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
