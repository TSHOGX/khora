package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/store"
)

type operationClaim struct {
	op     *model.Operation
	replay *model.OperationResult
}

func isOperationOwnershipConflict(err error) bool {
	return errors.Is(err, store.ErrOperationOwnershipConflict)
}

func (m *Mediator) claimOperation(ctx context.Context, scope string, key model.IdempotencyKey, semantic any) (operationClaim, error) {
	if key == "" {
		return operationClaim{}, nil
	}
	hash, err := model.RequestHash(semantic)
	if err != nil {
		return operationClaim{}, err
	}
	now := time.Now()
	op, err := model.BeginOperation(model.OperationID("op_"+randToken()), scope, key, hash, now, model.DefaultOperationLease)
	if err != nil {
		return operationClaim{}, err
	}
	if err := m.deps.Store.CreateOperation(ctx, op); err == nil {
		return operationClaim{op: &op}, nil
	} else if !errors.Is(err, store.ErrOperationKeyConflict) && !errors.Is(err, store.ErrConflict) {
		return operationClaim{}, err
	}

	existing, err := m.deps.Store.GetOperationByKey(ctx, scope, key)
	if err != nil {
		return operationClaim{}, err
	}
	if err := existing.SameRequest(hash); err != nil {
		return operationClaim{}, err
	}
	switch existing.State {
	case model.OperationSucceeded:
		return operationClaim{replay: existing.Result}, nil
	case model.OperationFailed:
		return operationClaim{}, model.NewContractError(existing.ErrorClass, "idempotent operation previously failed")
	case model.OperationInProgress:
		if !existing.TakeoverAllowed(now) {
			return operationClaim{}, model.ErrInProgress
		}
		existing, err = m.deps.Store.TakeoverOperation(ctx, existing.ID, existing.Attempt, now, model.DefaultOperationLease)
		if isOperationOwnershipConflict(err) {
			return operationClaim{}, model.ErrInProgress
		}
		if err != nil {
			return operationClaim{}, err
		}
		// The object row is the durable commit boundary for volume operations,
		// and a non-creating sandbox row is likewise enough to replay the
		// deterministic result. This closes the crash window after object commit
		// but before the ledger update: never clean up an object that is already
		// externally observable.
		if result, committed, err := m.committedOperationResult(ctx, existing); err != nil {
			return operationClaim{}, err
		} else if committed {
			if err := existing.Succeed(result, now); err != nil {
				return operationClaim{}, err
			}
			if err := m.deps.Store.UpdateOperation(ctx, existing); err != nil {
				return operationClaim{}, err
			}
			return operationClaim{replay: &result}, nil
		}
		if existing.RecoveryAction() == model.RecoveryCleanup {
			if err := m.advanceOperation(ctx, operationClaim{op: &existing}, model.OperationCleaning); err != nil {
				return operationClaim{}, err
			}
			if err := m.recoverOperation(ctx, existing); err != nil {
				return operationClaim{}, err
			}
			// Cleanup completed (or was idempotently absent), so a retry can
			// begin at the recorded boundary without losing the audit trail of
			// the prior phase.
			existing.Phase = model.OperationRecorded
		}
		if err := m.deps.Store.UpdateOperation(ctx, existing); err != nil {
			return operationClaim{}, err
		}
		return operationClaim{op: &existing}, nil
	default:
		return operationClaim{}, model.ErrInternal
	}
}

func (m *Mediator) committedOperationResult(ctx context.Context, op model.Operation) (model.OperationResult, bool, error) {
	switch op.Scope {
	case "volume.create", "volume.snapshot", "volume.clone":
		id := model.VolumeID(deterministicObjectID("vol_", op.ID))
		if _, err := m.deps.Store.GetVolume(ctx, id); err == nil {
			return model.OperationResult{Kind: model.ObjectVolume, ID: string(id)}, true, nil
		} else if !errors.Is(err, store.ErrVolumeNotFound) {
			return model.OperationResult{}, false, err
		}
	case "sandbox.create":
		id := model.SandboxID(deterministicObjectID("sbx_", op.ID))
		if rec, err := m.deps.Store.GetSandbox(ctx, id); err == nil {
			if rec.State != model.Creating {
				return model.OperationResult{Kind: model.ObjectSandbox, ID: string(id)}, true, nil
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return model.OperationResult{}, false, err
		}
	}
	return model.OperationResult{}, false, nil
}

func (m *Mediator) recoverOperation(ctx context.Context, op model.Operation) error {
	switch op.Scope {
	case "volume.create", "volume.snapshot", "volume.clone":
		id := deterministicObjectID("vol_", op.ID)
		if rec, err := m.deps.Store.GetVolume(ctx, model.VolumeID(id)); err == nil {
			backend, berr := m.backendForRecord(rec)
			if berr != nil {
				return fmt.Errorf("operation %s: resolve volume cleanup provider: %w", op.ID, berr)
			}
			if err := m.deps.Store.DeleteVolume(ctx, rec.ID); err != nil {
				return fmt.Errorf("operation %s: delete recovered volume record: %w", op.ID, err)
			}
			if err := backend.DestroyVolume(ctx, id); err != nil {
				return fmt.Errorf("operation %s: destroy recovered volume bytes: %w", op.ID, err)
			}
			return nil
		} else if !errors.Is(err, store.ErrVolumeNotFound) {
			return fmt.Errorf("operation %s: inspect recovered volume: %w", op.ID, err)
		}
		backends, err := m.configuredVolumeBackends()
		if err != nil {
			return fmt.Errorf("operation %s: resolve volume providers: %w", op.ID, err)
		}
		for _, selected := range backends {
			if err := selected.backend.DestroyVolume(ctx, id); err != nil {
				return fmt.Errorf("operation %s: volume cleanup %s via %s: %w", op.ID, id, selected.name, err)
			}
		}
	case "sandbox.create":
		id := model.SandboxID(deterministicObjectID("sbx_", op.ID))
		if rec, err := m.deps.Store.GetSandbox(ctx, id); err == nil {
			_ = m.deps.Store.DeleteMounts(ctx, id)
			if m.deps.Rootfs != nil {
				if ov, oerr := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged); oerr == nil {
					_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
				}
			}
			_ = m.deps.Store.DeleteSandbox(ctx, id, nil)
		}
	}
	return nil
}

