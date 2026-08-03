package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
)

// reconcile L1: pure logic against fakes. Proves the §4.4-A dual dispatch —
// creating→delete, paused→keep, running-alive→adopt (in keep), running-dead→
// demote (not in keep) — and that ReclaimOrphans is called with exactly the
// re-adopted running set.

func seedRec(t *testing.T, st store.Store, id, ov string, state model.Lifecycle) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	// §4.3 template FK: ensure "bare" exists first (idempotent across records).
	_ = st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, CreatedAt: now})
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID(ov),
		State: model.Creating, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// walk to the target resting state via legal CAS.
	switch state {
	case model.Creating:
		// leave as creating.
	case model.Running:
		if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil, nil); err != nil {
			t.Fatal(err)
		}
	case model.Paused:
		_ = st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil, nil)
		if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Running, model.Paused, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileDualDispatch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rp := &fakeRootfs{}
	rt := &fakeRuntime{adoptAlive: map[string]bool{
		"run_alive": true,
		"run_dead":  false,
	}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})

	seedRec(t, st, "creat_x", "ov_c", model.Creating)
	seedRec(t, st, "paused_x", "ov_p", model.Paused)
	seedRec(t, st, "run_alive", "ov_ra", model.Running)
	seedRec(t, st, "run_dead", "ov_rd", model.Running)

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// creating → deleted, publishing NOTHING (a crashed creating never reached a
	// visible running state, matching the live create-unwind path — no unpaired
	// "destroyed" for a sandbox no subscriber ever saw created).
	if _, err := st.GetSandbox(ctx, "creat_x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("creating record not deleted: %v", err)
	}
	if evs, _ := st.UnpublishedEvents(ctx); len(evs) > 0 {
		for _, e := range evs {
			if e.Sandbox == "creat_x" {
				t.Errorf("creating→delete published event %q; want none (never was visible)", e.Kind)
			}
		}
	}
	// paused → kept, still paused.
	if rec, err := st.GetSandbox(ctx, "paused_x"); err != nil || rec.State != model.Paused {
		t.Errorf("paused record changed: %+v err=%v", rec, err)
	}
	// running alive → still running + live handle present.
	if rec, _ := st.GetSandbox(ctx, "run_alive"); rec.State != model.Running {
		t.Errorf("alive running demoted: %s", rec.State)
	}
	if _, err := m.Endpoint("run_alive"); err != nil {
		t.Errorf("alive running has no live handle after adopt: %v", err)
	}
	// running dead → demoted to paused, no live handle.
	if rec, _ := st.GetSandbox(ctx, "run_dead"); rec.State != model.Paused {
		t.Errorf("dead running not demoted: %s", rec.State)
	}
	if _, err := m.Endpoint("run_dead"); err == nil {
		t.Error("dead running should have no live handle")
	}

	// ReclaimOrphans called with keep = exactly {run_alive}.
	var reclaimCall string
	for _, c := range rt.Calls() {
		if strings.HasPrefix(c, "ReclaimOrphans:") {
			reclaimCall = c
		}
	}
	if reclaimCall != "ReclaimOrphans:keep=run_alive" {
		t.Errorf("reclaim keep set = %q; want only run_alive", reclaimCall)
	}
}

// running→paused demote emits an event (§4.4-A ties to §4.4-C).
func TestReconcileDemoteEmitsEvent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rt := &fakeRuntime{adoptAlive: map[string]bool{"run_dead": false}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedRec(t, st, "run_dead", "ov_rd", model.Running)

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	evs, _ := st.UnpublishedEvents(ctx)
	found := false
	for _, e := range evs {
		// involuntary demote emits "crash-paused", distinct from user "paused"
		// and reaper "auto-paused" (§4.1 / §4.2).
		if e.Sandbox == "run_dead" && e.Kind == "crash-paused" && e.State == model.Paused {
			found = true
		}
	}
	if !found {
		t.Fatalf("no crash-paused event emitted on involuntary demote; got %+v", evs)
	}
}

