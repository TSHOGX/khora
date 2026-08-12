package orchestrator

// Background-loop OWNERSHIP (§1.1 the assembly).
//
// The three autonomous loops — the outbox publisher, its health monitor, and the §4.3 reaper —
// live here rather than in cmd/ because their lifetime is a MIDDLE STEP of the teardown sequence
// Engine.Shutdown documents and performs: stopped after readiness flips, before the store
// closes. A caller that owned them would have to split Shutdown open and re-implement the order
// at the call site, which is what cmd/orchestrator did — leaving the sequence stated in one
// place and executed in another, and the two did disagree.
//
// So the rule this file exists to enforce: EVERY WAIT IN THE TEARDOWN GRAPH IS BOUNDED
// BY THE ONE DEADLINE ITS CALLER SUPPLIES. A loop that ignores cancellation cannot
// extend shutdown past that deadline, for the same reason a stuck HTTP handler cannot
// (handlerTracker.wait) — an unbounded wait anywhere in the graph makes the whole
// shutdown unbounded, and the caller's --shutdown-timeout stops meaning anything.

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/TSHOGX/warren/internal/store"
)

// LoopConfig tunes the background loops. Zero values take the defaults below, so a
// caller that has no opinion passes the zero struct.
type LoopConfig struct {
	// PublishInterval is the outbox drain cadence (§4.4-C). Zero → 1s.
	PublishInterval time.Duration
	// MonitorInterval is how often publisher health is SAMPLED. Zero → 10s.
	MonitorInterval time.Duration
	// StallThreshold is how long without progress counts as unhealthy. Zero → 30 ×
	// PublishInterval.
	StallThreshold time.Duration
	// ReportStats receives each unhealthy sample. nil → logStats below.
	//
	// Sampling happens on an INDEPENDENT goroutine (§4.4-C operability) and that is
	// load-bearing rather than tidy: sampling on the drain itself would freeze the
	// numbers exactly when the drain blocks, so silence would mean both "healthy" and
	// "wedged". Publisher.Stats reads atomics for this reason.
	ReportStats func(store.Stats)
}

func (c LoopConfig) withDefaults() LoopConfig {
	if c.PublishInterval <= 0 {
		c.PublishInterval = time.Second
	}
	if c.MonitorInterval <= 0 {
		c.MonitorInterval = 10 * time.Second
	}
	if c.StallThreshold <= 0 {
		c.StallThreshold = 30 * c.PublishInterval
	}
	if c.ReportStats == nil {
		c.ReportStats = logStats
	}
	return c
}

// loopSet is the lifetime of the background loops: one context cancelling all of
// them, and one channel closed once every one has returned. Both are needed —
// cancelling says "stop", the channel says "stopped", and only the second is what the
// store may be closed behind.
//
// A CHANNEL rather than exposing the WaitGroup, because "have the loops exited" has to
// be a fact something else can OBSERVE, not just wait on. Two waiters on one WaitGroup
// both wake when it drains, in no order relative to each other — so a caller wanting to
// assert "stopped before the store closed" cannot express it against a WaitGroup at all.
// The channel is closed by the watcher below before any waiter proceeds, which makes the
// ordering readable rather than racy.
type loopSet struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	allDone chan struct{}
	started bool
}

// exited reports whether every loop has returned. False before StartLoops, since an
// engine with no loops has none outstanding — the distinction that matters to a caller
// is "could a loop still touch the store", and before StartLoops the answer is no.
func (l *loopSet) exited() bool {
	l.mu.Lock()
	done, started := l.allDone, l.started
	l.mu.Unlock()
	if !started {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// StartLoops starts the three §4.3/§4.4-C background loops. Call it once, AFTER the
// boot sequence (Reconcile and the sweeps) so no loop races the boot-time orphan
// reclaim and re-adoption, and before serving.
//
// The loops are stopped by Shutdown, not by the caller: see this file's header for why
// the assembly owns them. Calling twice is a no-op on the second call, so an embedder
// cannot accidentally double-start the reaper.
func (e *Engine) StartLoops(cfg LoopConfig) {
	cfg = cfg.withDefaults()

	e.loops.mu.Lock()
	defer e.loops.mu.Unlock()
	if e.loops.started {
		return
	}
	// context.Background, not a caller's context: the loops' lifetime is the ENGINE's,
	// and Shutdown is the one thing that ends it. Deriving from a request-scoped or
	// signal-scoped context would give the loops a second way to die, outside the
	// bounded sequence — which is the shape that produced an unbounded teardown here
	// before.
	ctx, cancel := context.WithCancel(context.Background())
	e.loops.cancel = cancel
	e.loops.allDone = make(chan struct{})
	e.loops.started = true

	e.loops.wg.Add(3)
	go func() { defer e.loops.wg.Done(); e.Publisher.Run(ctx, cfg.PublishInterval) }()
	go func() {
		defer e.loops.wg.Done()
		e.Publisher.Monitor(ctx, cfg.MonitorInterval, cfg.StallThreshold, cfg.ReportStats)
	}()
	go func() { defer e.loops.wg.Done(); e.Reaper.Run(ctx) }()

	// One watcher turns "the WaitGroup drained" into a fact anyone can observe, once.
	// It is what makes step 2's completion checkable from outside stopLoops.
	allDone := e.loops.allDone
	go func() { e.loops.wg.Wait(); close(allDone) }()
}

// stopLoops cancels the loops and waits, bounded by ctx, for them to exit. It returns
// ctx.Err() if a loop is still inside a substrate call when the deadline lands: not
// every loop iteration honors cancellation promptly, and one that does not must not
// extend shutdown past the budget the operator set.
//
// Returning rather than waiting means the store may close under a still-running loop,
// whose next call then fails. That is the deliberate trade and it is the safe half: a
// failed store call on the way out is logged and retried by the NEXT boot's loop,
// whereas a shutdown that overruns its TimeoutStopSec is SIGKILLed mid-transition,
// which is the outcome §4.4-A reconciliation then has to clean up.
func (e *Engine) stopLoops(ctx context.Context) error {
	e.loops.mu.Lock()
	cancel, allDone, started := e.loops.cancel, e.loops.allDone, e.loops.started
	e.loops.mu.Unlock()
	if !started {
		return nil
	}
	cancel()

	select {
	case <-allDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// logStats is the default unhealthy-sample reporter. This repo has no metrics
// dependency and this does not introduce one (§1.1 no heavyweight external
// dependencies): the requirement is that a wedged drain be DISTINGUISHABLE from an
// idle one, and the log is what this codebase already uses for that class of fact.
// /metrics exposes the same facts for a scraper (health.go).
//
// A halted publisher is the line that must be noticed: the event stream has STOPPED,
// which for a billing subscriber means charges stop accruing — it is not a degraded
// mode that heals itself, and clearing it is an operator action (Publisher.Resume).
func logStats(s store.Stats) {
	if s.Halted {
		log.Printf("ALERT: outbox publisher HALTED on event %s (seq %d) after %d attempts: %s — "+
			"the event stream has stopped; %d rows are undelivered and nothing behind this event will be published until an operator intervenes",
			s.HeadEventID, s.HeadSeq, s.Attempts, s.LastError, s.Backlog)
		return
	}
	log.Printf("WARN: outbox publisher unhealthy: head=%s seq=%d attempts=%d backlog=%d published=%d since_heartbeat=%s in_flight_for=%s last_err=%q",
		s.HeadEventID, s.HeadSeq, s.Attempts, s.Backlog, s.Published,
		s.SinceHeartbeat.Round(time.Millisecond), s.InFlightFor.Round(time.Millisecond), s.LastError)
}
