package orchestrator

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

type blockedCounts struct{ store.Store }

func (s blockedCounts) ListSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMetricsStillReportsLoopsWhenCountsTimeOut(t *testing.T) {
	base := store.NewMemory()
	eng := &Engine{Store: base, Mediator: lifecycle.New(lifecycle.Deps{Store: blockedCounts{base}}), Publisher: store.NewPublisher(base, store.NewDedupSink(func(store.Event) {}))}
	eng.Reaper = lifecycle.NewReaper(eng.Mediator, time.Second, time.Second)
	eng.health.startedAt = time.Now()
	done := make(chan struct{})
	rr := httptest.NewRecorder()
	go func() { defer close(done); eng.metricsHandler(rr, httptest.NewRequest("GET", "/metrics", nil)) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("counts hid process metrics")
	}
	body := rr.Body.String()
	if !strings.Contains(body, "khora_reaper_ticks_completed_total 0") || !strings.Contains(body, "khora_sandboxes omitted") {
		t.Fatalf("metrics: %s", body)
	}
}
