package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"warren/internal/model"
	"warren/internal/runtime"
	"warren/internal/store"
	"warren/internal/transport"
)

// reaper dispatch as pure logic against fakes (§4.3 health states → §4.1
// transitions). The real runsc round-trip (auto-pause after idle, live
// crash-pause, transport-down same-IP, crash-loop escalation, evict) lives in
// the substrate e2e (test/reaper_substrate_test.go); these pin the dispatch +
// the D3 activity-race guard + the D4b budget + the TryLock-skip
// deterministically.

// reaperEnv wires a mediator + reaper with a controllable Health client and
// Adopt verdicts, and installs a live-map handle for a `running` sandbox so
// classify can resolve an endpoint.
type reaperEnv struct {
	m  *Mediator
	r  *Reaper
	st *store.Memory
	rt *fakeRuntime
	cl *fakeClient
}

func newReaperEnv(t *testing.T, budget time.Duration) *reaperEnv {
	t.Helper()
	st := store.NewMemory()
	seedBare(t, st) // §4.3 template FK: both seedRunning/seedPaused reference "bare"
	rt := &fakeRuntime{adoptAlive: map[string]bool{}}
	cl := &fakeClient{healthy: true}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return cl },
		HealthTimeout: time.Second,
	})
	return &reaperEnv{m: m, r: NewReaper(m, time.Hour, budget), st: st, rt: rt, cl: cl}
}

// seedRunning inserts a `running` record with a live-map handle (so it is
// managed by this orchestrator, not a reconcile-takeover case).
func (e *reaperEnv) seedRunning(t *testing.T, id string, idleTimeout, retention time.Duration) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := e.st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Creating, IdleTimeout: idleTimeout, Retention: retention,
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.m.put(model.SandboxID(id), &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.1:9000"}})
}

func (e *reaperEnv) seedPaused(t *testing.T, id string, retention time.Duration, pausedAgo time.Duration) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := e.st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Creating, Retention: retention,
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	_ = e.st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil, nil)
	if err := e.st.CompareAndSetState(ctx, model.SandboxID(id), model.Running, model.Paused, nil, nil); err != nil {
		t.Fatal(err)
	}
	// backdate UpdatedAt so retention math sees it as paused `pausedAgo` ago.
	e.st.SetUpdatedAtForTest(model.SandboxID(id), now.Add(-pausedAgo))
}

// state returns the sandbox's lifecycle state, or the sentinel "<gone>" when the
// record has been deleted (which is how an evict/destroy presents). Only
// ErrNotFound maps to the sentinel: any OTHER store error is a fault, and
// collapsing it into "<gone>" would let a broken store read as a successful
// eviction.
func (e *reaperEnv) state(t *testing.T, id string) model.Lifecycle {
	t.Helper()
	rec, err := e.st.GetSandbox(context.Background(), model.SandboxID(id))
	if errors.Is(err, store.ErrNotFound) {
		return model.Lifecycle("<gone>")
	}
	if err != nil {
		t.Fatalf("GetSandbox(%s): %v", id, err)
	}
	return rec.State
}

func (e *reaperEnv) lastKind(id string) string {
	evs, _ := e.st.UnpublishedEvents(context.Background(), allUnpublished)
	kind := ""
	for _, ev := range evs {
		if ev.Sandbox == model.SandboxID(id) {
			kind = string(ev.Kind)
		}
	}
	return kind
}

// lastCause is lastKind for the §4.4-C cause field.
func (e *reaperEnv) lastCause(id string) model.EventCause {
	evs, _ := e.st.UnpublishedEvents(context.Background(), allUnpublished)
	cause := model.EventCause("<no event>")
	for _, ev := range evs {
		if ev.Sandbox == model.SandboxID(id) {
			cause = ev.Cause
		}
	}
	return cause
}

// idle past idle_timeout → auto-pause, emitting Kind "auto-paused" (§4.1: the
// idle Kind, distinct from user "paused" and reconcile "crash-paused").
func TestReaperAutoPausesIdle(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "idle", 10*time.Second, 0)
	e.cl.idle = 30 * time.Second // ≥ idle_timeout

	e.r.Sweep(context.Background())

	if got := e.state(t, "idle"); got != model.Paused {
		t.Fatalf("idle sandbox state=%s; want paused", got)
	}
	if k := e.lastKind("idle"); k != "auto-paused" {
		t.Fatalf("idle event Kind=%q; want auto-paused", k)
	}
}

