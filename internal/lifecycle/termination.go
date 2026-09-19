package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// releasePlacementLocked retains the writer fence until runtime teardown and
// mount release both succeed. The transition lock must remain held throughout.
func (m *Mediator) releasePlacementLocked(ctx context.Context, id model.SandboxID, stop bool) error {
	placement, err := m.deps.Store.GetPlacement(ctx, id)
	if err != nil && !errors.Is(err, store.ErrPlacementNotFound) {
		return err
	}
	m.mu.Lock()
	live := m.live[id]
	m.mu.Unlock()
	if placement.State == model.PlacementLost && live == nil {
		return nil
	}
	if err == nil {
		placement.State = model.PlacementStopping
		placement.UpdatedAt = time.Now()
		if err := m.deps.Store.UpdatePlacement(ctx, placement); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if live != nil {
		live.stopping = true
	}
	m.mu.Unlock()
	if live != nil && live.placement != nil {
		if stop {
			err = m.deps.Runtime.Stop(ctx, live.placement)
		} else {
			err = m.deps.Runtime.Destroy(ctx, live.placement)
		}
	} else {
		err = m.deps.Runtime.Reclaim(ctx, string(id))
	}
	if err != nil {
		return fmt.Errorf("placement teardown unresolved: %w", err)
	}
	if live != nil {
		for _, mount := range live.mounts {
			if mount != nil && mount.backend != nil && mount.materialized != nil {
				if err := mount.backend.Release(ctx, mount.materialized); err != nil {
					return fmt.Errorf("placement volume release unresolved: %w", err)
				}
			}
		}
		if live.merged != nil {
			if err := m.deps.Rootfs.Unmount(ctx, live.merged); err != nil {
				return fmt.Errorf("placement rootfs release unresolved: %w", err)
			}
		}
	}
	if placement.Epoch != 0 {
		placement.State = model.PlacementLost
		placement.UpdatedAt = time.Now()
		if err := m.deps.Store.UpdatePlacement(ctx, placement); err != nil {
			return err
		}
		m.pauseProcessRecords(ctx, id, placement.Epoch)
	}
	m.take(id)
	return nil
}

func (m *Mediator) requireTerminated(ctx context.Context, id model.SandboxID) error {
	placement, err := m.deps.Store.GetPlacement(ctx, id)
	if err != nil && !errors.Is(err, store.ErrPlacementNotFound) {
		return err
	}
	if err == nil && placement.State != model.PlacementLost {
		return fmt.Errorf("%w: previous placement teardown is unresolved", store.ErrConflict)
	}
	m.mu.Lock()
	live := m.live[id]
	m.mu.Unlock()
	if live != nil {
		return fmt.Errorf("%w: previous placement resources remain", store.ErrConflict)
	}
	return nil
}
