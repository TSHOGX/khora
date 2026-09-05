package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

func TestOperationErrorClassPreservesLedgerVerdicts(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class model.ErrorClass
	}{
		{name: "typed capacity", err: model.ErrCapacity, class: model.ErrorCapacity},
		{name: "admission capacity", err: ErrInsufficientCapacity, class: model.ErrorCapacity},
		{name: "wrapped enospc", err: fmt.Errorf("write: %w", syscall.ENOSPC), class: model.ErrorCapacity},
		{name: "wrapped edquot", err: fmt.Errorf("quota: %w", syscall.EDQUOT), class: model.ErrorCapacity},
		{name: "cap required", err: ErrCapRequired, class: model.ErrorInvalidArgument},
		{name: "uncapped disk not cappable", err: ErrUncappedDiskNotCappable, class: model.ErrorInvalidArgument},
		{name: "disk cap below usage", err: ErrDiskCapBelowUsage, class: model.ErrorInvalidArgument},
		{name: "volume cap below usage", err: ErrVolumeCapBelowUsage, class: model.ErrorInvalidArgument},
		{name: "invalid mount binding", err: store.ErrMountBindingInvalid, class: model.ErrorInvalidArgument},
		{name: "invalid declaration", err: network.ErrDeclarationInvalid, class: model.ErrorInvalidArgument},
		{name: "invalid provider", err: volume.ErrInvalidProvider, class: model.ErrorInvalidArgument},
		{name: "invalid errno", err: syscall.EINVAL, class: model.ErrorInvalidArgument},
		{name: "unsupported volume", err: volume.ErrUnsupported, class: model.ErrorUnsupported},
		{name: "unsupported volume cap", err: ErrVolumeCapUnsupported, class: model.ErrorUnsupported},
		{name: "unsupported lineage", err: ErrLineageTooDeep, class: model.ErrorUnsupported},
		{name: "missing sandbox", err: store.ErrNotFound, class: model.ErrorNotFound},
		{name: "missing template", err: store.ErrTemplateNotFound, class: model.ErrorNotFound},
		{name: "missing volume", err: store.ErrVolumeNotFound, class: model.ErrorNotFound},
		{name: "missing placement", err: store.ErrPlacementNotFound, class: model.ErrorNotFound},
		{name: "missing process", err: store.ErrProcessNotFound, class: model.ErrorNotFound},
		{name: "missing operation", err: store.ErrOperationNotFound, class: model.ErrorNotFound},
		{name: "missing path", err: syscall.ENOENT, class: model.ErrorNotFound},
		{name: "state conflict", err: store.ErrConflict, class: model.ErrorConflict},
		{name: "mount lease conflict", err: store.ErrMountLeaseConflict, class: model.ErrorConflict},
		{name: "volume in use", err: store.ErrVolumeInUse, class: model.ErrorConflict},
		{name: "volume descendants", err: store.ErrVolumeHasDescendants, class: model.ErrorConflict},
		{name: "sandbox mounts", err: store.ErrSandboxHasMounts, class: model.ErrorConflict},
		{name: "template in use", err: store.ErrInUse, class: model.ErrorConflict},
		{name: "process conflict", err: store.ErrProcessConflict, class: model.ErrorConflict},
		{name: "ownership conflict", err: store.ErrOperationOwnershipConflict, class: model.ErrorConflict},
		{name: "rootfs cap conflict", err: rootfs.ErrCapBelowUsage, class: model.ErrorConflict},
		{name: "resource terminated", err: ErrResourceTerminated, class: model.ErrorConflict},
		{name: "live writer", err: ErrVolumeLiveWriter, class: model.ErrorConflict},
		{name: "direct write", err: ErrVolumeDirectWriteInFlight, class: model.ErrorConflict},
		{name: "placement declaration conflict", err: network.ErrViaUnenforceableAtPlacement, class: model.ErrorConflict},
		{name: "provider unavailable", err: volume.ErrProviderUnavailable, class: model.ErrorUnavailable},
		{name: "provider unknown", err: volume.ErrUnknownProvider, class: model.ErrorUnavailable},
		{name: "typed invalid", err: model.ErrInvalidArgument, class: model.ErrorInvalidArgument},
		{name: "typed unsupported", err: model.ErrUnsupported, class: model.ErrorUnsupported},
		{name: "typed unavailable", err: model.ErrUnavailable, class: model.ErrorUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationErrorClass(tc.err); got != tc.class {
				t.Fatalf("operationErrorClass(%v) = %q; want %q", tc.err, got, tc.class)
			}
		})
	}
}

