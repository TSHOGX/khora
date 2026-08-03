package lifecycle

import (
	"context"
	"fmt"
	"time"

	"engine/internal/model"
	"engine/internal/store"
)

// Reaper is the engine's first autonomous background control loop (§4.3 /
// reaper-health D0): it is NOT an independent collector but a CONSUMER of the
// health model. Each tick it classifies every sandbox (§4.3 health states) and
// drives, per health state, the automatic/involuntary lifecycle transitions the
// state machine (§4.1) names but nothing else triggers — auto-pause, evict, and
// the LIVE (not just boot-time) crash-pause. It also folds in the periodic
// reconcile so one tick classifies once and both consumers dispatch off it
// (reaper-health D4: one tick classifies first, then dispatches).
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

	// downSince tracks, per sandbox, the CONTINUOUS transport-down streak D4b
	// measures (continuous transport-down duration) — the simplest measure D4b
	// picks, no
	// PID1 restart-count upcall needed. Keyed additionally by placement EPOCH (the
	// transport endpoint / IP, §4.3 placement-IP-as-epoch): a PID1 in-place restart
	// keeps the SAME IP (§5.E.1) and so is genuinely the same streak, while any
	// resume / pause+resume rebinds a NEW IP (§3.3) and MUST start a fresh streak.
	// Epoch-keying is what makes the streak survive an in-place restart yet reset
	// across a placement change that happened BETWEEN two sweeps (no intervening
	// sweep to observe the transition) — the case a "reset on observed non-down
	// outcome" alone would miss, letting a stale pre-resume timestamp escalate a
	// fresh transport-down straight past the budget. Reaper-loop-local (single
	// goroutine drives Sweep), so no lock.
	downSince map[model.SandboxID]downStreak
}

// downStreak is one sandbox's continuous transport-down streak (D4b): when it
// began, and the placement epoch (transport endpoint / IP) it began on. A streak
// whose epoch no longer matches the sandbox's current placement is stale (the
// placement was rebound by a resume) and is restarted, not honored.
type downStreak struct {
	since time.Time
	epoch string // placement transport endpoint at streak start (§4.3 IP-as-epoch)
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
		downSince:           make(map[model.SandboxID]downStreak),
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
	// §5.B / §4.4-A orphan-layer sweep, alongside the placement reconcile: reclaim
	// Promote-built layer bytes with no live templates row. An in-flight Promote
	// is protected by the mediator's buildingLayers keep-set. Non-fatal.
	if err := r.m.SweepOrphanLayers(ctx); err != nil {
		r.m.deps.Logf("reaper: orphan-layer sweep (retried next tick): %v", err)
	}
	// §4.4-A orphan-UNIT sweep: reclaim Overlay storage units backing no sandbox
	// row. This is what makes §4.4's licensed retry real rather than deferred to the
	// next restart — a DestroyOverlay that failed at 10:00 must not hold its
	// subvolume and quota row until reboot. Runs AFTER the reconcile above, which
	// may have deleted `creating` records, so their units are collected in the same
	// tick rather than the next one. Non-fatal, same skip-and-log discipline.
	if err := r.m.SweepOrphanOverlays(ctx); err != nil {
		r.m.deps.Logf("reaper: orphan-unit sweep (retried next tick): %v", err)
	}
}

// Sweep is one full pass: classify every sandbox and dispatch per health state /
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
	// downThisTick = the sandboxes RE-CONFIRMED continuously transport-down this
	// sweep. The D4b budget measures a CONTINUOUS streak (reaper-health D4b:
	// continuous transport-down duration), so any sandbox NOT re-confirmed this
	// tick — because it went healthy, paused, was skipped (held lock),
	// classified Unknown, or is simply gone — must have its streak dropped. We
	// record confirmations here and prune downSince to exactly this set at the
	// end, rather than deleting on each individual "good" outcome (which missed
	// the paused / Unknown / skipped paths and let a stale pre-pause timestamp
	// escalate a fresh transport-down straight past the budget).
	downThisTick := make(map[model.SandboxID]bool)
	for _, rec := range recs {
		switch rec.State {
		case model.Running:
			r.sweepRunning(ctx, rec, downThisTick)
		case model.Paused:
			r.sweepPaused(ctx, rec)
		}
	}
	// Prune every streak not re-confirmed transport-down this tick. Subsumes the
	// gone-record GC (a destroyed/evicted id is never re-confirmed) AND the
	// pause/resume/Unknown/skip resets — so the streak is truly CONTINUOUS.
	for id := range r.downSince {
		if !downThisTick[id] {
			delete(r.downSince, id)
		}
	}
}