// a healthy (recent I/O) running sandbox is left untouched.
func TestReaperLeavesHealthyRunning(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "busy", 10*time.Second, 0)
	e.cl.idle = 1 * time.Second // < idle_timeout

	e.r.Sweep(context.Background())

	if got := e.state(t, "busy"); got != model.Running {
		t.Fatalf("healthy sandbox state=%s; want running", got)
	}
}

// D3 activity-race guard: a sandbox classified idle at sweep, but active again
// by the lock-held re-read, is NOT auto-paused. CAS on lifecycle-state alone
// would not catch it (state is still running); the lock-held activity re-read
// does. We simulate it with an idleFn that returns idle on the first probe (the
// sweep classify) and active on the second (the auto-pause re-read).
func TestReaperActivityRaceGuardAbortsAutoPause(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "racy", 10*time.Second, 0)
	call := 0
	e.cl.idleFn = func() time.Duration {
		call++
		if call == 1 {
			return 30 * time.Second // sweep classify: idle
		}
		return 1 * time.Second // lock-held re-read: active again
	}

	e.r.Sweep(context.Background())

	if got := e.state(t, "racy"); got != model.Running {
		t.Fatalf("sandbox active-in-window was auto-paused: state=%s; want running (D3 guard)", got)
	}
	if call < 2 {
		t.Fatalf("auto-pause did not re-read activity under the lock (probes=%d; want ≥2)", call)
	}
}

// dead-recoverable (L2 down + L1 dead) under a LIVE orchestrator → crash-pause
// (§4.2 involuntary pause), Kind "crash-paused". Proves the live reaper drives
// the same demote reconcile does at boot.
func TestReaperCrashPausesDeadRecoverable(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "dead", 10*time.Second, 0)
	e.cl.healthy = false                             // L2 down
	e.rt.adoptAlive = map[string]bool{"dead": false} // L1 dead

	e.r.Sweep(context.Background())

	if got := e.state(t, "dead"); got != model.Paused {
		t.Fatalf("dead-recoverable state=%s; want paused", got)
	}
	if k := e.lastKind("dead"); k != "crash-paused" {
		t.Fatalf("dead-recoverable Kind=%q; want crash-paused", k)
	}
}

// transport-down (L2 down + L1 alive) → WAIT, not crash-pause. Stays running;
// PID1 restarts the transport child in place (§5.E.1). No escalation within
// budget.
func TestReaperWaitsOnTransportDownWithinBudget(t *testing.T) {
	e := newReaperEnv(t, time.Hour) // generous budget
	e.seedRunning(t, "td", 10*time.Second, 0)
	e.cl.healthy = false                          // L2 down
	e.rt.adoptAlive = map[string]bool{"td": true} // L1 alive

	e.r.Sweep(context.Background())
	e.r.Sweep(context.Background()) // second tick, still within budget

	if got := e.state(t, "td"); got != model.Running {
		t.Fatalf("transport-down state=%s; want running (wait for in-place restart, not crash-pause)", got)
	}
	if _, tracked := e.r.downSince["td"]; !tracked {
		t.Fatal("transport-down streak not tracked for budget accounting (D4b)")
	}
}

