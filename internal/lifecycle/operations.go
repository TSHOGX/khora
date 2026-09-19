package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

type operationClaim struct {
	op      *model.Operation
	replay  *model.OperationResult
	lease   *operationLease
	cleanup bool
}

// operationLease keeps a live attempt's ownership token fresh while an
// external provider call is in progress. If renewal fails, the derived context
// is canceled so providers that honor context can stop before another attempt
// is allowed to take over.
type operationLease struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func (k *operationLease) setErr(err error) {
	k.mu.Lock()
	if k.err == nil {
		k.err = err
	}
	k.mu.Unlock()
}

func (k *operationLease) Err() error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

func (k *operationLease) stop() {
	if k == nil {
		return
	}
	k.cancel()
	<-k.done
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
			// Cleanup may have been recorded before the previous attempt
			// discovered that the durable object row was already committed. The
			// verified row is the commit boundary, so normalize that stale phase
			// before applying the terminal replay result.
			if existing.Phase == model.OperationCleaning {
				existing.Phase = model.OperationMaterialized
			}
			if err := existing.Succeed(result, now); err != nil {
				return operationClaim{}, err
			}
			if err := m.deps.Store.UpdateOperation(ctx, existing); err != nil {
				return operationClaim{}, err
			}
			return operationClaim{replay: &result}, nil
		}
		return operationClaim{op: &existing, cleanup: existing.RecoveryAction() == model.RecoveryCleanup}, nil
	default:
		return operationClaim{}, model.ErrInternal
	}
}

func (m *Mediator) startOperationLease(parent context.Context, claim operationClaim) (context.Context, operationClaim, func()) {
	if claim.op == nil {
		return parent, claim, func() {}
	}
	if claim.lease != nil {
		return claim.lease.ctx, claim, claim.lease.stop
	}
	return m.startOperationLeaseWith(parent, claim, model.DefaultOperationLease, model.DefaultOperationLease/3)
}

// startOperationLeaseWith is split out so the ownership behavior can be tested
// with a short lease without making production timing a test dependency.
func (m *Mediator) startOperationLeaseWith(parent context.Context, claim operationClaim, lease, interval time.Duration) (context.Context, operationClaim, func()) {
	if claim.op == nil || lease <= 0 || interval <= 0 {
		return parent, claim, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	k := &operationLease{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	claim.lease = k
	go func(id model.OperationID, attempt uint32) {
		defer close(k.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, interval)
				err := m.deps.Store.RenewOperation(renewCtx, id, attempt, now, lease)
				renewCancel()
				if err != nil {
					k.setErr(err)
					cancel()
					return
				}
			}
		}
	}(claim.op.ID, claim.op.Attempt)
	return ctx, claim, k.stop
}

func (m *Mediator) prepareOperation(ctx context.Context, claim operationClaim) (operationClaim, error) {
	if claim.op == nil || !claim.cleanup {
		return claim, nil
	}
	if err := m.operationOwned(ctx, claim); err != nil {
		return operationClaim{}, err
	}
	if err := m.advanceOperation(ctx, claim, model.OperationCleaning); err != nil {
		return operationClaim{}, err
	}
	if err := m.recoverOperation(ctx, *claim.op); err != nil {
		// Leave the durable row in OperationCleaning. A later retry can
		// safely take over and retry cleanup without another owner doing work.
		return operationClaim{}, err
	}
	claim.op.Phase = model.OperationRecorded
	if err := m.deps.Store.UpdateOperation(ctx, *claim.op); err != nil {
		return operationClaim{}, err
	}
	claim.cleanup = false
	return claim, nil
}

// reconcileOperation completes the cleanup half of an expired operation during
// restart reconciliation. Recovery must not wait for a caller to retry: a
// materialization window can contain provider bytes with no durable result, and
// leaving those bytes until a future request would make restart leak-prone.
// The operation lease still fences the cleanup, and its heartbeat cancellation
// prevents a losing attempt from continuing when another owner takes over.
func (m *Mediator) reconcileOperation(ctx context.Context, op model.Operation) error {
	// A durable object row is the commit boundary. If the process died after
	// inserting that row but before terminalizing the ledger, replay the result;
	// never run cleanup against an object that is already externally observable.
	if result, committed, err := m.committedOperationResult(ctx, op); err != nil {
		return err
	} else if committed {
		// A row can be observed even if a previous attempt had marked the phase
		// cleaning. The verified row wins over the stale phase; normalize to the
		// materialized boundary so Succeed can make the terminal result durable.
		if op.Phase == model.OperationCleaning {
			op.Phase = model.OperationMaterialized
		}
		if err := op.Succeed(result, time.Now()); err != nil {
			return err
		}
		return m.deps.Store.UpdateOperation(ctx, op)
	}
	claim := operationClaim{op: &op, cleanup: op.RecoveryAction() == model.RecoveryCleanup}
	if !claim.cleanup {
		return nil
	}
	opCtx, owned, stop := m.startOperationLease(ctx, claim)
	defer stop()
	_, err := m.prepareOperation(opCtx, owned)
	return err
}

