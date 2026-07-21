package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"engine/internal/model"
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
		if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil); err != nil {
			t.Fatal(err)
		}
	case model.Paused:
		_ = st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil)
		if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Running, model.Paused, nil); err != nil {
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

// Reconcile is idempotent: a second pass over an already-reconciled store makes
// no further changes (B7 幂等).
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
