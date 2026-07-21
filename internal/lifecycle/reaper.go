package lifecycle

import (
	"context"
	"time"

	"engine/internal/model"
	"engine/internal/store"
)

// Reaper is the engine's first autonomous background control loop (§4.3 /
// reaper-health D0): it is NOT an independent collector but a CONSUMER of the
// health model. Each tick it classifies every sandbox (§4.3 four quadrants) and
// drives, per quadrant, the automatic/involuntary lifecycle transitions the
// state machine (§4.1) names but nothing else triggers — auto-pause, evict, and
// the LIVE (not just boot-time) crash-pause. It also folds in the periodic
// reconcile (stage 5) so one tick classifies once and both consumers dispatch
// off it (reaper-health D4: 同一个 tick 先分类、再分派).
//
// Dispositions go through the §4.4-B transition lock + CAS on the Mediator; the
// Reaper owns only the loop cadence and the transport-down restart budget
// tracker (D4b), which must persist across ticks.
type Reaper struct {
	m *Mediator

	// interval is the sweep cadence (reaper-health D5 numeric knob): trades
	// detection latency against sweep overhead. Coarse enough that a single
	// serial full-scan finishes well within one tick.
	interval time.Duration

	// transportDownBudget bounds how long a sandbox may stay CONTINUOUSLY
	// transport-down before the reaper escalates it to dead-recoverable → resume
	// (D4b: break a crash-looping transport child that never comes healthy).
	// Generous by default so a slow legitimate PID1 restart is never mistaken
	// for a crash-loop (reaper-health D5).
	transportDownBudget time.Duration

	// downSince tracks, per sandbox, when it was FIRST seen continuously
	// transport-down this streak. Reset the moment it classifies as anything
	// else. Reaper-loop-local (single goroutine drives Sweep), so no lock. This
	// is the "连续 transport-down 时长" measure D4b picks as simplest — no PID1
	// restart-count upcall needed.
	downSince map[model.SandboxID]time.Time
}

// reaper defaults (reaper-health D5 numeric knobs; bound no invariant's shape).
const (
	defaultSweepInterval       = 30 * time.Second
	defaultTransportDownBudget = 2 * time.Minute
)

// NewReaper builds the reaper for a mediator. Zero interval/budget fall back to
// the conservative defaults.
func NewReaper(m *Mediator, interval, transportDownBudget time.Duration) *Reaper {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	if transportDownBudget <= 0 {
		transportDownBudget = defaultTransportDownBudget
	}
	return &Reaper{
		m:                   m,
		interval:            interval,
		transportDownBudget: transportDownBudget,
		downSince:           make(map[model.SandboxID]time.Time),
	}
}

// Run drives one tick on a ticker until ctx is cancelled (§4.3 autonomous
// loop). The orchestrator starts it after startup reconcile, in its own
// goroutine.
func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick is one full cycle: periodic reconcile THEN sweep (reaper-health D4: one
// tick, the two consumers of the health model run in order). Reconcile handles
// "state vs reality" (orphan placements, dead `running` → crash-pause, takeover);
// the sweep handles "state right but should advance" (TTL auto-pause/evict,
// transport-down budget). Reconcile first so a `running` record whose unit died
// is demoted before the sweep would redundantly reach the same verdict; the
// sweep is idempotent either way. Reconcile failure is logged, not fatal — the
// sweep still runs, and reconcile retries next tick (§4.4-A skip-and-log).
func (r *Reaper) tick(ctx context.Context) {
	if err := r.m.ReconcilePeriodic(ctx); err != nil {
		r.m.deps.Logf("reaper: periodic reconcile (retried next tick): %v", err)
	}
	r.Sweep(ctx)
}

// Sweep is one full pass: classify every sandbox and dispatch per quadrant /
// per TTL (reaper-health D4). Serial per-sandbox, single full scan — enough for
// a single node with a bounded sandbox count; batching/rate-limiting is an
// additive tuning change that does not alter this shape (reaper-health D4).
// Failure policy is skip-and-log (§4.4-A): one un-dispatchable sandbox never
// aborts the rest, and the sweep is idempotent so a skipped item retries next
// tick.
func (r *Reaper) Sweep(ctx context.Context) {
	recs, err := r.m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		r.m.deps.Logf("reaper: list sandboxes (retried next tick): %v", err)
		return
	}
	seen := make(map[model.SandboxID]bool, len(recs))
	for _, rec := range recs {
		seen[rec.ID] = true
		switch rec.State {
		case model.Running:
			r.sweepRunning(ctx, rec)
		case model.Paused:
			r.sweepPaused(ctx, rec)
		}
	}
	// forget transport-down streaks for sandboxes that are gone (destroyed /
	// evicted) so the tracker cannot leak or misfire on a recycled ID.
	for id := range r.downSince {
		if !seen[id] {
			delete(r.downSince, id)
		}
	}
}

