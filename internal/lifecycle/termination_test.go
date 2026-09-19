package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
)

type blockedStopRuntime struct {
	runtime.Provider
	entered, release chan struct{}
}

func (r *blockedStopRuntime) Stop(ctx context.Context, p *runtime.Placement) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.Provider.Stop(ctx, p)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBlockedStopRetainsExclusiveWriterFence(t *testing.T) {
	ctx := context.Background()
	m, st, _, rt := mountFixture(t)
	volume, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Create(ctx, CreateSpec{Template: "bare", Volumes: []model.Mount{{Volume: volume, Path: "/data", Access: model.Exclusive}}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockedStopRuntime{Provider: rt, entered: make(chan struct{}), release: make(chan struct{})}
	m.deps.Runtime = blocked
	done := make(chan error, 1)
	go func() { done <- m.Pause(ctx, id) }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("stop was not reached")
	}
	if _, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("new")); !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("import bypassed blocked stop: %v", err)
	}
	placement, err := st.GetPlacement(ctx, id)
	if err != nil || placement.State != model.PlacementStopping {
		t.Fatalf("no durable stopping fence: %+v %v", placement, err)
	}
	if _, err := m.Endpoint(id); err == nil {
		t.Fatal("stopping endpoint remained addressable")
	}
	close(blocked.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("after stop")); err != nil {
		t.Fatal(err)
	}
}

func TestFailedStopBlocksReuseAcrossRestartUntilReclaimed(t *testing.T) {
	ctx := context.Background()
	m, st, _, rt := mountFixture(t)
	volume, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Create(ctx, CreateSpec{Template: "bare", Volumes: []model.Mount{{Volume: volume, Path: "/data", Access: model.Exclusive}}})
	if err != nil {
		t.Fatal(err)
	}
	rt.failStop = true
	if err := m.Pause(ctx, id); err == nil {
		t.Fatal("stop failure hidden")
	}
	restarted := New(m.deps)
	if _, err := restarted.ImportVolume(ctx, volume, "file", strings.NewReader("new")); !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("restart lost writer fence: %v", err)
	}
	if err := restarted.Resume(ctx, id); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restart resumed unverified writer: %v", err)
	}
	var detached []model.Mount
	if _, err := restarted.Rebind(ctx, id, RebindSpec{Mounts: &detached}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("detached unverified writer: %v", err)
	}
	rt.failDestroy = true
	if err := restarted.Destroy(ctx, id); err == nil {
		t.Fatal("destroy dropped unresolved placement")
	}
	mounts, err := st.ListMounts(ctx, id)
	if err != nil || len(mounts) != 1 {
		t.Fatalf("exclusive lease lost: %+v %v", mounts, err)
	}
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ImportVolume(ctx, volume, "file", strings.NewReader("new")); !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("failed recovery released writer fence: %v", err)
	}
	rt.failDestroy = false
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ImportVolume(ctx, volume, "file", strings.NewReader("recovered")); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCreateRetainsMountsUntilOrphanWriterCleanup(t *testing.T) {
	ctx := context.Background()
	m, st, _, rt := mountFixture(t)
	volume, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	rt.failLaunch = true
	rt.failDestroy = true
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", Volumes: []model.Mount{{Volume: volume, Path: "/data", Access: model.Exclusive}}}); err == nil {
		t.Fatal("failed launch succeeded")
	}
	records, err := st.ListSandboxes(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("unresolved cleanup authority lost: %+v %v", records, err)
	}
	mounts, err := st.ListMounts(ctx, records[0].ID)
	if err != nil || len(mounts) != 1 {
		t.Fatalf("unresolved mount lease lost: %+v %v", mounts, err)
	}
	if _, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("overwrite")); !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("failed launch admitted direct writes: %v", err)
	}
	rt.failLaunch = false
	rt.failDestroy = false
	if err := m.ReconcilePeriodic(ctx); err != nil {
		t.Fatal(err)
	}
	records, err = st.ListSandboxes(ctx)
	if err != nil || len(records) != 0 {
		t.Fatalf("cleanup retry did not remove failed create: %+v %v", records, err)
	}
	if _, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("after cleanup")); err != nil {
		t.Fatal(err)
	}
}

type heldMountSnapshot struct {
	store.Store
	entered, release chan struct{}
	held             atomic.Bool
}

func (s *heldMountSnapshot) ListMountsByVolume(ctx context.Context, id model.VolumeID) ([]model.Mount, error) {
	rows, err := s.Store.ListMountsByVolume(ctx, id)
	if !s.held.Swap(true) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return rows, err
}

func TestDirectWriteCannotUseMountSnapshotBeforeNewWriter(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	volume, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &heldMountSnapshot{Store: st, entered: make(chan struct{}), release: make(chan struct{})}
	m.deps.Store = snapshot
	imported := make(chan error, 1)
	go func() {
		_, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("racing write"))
		imported <- err
	}()
	select {
	case <-snapshot.entered:
	case <-time.After(time.Second):
		t.Fatal("snapshot was not captured")
	}
	id, err := m.Create(ctx, CreateSpec{Template: "bare", Volumes: []model.Mount{{Volume: volume, Path: "/data", Access: model.Exclusive}}})
	if err != nil {
		close(snapshot.release)
		t.Fatal(err)
	}
	close(snapshot.release)
	if err := <-imported; !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("stale mount snapshot bypassed new live writer: %v", err)
	}
	if err := m.Pause(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportVolume(ctx, volume, "file", strings.NewReader("after pause")); err != nil {
		t.Fatal(err)
	}
}
