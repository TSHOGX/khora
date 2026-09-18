package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
	"github.com/TSHOGX/khora/internal/waitfor"
)

func TestReclamationErrorsKeepClassesWithoutNewlines(t *testing.T) {
	other := errors.New("backend failed")
	err := reclaimErrors(context.DeadlineExceeded, other)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, other) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("aggregation: %v", err)
	}
}

type blockingReclaimVolume struct {
	*namedFakeVolume
	entered chan struct{}
}

func (b *blockingReclaimVolume) Reclaim(ctx context.Context) (int, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestReclaimDeadlineKeepsOtherBackendsAndLaterTicksAlive(t *testing.T) {
	stuck := &blockingReclaimVolume{namedFakeVolume: &namedFakeVolume{newFakeVolume(), "a"}}
	stuck.pendingCleanup = 1
	healthy := &namedFakeVolume{newFakeVolume(), "b"}
	healthy.pendingCleanup = 1
	registry, err := volume.NewRegistryWithDefault("a", stuck, healthy)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: store.NewMemory(), Rootfs: &fakeRootfs{}, Runtime: &fakeRuntime{}, VolumeRegistry: registry, QuotaReclaimTimeout: 10 * time.Millisecond, Logf: func(string, ...any) {}})
	if err := m.ReclaimVolumeStorage(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reclaim: %v", err)
	}
	if healthy.pendingCount() != 0 || stuck.pendingCount() != 1 {
		t.Fatal("deadline lost pending work or blocked another backend")
	}
	r := NewReaper(m, time.Second, time.Second)
	for i := 0; i < 2; i++ {
		r.tick(context.Background())
	}
	stats := r.Stats()
	if stats.TicksCompleted != 2 || stats.QuotaTimeouts != 2 || stats.LastCompleted.IsZero() {
		t.Fatalf("stats: %+v", stats)
	}
	if len(r.telemetryQueue) != 1 {
		t.Fatal("timeout prevented telemetry scheduling")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.tick(ctx)
	if r.Stats() != stats {
		t.Fatal("cancelled tick advanced stats")
	}
}

func TestReaperProgressStopsWhileTickIsBlocked(t *testing.T) {
	stuck := &blockingReclaimVolume{namedFakeVolume: &namedFakeVolume{newFakeVolume(), "a"}, entered: make(chan struct{}, 1)}
	stuck.pendingCleanup = 1
	m := New(Deps{Store: store.NewMemory(), Rootfs: &fakeRootfs{}, Runtime: &fakeRuntime{}, Volume: stuck, QuotaReclaimTimeout: time.Hour, Logf: func(string, ...any) {}})
	r := NewReaper(m, time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	select {
	case <-stuck.entered:
	case <-time.After(time.Second):
		t.Fatal("tick did not enter reclamation")
	}
	if s := r.Stats(); s.TicksCompleted != 0 || !s.LastCompleted.IsZero() {
		t.Fatalf("false progress: %+v", s)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not exit")
	}
	if r.Stats().TicksCompleted != 0 {
		t.Fatal("cancelled tick counted")
	}
}

func TestEmptyReaperReportsProgress(t *testing.T) {
	m := New(Deps{Store: store.NewMemory(), Rootfs: &fakeRootfs{}, Runtime: &fakeRuntime{}, Logf: func(string, ...any) {}})
	r := NewReaper(m, time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	waitfor.Must(t, "empty reaper progress", time.Second, time.Millisecond, func() bool { return r.Stats().TicksCompleted > 1 })
	cancel()
	<-done
}

type unreadableReclaimStore struct{ store.Store }

func (unreadableReclaimStore) ListVolumes(context.Context) ([]model.Volume, error) {
	return nil, errors.New("store should not be read for backend residue")
}

func TestReclaimDoesNotReadStore(t *testing.T) {
	b := newFakeVolume()
	b.pendingCleanup = 1
	m := New(Deps{Store: unreadableReclaimStore{store.NewMemory()}, Volume: b})
	if err := m.ReclaimVolumeStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.pendingCount() != 0 {
		t.Fatal("residue was not reclaimed")
	}
}
