package lifecycle

import (
	"context"
	"errors"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

type BindingInfo struct {
	SandboxInfo
	Mounts []model.Mount
}

// InspectBinding serializes configuration and validator reads with lifecycle
// mutations. It returns portable declarations only; no backend usage call can
// block the transition lock while inspecting the configuration.
func (m *Mediator) InspectBinding(ctx context.Context, id model.SandboxID) (BindingInfo, error) {
	lock := m.lock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return BindingInfo{}, err
	}
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return BindingInfo{}, err
	}
	info := BindingInfo{SandboxInfo: SandboxInfo{Sandbox: rec}}
	placement, err := m.deps.Store.GetPlacement(ctx, id)
	if err == nil {
		info.Placement = &placement
	} else if !errors.Is(err, store.ErrPlacementNotFound) {
		return BindingInfo{}, err
	}
	info.Mounts, err = m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return BindingInfo{}, err
	}
	identity, err := m.deps.Store.Identity(ctx)
	if err != nil {
		return BindingInfo{}, err
	}
	info.MutationRevision = mutationRevision(identity, rec, info.Placement)
	return info, nil
}
