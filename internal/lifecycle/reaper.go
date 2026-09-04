package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// Reaper is the engine's first autonomous background control loop (health and reclamation /
// health classification constraints): it is NOT an independent collector but a CONSUMER of the
// health model. Each tick it classifies every sandbox (health and reclamation health states) and
// drives, per health state, the automatic/involuntary lifecycle transitions the
// state machine (the lifecycle state machine) names but nothing else triggers — auto-pause, evict, and
// the LIVE (not just boot-time) crash-pause. It also folds in the periodic
// reconcile so one tick classifies once and both consumers dispatch off it
// (periodic reconciliation: one tick classifies first, then dispatches).
//
// Dispositions go through the per-sandbox transition serialization transition lock + CAS on the Mediator; the
// Reaper owns only the loop cadence and the transport-down restart budget
// tracker (health classification), which must persist across ticks.
type Reaper struct {
	m *Mediator

	// interval is the sweep cadence (the health and reaper boundary numeric knob): trades
	// detection latency against sweep overhead. Coarse enough that a single
	// serial full-scan finishes well within one tick.
	interval time.Duration

	// transportDownBudget bounds how long a sandbox may stay CONTINUOUSLY
	// transport-down before the reaper escalates it to dead-recoverable → resume
	// (health classification: break a crash-looping transport child that never comes healthy).
	// Generous by default so a slow legitimate PID1 restart is never mistaken
	// for a crash-loop (the health and reaper boundary).
	transportDownBudget time.Duration

	// downSince tracks, per sandbox, the CONTINUOUS transport-down streak health classification
	// measures (continuous transport-down duration) — the simplest measure health classification
	// picks, no PID1 restart-count upcall needed. Keyed additionally by placement
	// EPOCH (the transport endpoint / IP, health and reclamation placement-IP-as-epoch): a PID1
	// in-place restart keeps the SAME IP (the guest startup topology) and so is genuinely the same
	// streak, while any resume / pause+resume rebinds a NEW IP (the ID-reference model) and MUST start
	// a fresh one. Epoch-keying is what makes the streak reset across a placement
	// change that happened BETWEEN two sweeps (no intervening sweep to observe the
	// transition) — the case a "reset on observed non-down outcome" alone would miss,
	// letting a stale pre-resume timestamp escalate a fresh transport-down straight
	// past the budget. Reaper-loop-local (single goroutine drives Sweep), so no lock.
	downSince     map[model.SandboxID]downStreak
	lastTelemetry map[model.SandboxID]time.Time
	// telemetryQueue is a one-slot handoff from the maintenance tick to the
	// sampler. Sampling may perform provider and Store I/O; keeping it off the
	// reaper goroutine means a slow metrics backend cannot delay lifecycle
	// reconciliation. The single slot bounds work and drops a tick while a
	// previous sample pass is still running.
	telemetryQueue chan context.Context
}

// downStreak is one sandbox's continuous transport-down streak (health classification): when it
// began, and the placement epoch (transport endpoint / IP) it began on. A streak
// whose epoch no longer matches the sandbox's current placement is stale (the
// placement was rebound by a resume) and is restarted, not honored.
type downStreak struct {
	since time.Time
	epoch string // explicit Placement epoch, with endpoint fallback for legacy records
}

// reaper defaults (the health and reaper boundary numeric knobs; bound no invariant's shape).
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
		lastTelemetry:       make(map[model.SandboxID]time.Time),
		telemetryQueue:      make(chan context.Context, 1),
	}
}

// Run drives one tick on a ticker until ctx is cancelled (health and reclamation autonomous
// loop). The orchestrator starts it after startup reconcile, in its own
// goroutine.
func (r *Reaper) Run(ctx context.Context) {
	go r.runTelemetry(ctx)
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

// runTelemetry drains the bounded sampling handoff. It is owned by Run so the
// worker's lifetime follows the reaper context and no goroutine survives a
// normal shutdown.
func (r *Reaper) runTelemetry(ctx context.Context) {
	if r.telemetryQueue == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case sampleCtx := <-r.telemetryQueue:
			r.sampleTelemetry(sampleCtx)
		}
	}
}