// sweepRunning classifies one `running` sandbox and disposes per health state, all
// under a NON-BLOCKING transition lock (reaper-health D3): a sandbox with a held
// lock has a verb driving it (in-flight resume / user pause) → skip this round,
// no race, no misclassification. The lock is held across classify AND dispose so
// the disposition acts on a lock-consistent view (D3), not a stale pre-lock one.
func (r *Reaper) sweepRunning(ctx context.Context, rec model.Sandbox, downThisTick map[model.SandboxID]bool) {
	l, ok := r.m.tryLock(rec.ID)
	if !ok {
		// a verb holds the lock — skip (single-flight guard, D3). NOT re-confirmed
		// transport-down this tick, so its streak (if any) is pruned at sweep end:
		// a sandbox a verb just touched is not "continuously" down.
		return
	}
	defer l.Unlock()

	// Re-read under the lock: the record may have changed state between the
	// ListSandboxes snapshot and acquiring the lock (a user verb that finished
	// before we tryLock'd). Dispose only if it is still `running`.
	cur, err := r.m.deps.Store.GetSandbox(ctx, rec.ID)
	if err != nil || cur.State != model.Running {
		return
	}

	h, cause := r.m.classify(ctx, cur)
	switch h {
	case HealthIdle:
		r.autoPauseLocked(ctx, cur)
	case HealthDeadRecoverable, HealthResourceTerminated:
		// LIVE crash-pause (§4.2 involuntary pause): the isolation unit died under
		// a running orchestrator. Same disposition reconcile does at boot, driven
		// now by the reaper. Not re-confirmed transport-down → streak pruned at
		// sweep end (this is death, not a transient).
		//
		// BOTH health states crash-pause — the state is `paused` either way. What differs
		// is the CAUSE committed alongside it (§4.3): the classifier read it while the
		// placement was still intact, and pauseLocked persists it in the SAME commit
		// as the CAS, because a moment later teardown destroys the evidence. That
		// recorded cause is what makes resume refuse a resource-terminated sandbox
		// while a dead-recoverable one resumes normally — identical states,
		// opposite dispositions, separated only by this value.
		dc := deathCauseOf(cause)
		if err := r.m.pauseLocked(ctx, cur.ID, "crash-paused", &dc); err != nil && err != store.ErrConflict {
			r.m.deps.Logf("reaper: crash-pause %s (retried next tick): %v", cur.ID, err)
		} else if err == nil {
			r.m.deps.Logf("reaper: %s crash-paused, health=%s death-cause=%s%s",
				cur.ID, h, dc, resumeNote(h, cur.Retention))
		}
	case HealthTransportDown:
		// re-confirm the streak this tick BEFORE dispatch; the D4b budget is read
		// inside. This is the ONLY outcome that keeps a streak alive to sweep end.
		downThisTick[cur.ID] = true
		r.handleTransportDownLocked(ctx, cur)
	case HealthHealthy:
		// keep running; not re-confirmed → streak pruned at sweep end.
	default:
		// HealthUnknown (uncertain L1 / no live handle): do nothing, retry next
		// tick (protect-on-uncertainty, reaper-health D3). Not re-confirmed → a
		// prior streak is pruned (an Unknown tick breaks continuity, D4b).
	}
}

// resumeNote spells out the disposition in the log line, so an operator reading
// "crash-paused" can tell whether the engine intends to bring it back (§4.3's two
// parallel health states are otherwise indistinguishable in a log).
func resumeNote(h Health, retention time.Duration) string {
	if h != HealthResourceTerminated {
		return ""
	}
	// Name the remedy AND the deadline. "Waiting for a human" has a clock: retention
	// runs from
	// this demotion's UpdatedAt, and past it the reaper evicts — Overlay artifact
	// included (§4.3). An operator who is not told that will assume the sandbox
	// waits indefinitely.
	if retention <= 0 {
		return " (will NOT resume: raise memory_byte via PATCH, or destroy it; retention=0 so it is never evicted, §4.3)"
	}
	return fmt.Sprintf(" (will NOT resume: raise memory_byte via PATCH, or destroy it; evicted in %s if nobody intervenes, Overlay included, §4.3)", retention)
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
	if h, _ := r.m.classify(ctx, rec); h != HealthIdle {
		// became active (or unreachable) in the window → abort, re-judge next tick.
		return
	}
	// idle → not re-confirmed transport-down this tick, so its streak (if any) is
	// pruned at sweep end; no explicit delete needed here.
	// nil cause: an idle auto-pause is not a death and must not stamp one.
	if err := r.m.pauseLocked(ctx, rec.ID, "auto-paused", nil); err != nil && err != store.ErrConflict {
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
	// The streak is tied to the placement epoch (IP). Resolve the sandbox's
	// CURRENT endpoint: an in-place PID1 restart keeps the same IP (§5.E.1) so the
	// streak continues; a resume rebound a new IP (§3.3) so any prior streak is
	// stale and this is a fresh one. A missing endpoint (no live handle) shouldn't
	// reach here — transport-down means L1 alive + a live-map endpoint that failed
	// L2 — but treat it defensively as a fresh streak with an empty epoch.
	epoch, _ := r.m.Endpoint(rec.ID)

	streak, tracked := r.downSince[rec.ID]
	if !tracked || streak.epoch != epoch {
		// first sighting this streak, OR the placement changed under us (stale
		// epoch from a between-sweep resume) → start a fresh streak on this epoch.
		r.downSince[rec.ID] = downStreak{since: time.Now(), epoch: epoch}
		return // wait for the in-place restart
	}
	if time.Since(streak.since) < r.transportDownBudget {
		return // still within budget on the same epoch — keep waiting
	}
	// past the restart budget on ONE continuous epoch → escalate (D4b): crash-pause,
	// then resume.
	r.m.deps.Logf("reaper: %s transport-down past budget (%s) → escalate to resume (D4b)", rec.ID, r.transportDownBudget)
	delete(r.downSince, rec.ID)
	// nil cause: escalation is not a death observation — L1 is ALIVE here (that is
	// what transport-down means), so there is nothing dead to explain and stamping
	// a cause would be inventing one (§2.4: no guessing).
	if err := r.m.pauseLocked(ctx, rec.ID, "crash-paused", nil); err != nil {
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