func (m *Mediator) operationOwned(ctx context.Context, claim operationClaim) error {
	if claim.op == nil {
		return nil
	}
	if err := claim.lease.Err(); err != nil {
		return model.ErrInProgress
	}
	current, err := m.deps.Store.GetOperation(ctx, claim.op.ID)
	if err != nil {
		return err
	}
	if current.Attempt != claim.op.Attempt || current.State != model.OperationInProgress || !current.LeaseUntil.After(time.Now()) {
		return model.ErrInProgress
	}
	return nil
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
		if len(backends) == 0 {
			// An empty provider set is not evidence that the deterministic unit
			// is absent. Keep the operation in its cleanup phase so a later boot
			// with the recorded backend can reclaim the bytes; silently marking
			// cleanup complete would convert an unavailable provider into a leak.
			return fmt.Errorf("operation %s: no volume provider available for cleanup: %w", op.ID, volume.ErrProviderUnavailable)
		}
		for _, selected := range backends {
			if err := selected.backend.DestroyVolume(ctx, id); err != nil {
				return fmt.Errorf("operation %s: volume cleanup %s via %s: %w", op.ID, id, selected.name, err)
			}
		}
	case "sandbox.create":
		id := model.SandboxID(deterministicObjectID("sbx_", op.ID))
		lock := m.lock(id)
		lock.Lock()
		defer lock.Unlock()
		if rec, err := m.deps.Store.GetSandbox(ctx, id); err == nil {
			if err := m.releasePlacementLocked(ctx, id, false); err != nil {
				return err
			}
			if err := m.deps.Store.DeleteMounts(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("operation %s: delete recovered sandbox mounts: %w", op.ID, err)
			}
			if m.deps.Rootfs != nil && rec.Overlay != "" {
				ov, oerr := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
				if oerr != nil && !errors.Is(oerr, os.ErrNotExist) {
					return fmt.Errorf("operation %s: open recovered sandbox overlay: %w", op.ID, oerr)
				}
				if oerr == nil {
					if err := m.deps.Rootfs.DestroyOverlay(ctx, ov); err != nil && !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("operation %s: destroy recovered sandbox overlay: %w", op.ID, err)
					}
				}
			}
			if err := m.deps.Store.DeleteSandbox(ctx, id, nil); err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("operation %s: delete recovered sandbox record: %w", op.ID, err)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("operation %s: inspect recovered sandbox: %w", op.ID, err)
		}
	}
	return nil
}

func (m *Mediator) advanceOperation(ctx context.Context, claim operationClaim, phase model.OperationPhase) error {
	if claim.op == nil {
		return nil
	}
	if err := m.operationOwned(ctx, claim); err != nil {
		return err
	}
	current, err := m.deps.Store.GetOperation(ctx, claim.op.ID)
	if err != nil {
		return err
	}
	if current.Attempt != claim.op.Attempt {
		return model.ErrInProgress
	}
	if claim.lease != nil && current.LeaseUntil.After(claim.op.LeaseUntil) {
		claim.op.LeaseUntil = current.LeaseUntil
	}
	if err := current.Advance(phase, time.Now()); err != nil {
		return err
	}
	*claim.op = current
	if err := m.deps.Store.UpdateOperation(ctx, current); isOperationOwnershipConflict(err) {
		return model.ErrInProgress
	} else {
		return err
	}
}

func (m *Mediator) succeedOperation(ctx context.Context, claim operationClaim, result model.OperationResult) error {
	if claim.op == nil {
		return nil
	}
	if err := m.operationOwned(ctx, claim); err != nil {
		return err
	}
	current, err := m.deps.Store.GetOperation(ctx, claim.op.ID)
	if err != nil {
		return err
	}
	if current.Attempt != claim.op.Attempt {
		return model.ErrInProgress
	}
	if err := current.Succeed(result, time.Now()); err != nil {
		return err
	}
	*claim.op = current
	if err := m.deps.Store.UpdateOperation(ctx, current); isOperationOwnershipConflict(err) {
		return model.ErrInProgress
	} else {
		return err
	}
}

func (m *Mediator) failOperation(ctx context.Context, claim operationClaim, err error) {
	if claim.op == nil || err == nil {
		return
	}
	if claim.lease != nil && claim.lease.Err() != nil {
		return
	}
	// Stop renewal before terminalizing, then read the authoritative row. This
	// avoids writing a stale lease snapshot after a heartbeat raced the failure
	// path, while retaining the attempt fence.
	if claim.lease != nil {
		claim.lease.stop()
	}
	current, gerr := m.deps.Store.GetOperation(ctx, claim.op.ID)
	if gerr != nil || current.Attempt != claim.op.Attempt || current.State != model.OperationInProgress {
		return
	}
	class := operationErrorClass(err)
	if ferr := current.Fail(class, time.Now()); ferr != nil {
		m.deps.Logf("operation %s: record failure class: %v", current.ID, ferr)
		return
	}
	if ferr := m.deps.Store.UpdateOperation(ctx, current); ferr != nil && !isOperationOwnershipConflict(ferr) {
		m.deps.Logf("operation %s: persist failure class: %v", current.ID, ferr)
	}
}