// skip-and-log: a `running` survivor whose Adopt probe ERRORS is unresolved, not
// dead. Reconcile must NOT abort (other sandboxes still reconcile), must NOT
// reclaim the uncertain survivor's placement (protect-on-uncertainty: a
// possibly-live container is never torn down), must leave its record `running`,
// and must log the skip. A clean paused sandbox alongside it reconciles normally.
func TestReconcileSkipAndLogOnUncertainSurvivor(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rt := &fakeRuntime{
		adoptErr: map[string]error{"run_uncertain": errors.New("runsc state: transient")},
	}
	var logs []string
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
		Logf:          func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})

	seedRec(t, st, "run_uncertain", "ov_u", model.Running)
	seedRec(t, st, "paused_ok", "ov_p", model.Paused)

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile must not abort on an uncertain survivor: %v", err)
	}

	// uncertain survivor: record still running, placement PROTECTED (in keep set).
	if rec, _ := st.GetSandbox(ctx, "run_uncertain"); rec.State != model.Running {
		t.Errorf("uncertain survivor state=%s; want running (not demoted, not deleted)", rec.State)
	}
	var reclaimCall string
	for _, c := range rt.Calls() {
		if strings.HasPrefix(c, "ReclaimOrphans:") {
			reclaimCall = c
		}
	}
	if reclaimCall != "ReclaimOrphans:keep=run_uncertain" {
		t.Errorf("uncertain survivor not protected from reclaim: keep=%q; want run_uncertain", reclaimCall)
	}
	// the paused sandbox alongside it still reconciled fine.
	if rec, _ := st.GetSandbox(ctx, "paused_ok"); rec.State != model.Paused {
		t.Errorf("paused sandbox disturbed: %s", rec.State)
	}
	// the skip was logged.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "run_uncertain") && strings.Contains(l, "protected") {
			found = true
		}
	}
	if !found {
		t.Errorf("skip not logged; logs=%v", logs)
	}
}

