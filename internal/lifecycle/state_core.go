package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

func (m *Mediator) nextPlacementEpoch(ctx context.Context, id model.SandboxID) (model.PlacementEpoch, error) {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if errors.Is(err, store.ErrPlacementNotFound) {
		return model.FirstPlacementEpoch, nil
	}
	if err != nil {
		return 0, err
	}
	return model.NextPlacementEpoch(p.Epoch)
}

func (m *Mediator) currentPlacementEpoch(ctx context.Context, id model.SandboxID) (model.PlacementEpoch, error) {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if errors.Is(err, store.ErrPlacementNotFound) {
		return model.FirstPlacementEpoch, nil
	}
	if err != nil {
		return 0, err
	}
	return p.Epoch, nil
}

func (m *Mediator) persistPlacement(ctx context.Context, id model.SandboxID, epoch model.PlacementEpoch, state model.PlacementState) error {
	now := time.Now()
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if errors.Is(err, store.ErrPlacementNotFound) {
		return m.deps.Store.CreatePlacement(ctx, model.Placement{
			Sandbox: id, Epoch: epoch, Provider: m.deps.Runtime.Name(), State: state,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	if err != nil {
		return err
	}
	if epoch < p.Epoch {
		return model.NewContractError(model.ErrorStaleEpoch, fmt.Sprintf("placement epoch %d is older than current epoch %d", epoch, p.Epoch))
	}
	if epoch > p.Epoch {
		p.CreatedAt = now
	}
	p.Epoch = epoch
	p.Provider = m.deps.Runtime.Name()
	p.State = state
	p.UpdatedAt = now
	return m.deps.Store.UpdatePlacement(ctx, p)
}

func (m *Mediator) markPlacementLost(ctx context.Context, id model.SandboxID) {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if errors.Is(err, store.ErrPlacementNotFound) {
		return
	}
	if err != nil {
		m.deps.Logf("placement %s: read before loss update: %v", id, err)
		return
	}
	p.State = model.PlacementLost
	p.UpdatedAt = time.Now()
	if err := m.deps.Store.UpdatePlacement(ctx, p); err != nil {
		m.deps.Logf("placement %s: persist lost state: %v", id, err)
	}
	m.pauseProcessRecords(ctx, id, p.Epoch)
}

func (m *Mediator) pauseProcessRecords(ctx context.Context, id model.SandboxID, epoch model.PlacementEpoch) {
	procs, err := m.deps.Store.ListProcesses(ctx, id)
	if err != nil {
		m.deps.Logf("processes %s: list during placement loss: %v", id, err)
		return
	}
	for _, p := range procs {
		if (p.State != model.ProcessRunning && p.State != model.ProcessStarting) || p.PlacementEpoch != epoch {
			continue
		}
		p.State = model.ProcessPaused
		p.PID = 0
		if err := m.deps.Store.UpdateProcess(ctx, p); err != nil {
			m.deps.Logf("process %s: persist paused state: %v", p.ID, err)
		}
	}
}

// ValidatePlacementEpoch fails closed when a data-plane request addresses a
// different realization from the durable current record.
func (m *Mediator) ValidatePlacementEpoch(ctx context.Context, id model.SandboxID, epoch model.PlacementEpoch) error {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if err != nil {
		return err
	}
	if p.State != model.PlacementActive {
		return model.NewContractError(model.ErrorConflict, "placement is not active")
	}
	return p.ValidateRequest(id, epoch)
}

// Placement returns the durable current realization. It carries no provider
// handle or host path, so callers can use it for epoch validation and snapshots.
func (m *Mediator) Placement(ctx context.Context, id model.SandboxID) (model.Placement, error) {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if errors.Is(err, store.ErrPlacementNotFound) {
		return model.Placement{}, model.ErrNotFound
	}
	return p, err
}

func (m *Mediator) Process(ctx context.Context, id model.ProcessID) (model.Process, error) {
	p, err := m.deps.Store.GetProcess(ctx, id)
	if errors.Is(err, store.ErrProcessNotFound) {
		return model.Process{}, model.ErrNotFound
	}
	return p, err
}

func (m *Mediator) CreateProcess(ctx context.Context, p model.Process) error {
	return m.deps.Store.CreateProcess(ctx, p)
}

func (m *Mediator) UpdateProcess(ctx context.Context, p model.Process) error {
	return m.deps.Store.UpdateProcess(ctx, p)
}

// ReconcileStateCore reclaims expired process records and takes over expired
// in-progress operation leases without erasing their side-effect phase.
func (m *Mediator) ReconcileStateCore(ctx context.Context) error {
	retention := m.deps.ProcessRetention.ExitedFor
	if retention <= 0 {
		retention = model.DefaultProcessExitRetention
	}
	if _, err := m.deps.Store.ReclaimProcesses(ctx, time.Now().Add(-retention)); err != nil {
		return fmt.Errorf("state core: reclaim processes: %w", err)
	}
	ops, err := m.deps.Store.ListOperations(ctx, model.OperationInProgress)
	if err != nil {
		return fmt.Errorf("state core: list operations: %w", err)
	}
	now := time.Now()
	for _, op := range ops {
		if !op.TakeoverAllowed(now) {
			continue
		}
		claimed, err := m.deps.Store.TakeoverOperation(ctx, op.ID, op.Attempt, now, model.DefaultOperationLease)
		if errors.Is(err, store.ErrOperationOwnershipConflict) {
			continue
		}
		if err != nil {
			m.deps.Logf("operation %s: takeover persist failed: %v", op.ID, err)
			continue
		}
		m.deps.Logf("operation %s: restart takeover, recovery=%s", claimed.ID, claimed.RecoveryAction())
	}
	return nil
}