func operationErrorClass(err error) model.ErrorClass {
	if class := model.ErrorClassOf(err); class != model.ErrorInternal {
		return class
	}
	switch {
	// Quota exhaustion is a durable capacity verdict.  In particular, an
	// idempotent retry of a failed create must remain 507 rather than becoming
	// an unrelated 503. Providers may surface either the admission sentinel or
	// the POSIX errno, including through contextual wrappers.
	case errors.Is(err, ErrInsufficientCapacity), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return model.ErrorCapacity

	// Deterministic request/configuration refusals cannot be repaired by
	// repeating the same request. Keep capability failures in the unsupported
	// class so protocol edges can preserve that distinction.
	case errors.Is(err, ErrCapRequired),
		errors.Is(err, ErrUncappedDiskNotCappable),
		errors.Is(err, ErrDiskCapBelowUsage),
		errors.Is(err, ErrVolumeCapBelowUsage),
		errors.Is(err, store.ErrMountBindingInvalid),
		errors.Is(err, network.ErrDeclarationInvalid),
		errors.Is(err, network.ErrSecondHopUIDIsTheEngineItself),
		errors.Is(err, volume.ErrInvalidProvider),
		errors.Is(err, fs.ErrInvalid),
		errors.Is(err, syscall.EINVAL):
		return model.ErrorInvalidArgument
	case errors.Is(err, ErrVolumeCapUnsupported), errors.Is(err, volume.ErrUnsupported), errors.Is(err, ErrLineageTooDeep):
		return model.ErrorUnsupported

	// Missing durable objects remain not-found across retries and restarts.
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTemplateNotFound),
		errors.Is(err, store.ErrVolumeNotFound), errors.Is(err, store.ErrPlacementNotFound),
		errors.Is(err, store.ErrProcessNotFound), errors.Is(err, store.ErrOperationNotFound),
		errors.Is(err, fs.ErrNotExist):
		return model.ErrorNotFound

	// State/reference conflicts are distinct from provider availability: a
	// caller may retry after the conflicting state changes, but the object is
	// present and the operation itself was understood.
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrMountLeaseConflict),
		errors.Is(err, store.ErrVolumeInUse), errors.Is(err, store.ErrVolumeHasDescendants),
		errors.Is(err, store.ErrSandboxHasMounts), errors.Is(err, store.ErrInUse),
		errors.Is(err, store.ErrProcessConflict), errors.Is(err, store.ErrOperationKeyConflict),
		errors.Is(err, store.ErrOperationOwnershipConflict), errors.Is(err, rootfs.ErrCapBelowUsage),
		errors.Is(err, ErrResourceTerminated), errors.Is(err, ErrVolumeLiveWriter),
		errors.Is(err, ErrVolumeDirectWriteInFlight), errors.Is(err, network.ErrViaUnenforceableAtPlacement):
		return model.ErrorConflict

	// A durable provider reference that cannot currently be resolved is an
	// availability fault. The row exists; assembly can be repaired and retried.
	case errors.Is(err, volume.ErrProviderUnavailable), errors.Is(err, volume.ErrUnknownProvider):
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

func semanticSandboxCreate(spec CreateSpec, network model.NetworkDecl) any {
	mounts := make([]semanticMount, 0, len(spec.Volumes))
	for _, mt := range spec.Volumes {
		mounts = append(mounts, semanticMount{Volume: string(mt.Volume), Path: store.NormalizeMountPath(mt.Path), Access: string(mt.Access)})
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Path != mounts[j].Path {
			return mounts[i].Path < mounts[j].Path
		}
		if mounts[i].Volume != mounts[j].Volume {
			return mounts[i].Volume < mounts[j].Volume
		}
		return mounts[i].Access < mounts[j].Access
	})
	// Preserve caller intent for optional TTLs rather than hashing the
	// materialized defaults. A nil field means "use the deployment default";
	// hashing its current value would make a same-key retry conflict after a
	// restart that changed that default. Explicit zero remains distinct from
	// omission, because zero deliberately disables the deadline.
	var idleNanos, retentionNanos *int64
	if spec.IdleTimeout != nil {
		v := int64(*spec.IdleTimeout)
		idleNanos = &v
	}
	if spec.Retention != nil {
		v := int64(*spec.Retention)
		retentionNanos = &v
	}
	return struct {
		Template       string            `json:"template"`
		IdleNanos      *int64            `json:"idle_nanos,omitempty"`
		RetentionNanos *int64            `json:"retention_nanos,omitempty"`
		Limits         model.Limits      `json:"limits"`
		Volumes        []semanticMount   `json:"volumes"`
		Network        model.NetworkDecl `json:"network"`
	}{string(spec.Template), idleNanos, retentionNanos, spec.Limits, mounts, network.Normalize()}
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
