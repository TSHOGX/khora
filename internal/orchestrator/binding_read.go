package orchestrator

import (
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"net/http"
)

type bindingReadResp struct {
	SandboxID        string              `json:"sandbox_id"`
	MutationRevision string              `json:"mutation_revision"`
	State            string              `json:"state"`
	PlacementEpoch   uint64              `json:"placement_epoch"`
	PlacementState   string              `json:"placement_state"`
	TemplateID       string              `json:"template_id"`
	CPUMillis        int64               `json:"cpu_millis"`
	MemoryByte       int64               `json:"memory_byte"`
	DiskByte         int64               `json:"disk_byte"`
	IdleTimeout      string              `json:"idle_timeout"`
	Retention        string              `json:"retention"`
	Network          networkReadResp     `json:"network"`
	Volumes          []volumeBindingItem `json:"volumes"`
}

func (e *Engine) handleBindingGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	info, err := e.Mediator.InspectBinding(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	b := info.Sandbox
	response := bindingReadResp{SandboxID: string(b.ID), MutationRevision: info.MutationRevision, State: string(b.State), TemplateID: string(b.Template), CPUMillis: b.Limits.CPUMillis, MemoryByte: b.Limits.MemoryByte, DiskByte: b.Limits.DiskByte, IdleTimeout: b.IdleTimeout.String(), Retention: b.Retention.String(), Network: encodeNetwork(b.Network, nil, nil, network.CounterAttribution{}), Volumes: make([]volumeBindingItem, 0, len(info.Mounts))}
	response.Network.State = string(b.State)
	if info.Placement != nil {
		response.PlacementEpoch = uint64(info.Placement.Epoch)
		response.PlacementState = string(info.Placement.State)
	}
	for _, mount := range info.Mounts {
		response.Volumes = append(response.Volumes, volumeBindingItem{VolumeID: string(mount.Volume), Path: mount.Path, Access: string(mount.Access)})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}