func TestFailedVolumeOperationRetriesKeepClassAcrossStores(t *testing.T) {
	type storeCase struct {
		name string
		open func(t *testing.T) (store.Store, func() store.Store)
	}
	cases := []storeCase{
		{
			name: "memory",
			open: func(t *testing.T) (store.Store, func() store.Store) {
				return store.NewMemory(), nil
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) (store.Store, func() store.Store) {
				path := filepath.Join(t.TempDir(), "state.db")
				s, err := store.OpenSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
				current := s
				t.Cleanup(func() { _ = current.Close() })
				return s, func() store.Store {
					_ = current.Close()
					next, err := store.OpenSQLite(path)
					if err != nil {
						t.Fatal(err)
					}
					current = next
					t.Cleanup(func() { _ = next.Close() })
					return next
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, verdict := range []struct {
				name      string
				capByte   int64
				wantClass model.ErrorClass
			}{
				{name: "capacity", capByte: 2, wantClass: model.ErrorCapacity},
				{name: "cap-required", capByte: 0, wantClass: model.ErrorInvalidArgument},
			} {
				t.Run(verdict.name, func(t *testing.T) {
					st, reopen := tc.open(t)
					m := New(Deps{
						Store:        st,
						Rootfs:       &fakeRootfs{},
						Volume:       newFakeVolume(),
						NodeCapacity: NodeCapacity{DiskByte: 1},
						Logf:         func(string, ...any) {},
					})
					key := model.IdempotencyKey("failed-" + verdict.name)
					if _, err := m.CreateVolumeWithKey(context.Background(), verdict.capByte, key); err == nil {
						t.Fatal("first operation unexpectedly succeeded")
					}
					op, err := st.GetOperationByKey(context.Background(), "volume.create", key)
					if err != nil {
						t.Fatalf("read failed operation: %v", err)
					}
					if op.State != model.OperationFailed || op.ErrorClass != verdict.wantClass {
						t.Fatalf("failed operation = state %q class %q; want failed/%q", op.State, op.ErrorClass, verdict.wantClass)
					}
					if reopen != nil {
						st = reopen()
						m = New(Deps{
							Store:        st,
							Rootfs:       &fakeRootfs{},
							Volume:       newFakeVolume(),
							NodeCapacity: NodeCapacity{DiskByte: 1},
							Logf:         func(string, ...any) {},
						})
					}
					_, retryErr := m.CreateVolumeWithKey(context.Background(), verdict.capByte, key)
					if got := model.ErrorClassOf(retryErr); got != verdict.wantClass {
						t.Fatalf("idempotent retry error class = %q (%v); want %q", got, retryErr, verdict.wantClass)
					}
				})
			}
		})
	}
}

func TestErrorCapacityValidatesAndRoundTrips(t *testing.T) {
	op, err := model.BeginOperation("op_capacity", "volume.create", "capacity-key", model.DigestBytes([]byte("capacity")), nowForOperationClassTest(), model.DefaultOperationLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Fail(model.ErrorCapacity, op.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatalf("Fail(capacity): %v", err)
	}
	if err := op.Validate(); err != nil {
		t.Fatalf("failed capacity operation invalid: %v", err)
	}
}

func nowForOperationClassTest() time.Time { return time.Unix(100, 0).UTC() }
