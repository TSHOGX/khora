package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/store"
)

// THE ORDERING TEST, and the reason this file exists. Steps 2 and 5 of Shutdown's
// sequence are "stop the loops" and "close the store", in that order, because a loop
// tick landing after the close hits a closed store — a spurious error on the way out
// that reads like a defect. That ordering used to be unassertable: the loops were owned
// by cmd/orchestrator's main(), so the real teardown sequence was assembled inside a
// signal handler and no test could call it. Every drain test exercised a path with no
// loops in it at all, which is why the order could drift for eight commits without one
// failing.
func TestShutdownStopsLoopsBeforeClosingStore(t *testing.T) {
	eng := healthEngine(t)

	// The assertion is made AT THE MOMENT OF THE CLOSE, against the loopSet's own
	// published fact — not by a second waiter on the same WaitGroup, which would wake in
	// no order relative to Shutdown's waiter and so could not witness the ordering at
	// all.
	var storeClosed bool
	eng.Store = &closeTrackingStore{Store: eng.Store, close: func() {
		storeClosed = true
		if !eng.loops.exited() {
			t.Error("the store closed while a background loop was still running — a tick landing after " +
				"this logs a spurious store-closed error on the way out (Shutdown step 2 must precede step 5)")
		}
	}}

	// A fast cadence so every loop takes several real ticks inside the window, rather
	// than sitting on a 1s timer and exiting from a state indistinguishable from
	// never-started.
	eng.StartLoops(LoopConfig{
		PublishInterval: time.Millisecond,
		MonitorInterval: time.Millisecond,
		ReportStats:     func(store.Stats) {},
	})
	time.Sleep(10 * time.Millisecond)
	if eng.loops.exited() {
		t.Fatal("the loops exited on their own before the shutdown — the test is not observing running loops")
	}

	if err := eng.Shutdown(context.Background(), nil); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !storeClosed {
		t.Error("Shutdown did not close the store")
	}
}

// A loop stuck below cancellation must not extend shutdown past the operator's
// --shutdown-timeout: one unbounded wait anywhere in the teardown graph makes the whole
// shutdown unbounded, and the process is then SIGKILLed instead of exiting.
func TestStopLoopsHonorsTheDeadline(t *testing.T) {
	eng := healthEngine(t)

	// Occupy the WaitGroup the way a loop wedged in a substrate call would: cancelled,
	// but not yet returned.
	eng.loops.started = true
	eng.loops.cancel = func() {}
	eng.loops.wg.Add(1)
	t.Cleanup(eng.loops.wg.Done)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := eng.stopLoops(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopLoops error = %v, want deadline exceeded", err)
	}
}

// stopLoops on an engine whose loops were never started is a no-op rather than a nil
// dereference: every substrate test and every embedder assembles an Engine and shuts it
// down without ever calling StartLoops.
func TestStopLoopsWithoutStartIsANoOp(t *testing.T) {
	eng := healthEngine(t)
	if err := eng.stopLoops(context.Background()); err != nil {
		t.Fatalf("stopLoops on an unstarted engine: %v", err)
	}
}

// StartLoops twice must not start a second reaper. Two reapers would run the health and reclamation
// sweeps concurrently, and the per-sandbox transition lock (per-sandbox transition serialization) is what would then
// be carrying the concurrency rather than the design.
func TestStartLoopsIsIdempotent(t *testing.T) {
	eng := healthEngine(t)
	eng.StartLoops(LoopConfig{ReportStats: func(store.Stats) {}})
	eng.StartLoops(LoopConfig{ReportStats: func(store.Stats) {}})
	t.Cleanup(func() {
		if err := eng.stopLoops(context.Background()); err != nil {
			t.Errorf("stopLoops: %v", err)
		}
	})

	// Three loops, not six.
	done := make(chan struct{})
	go func() { eng.loops.wg.Wait(); close(done) }()
	if err := eng.stopLoops(context.Background()); err != nil {
		t.Fatalf("stopLoops: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second StartLoops started loops the first call's cancel does not stop")
	}
}
