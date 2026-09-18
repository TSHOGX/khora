package orchestrator

import (
	"github.com/TSHOGX/khora/internal/model"
	"net/http"
)

type engineInfo struct {
	StoreID model.StoreID `json:"store_id"`
	Version string        `json:"version"`
}

func (e *Engine) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := e.Store.Identity(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	version := e.version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, http.StatusOK, engineInfo{StoreID: id, Version: version})
}