// D4b escalation: transport-down PAST the restart budget → escalate to
// dead-recoverable = crash-pause then resume (new epoch), so a crash-looping
// transport child cannot wedge the sandbox forever. Ends running again.
func TestReaperEscalatesTransportDownPastBudget(t *testing.T) {
	e := newReaperEnv(t, time.Hour)
	e.seedRunning(t, "loop", 10*time.Second, 0)
	e.cl.healthy = false                            // L2 down (crash-looping child)
	e.rt.adoptAlive = map[string]bool{"loop": true} // L1 alive

	// first sweep: record the streak start.
	e.r.Sweep(context.Background())
	if got := e.state(t, "loop"); got != model.Running {
		t.Fatalf("after first tick state=%s; want running (within budget)", got)
	}
	// backdate the streak start past the budget, on the SAME placement epoch the
	// live handle carries (10.0.0.1:9000) so it is a continuous same-IP streak →
	// next sweep escalates (D4b).
	e.r.downSince["loop"] = downStreak{since: time.Now().Add(-2 * time.Hour), epoch: "10.0.0.1:9000"}

	// The escalating sweep probes Health twice: once in classify (must be DOWN so
	// it classifies transport-down and escalates), then again at the resume's ⑦
	// readiness gate (must be UP so the new cold-start comes healthy). Drive the
	// transition by probe count.
	probe := 0
	e.cl.healthyFn = func() bool {
		probe++
		return probe > 1 // #1 (classify) down; #2+ (⑦ gate) up
	}

	e.r.Sweep(context.Background())

	if got := e.state(t, "loop"); got != model.Running {
		t.Fatalf("after escalation state=%s; want running (crash-pause→resume, D4b)", got)
	}
	// the escalation went through crash-pause then resume: the last event is
	// "resumed", and a "crash-paused" preceded it.
	evs, _ := e.st.UnpublishedEvents(context.Background(), allUnpublished)
	var kinds []string
	for _, ev := range evs {
		if ev.Sandbox == "loop" {
			kinds = append(kinds, string(ev.Kind))
		}
	}
	sawCrashPause, sawResume := false, false
	for _, k := range kinds {
		if k == "crash-paused" {
			sawCrashPause = true
		}
		if k == "resumed" {
			sawResume = true
		}
	}
	if !sawCrashPause || !sawResume {
		t.Fatalf("escalation events=%v; want both crash-paused and resumed (D4b)", kinds)
	}
	if _, tracked := e.r.downSince["loop"]; tracked {
		t.Fatal("transport-down streak not cleared after escalation")
	}
}

// a healthy sweep clears a prior transport-down streak (recovery ends the
// budget accounting — the next down starts a fresh streak).
func TestReaperHealthyClearsTransportDownStreak(t *testing.T) {
	e := newReaperEnv(t, time.Hour)
	e.seedRunning(t, "flap", 10*time.Second, 0)
	e.cl.healthy = false
	e.rt.adoptAlive = map[string]bool{"flap": true}
	e.r.Sweep(context.Background())
	if _, tracked := e.r.downSince["flap"]; !tracked {
		t.Fatal("streak not tracked after transport-down")
	}
	// recovers.
	e.cl.healthy = true
	e.r.Sweep(context.Background())
	if _, tracked := e.r.downSince["flap"]; tracked {
		t.Fatal("healthy sweep did not clear the transport-down streak")
	}
}

// D4b regression: a transport-down streak must NOT survive a between-tick
// placement change (pause+resume rebinds a new IP, §3.3). Without epoch-keying, a
// stale pre-resume start time would let a FRESH transport-down escalate straight
// past the budget on the very next tick — bypassing the restart budget the whole
// mechanism exists to grant. Here we simulate the streak having begun on an OLD
// epoch; the sandbox now lives on a new one, so the reaper must start a fresh
// streak (within budget → WAIT), not escalate.
func TestReaperTransportDownStreakResetsAcrossEpochChange(t *testing.T) {
	e := newReaperEnv(t, time.Hour)
	e.seedRunning(t, "moved", 10*time.Second, 0) // live handle @ 10.0.0.1:9000
	e.cl.healthy = false                         // L2 down
	e.rt.adoptAlive = map[string]bool{"moved": true}

	// a long-past streak, but recorded on a DIFFERENT (old) placement epoch — as
	// if the sandbox had been transport-down before a resume rebound its IP.
	e.r.downSince["moved"] = downStreak{since: time.Now().Add(-2 * time.Hour), epoch: "10.0.0.99:9000"}

	e.r.Sweep(context.Background())

	// must NOT have escalated (that would have crash-paused→resumed); still running
	// and waiting, with a FRESH streak on the current epoch.
	if got := e.state(t, "moved"); got != model.Running {
		t.Fatalf("state=%s; want running — a stale-epoch streak must not escalate a fresh down (D4b)", got)
	}
	streak, tracked := e.r.downSince["moved"]
	if !tracked {
		t.Fatal("fresh streak not tracked after epoch change")
	}
	if streak.epoch != "10.0.0.1:9000" {
		t.Fatalf("streak epoch=%q; want the current placement 10.0.0.1:9000 (fresh streak)", streak.epoch)
	}
	if time.Since(streak.since) > time.Minute {
		t.Fatalf("streak start not reset to now on epoch change (since=%s ago)", time.Since(streak.since))
	}
}

