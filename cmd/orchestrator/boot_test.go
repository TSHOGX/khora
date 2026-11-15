package main

import (
	"context"
	"errors"
	"flag"
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

func TestZeroTTLFlagsStayDisabled(t *testing.T) {
	var values orchestratorFlags
	fs := newOrchestratorFlagSet("orchestrator", flag.ContinueOnError, &values)
	if err := fs.Parse([]string{"--idle-timeout", "0s", "--retention", "0s"}); err != nil {
		t.Fatal(err)
	}
	if values.idleTimeout != 0 || values.retention != 0 {
		t.Fatalf("parsed idle=%s retention=%s; want 0s/0s", values.idleTimeout, values.retention)
	}
	cfg := orchestrator.Config{
		DefaultIdleTimeout: &values.idleTimeout,
		DefaultRetention:   &values.retention,
	}
	if cfg.DefaultIdleTimeout == nil || *cfg.DefaultIdleTimeout != 0 || cfg.DefaultRetention == nil || *cfg.DefaultRetention != 0 {
		t.Fatal("explicit zero flag values were dropped before assembly")
	}
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