// tick is one full cycle: periodic reconcile THEN sweep (periodic reconciliation: one
// tick, the two consumers of the health model run in order). Reconcile handles
// "state vs reality" (orphan placements, dead `running` → crash-pause, takeover);
// the sweep handles "state right but should advance" (TTL auto-pause/evict,
// transport-down budget). Reconcile first so a `running` record whose unit died
// is demoted before the sweep would redundantly reach the same verdict; the
// sweep is idempotent either way. Reconcile failure is logged, not fatal — the
// sweep still runs, and reconcile retries next tick (atomic transition rollback skip-and-log).
func (r *Reaper) tick(ctx context.Context) {
	if err := r.m.ReconcilePeriodic(ctx); err != nil {
		r.m.deps.Logf("reaper: periodic reconcile (retried next tick): %v", err)
	}
	r.Sweep(ctx)
	// the Rootfs subsystem / atomic transition rollback orphan-layer sweep, alongside the placement reconcile: reclaim
	// Promote-built layer bytes with no live templates row. An in-flight Promote
	// is protected by the mediator's buildingLayers keep-set. Non-fatal.
	if err := r.m.SweepOrphanLayers(ctx); err != nil {
		r.m.deps.Logf("reaper: orphan-layer sweep (retried next tick): %v", err)
	}
	// atomic transition rollback orphan-UNIT sweep: reclaim Overlay storage units backing no sandbox
	// row. This is what makes the transition contract's licensed retry real rather than deferred to the
	// next restart — a DestroyOverlay that failed at 10:00 must not hold its
	// subvolume and quota row until reboot. Runs AFTER the reconcile above, which
	// may have deleted `creating` records, so their units are collected in the same
	// tick rather than the next one. Non-fatal, same skip-and-log discipline.
	if err := r.m.SweepOrphanOverlays(ctx); err != nil {
		r.m.deps.Logf("reaper: orphan-unit sweep (retried next tick): %v", err)
	}
	// the Volume subsystem orphan-VOLUME sweep. Same reasoning as the Overlay unit sweep: a
	// DestroyVolume whose byte reclamation failed must not hold its unit until reboot.
	if err := r.m.SweepOrphanVolumes(ctx); err != nil {
		r.m.deps.Logf("reaper: orphan-volume sweep (retried next tick): %v", err)
	}
	// the Volume subsystem / Volume quota cleanup LEAKED QUOTA ROWS, and this one has to be periodic rather
	// than boot-only for a mechanical reason: the transition to reclaimable is
	// CONDITION-triggered, not time-triggered. A `<squota space holder>` becomes
	// `<stale>` the moment its last live sharer goes away, and the FIRST attempt after
	// that succeeds — so the round must be POLLED. Doing it at boot alone would hold
	// leaked rows, each of which can hand its cap to an unrelated new volume after a
	// remount, until the next restart.
	if err := r.m.ReclaimQuotaRows(ctx); err != nil {
		r.m.deps.Logf("reaper: quota-row reclamation (retried next tick): %v", err)
	}
	// the Volume subsystem / volume cap enforcement CAP REPAIR, periodic for the same reason the sweeps above
	// are: the repair state is created at RUNTIME, not only by a crash before boot —
	// any failure between a snapshot and its cap write leaves a registered, live,
	// UNCAPPED volume whose record still promises N to the resource admission gate. Boot-only repair
	// would hold that divergence until the next restart, and it is INVISIBLE while it
	// lasts (RepairVolumeCaps states why nothing else can find it).
	if err := r.m.RepairVolumeCaps(ctx); err != nil {
		r.m.deps.Logf("reaper: volume-cap repair (retried next tick): %v", err)
	}
	// the transactional outbox's retention window. LAST in the tick, and on the reaper rather than the
	// publisher goroutine, both deliberately:
	//
	//   - The reaper already IS the engine's periodic-reclamation loop, and this is the
	//     same shape as every sweep above it: idempotent, non-fatal, retried next tick.
	//   - Not the publisher's goroutine, because that goroutine's wedging must stay
	//     DETECTABLE (the transactional outbox): its heartbeat means "the drain completed a pass", and
	//     adding an unrelated store operation to it would give that signal a second way
	//     to stall, so "the drain is stuck" and "the trim is stuck" would be
	//     indistinguishable — the exact confusion  exists to prevent.
	//
	// It has no ordering dependency on the sweeps (nothing else touches the outbox), so
	// it sits after the reconcile that may APPEND events rather than before it.
	if err := r.m.TrimOutbox(ctx); err != nil {
		r.m.deps.Logf("reaper: outbox trim (retried next tick): %v", err)
	}
	r.scheduleTelemetry(ctx)
}

// scheduleTelemetry is deliberately non-blocking. A full queue means the
// previous pass is still doing best-effort sampling; dropping this tick keeps
// lifecycle reconciliation bounded and the next tick can try again.
func (r *Reaper) scheduleTelemetry(ctx context.Context) {
	if r.telemetryQueue == nil {
		return
	}
	select {
	case r.telemetryQueue <- ctx:
	default:
		r.m.deps.Logf("reaper: telemetry sample skipped (sampler busy)")
	}
}