// D4b regression: a HealthUnknown tick (uncertain L1 / momentarily absent live
// handle) breaks continuity — the streak must be pruned, so a subsequent
// transport-down starts a fresh budget rather than inheriting the old start time.
func TestReaperTransportDownStreakClearedByUnknownTick(t *testing.T) {
	e := newReaperEnv(t, time.Hour)
	e.seedRunning(t, "u", 10*time.Second, 0)
	e.cl.healthy = false // L2 down
	e.rt.adoptAlive = map[string]bool{"u": true}
	e.r.Sweep(context.Background())
	if _, tracked := e.r.downSince["u"]; !tracked {
		t.Fatal("streak not tracked after transport-down")
	}

	// next tick classifies Unknown: uncertain L1 (Adopt errors) while L2 still down.
	e.rt.adoptErr = map[string]error{"u": context.DeadlineExceeded}
	e.r.Sweep(context.Background())
	if _, tracked := e.r.downSince["u"]; tracked {
		t.Fatal("Unknown tick did not clear the transport-down streak (breaks D4b continuity)")
	}
}

// evict: a paused sandbox past retention → destroyed (§4.1 evict = reaper's
// automatic destroy). A paused sandbox within retention is left alone.
func TestReaperEvictsPausedPastRetention(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedPaused(t, "old", 1*time.Hour, 2*time.Hour)     // past retention
	e.seedPaused(t, "fresh", 1*time.Hour, 1*time.Minute) // within retention

	e.r.Sweep(context.Background())

	if got := e.state(t, "old"); got != model.Lifecycle("<gone>") {
		t.Fatalf("paused-past-retention state=%s; want destroyed (record gone)", got)
	}
	if k := e.lastKind("old"); k != "destroyed" {
		t.Fatalf("evict Kind=%q; want destroyed", k)
	}
	if got := e.state(t, "fresh"); got != model.Paused {
		t.Fatalf("within-retention paused state=%s; want paused (not evicted)", got)
	}
}

// TestEvictAndDestroyAreDistinguishableFromTheEventAlone pins §4.4-C's cause on the
// kind where it is the ONLY carrier. Both routes emit Kind "destroyed" (there is no
// `evict` kind, deliberately) and both DELETE the record, so a subscriber that cannot
// separate them from the payload can never separate them at all: the follow-up GET is a
// contractual 404. The assertion that matters is the PAIR — either cause alone would
// pass a test that just checked "some cause is set".
func TestEvictAndDestroyAreDistinguishableFromTheEventAlone(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedPaused(t, "ttl", 1*time.Hour, 2*time.Hour) // past retention → the reaper's route
	e.seedPaused(t, "asked", 0, 1000*time.Hour)      // retention 0 = never evicted

	e.r.Sweep(context.Background())
	if err := e.m.Destroy(context.Background(), model.SandboxID("asked")); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got := e.lastCause("ttl"); got != model.CauseRetentionExpired {
		t.Fatalf("evict cause=%q; want %q — a TTL fired with nobody looking, and after the "+
			"record is gone this event is the only place that is readable",
			got, model.CauseRetentionExpired)
	}
	if got := e.lastCause("asked"); got != model.CauseRequested {
		t.Fatalf("Destroy verb cause=%q; want %q", got, model.CauseRequested)
	}
	// Same Kind on both: the closed six-kind set is intact and the cause is what
	// separates them (a seventh kind would break every subscriber's exhaustive match).
	if k1, k2 := e.lastKind("ttl"), e.lastKind("asked"); k1 != "destroyed" || k2 != "destroyed" {
		t.Fatalf("kinds = %q / %q; want destroyed on BOTH routes", k1, k2)
	}
}