// sweepRunning classifies one `running` sandbox and disposes per quadrant, all
// under a NON-BLOCKING transition lock (reaper-health D3): a sandbox with a held
// lock has a verb driving it (in-flight resume / user pause) → skip this round,
// no race, no misclassification. The lock is held across classify AND dispose so
// the disposition acts on a lock-consistent view (D3), not a stale pre-lock one.
func (r *Reaper) sweepRunning(ctx context.Context, rec model.Sandbox) {
	l, ok := r.m.tryLock(rec.ID)
	if !ok {
		return // a verb holds the lock — skip (single-flight guard, D3)
	}
	defer l.Unlock()

	// Re-read under the lock: the record may have changed state between the
	// ListSandboxes snapshot and acquiring the lock (a user verb that finished
	// before we tryLock'd). Dispose only if it is still `running`.
	cur, err := r.m.deps.Store.GetSandbox(ctx, rec.ID)
	if err != nil || cur.State != model.Running {
		return
	}

	switch h := r.m.classify(ctx, cur); h {
	case HealthIdle:
		r.autoPauseLocked(ctx, cur)
	case HealthDeadRecoverable:
		// LIVE crash-pause (§4.2 involuntary pause): the isolation unit died under
		// a running orchestrator. Same disposition reconcile does at boot, driven
		// now by the reaper. Clear any transport-down streak — this is death, not
		// a transient.
		delete(r.downSince, cur.ID)
		if err := r.m.pauseLocked(ctx, cur.ID, "crash-paused"); err != nil && err != store.ErrConflict {
			r.m.deps.Logf("reaper: crash-pause %s (retried next tick): %v", cur.ID, err)
		}
	case HealthTransportDown:
		r.handleTransportDownLocked(ctx, cur)
	case HealthHealthy:
		// keep running; a healthy sandbox ends any transport-down streak.
		delete(r.downSince, cur.ID)
	default:
		// HealthUnknown (uncertain L1 / no live handle): do nothing, retry next
		// tick (protect-on-uncertainty, reaper-health D3).
	}
}

// autoPauseLocked auto-pauses an idle sandbox — with the D3 activity-race guard.
// CALLER HOLDS the lock. The sweep's classify→dispose window can be seconds; a
// sandbox that became active inside it is STILL `running`, so §4.4-B's CAS (which
// compares lifecycle-state only) would NOT catch it and would freeze a sandbox
// that just started working. So we RE-READ activity under the lock (one Health
// probe; the lock is held for the whole pause anyway, so the extra RPC is
// negligible) and abort if it is no longer idle. Disposition thus rests on the
// lock-latest {state + activity} double snapshot, not the sweep-start
// classification (reaper-health D3).
func (r *Reaper) autoPauseLocked(ctx context.Context, rec model.Sandbox) {
	if h := r.m.classify(ctx, rec); h != HealthIdle {
		// became active (or unreachable) in the window → abort, re-judge next tick.
		return
	}
	delete(r.downSince, rec.ID)
	if err := r.m.pauseLocked(ctx, rec.ID, "auto-paused"); err != nil && err != store.ErrConflict {
		r.m.deps.Logf("reaper: auto-pause %s (retried next tick): %v", rec.ID, err)
	}
}

// handleTransportDownLocked applies the D4b restart budget. CALLER HOLDS the
// lock. A transport-down sandbox is a transient — PID1 is restarting the
// transport child in place on the SAME IP (§5.E.1) — so the default action is
// WAIT (do nothing). But we track how long it has been CONTINUOUSLY down; past
// the budget it is a crash-loop, not a slow restart, so we escalate to
// dead-recoverable: crash-pause then resume (new IP, new epoch) to break the
// wedge (D4b). Both steps run under the held lock, same path a user
// pause+resume would take.
func (r *Reaper) handleTransportDownLocked(ctx context.Context, rec model.Sandbox) {
	first, tracked := r.downSince[rec.ID]
	if !tracked {
		r.downSince[rec.ID] = time.Now()
		return // first sighting this streak — wait for the in-place restart
	}
	if time.Since(first) < r.transportDownBudget {
		return // still within budget — keep waiting
	}
	// past the restart budget → escalate (D4b). crash-pause, then resume.
	r.m.deps.Logf("reaper: %s transport-down past budget (%s) → escalate to resume (D4b)", rec.ID, r.transportDownBudget)
	delete(r.downSince, rec.ID)
	if err := r.m.pauseLocked(ctx, rec.ID, "crash-paused"); err != nil {
		if err != store.ErrConflict {
			r.m.deps.Logf("reaper: escalate crash-pause %s (retried next tick): %v", rec.ID, err)
		}
		return
	}
	// now paused → resume (cold-start, new epoch). Re-read for a fresh record.
	paused, err := r.m.deps.Store.GetSandbox(ctx, rec.ID)
	if err != nil {
		r.m.deps.Logf("reaper: escalate resume %s: reread: %v", rec.ID, err)
		return
	}
	if err := r.m.resumeLocked(ctx, paused); err != nil {
		// resume failed → stays paused (§4.4-A); a later user/next-tick resume can
		// retry. Not fatal.
		r.m.deps.Logf("reaper: escalate resume %s failed (stays paused): %v", rec.ID, err)
	}
}

// sweepPaused evicts a `paused` sandbox past its retention (§4.1: evict = the
// reaper's automatic destroy of a paused sandbox). Retention is measured from
// the record's UpdatedAt — the moment it entered `paused` (the last state
// change). A zero Retention means "no eviction deadline" (opt out). Guarded by
// the non-blocking lock + a re-read so a concurrent resume is never evicted out
// from under.
func (r *Reaper) sweepPaused(ctx context.Context, rec model.Sandbox) {
	if rec.Retention <= 0 {
		return // opted out of eviction
	}
	if time.Since(rec.UpdatedAt) < rec.Retention {
		return // within retention
	}
	l, ok := r.m.tryLock(rec.ID)
	if !ok {
		return // a verb (e.g. resume) holds it — skip
	}
	defer l.Unlock()

	// re-read under the lock: a resume may have flipped it to running, or another
	// sweep already evicted it. Evict only a still-`paused` record still past
	// retention.
	cur, err := r.m.deps.Store.GetSandbox(ctx, rec.ID)
	if err != nil || cur.State != model.Paused {
		return
	}
	if time.Since(cur.UpdatedAt) < cur.Retention {
		return
	}
	if err := r.m.destroyLocked(ctx, cur); err != nil {
		r.m.deps.Logf("reaper: evict %s (retried next tick): %v", cur.ID, err)
	}
}