func (m *Mediator) advanceOperation(ctx context.Context, claim operationClaim, phase model.OperationPhase) error {
	if claim.op == nil {
		return nil
	}
	if err := claim.op.Advance(phase, time.Now()); err != nil {
		return err
	}
	if err := m.deps.Store.UpdateOperation(ctx, *claim.op); isOperationOwnershipConflict(err) {
		return model.ErrInProgress
	} else {
		return err
	}
}

func (m *Mediator) succeedOperation(ctx context.Context, claim operationClaim, result model.OperationResult) error {
	if claim.op == nil {
		return nil
	}
	if err := claim.op.Succeed(result, time.Now()); err != nil {
		return err
	}
	if err := m.deps.Store.UpdateOperation(ctx, *claim.op); isOperationOwnershipConflict(err) {
		return model.ErrInProgress
	} else {
		return err
	}
}

func (m *Mediator) failOperation(ctx context.Context, claim operationClaim, err error) {
	if claim.op == nil || err == nil || claim.op.State != model.OperationInProgress {
		return
	}
	class := operationErrorClass(err)
	if ferr := claim.op.Fail(class, time.Now()); ferr != nil {
		m.deps.Logf("operation %s: record failure class: %v", claim.op.ID, ferr)
		return
	}
	if ferr := m.deps.Store.UpdateOperation(ctx, *claim.op); ferr != nil && !isOperationOwnershipConflict(ferr) {
		m.deps.Logf("operation %s: persist failure class: %v", claim.op.ID, ferr)
	}
}

func operationErrorClass(err error) model.ErrorClass {
	if class := model.ErrorClassOf(err); class != model.ErrorInternal {
		return class
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound), errors.Is(err, store.ErrVolumeNotFound):
		return model.ErrorNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrMountLeaseConflict), errors.Is(err, store.ErrVolumeInUse):
		return model.ErrorConflict
	case errors.Is(err, ErrInsufficientCapacity), errors.Is(err, ErrCapRequired):
		return model.ErrorUnavailable
	default:
		return model.ErrorInternal
	}
}

func deterministicObjectID(prefix string, op model.OperationID) string {
	sum := sha256.Sum256([]byte(op))
	return prefix + hex.EncodeToString(sum[:8])
}

type semanticMount struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
	Access string `json:"access"`
}

func semanticSandboxCreate(spec CreateSpec, idle, retention time.Duration, network model.NetworkDecl) any {
	mounts := make([]semanticMount, 0, len(spec.Volumes))
	for _, mt := range spec.Volumes {
		mounts = append(mounts, semanticMount{Volume: string(mt.Volume), Path: store.NormalizeMountPath(mt.Path), Access: string(mt.Access)})
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Path != mounts[j].Path {
			return mounts[i].Path < mounts[j].Path
		}
		return mounts[i].Volume < mounts[j].Volume
	})
	return struct {
		Template       string            `json:"template"`
		IdleNanos      int64             `json:"idle_nanos"`
		RetentionNanos int64             `json:"retention_nanos"`
		Limits         model.Limits      `json:"limits"`
		Volumes        []semanticMount   `json:"volumes"`
		Network        model.NetworkDecl `json:"network"`
	}{string(spec.Template), int64(idle), int64(retention), spec.Limits, mounts, network.Normalize()}
}

func operationReplayID(claim operationClaim, kind model.ObjectKind) (string, error) {
	if claim.replay == nil {
		return "", nil
	}
	if claim.replay.Kind != kind || claim.replay.ID == "" {
		return "", fmt.Errorf("operation replay result has kind %q, want %q", claim.replay.Kind, kind)
	}
	return claim.replay.ID, nil
}
