package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/orchestrator"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

type delayedBootReclaim struct {
	volume.Backend
	calls    int
	timedOut bool
}

func (b *delayedBootReclaim) Reclaim(ctx context.Context) (int, error) {
	b.calls++
	if b.calls == 1 {
		<-ctx.Done()
		b.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		return 0, ctx.Err()
	}
	return 1, nil
}

func TestBootReclaimTimeoutStillServesAndRetries(t *testing.T) {
	dir, err := volume.NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &delayedBootReclaim{Backend: dir}
	st := store.NewMemory()
	med := lifecycle.New(lifecycle.Deps{Store: st, Volume: backend, QuotaReclaimTimeout: 10 * time.Millisecond, Logf: t.Logf})
	eng := &orchestrator.Engine{Store: st, Mediator: med, Publisher: store.NewPublisher(st, store.NewDedupSink(func(store.Event) {}))}
	done := make(chan struct{})
	go func() { defer close(done); bootVolumeStorage(context.Background(), eng) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("boot did not return")
	}
	if !backend.timedOut {
		t.Fatal("fault did not reach budget")
	}
	eng.MarkReady()
	rr := httptest.NewRecorder()
	eng.HTTPHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 200 {
		t.Fatalf("not serving after recovery timeout: %d %s", rr.Code, rr.Body.String())
	}
	if err := eng.ReclaimVolumeStorage(context.Background()); err != nil || backend.calls != 2 {
		t.Fatalf("retry calls=%d err=%v", backend.calls, err)
	}
}