func (r *Reaper) sampleTelemetry(ctx context.Context) {
	policy := r.m.deps.TelemetryPolicy
	if policy.Interval <= 0 {
		return
	}
	recs, err := r.m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	seen := make(map[model.SandboxID]bool, len(recs))
	for _, rec := range recs {
		seen[rec.ID] = true
		if last := r.lastTelemetry[rec.ID]; !last.IsZero() && now.Sub(last) < policy.Interval {
			continue
		}
		if err := r.m.SampleTelemetry(ctx, rec.ID); err != nil {
			r.m.deps.Logf("reaper: telemetry sample %s: %v", rec.ID, err)
		}
		r.lastTelemetry[rec.ID] = now
	}
	for id := range r.lastTelemetry {
		if !seen[id] {
			delete(r.lastTelemetry, id)
		}
	}
	if err := r.m.TrimTelemetry(ctx); err != nil {
		r.m.deps.Logf("reaper: telemetry trim: %v", err)
	}
}

// Sweep is one full pass: classify every sandbox and dispatch per health state /
// per TTL (periodic reconciliation). Serial per-sandbox, single full scan — enough for
// a single node with a bounded sandbox count; batching/rate-limiting is an
// additive tuning change that does not alter this shape (periodic reconciliation).
// Failure policy is skip-and-log (atomic transition rollback): one un-dispatchable sandbox never
// aborts the rest, and the sweep is idempotent so a skipped item retries next
// tick.
func (r *Reaper) Sweep(ctx context.Context) {
	recs, err := r.m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		r.m.deps.Logf("reaper: list sandboxes (retried next tick): %v", err)
		return
	}
	// downThisTick = the sandboxes RE-CONFIRMED continuously transport-down this
	// sweep. The health classification budget measures a CONTINUOUS streak (the transport-down restart budget:
	// continuous transport-down duration), so any sandbox NOT re-confirmed this
	// tick — because it went healthy, paused, was skipped (held lock), classified
	// Unknown, or is simply gone — must have its streak dropped. Confirmations are
	// recorded here and downSince pruned to exactly this set at the end, rather than
	// deleting on each individual "good" outcome — that shape misses the paused /
	// Unknown / skipped paths, letting a stale pre-pause timestamp escalate a fresh
	// transport-down straight past the budget.
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
// under a NON-BLOCKING transition lock (reaper serialization): a sandbox with a held
// lock has a verb driving it (in-flight resume / user pause) → skip this round,
// no race, no misclassification. The lock is held across classify AND dispose so
// the disposition acts on a lock-consistent view (health classification), not a stale pre-lock one.
func (r *Reaper) sweepRunning(ctx context.Context, rec model.Sandbox, downThisTick map[model.SandboxID]bool) {
	l, ok := r.m.tryLock(rec.ID)
	if !ok {
		// a verb holds the lock — skip (single-flight guard, health classification). A sandbox a verb just
		// touched is not "continuously" down, and Sweep's prune enforces that.
		return
	}
	defer l.Unlock()

	// Re-read under the lock: a user verb may have changed the record's state between
	// the ListSandboxes snapshot and this lock acquisition.
	cur, err := r.m.deps.Store.GetSandbox(ctx, rec.ID)
	if err != nil || cur.State != model.Running {
		return
	}

	h, cause := r.m.classify(ctx, cur)
	switch h {
	case HealthIdle:
		r.autoPauseLocked(ctx, cur)
	case HealthDeadRecoverable, HealthResourceTerminated:
		// LIVE crash-pause (FS-first pause and resume involuntary pause): the isolation unit died under
		// a running orchestrator. Same disposition reconcile does at boot, driven
		// now by the reaper.
		//
		// BOTH health states crash-pause — the state is `paused` either way, and only
		// the CAUSE committed alongside it separates them (health and reclamation, HealthResourceTerminated).
		// pauseLocked must persist it in the SAME commit as the CAS, because a moment
		// later teardown destroys the evidence the classifier read.
		dc := deathCauseOf(cause)
		if err := r.m.pauseLocked(ctx, cur.ID, "crash-paused", &dc); err != nil && err != store.ErrConflict {
			r.m.deps.Logf("reaper: crash-pause %s (retried next tick): %v", cur.ID, err)
		} else if err == nil {
			r.m.deps.Logf("reaper: %s crash-paused, health=%s death-cause=%s%s",
				cur.ID, h, dc, resumeNote(h, cur.Retention))
		}
	case HealthTransportDown:
		// re-confirm the streak this tick BEFORE dispatch; the health classification budget is read
		// inside. This is the ONLY outcome that keeps a streak alive to sweep end.
		downThisTick[cur.ID] = true
		r.handleTransportDownLocked(ctx, cur)
	case HealthHealthy:
		// keep running.
	default:
		// HealthUnknown (uncertain L1 / no live handle): do nothing, retry next
		// tick (protect-on-uncertainty, reaper serialization). An Unknown tick breaks
		// continuity, so a prior streak is pruned (health classification).
	}
}