// periodic reconcile (§4.4-A live variant / reaper-health D4): a `running`
// sandbox this process built (live-map handle present) is protected — kept from
// reclaim WITHOUT a redundant Adopt re-recognition (that is the crash-recovery
// path, unnecessary when the handle is already held). Its placement lands in keep.
func TestPeriodicReconcileProtectsLiveSurvivor(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rt := &fakeRuntime{adoptAlive: map[string]bool{}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	// a running sandbox WITH a live handle (built by this process).
	seedRec(t, st, "live_run", "ov_lr", model.Running)
	m.put("live_run", &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.5:9000"}})

	if err := m.ReconcilePeriodic(ctx); err != nil {
		t.Fatalf("ReconcilePeriodic: %v", err)
	}

	// still running, still holds its live handle.
	if rec, _ := st.GetSandbox(ctx, "live_run"); rec.State != model.Running {
		t.Fatalf("live survivor demoted: %s", rec.State)
	}
	if _, err := m.Endpoint("live_run"); err != nil {
		t.Fatalf("live survivor lost its handle: %v", err)
	}
	// NOT re-adopted (we already held the handle) — Adopt must not have been called.
	for _, c := range rt.Calls() {
		if c == "Adopt:live_run" {
			t.Fatalf("periodic reconcile re-adopted a live survivor (should skip: D4)")
		}
	}
	// protected in keep.
	var reclaimCall string
	for _, c := range rt.Calls() {
		if strings.HasPrefix(c, "ReclaimOrphans:") {
			reclaimCall = c
		}
	}
	if reclaimCall != "ReclaimOrphans:keep=live_run" {
		t.Fatalf("live survivor not protected: keep=%q; want live_run", reclaimCall)
	}
}

// periodic reconcile skips a `running` sandbox whose transition lock is HELD (an
// in-flight resume/verb) — it must NOT be treated as an orphan or demoted, just
// protected and retried (§4.4-A live variant addition 1).
func TestPeriodicReconcileSkipsLockedSandbox(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rt := &fakeRuntime{adoptAlive: map[string]bool{}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedRec(t, st, "held_run", "ov_hr", model.Running)
	// NO live handle — so if not for the held lock, the periodic path would run
	// adopt/demote. Holding the lock must make it skip that entirely.
	l := m.lock("held_run")
	l.Lock()
	defer l.Unlock()

	if err := m.ReconcilePeriodic(ctx); err != nil {
		t.Fatalf("ReconcilePeriodic: %v", err)
	}
	// not demoted, and Adopt never ran (we skipped before probing).
	if rec, _ := st.GetSandbox(ctx, "held_run"); rec.State != model.Running {
		t.Fatalf("locked sandbox demoted: %s; want running (skip)", rec.State)
	}
	for _, c := range rt.Calls() {
		if c == "Adopt:held_run" {
			t.Fatalf("periodic reconcile probed a lock-held sandbox (should skip: §4.4-A)")
		}
	}
}

// waitForCall polls fn's recorded calls until `want` appears or the deadline
// hits (used to synchronize on a verb reaching its gated Launch, i.e. holding
// the transition lock).
func waitForCall(t *testing.T, calls func() []string, wantPrefix string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range calls() {
			if strings.HasPrefix(c, wantPrefix) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for call with prefix %q; got %v", wantPrefix, calls())
}

// periodic reconcile must NOT tear down an IN-FLIGHT create (§4.4-A: a lock-held
// sandbox is skipped in ALL states — reaper-health D4 addition 1). A live Create
// holds the
// transition lock across its whole cold-start with the record reading `creating`;
// a periodic tick in that window must skip it, NOT delete the record + destroy
// the overlay it is building. The lock-held skip applies in ALL states, not just
// `running`.
func TestPeriodicReconcileSkipsInFlightCreate(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st) // §4.3 template FK
	rp := &fakeRootfs{}
	gate := make(chan struct{})
	rt := &fakeRuntime{launchGate: gate, adoptAlive: map[string]bool{}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})

	done := make(chan struct{})
	var sid model.SandboxID
	var createErr error
	go func() {
		sid, createErr = m.Create(ctx, CreateSpec{Template: "bare"})
		close(done)
	}()

	// wait until Create is blocked inside Launch → lock held, record `creating`.
	waitForCall(t, rt.Calls, "Launch:") // any Launch; only this create is running
	// resolve the in-flight creating record id.
	recs, _ := st.ListSandboxes(ctx)
	var creatingID model.SandboxID
	for _, r := range recs {
		if r.State == model.Creating {
			creatingID = r.ID
		}
	}
	if creatingID == "" {
		t.Fatal("no creating record while Create is mid-Launch")
	}

	rootfsBefore := len(rp.Calls())
	if err := m.ReconcilePeriodic(ctx); err != nil {
		t.Fatalf("ReconcilePeriodic: %v", err)
	}

	// the in-flight create must survive untouched: record still creating, overlay
	// NOT destroyed by the reconcile.
	if rec, err := st.GetSandbox(ctx, creatingID); err != nil || rec.State != model.Creating {
		t.Fatalf("in-flight create disturbed: state=%s err=%v; want creating (skipped)", rec.State, err)
	}
	for _, c := range rp.Calls()[rootfsBefore:] {
		if c == "DestroyOverlay:"+string(recs[0].Overlay) || c == "AdoptMerged:"+string(recs[0].Overlay) {
			t.Fatalf("reconcile touched an in-flight create's overlay: %s", c)
		}
	}

	close(gate) // let Create finish
	<-done
	if createErr != nil {
		t.Fatalf("Create failed: %v", createErr)
	}
	if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Running {
		t.Fatalf("create did not reach running after reconcile skip: %s", rec.State)
	}
}

// periodic reconcile must NOT tear down an IN-FLIGHT resume (§4.4-A, ALL states).
// A live Resume holds the lock across cold-start with the record reading `paused`;
// a periodic tick must skip it, NOT unmount the merged overlay it just mounted or
// reclaim the placement it just launched — the `paused`-arm of the same skip.
func TestPeriodicReconcileSkipsInFlightResume(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rp := &fakeRootfs{}
	gate := make(chan struct{})
	rt := &fakeRuntime{launchGate: gate, adoptAlive: map[string]bool{}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedRec(t, st, "res_x", "ov_res", model.Paused)

	done := make(chan struct{})
	var resumeErr error
	go func() {
		resumeErr = m.Resume(ctx, "res_x")
		close(done)
	}()

	// wait until Resume is blocked inside Launch → lock held, record still `paused`.
	waitForCall(t, rt.Calls, "Launch:res_x")

	rootfsBefore := len(rp.Calls())
	if err := m.ReconcilePeriodic(ctx); err != nil {
		t.Fatalf("ReconcilePeriodic: %v", err)
	}

	// the reconcile must not have unmounted / re-adopted the resume's overlay.
	for _, c := range rp.Calls()[rootfsBefore:] {
		if c == "AdoptMerged:ov_res" || c == "Unmount:/fake/merged/ov_res" || c == "DestroyOverlay:ov_res" {
			t.Fatalf("reconcile touched an in-flight resume's overlay: %s", c)
		}
	}
	// its placement must be PROTECTED in keep (held lock → skip → keep).
	var reclaimCall string
	for _, c := range rt.Calls() {
		if strings.HasPrefix(c, "ReclaimOrphans:") {
			reclaimCall = c
		}
	}
	if !strings.Contains(reclaimCall, "res_x") {
		t.Fatalf("in-flight resume not protected from reclaim: keep=%q; want it to contain res_x", reclaimCall)
	}

	close(gate) // let Resume finish
	<-done
	if resumeErr != nil {
		t.Fatalf("Resume failed: %v", resumeErr)
	}
	if rec, _ := st.GetSandbox(ctx, "res_x"); rec.State != model.Running {
		t.Fatalf("resume did not reach running after reconcile skip: %s", rec.State)
	}
}

// Reconcile is idempotent: a second pass over an already-reconciled store makes
// no further changes.
func TestReconcileIdempotent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rt := &fakeRuntime{adoptAlive: map[string]bool{"run_alive": true}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedRec(t, st, "paused_x", "ov_p", model.Paused)
	seedRec(t, st, "run_alive", "ov_ra", model.Running)

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}
	before, _ := st.ListSandboxes(ctx)
	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	after, _ := st.ListSandboxes(ctx)
	if len(before) != len(after) {
		t.Fatalf("second reconcile changed record count: %d → %d", len(before), len(after))
	}
	for _, r := range after {
		if r.State != model.Paused && r.State != model.Running {
			t.Fatalf("record %s in unexpected state after 2nd reconcile: %s", r.ID, r.State)
		}
	}
}
