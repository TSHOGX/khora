package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

var ErrPreconditionFailed = errors.New("sandbox mutation revision changed")

// mutationRevision includes the durable store identity and placement fence.
// A binding changed and then restored still has a newer sandbox revision.
func mutationRevision(identity model.StoreID, record model.Sandbox, placement *model.Placement) string {
	state := struct {
		StoreID        model.StoreID
		SandboxID      model.SandboxID
		Revision       uint64
		State          model.Lifecycle
		Epoch          model.PlacementEpoch
		PlacementState model.PlacementState
	}{StoreID: identity, SandboxID: record.ID, Revision: record.Revision, State: record.State}
	if placement != nil {
		state.Epoch = placement.Epoch
		state.PlacementState = placement.State
	}
	raw, _ := json.Marshal(state)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// checkMutationRevisionLocked runs after acquiring the transition lock and
// before any mutation. A stale request cannot target a newer realization.
func (m *Mediator) checkMutationRevisionLocked(ctx context.Context, id model.SandboxID, expected string) error {
	if expected == "" {
		return nil
	}
	record, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	var placement *model.Placement
	current, err := m.deps.Store.GetPlacement(ctx, id)
	if err == nil {
		placement = &current
	} else if !errors.Is(err, store.ErrPlacementNotFound) {
		return err
	}
	identity, err := m.deps.Store.Identity(ctx)
	if err != nil {
		return err
	}
	if expected != mutationRevision(identity, record, placement) {
		return ErrPreconditionFailed
	}
	return nil
}