// resumeNote spells out the disposition in the log line, so an operator reading
// "crash-paused" can tell whether the engine intends to bring it back (health and reclamation's two
// parallel health states are otherwise indistinguishable in a log).
func resumeNote(h Health, retention time.Duration) string {
	if h != HealthResourceTerminated {
		return ""
	}
	// Name the remedy AND the deadline: retention runs from this demotion's UpdatedAt,
	// and past it the reaper evicts — Overlay artifact included (health and reclamation).
	if retention <= 0 {
		return " (will NOT resume: raise memory_byte via PATCH, or destroy it; retention=0 so it is never evicted, health and reclamation)"
	}
	return fmt.Sprintf(" (will NOT resume: raise memory_byte via PATCH, or destroy it; evicted in %s if nobody intervenes, Overlay included, health and reclamation)", retention)
}

// autoPauseLocked auto-pauses an idle sandbox — with the health classification activity-race guard.
// CALLER HOLDS the lock. The sweep's classify→dispose window can be seconds; a
// sandbox that became active inside it is STILL `running`, so per-sandbox transition serialization's CAS (which
// compares lifecycle-state only) would NOT catch it and would freeze a sandbox
// that just started working. So we RE-READ activity under the lock (one Health
// probe; the lock is held for the whole pause anyway, so the extra RPC is
// negligible) and abort if it is no longer idle. Disposition thus rests on the
// lock-latest {state + activity} double snapshot, not the sweep-start
// classification (reaper serialization).
func (r *Reaper) autoPauseLocked(ctx context.Context, rec model.Sandbox) {
	if h, _ := r.m.classify(ctx, rec); h != HealthIdle {
		// became active (or unreachable) in the window → abort, re-judge next tick.
		return
	}
	// nil cause: an idle auto-pause is not a death and must not stamp one.
	if err := r.m.pauseLocked(ctx, rec.ID, "auto-paused", nil); err != nil && err != store.ErrConflict {
		r.m.deps.Logf("reaper: auto-pause %s (retried next tick): %v", rec.ID, err)
	}
}

// handleTransportDownLocked applies the health classification restart budget. CALLER HOLDS the
// lock. A transport-down sandbox is a transient — PID1 is restarting the
// transport child in place on the SAME IP (the guest startup topology) — so the default action is
// WAIT (do nothing). But we track how long it has been CONTINUOUSLY down; past
// the budget it is a crash-loop, not a slow restart, so we escalate to
// dead-recoverable: crash-pause then resume (new IP, new epoch) to break the
// wedge (health classification). Both steps run under the held lock, same path a user
// pause+resume would take.
func (r *Reaper) handleTransportDownLocked(ctx context.Context, rec model.Sandbox) {
	// Resolve the sandbox's CURRENT endpoint — the streak's epoch (see downSince). A
	// missing endpoint (no live handle) shouldn't reach here — transport-down means L1
	// alive + a live-map endpoint that failed L2 — but treat it defensively as a fresh
	// streak with an empty epoch.
	epoch := ""
	if pl, err := r.m.Placement(ctx, rec.ID); err == nil {
		epoch = fmt.Sprintf("%d", pl.Epoch)
	} else {
		epoch, _ = r.m.Endpoint(rec.ID)
	}

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
	// past the restart budget on ONE continuous epoch → escalate (health classification): crash-pause,
	// then resume.
	r.m.deps.Logf("reaper: %s transport-down past budget (%s) → escalate to resume (health classification)", rec.ID, r.transportDownBudget)
	delete(r.downSince, rec.ID)
	// nil cause: escalation is not a death observation — L1 is ALIVE here (that is
	// what transport-down means), so there is nothing dead to explain and stamping
	// a cause would be inventing one (the provider security contract: no guessing).
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
		// resume failed → stays paused (atomic transition rollback); a later user/next-tick resume can
		// retry. Not fatal.
		r.m.deps.Logf("reaper: escalate resume %s failed (stays paused): %v", rec.ID, err)
	}
}

// sweepPaused evicts a `paused` sandbox past its retention (the lifecycle state machine: evict = the
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
	// CauseRetentionExpired, not CauseRequested: nobody asked for this — a TTL fired
	// while nobody was looking, which is a different fact for a subscriber and one it
	// can never recover afterwards (the record is gone; the transactional outbox).
	if err := r.m.destroyLocked(ctx, cur, model.CauseRetentionExpired); err != nil {
		r.m.deps.Logf("reaper: evict %s (retried next tick): %v", cur.ID, err)
	}
}