// A cause is populated only where a kind has more than one route: the pause-like kinds
// are already three distinct kinds, so they carry none. Checked because the opposite
// mistake (stamping a cause everywhere "for consistency") would make the field's
// presence meaningless as a signal.
func TestSingleRouteKindsCarryNoCause(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "idle", 10*time.Second, 0)
	e.cl.idle = 30 * time.Second // ≥ idle_timeout → auto-pause

	e.r.Sweep(context.Background())

	if k := e.lastKind("idle"); k != "auto-paused" {
		t.Fatalf("Kind=%q; want auto-paused", k)
	}
	if got := e.lastCause("idle"); got != model.CauseNotApplicable {
		t.Fatalf("auto-paused cause=%q; want empty — the kind IS the why here", got)
	}
}

// zero retention = opted out of eviction: never evicted however old.
func TestReaperZeroRetentionNeverEvicts(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedPaused(t, "keep", 0, 1000*time.Hour)
	e.r.Sweep(context.Background())
	if got := e.state(t, "keep"); got != model.Paused {
		t.Fatalf("zero-retention paused state=%s; want paused (never evicted)", got)
	}
}

// TryLock-skip: a sandbox whose transition lock is HELD (a verb driving it) is
// skipped — the reaper neither classifies nor disposes it, so no race with an
// in-flight resume and no misclassification into dead-recoverable.
func TestReaperSkipsLockedSandbox(t *testing.T) {
	e := newReaperEnv(t, time.Minute)
	e.seedRunning(t, "held", 10*time.Second, 0)
	e.cl.idle = 30 * time.Second // would auto-pause if not skipped

	// hold the lock as a concurrent verb would.
	l := e.m.lock("held")
	l.Lock()
	defer l.Unlock()

	e.r.Sweep(context.Background())

	if got := e.state(t, "held"); got != model.Running {
		t.Fatalf("locked sandbox was disposed: state=%s; want running (TryLock-skip)", got)
	}
}

// TestReaperTickRunsEveryMaintenanceRound pins that one tick performs ALL FOUR
// periodic maintenance rounds, not just the sweeps someone happened to remember.
//
// It exists because `RepairVolumeCaps` was wired at BOOT ONLY and nothing noticed.
// That gap is invisible by construction: every round here is best-effort and
// skip-and-log, so a missing one produces no error, no failed test and no log line —
// the only symptom is a divergence that persists until the next process restart. For
// the cap-repair round specifically the divergence is itself undetectable while it
// lasts (volume.md D7.3: the limit read maps "never capped" and 0 onto each other),
// so "it will be caught later" is false.
//
// Asserted by OBSERVED BACKEND CALLS rather than by reading the source, so it fails
// if a round is deleted, short-circuited, or made conditional on something a plain
// tick does not satisfy.
func TestReaperTickRunsEveryMaintenanceRound(t *testing.T) {
	vol := newFakeVolume()
	m, _ := newLimitsMediator(t, &fakeRootfs{}, &fakeRuntime{}, NodeCapacity{})
	m.deps.Volume = vol
	r := &Reaper{m: m}

	r.tick(context.Background())

	// Seed the two conditions that make the later rounds REACH the backend: a leaked
	// row id to reclaim, and a volume whose record promises a cap the storage lacks.
	// Without them both rounds legitimately short-circuit (no ids / no records) and the
	// test could not tell "ran and had nothing to do" from "never ran at all" — which
	// is the whole failure mode being guarded.
	id, err := m.CreateVolume(context.Background(), 8<<20)
	if err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	vol.mu.Lock()
	vol.deletedRows[4242] = true        // a leaked quota row for the reclamation round
	delete(vol.appliedCaps, string(id)) // storage "forgets" the cap → the D7.3 repair state
	vol.mu.Unlock()

	before := strings.Join(vol.Calls(), ",")
	r.tick(context.Background())
	got := strings.Join(vol.Calls(), ",")

	for call, why := range map[string]string{
		"ListUnitIDs":      "§5.C orphan-volume sweep",
		"ReclaimQuotaRows": "§5.C/D7 leaked quota-row reclamation",
		"SetCapacity":      "§5.C/D7.3 volume-cap repair",
	} {
		if !strings.Contains(strings.TrimPrefix(got, before), call) {
			t.Errorf("one tick never called %s (%s) — a periodic maintenance round is missing, and every round here is best-effort skip-and-log, so its absence raises NOTHING at runtime. Calls during the tick: %s", call, why, strings.TrimPrefix(got, before))
		}
	}
}
