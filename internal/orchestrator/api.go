package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
)

// HTTPHandler exposes the minimal §6.1 sandbox-centric API — ID-addressed, no
// host paths / placement exposed (§6.2). M1 scope: sandbox lifecycle
// (create/get/pause/resume/destroy/list) + process.Start + fs read/write, all
// proxied by sandbox ID (the orchestrator resolves the current endpoint
// internally via the mediator, so placement never leaves the engine).
//
// This is a deliberately small net/http router (no framework) — the §6.1 REST
// shape; the Connect-RPC↔REST projection is still being aligned (open-decisions
// §5.E). process.Start streams NDJSON events over a chunked response.
func (e *Engine) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

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

	// /v1/sandboxes/{id}[/pause|/resume|/processes|/fs]
	mux.HandleFunc("/v1/sandboxes/", e.handleSandboxSub)

	return mux
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
	case sub == "processes" && r.Method == http.MethodPost:
		e.handleStart(w, r, id)
	case strings.HasPrefix(sub, "fs") && r.Method == http.MethodPut:
		e.handleFsWrite(w, r, id)
	case strings.HasPrefix(sub, "fs") && r.Method == http.MethodGet:
		e.handleFsRead(w, r, id)
	default:
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

type startReq struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

// handleStart proxies process.Start, streaming events as NDJSON (one JSON
// object per line: {kind,pid,stream,data,exit_code}).
func (e *Engine) handleStart(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	var req startReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		statusFor(w, err)
		return
	}
	ch, err := cl.Start(r.Context(), transport.StartSpec{
		Cmd: req.Cmd, Args: req.Args, Env: req.Env, Cwd: req.Cwd,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flush, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for ev := range ch {
		_ = enc.Encode(map[string]any{
			"kind": ev.Kind, "pid": ev.PID, "stream": ev.Stream,
			"data": string(ev.Data), "exit_code": ev.ExitCode,
		})
		if flush != nil {
			flush.Flush()
		}
	}
}

func (e *Engine) handleFsWrite(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	path := r.URL.Query().Get("path")
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		statusFor(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := cl.Write(ctx, path, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) handleFsRead(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	path := r.URL.Query().Get("path")
	cl, err := e.Mediator.Client(id)
	if err != nil {
		statusFor(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, err := cl.Read(ctx, path, 0, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(data)
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
