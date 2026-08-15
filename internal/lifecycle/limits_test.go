package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// Unit tests for the resource admission resource-limit Binding and the health and reclamation death-cause
// disposition — the mediator-side logic, as pure logic. The substrate behaviour
// (does cpu.max really throttle, does the RAM cap really terminate) is asserted
// on real runsc in test/resource_substrate_test.go; these cover the parts that
// are about ORCHESTRATION and would be invisible in an e2e pass/fail.

func newLimitsMediator(t *testing.T, rp *fakeRootfs, rt *fakeRuntime, capacity NodeCapacity) (*Mediator, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	seedBare(t, st)
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		NodeCapacity:  capacity,
	})
	return m, st
}

// The caps are a BINDING: create materializes them
// into the record, and every placement RE-DERIVES them from that record rather
// than reading back a frozen placement value. The distinction is observable here
// in a way it is not on the substrate: we mutate the stored record while the
// sandbox is paused and assert the NEXT placement uses the NEW value.
func TestLimitsAreABindingRematerializedEachPlacement(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})

	want := model.Limits{CPUMillis: 500, MemoryByte: 192 << 20, DiskByte: 64 << 20}
	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: want})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// materialized into the record (resource admission: SoT is the control-plane record).
	rec, _ := st.GetSandbox(ctx, sid)
	if rec.Limits != want {
		t.Fatalf("record limits = %+v; want %+v", rec.Limits, want)
	}
	// ...and pushed across the runtime provider contract seam on the create placement. The capacity axis
	// must NOT be in the runtime spec — it goes to the rootfs provider (resource admission).
	if got := rt.launchLimits[0]; got.CPUMillis != 500 || got.MemoryByte != 192<<20 {
		t.Fatalf("launch limits = %+v; want cpu=500 mem=%d", got, 192<<20)
	}
	if !contains(rp.Calls(), fmt.Sprintf("CreateOverlay:%s;lower=bare;cap=%d", rec.Overlay, 64<<20)) {
		t.Fatalf("capacity cap did not reach the rootfs provider at create: %v", rp.Calls())
	}

	// Now the real assertion: pause, EDIT the stored caps, resume.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	edited := model.Limits{CPUMillis: 1500, MemoryByte: 512 << 20, DiskByte: 128 << 20}
	st.SetLimitsForTest(sid, edited)

	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := rt.launchLimits[len(rt.launchLimits)-1]; got.CPUMillis != 1500 || got.MemoryByte != 512<<20 {
		t.Fatalf("resume replayed stale limits %+v; want the EDITED cpu=1500 mem=%d (resource admission Binding, not frozen state)", got, 512<<20)
	}
	rec2, _ := st.GetSandbox(ctx, sid)
	if !contains(rp.Calls(), fmt.Sprintf("OpenOverlay:%s;lower=bare;cap=%d", rec2.Overlay, 128<<20)) {
		t.Fatalf("resume did not re-apply the EDITED capacity cap: %v", rp.Calls())
	}
}

// Handle-only Overlay rebuilds must pass CapUnchanged, never 0. Passing 0 would
// mean "remove the cap", so a destroy / Promote-read / orphan-reclaim / adopt
// would silently un-cap a sandbox as a side effect of reconstructing a handle.
func TestHandleOnlyRebuildsDoNotTouchTheCap(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 64 << 20}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, _ := st.GetSandbox(ctx, sid)
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// destroy reopens the Overlay purely to reclaim bytes — it is not a placement,
	// so it must declare "touch no quota".
	if !contains(rp.Calls(), fmt.Sprintf("OpenOverlay:%s;lower=bare;cap=unchanged", rec.Overlay)) {
		t.Fatalf("destroy's handle rebuild did not use CapUnchanged: %v", rp.Calls())
	}
	if contains(rp.Calls(), fmt.Sprintf("OpenOverlay:%s;lower=bare;cap=0", rec.Overlay)) {
		t.Fatal("destroy passed cap=0 (would CLEAR a live sandbox's quota) instead of CapUnchanged")
	}
}

// health and reclamation — the two dead HEALTH STATES are parallel, not nested: identical
// observations (L1 dead, persistent state intact), OPPOSITE dispositions,
// separated only by the death cause. Both crash-pause; only resource-terminated
// refuses to resume, and a dead-recoverable one really does come back up.
func TestDeathCauseSeparatesTheTwoParallelDeadHealthStates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		cause      runtime.DeathCause
		wantHealth Health
		wantStored model.DeathCause
		wantResume bool // may it be resumed afterwards?
	}{
		{"resource cap → refuse resume", runtime.CauseResourceExceeded, HealthResourceTerminated, model.CauseResourceExceeded, false},
		{"other death → resume", runtime.CauseGone, HealthDeadRecoverable, model.CauseGone, true},
		{"undeterminable → dead-recoverable (structural self-correction)", runtime.CauseUnknown, HealthDeadRecoverable, model.CauseUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := &fakeRootfs{}
			rt := &fakeRuntime{
				adoptAlive: map[string]bool{},
				adoptCause: map[string]runtime.DeathCause{},
			}
			st := store.NewMemory()
			seedBare(t, st)
			// L2 is driven by this flag: DOWN while we classify the dead unit, then
			// flipped UP so the resume below can actually reach `running`. Without the
			// flip a permitted resume would still fail at the health gate, and the
			// test would pass on the wrong error, proving only "the guard did not fire"
			// rather than "the resume was allowed".
			l2Up := false
			m := New(Deps{
				Store: st, Runtime: rt, Rootfs: rp,
				Volume: newFakeVolume(),
				Dial: func(transport.Endpoint) transport.Client {
					return &fakeClient{healthyFn: func() bool { return l2Up }}
				},
				HealthTimeout: 200 * time.Millisecond,
			})
			// Build the `running` state directly rather than via Create: L2 is down at
			// this point (to reach the DEAD path below), so a create could not pass its
			//  readiness gate.
			sid2 := model.SandboxID("sbx_dead")
			now := time.Now()
			if err := st.CreateSandbox(ctx, model.Sandbox{
				ID: sid2, Template: "bare", Overlay: "ov_dead", State: model.Creating,
				Limits: model.Limits{MemoryByte: 192 << 20}, CreatedAt: now, UpdatedAt: now,
			}, nil); err != nil {
				t.Fatal(err)
			}
			if err := st.CompareAndSetState(ctx, sid2, model.Creating, model.Running, nil, nil); err != nil {
				t.Fatal(err)
			}
			m.put(sid2, &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.9:9000"}})
			rt.adoptAlive[string(sid2)] = false
			rt.adoptCause[string(sid2)] = tc.cause

			// classify must carry the cause OUT with the health state (the lifecycle contract).
			rec, _ := st.GetSandbox(ctx, sid2)
			h, gotCause := m.classify(ctx, rec)
			if h != tc.wantHealth {
				t.Fatalf("health = %s; want %s", h, tc.wantHealth)
			}
			if gotCause != tc.cause {
				t.Fatalf("classify dropped the cause: got %v want %v (the lifecycle contract: read-and-discard loses it forever)", gotCause, tc.cause)
			}

			// disposition: crash-pause, persisting the cause in the SAME commit.
			dc := deathCauseOf(gotCause)
			if err := m.pauseLocked(ctx, sid2, "crash-paused", &dc); err != nil {
				t.Fatalf("crash-pause: %v", err)
			}
			after, _ := st.GetSandbox(ctx, sid2)
			if after.State != model.Paused {
				t.Fatalf("state = %s; want paused (both health states crash-pause)", after.State)
			}
			if after.DeathCause != tc.wantStored {
				t.Fatalf("stored death cause = %q; want %q", after.DeathCause, tc.wantStored)
			}

			// The disposition that actually differs.
			l2Up = true
			err := m.Resume(ctx, sid2)
			if tc.wantResume {
				if err != nil {
					t.Fatalf("resume of a %s death = %v; want it to SUCCEED (health and reclamation dead-recoverable resumes)", tc.cause, err)
				}
				back, _ := st.GetSandbox(ctx, sid2)
				if back.State != model.Running {
					t.Fatalf("state = %s after a permitted resume; want running", back.State)
				}
			}
			if !tc.wantResume {
				if !errors.Is(err, ErrResourceTerminated) {
					t.Fatalf("resume err = %v; want ErrResourceTerminated (health and reclamation: resuming re-triggers the cap = crash loop)", err)
				}
				stayed, _ := st.GetSandbox(ctx, sid2)
				if stayed.State != model.Paused {
					t.Fatalf("state = %s; want it to STAY paused", stayed.State)
				}
			}
		})
	}
}

// health and reclamation bounded self-correction: an Unknown death resumes, and a SUCCESSFUL resume
// CLEARS the recorded cause. Without the clear, a stale cause would accumulate and
// the "second termination is observed in the control plane's plain view" argument
// would rest on a value from the previous life.
func TestSuccessfulResumeClearsTheDeathCause(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// crash-pause it with an Unknown cause (the health and reclamation fallback case).
	dc := model.CauseUnknown
	if err := m.pauseLocked(ctx, sid, "crash-paused", &dc); err != nil {
		t.Fatalf("crash-pause: %v", err)
	}
	if rec, _ := st.GetSandbox(ctx, sid); rec.DeathCause != model.CauseUnknown {
		t.Fatalf("death cause = %q; want unknown", rec.DeathCause)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume (unknown cause must be resumable): %v", err)
	}
	rec, _ := st.GetSandbox(ctx, sid)
	if rec.DeathCause != model.CauseNone {
		t.Fatalf("death cause = %q after a successful resume; want cleared (it no longer explains anything)", rec.DeathCause)
	}
}

// The resource-admission gate. Asserts the FORMULA,
// including the term that is easy to omit: measured layer bytes, which no
// per-sandbox cap covers under squota (uncapped rootfs accounting).
func TestAdmissionGateEnforcesTheAccountingFormula(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30

	t.Run("uncapped bytes count against the node budget", func(t *testing.T) {
		rp := &fakeRootfs{uncappedBytes: 6 * gib} // Template/layer bytes on disk
		rt := &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 10 * gib})

		// 6 GiB of layers + a 5 GiB cap = 11 GiB > 10 GiB → refused. Were layer bytes
		// omitted from the sum, this would wrongly be admitted at 5 GiB.
		_, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 5 * gib}})
		if !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("create err = %v; want ErrInsufficientCapacity (6 GiB layers + 5 GiB cap > 10 GiB)", err)
		}
		// and NOTHING was built: no overlay, no placement (atomic transition rollback no half-build).
		for _, c := range rp.Calls() {
			if len(c) >= 13 && c[:13] == "CreateOverlay" {
				t.Fatalf("refused create still allocated an Overlay: %v", rp.Calls())
			}
		}
		if len(rt.Calls()) != 0 {
			t.Fatalf("refused create still touched the runtime: %v", rt.Calls())
		}
	})

	t.Run("committed caps accumulate", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 10 * gib})
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 6 * gib}}); err != nil {
			t.Fatalf("first create: %v", err)
		}
		// 6 committed + 5 requested > 10 → the second is refused even though 5 alone fits.
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 5 * gib}}); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("second create err = %v; want ErrInsufficientCapacity", err)
		}
		// one that DOES fit is admitted.
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 3 * gib}}); err != nil {
			t.Fatalf("fitting create refused: %v", err)
		}
	})

	t.Run("resume excludes its own committed promise", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		// budget exactly equals the single sandbox's cap: a resume must not
		// double-count the sandbox's own reservation against itself.
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 8 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 8 * gib}})
		if err != nil {
			t.Fatalf("create at exactly the budget: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if err := m.Resume(ctx, sid); err != nil {
			t.Fatalf("resume of an unchanged sandbox was refused: %v (its own promise was double-counted)", err)
		}
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Running {
			t.Fatalf("state = %s; want running", rec.State)
		}
	})

	t.Run("refused resume stays paused", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 8 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 4 * gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		// layers grew while it was paused (e.g. a Promote), so its cap no longer fits.
		rp.mu.Lock()
		rp.uncappedBytes = 6 * gib
		rp.mu.Unlock()
		if err := m.Resume(ctx, sid); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("resume err = %v; want ErrInsufficientCapacity", err)
		}
		// atomic transition rollback: atomic return to the SOURCE state.
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Paused {
			t.Fatalf("state = %s; want it to stay paused (atomic transition rollback)", rec.State)
		}
	})

	t.Run("memory is a separate budget", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{MemoryByte: 4 * gib})
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 3 * gib}}); err != nil {
			t.Fatalf("first create: %v", err)
		}
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 2 * gib}}); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("err = %v; want ErrInsufficientCapacity (3+2 > 4 GiB RAM)", err)
		}
		// A huge DISK request is unaffected: the axes are not fungible (resource admission), and
		// disk is UNMETERED here so its size is nobody's business. The memory cap is
		// present because memory IS metered on this node, and a metered axis requires
		// a cap (resource admission) — that premise is what the next subtest pins down.
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 900 * gib, MemoryByte: gib}}); err != nil {
			t.Fatalf("disk request refused against a RAM-only budget: %v", err)
		}
	})

	// the resource-cap write rules: once an axis is metered, every admitted sandbox must carry a cap on it.
	t.Run("a metered axis requires a cap", func(t *testing.T) {
		t.Run("disk metered, disk cap omitted → refused", func(t *testing.T) {
			m, st := newLimitsMediator(t, &fakeRootfs{}, &fakeRuntime{}, NodeCapacity{DiskByte: 8 * gib})
			_, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}})
			// The two verdicts must stay DISTINCT sentinels: nothing is full here, so
			// reporting insufficient capacity would be a lie, and the API maps them to
			// different statuses (400 vs 507 — the orchestrator's
			// TestAdmissionVerdictsMapToDistinctStatuses pins that half). Asserted as
			// one condition because `errors.Is(ErrInsufficientCapacity)` is unreachable
			// once the first check has passed, so a separate `if` could never fail.
			if !errors.Is(err, ErrCapRequired) || errors.Is(err, ErrInsufficientCapacity) {
				t.Fatalf("err = %v; want ErrCapRequired and NOT ErrInsufficientCapacity", err)
			}
			// refused at PROMISE time → no record, nothing half-built (atomic transition rollback).
			if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
				t.Fatalf("refused create left %d record(s); want none", len(recs))
			}
		})

		t.Run("memory metered, memory cap omitted → refused", func(t *testing.T) {
			m, _ := newLimitsMediator(t, &fakeRootfs{}, &fakeRuntime{}, NodeCapacity{MemoryByte: 4 * gib})
			if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}}); !errors.Is(err, ErrCapRequired) {
				t.Fatalf("err = %v; want ErrCapRequired", err)
			}
		})

		// The mirror property, and the one that keeps `0 = unlimited` honest: on an
		// UNMETERED axis an omitted cap is a legal, meaningful resting value.
		t.Run("unmetered axis: uncapped stays legal", func(t *testing.T) {
			m, _ := newLimitsMediator(t, &fakeRootfs{}, &fakeRuntime{}, NodeCapacity{})
			if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}}); err != nil {
				t.Fatalf("uncapped create on an unmetered node: %v", err)
			}
		})

		// resume is gated too, so turning a budget ON while uncapped records exist
		// wedges them at `paused` (atomic transition rollback source state). The known residual of this
		// rule; the remedy is "PATCH a cap onto the record, then resume".
		t.Run("resume of an uncapped record on a now-metered node → refused, stays paused", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			m.deps.NodeCapacity = NodeCapacity{DiskByte: 8 * gib} // operator meters the axis
			if err := m.Resume(ctx, sid); !errors.Is(err, ErrCapRequired) {
				t.Fatalf("resume err = %v; want ErrCapRequired", err)
			}
			if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Paused {
				t.Fatalf("state = %s; want paused (atomic transition rollback)", rec.State)
			}
		})
	})

	t.Run("unmeasurable backend fails CLOSED", func(t *testing.T) {
		rp := &fakeRootfs{measureErr: errors.New("backend unavailable")}
		rt := &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 10 * gib})
		// The inequality cannot be evaluated → refuse. Admitting on an unknown total
		// is how a node gets overcommitted, and the cost lands on unrelated sandboxes.
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}}); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("err = %v; want ErrInsufficientCapacity (fail closed)", err)
		}
	})

	t.Run("no declared budget is unmetered", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{}) // zero = unmetered
		// The engine must not invent a capacity number; an undeclared budget gates
		// nothing rather than guessing one.
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 900 * gib}}); err != nil {
			t.Fatalf("unmetered node refused a create: %v", err)
		}
	})
}

// Promote must clear the SAME inequality as create (resource admission: any control-plane action
// that increases uncapped bytes; uncapped rootfs accounting's residual). A frozen layer is
// charged to no sandbox cap, so it lands in the MEASURED term — the harm of not
// gating it is not a failed Promote but an UNRELATED sandbox meeting ENOSPC nowhere
// near its own cap, the data-plane exposure boundary containment broken from the other side.
func TestPromotePassesTheNodeCapacityGate(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30

	// helper: a paused sandbox with a declared cap, on a node with the given budget.
	setup := func(t *testing.T, budget, promoteCost, uncapped int64) (*Mediator, *store.Memory, *fakeRootfs) {
		t.Helper()
		rp := &fakeRootfs{promoteCost: promoteCost, uncappedBytes: uncapped}
		m, st := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{DiskByte: budget})
		return m, st, rp
	}

	t.Run("over-budget promote is REFUSED and leaves no trace", func(t *testing.T) {
		// 6 GiB committed cap + 1 GiB existing layers + a 4 GiB freeze = 11 > 10.
		m, st, rp := setup(t, 10*gib, 4*gib, gib)
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 6 * gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		tid, err := m.Promote(ctx, sid)
		if !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("promote err = %v; want ErrInsufficientCapacity (6+1+4 > 10 GiB)", err)
		}
		if tid != "" {
			t.Fatalf("refused promote returned a TemplateID %q", tid)
		}
		// no layer bytes: the freeze was never called, so there is nothing for the
		// sweep to reclaim (a refusal must not leave an orphan).
		for _, c := range rp.Calls() {
			if strings.HasPrefix(c, "PromoteToTemplate:") {
				t.Fatalf("refused promote still froze a layer: %v", rp.Calls())
			}
		}
		// and no `templates` row beyond the seeded builtin.
		tmpls, err := st.ListTemplates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tm := range tmpls {
			if !tm.Builtin {
				t.Fatalf("refused promote inserted a templates row: %+v", tm)
			}
		}
		// legal resting state: the source is untouched (atomic transition rollback source state).
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Paused {
			t.Fatalf("state = %s; want paused (unchanged by a refusal)", rec.State)
		}
	})

	t.Run("a promote that FITS is admitted", func(t *testing.T) {
		// 6 committed + 1 layer + 2 freeze = 9 ≤ 10.
		m, st, rp := setup(t, 10*gib, 2*gib, gib)
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 6 * gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		tid, err := m.Promote(ctx, sid)
		if err != nil {
			t.Fatalf("fitting promote refused: %v", err)
		}
		if _, err := st.GetTemplate(ctx, tid); err != nil {
			t.Fatalf("admitted promote left no templates row: %v", err)
		}
		// The gate consulted the provider's forecast, and did so BEFORE the freeze —
		// a forecast taken afterwards would be measuring a decision already made.
		forecast, freeze := idxOfPrefix(rp.Calls(), "PromoteCostBytes:"), idxOfPrefix(rp.Calls(), "PromoteToTemplate:")
		if forecast < 0 {
			t.Fatalf("gate did not consult the provider forecast: %v", rp.Calls())
		}
		if freeze < 0 || forecast > freeze {
			t.Fatalf("forecast did not precede the freeze: %v", rp.Calls())
		}
	})

	// The gate runs BEFORE the quiesce, on purpose: placing it after would buy an
	// exact byte count at the price of a refusal that MUTATED state — a running
	// sandbox left `paused` because the node was full. atomic transition rollback wants a failed
	// transition to return to its source state.
	t.Run("refused promote of a RUNNING source leaves it running", func(t *testing.T) {
		m, st, rp := setup(t, 10*gib, 5*gib, gib)
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 6 * gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := m.Promote(ctx, sid); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("promote err = %v; want ErrInsufficientCapacity", err)
		}
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Running {
			t.Fatalf("state = %s; want RUNNING — a capacity refusal must not stop the sandbox as a side effect", rec.State)
		}
		// no quiesce happened at all: the placement is still live.
		for _, c := range rp.Calls() {
			if strings.HasPrefix(c, "Unmount:") {
				t.Fatalf("refused promote quiesced the source anyway: %v", rp.Calls())
			}
		}
	})

	t.Run("unmetered node (capacity 0) still allows promote", func(t *testing.T) {
		rp := &fakeRootfs{promoteCost: 900 * gib}
		m, st := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tid, err := m.Promote(ctx, sid)
		if err != nil {
			t.Fatalf("promote on an unmetered node was refused: %v", err)
		}
		if _, err := st.GetTemplate(ctx, tid); err != nil {
			t.Fatalf("templates row missing: %v", err)
		}
	})

	// A RAM-only budget must not gate a Promote: uncapped bytes are disk by
	// construction and the two budgets are not fungible (resource admission).
	t.Run("memory-only budget does not gate promote", func(t *testing.T) {
		rp := &fakeRootfs{promoteCost: 900 * gib}
		m, _ := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{MemoryByte: 4 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := m.Promote(ctx, sid); err != nil {
			t.Fatalf("promote gated against a RAM budget: %v", err)
		}
	})

	// Fail CLOSED: the inequality cannot be evaluated, so the freeze is refused.
	t.Run("unforecastable freeze fails closed", func(t *testing.T) {
		rp := &fakeRootfs{promoteCostErr: errors.New("backend unavailable")}
		m, st := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{DiskByte: 10 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := m.Promote(ctx, sid); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("promote err = %v; want ErrInsufficientCapacity (fail closed)", err)
		}
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Running {
			t.Fatalf("state = %s; want running (nothing mutated)", rec.State)
		}
	})
}

// The concurrency half of the Promote gate. The inequality is node-GLOBAL while the
// transition lock is per-sandbox, so a gate that only measured could be raced: the
// bytes a Promote is about to write are invisible to UncappedBytes until the copy
// lands. The mediator therefore publishes a RESERVATION under admitMu, and this
// test pins that a create racing an in-flight freeze sees it.
func TestInFlightPromoteBytesAreVisibleToTheGate(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30

	gate := make(chan struct{})
	reached := make(chan struct{})
	rp := &fakeRootfs{promoteCost: 5 * gib, promoteGate: gate, promoteReached: reached}
	m, st := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{DiskByte: 10 * gib})

	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 2 * gib}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Promote is admitted (2 committed + 5 growth = 7 ≤ 10) and then blocks inside
	// the freeze, holding a 5 GiB reservation with ZERO bytes yet measurable.
	done := make(chan error, 1)
	go func() {
		_, err := m.Promote(ctx, sid)
		done <- err
	}()
	<-reached

	// 2 committed + 0 measured + 5 reserved + 4 requested = 11 > 10 → refused.
	// Without the reservation this would be 2+0+4 = 6 and would WRONGLY be admitted,
	// leaving the node overcommitted the moment the layer lands.
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 4 * gib}}); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("create racing an in-flight promote = %v; want ErrInsufficientCapacity (the reserved 5 GiB must count)", err)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Reservation RELEASED once the bytes are real: the same create now fits (the
	// fake measures 0, so 2+0+0+4 = 6 ≤ 10). A leaked reservation would shrink the
	// node's admissible capacity permanently.
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 4 * gib}}); err != nil {
		t.Fatalf("create after the promote settled = %v; want admitted (reservation leaked?)", err)
	}
	if got := m.pendingUncapped(); got != 0 {
		t.Fatalf("pending uncapped bytes = %d after the promote settled; want 0", got)
	}
	if recs, _ := st.ListSandboxes(ctx); len(recs) != 2 {
		t.Fatalf("sandbox count = %d; want 2", len(recs))
	}
}

// A REFUSED promote must not leak its reservation either — the gate never reserved,
// so a later create sees the full budget.
func TestRefusedPromoteReservesNothing(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30
	rp := &fakeRootfs{promoteCost: 20 * gib}
	m, _ := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{DiskByte: 10 * gib})
	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Promote(ctx, sid); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("promote err = %v; want refused", err)
	}
	if got := m.pendingUncapped(); got != 0 {
		t.Fatalf("refused promote left %d reserved bytes; want 0", got)
	}
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 8 * gib}}); err != nil {
		t.Fatalf("create after a refused promote = %v; want admitted (full budget available)", err)
	}
}

// A FAILED freeze (bytes attempted, error returned) must also release. Otherwise a
// flapping provider would ratchet the node's admissible capacity down to nothing.
func TestFailedFreezeReleasesItsReservation(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30
	rp := &fakeRootfs{promoteCost: 5 * gib, failPromote: true}
	m, _ := newLimitsMediator(t, rp, &fakeRuntime{}, NodeCapacity{DiskByte: 10 * gib})
	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Promote(ctx, sid); err == nil {
		t.Fatal("promote with a failing freeze should error")
	}
	if got := m.pendingUncapped(); got != 0 {
		t.Fatalf("failed freeze left %d reserved bytes; want 0", got)
	}
}

// idxOfPrefix is the position of the first call with the given prefix, or -1 —
// enough to assert both "it happened" and "it happened before that other thing".
func idxOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// compile-time: the fakes satisfy the provider seams.
var (
	_ rootfs.Provider  = (*fakeRootfs)(nil)
	_ runtime.Provider = (*fakeRuntime)(nil)
)

// resource admission / the control-plane API PATCH — the Binding is WRITABLE, and the write point behaves per
// consumer: caps re-materialize at the next placement (so `paused` only), TTLs are
// re-read by the reaper every sweep (so either state, immediately).
func TestUpdateBindingRewritesTheBinding(t *testing.T) {
	ctx := context.Background()
	gib := int64(1) << 30

	t.Run("caps editable from paused, visible in the record", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 128 << 20, DiskByte: 64 << 20}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		want := int64(512 << 20)
		if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &want}); err != nil {
			t.Fatalf("UpdateBinding: %v", err)
		}
		rec, _ := st.GetSandbox(ctx, sid)
		if rec.Limits.MemoryByte != want {
			t.Fatalf("memory = %d; want %d", rec.Limits.MemoryByte, want)
		}
		// omitted fields unchanged — PATCH swaps one COMPLETE binding for another
		// (the API error contract: never leaves a field unresolved).
		if rec.Limits.DiskByte != 64<<20 {
			t.Fatalf("disk = %d; want it untouched at %d", rec.Limits.DiskByte, 64<<20)
		}
		// ...and the edited cap really reaches the seam on the NEXT placement, which
		// is what makes it a Binding rather than a stored-but-inert number (resource admission).
		if err := m.Resume(ctx, sid); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if got := rt.launchLimits[len(rt.launchLimits)-1]; got.MemoryByte != want {
			t.Fatalf("resume materialized memory=%d; want the PATCHed %d", got.MemoryByte, want)
		}
	})

	t.Run("caps refused while running, TTL allowed", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 128 << 20}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		bigger := int64(256 << 20)
		if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &bigger}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("cap edit while running = %v; want ErrConflict (pause first, resource admission)", err)
		}
		// TTL has no placement-local materialization, so it is editable live.
		ttl := 5 * time.Minute
		if err := patchBinding(m, ctx, sid, store.Binding{IdleTimeout: &ttl}); err != nil {
			t.Fatalf("TTL edit while running: %v", err)
		}
		if rec, _ := st.GetSandbox(ctx, sid); rec.IdleTimeout != ttl {
			t.Fatalf("idle_timeout = %s; want %s", rec.IdleTimeout, ttl)
		}
	})

	// health and reclamation offers "raise the cap" as the resource-terminated remedy, but the resume
	// guard reads death_cause and NOTHING else clears it —
	// only a successful resume does, which the guard itself prevents. So without
	// clearing it here the documented remedy cannot complete, ever.
	t.Run("raising memory clears the resource-terminated block", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
		sid := model.SandboxID("sbx_rt")
		now := time.Now()
		if err := st.CreateSandbox(ctx, model.Sandbox{
			ID: sid, Template: "bare", Overlay: "ov_rt", State: model.Paused,
			Limits: model.Limits{MemoryByte: 128 << 20}, DeathCause: model.CauseResourceExceeded,
			CreatedAt: now, UpdatedAt: now,
		}, nil); err != nil {
			t.Fatal(err)
		}
		// baseline: resume is refused on the persisted fact.
		if err := m.Resume(ctx, sid); !errors.Is(err, ErrResourceTerminated) {
			t.Fatalf("resume before raise = %v; want ErrResourceTerminated", err)
		}
		bigger := int64(512 << 20)
		if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &bigger}); err != nil {
			t.Fatalf("raise: %v", err)
		}
		if rec, _ := st.GetSandbox(ctx, sid); rec.DeathCause != model.CauseNone {
			t.Fatalf("death cause = %q; want it CLEARED by the raise (health and reclamation)", rec.DeathCause)
		}
		if err := m.Resume(ctx, sid); err != nil {
			t.Fatalf("resume after raise: %v — the raise-the-cap remedy must actually complete", err)
		}
	})

	// The mirror: an unrelated edit, or a LOWERING, must NOT wipe the record of why
	// the sandbox died — that would re-arm the crash loop the refusal exists to stop.
	t.Run("unrelated and lowering edits do not clear the cause", func(t *testing.T) {
		//
		// The record is created WITH a disk cap so the disk case is an `N → M`: `0 → N` is
		// its own refused write (ErrUncappedDiskNotCappable), and an uncapped record would
		// fail this subtest on the edit's admission instead of on the property under test.
		for _, tc := range []struct {
			name string
			b    func() store.Binding
		}{
			{"disk edit (N → M)", func() store.Binding { v := int64(gib); return store.Binding{DiskByte: &v} }},
			{"memory lowered", func() store.Binding { v := int64(64 << 20); return store.Binding{MemoryByte: &v} }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rp, rt := &fakeRootfs{}, &fakeRuntime{}
				m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
				sid := model.SandboxID("sbx_keep")
				now := time.Now()
				if err := st.CreateSandbox(ctx, model.Sandbox{
					ID: sid, Template: "bare", Overlay: "ov_keep", State: model.Paused,
					Limits:     model.Limits{MemoryByte: 128 << 20, DiskByte: 64 << 20},
					DeathCause: model.CauseResourceExceeded,
					CreatedAt:  now, UpdatedAt: now,
				}, nil); err != nil {
					t.Fatal(err)
				}
				if err := patchBinding(m, ctx, sid, tc.b()); err != nil {
					t.Fatalf("update: %v", err)
				}
				if rec, _ := st.GetSandbox(ctx, sid); rec.DeathCause != model.CauseResourceExceeded {
					t.Fatalf("death cause = %q; want it PRESERVED (only a memory RAISE lifts it)", rec.DeathCause)
				}
			})
		}
	})

	// resource admission — `0 → N` on the DISK axis is a REFUSED WRITE, not a raise (why a 200 bricks
	// the sandbox: ErrUncappedDiskNotCappable). The refusal is DIRECTIONAL, so all three
	// are asserted together: only from-uncapped is refused; dropping and changing stay legal.
	t.Run("disk 0 → N is refused; N → 0 and N → M stay legal", func(t *testing.T) {
		t.Run("0 → N refused, and the STORE IS UNCHANGED", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			// created UNCAPPED on disk → its unit is a plain directory, no quota row.
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 128 << 20}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			before, _ := st.GetSandbox(ctx, sid)
			want := int64(gib)
			err = patchBinding(m, ctx, sid, store.Binding{DiskByte: &want})
			if !errors.Is(err, ErrUncappedDiskNotCappable) {
				t.Fatalf("PATCH disk 0 → %d = %v; want ErrUncappedDiskNotCappable (resource admission: the cap is unmaterializable, so accepting it leaves the sandbox unresumable)", want, err)
			}
			// The refusal must be visible AT THE WRITE POINT with ZERO side effects — that
			// is the reason it is here and not at the next placement (resource admission). Asserted on
			// the whole record, not just the disk field: a refusal that committed the
			// binding's OTHER fields would be a partial write nobody asked for.
			after, _ := st.GetSandbox(ctx, sid)
			if after.Limits != before.Limits {
				t.Fatalf("refused PATCH changed the limits: %+v → %+v; the refusal must change NOTHING", before.Limits, after.Limits)
			}
			if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("refused PATCH moved the record (state %s→%s, UpdatedAt %s→%s)", before.State, after.State, before.UpdatedAt, after.UpdatedAt)
			}
			// ...and the sandbox is still RESUMABLE, which is the harm being prevented. Had
			// the cap been written, every placement would refuse it from here on.
			if err := m.Resume(ctx, sid); err != nil {
				t.Fatalf("resume after the refused PATCH = %v; want success — the refusal exists precisely to keep this working", err)
			}
		})

		t.Run("N → 0 drops the cap", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			zero := int64(0)
			if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &zero}); err != nil {
				t.Fatalf("PATCH disk N → 0 = %v; want it ADMITTED (dropping a cap only clears it, resource admission)", err)
			}
			if rec, _ := st.GetSandbox(ctx, sid); rec.Limits.DiskByte != 0 {
				t.Fatalf("disk = %d; want 0 (the cap was dropped)", rec.Limits.DiskByte)
			}
		})

		// The metered node is the case the subtest above does NOT cover: it runs on
		// NodeCapacity{}, where resource admission makes an uncapped sandbox legal, so dropping a cap
		// there must stay allowed. Metered, the same edit is the uncapped create `admit`
		// refuses (ErrCapRequired) written at a different time, and it is irreversible —
		// re-capping is then refused by ErrUncappedDiskNotCappable, whose message would be
		// factually wrong about this record. Both halves are in the failure message below.
		t.Run("N → 0 is refused while the disk axis is METERED", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 10 * gib})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 4 * gib, MemoryByte: 128 << 20}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			before, _ := st.GetSandbox(ctx, sid)
			zero := int64(0)
			err = patchBinding(m, ctx, sid, store.Binding{DiskByte: &zero})
			if !errors.Is(err, ErrCapRequired) {
				t.Fatalf("PATCH disk %d → 0 on a node metering disk = %v; want ErrCapRequired. Accepting it drops %d bytes off the budget's left side while the sandbox stays free to consume the whole node — the same state resource admission refuses at create — and the sandbox can then never be re-capped, because `0 → N` is refused with a message that would be factually wrong about it",
					before.Limits.DiskByte, err, before.Limits.DiskByte)
			}
			// A refusal changes nothing, as with the `0 → N` arm.
			after, _ := st.GetSandbox(ctx, sid)
			if after.Limits != before.Limits {
				t.Fatalf("refused PATCH changed the limits: %+v → %+v", before.Limits, after.Limits)
			}
			// And an unmetered node still allows the same edit — the refusal is gated on the
			// axis being metered, not applied unconditionally, or it would revoke a
			// capability resource admission grants.
			mu, _ := newLimitsMediator(t, &fakeRootfs{}, &fakeRuntime{}, NodeCapacity{})
			sid2, err := mu.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
			if err != nil {
				t.Fatalf("create on unmetered: %v", err)
			}
			if err := mu.Pause(ctx, sid2); err != nil {
				t.Fatalf("pause: %v", err)
			}
			if err := patchBinding(mu, ctx, sid2, store.Binding{DiskByte: &zero}); err != nil {
				t.Fatalf("PATCH disk N → 0 on an UNMETERED node = %v; want it admitted — resource admission makes an uncapped sandbox legal there, so refusing would revoke a granted capability", err)
			}
		})

		t.Run("N → M changes the cap", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			// Both directions of an N → M edit: the unit is already a quota unit, so
			// neither is unmaterializable.
			for _, want := range []int64{4 * gib, 512 << 20} {
				if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &want}); err != nil {
					t.Fatalf("PATCH disk N → %d = %v; want it ADMITTED", want, err)
				}
				if rec, _ := st.GetSandbox(ctx, sid); rec.Limits.DiskByte != want {
					t.Fatalf("disk = %d; want %d", rec.Limits.DiskByte, want)
				}
			}
		})

		// The refusal is scoped to DISK. Memory and CPU do not materialize as storage
		// units, so `0 → N` on them is an ordinary raise and must stay one.
		t.Run("memory and cpu 0 → N are ordinary raises", func(t *testing.T) {
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			mem, cpu := int64(256<<20), int64(500)
			if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &mem, CPUMillis: &cpu}); err != nil {
				t.Fatalf("PATCH memory/cpu 0 → N = %v; want it ADMITTED (only the disk axis has an unmaterializable direction)", err)
			}
			rec, _ := st.GetSandbox(ctx, sid)
			if rec.Limits.MemoryByte != mem || rec.Limits.CPUMillis != cpu {
				t.Fatalf("limits = %+v; want memory=%d cpu=%d", rec.Limits, mem, cpu)
			}
		})
	})

	// resource admission — an `N → M` disk edit must leave the cap STRICTLY above the Overlay unit's
	// current usage: the substrate accepts a below-usage cap with ZERO validation and is
	// then wedged permanently, and on the sandbox side that also makes it unresumable.
	// The measured wedge and the Rootfs subsystem half: ErrDiskCapBelowUsage.
	t.Run("disk N → M below current usage is refused", func(t *testing.T) {
		const cap0 = 64 << 20
		// newCapped builds a paused, disk-capped sandbox whose unit reports `used` bytes.
		newCapped := func(t *testing.T, used int64) (*Mediator, *store.Memory, *fakeRootfs, model.SandboxID) {
			t.Helper()
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: cap0}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := m.Pause(ctx, sid); err != nil {
				t.Fatalf("pause: %v", err)
			}
			rp.mu.Lock()
			rp.overlayUsed, rp.overlayLimit = used, cap0
			rp.mu.Unlock()
			return m, st, rp, sid
		}

		// The boundary is what the strict inequality is FOR: `cap == usage` is a wedged state
		// too (measured, ErrDiskCapBelowUsage), so it means FULL rather than "exactly
		// enough" and must be refused alongside anything below it.
		for _, tc := range []struct {
			name string
			used int64
			cap  int64
			want error
		}{
			{"below usage", 32 << 20, 16 << 20, ErrDiskCapBelowUsage},
			{"EQUAL to usage means FULL, not exactly enough", 32 << 20, 32 << 20, ErrDiskCapBelowUsage},
			{"one byte above usage is legal", 32 << 20, (32 << 20) + 1, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m, st, _, sid := newCapped(t, tc.used)
				before, _ := st.GetSandbox(ctx, sid)
				err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &tc.cap})
				if !errors.Is(err, tc.want) {
					t.Fatalf("PATCH disk %d → %d with usage %d = %v; want %v", cap0, tc.cap, tc.used, err, tc.want)
				}
				after, _ := st.GetSandbox(ctx, sid)
				if tc.want == nil {
					if after.Limits.DiskByte != tc.cap {
						t.Fatalf("disk = %d; want %d (a cap strictly above usage is an ordinary edit)", after.Limits.DiskByte, tc.cap)
					}
					return
				}
				// A refusal must change NOTHING — that is the reason it lives at the write
				// point rather than at the placement that would wedge the unit.
				if after.Limits != before.Limits {
					t.Fatalf("refused PATCH changed the limits: %+v → %+v", before.Limits, after.Limits)
				}
				if err := m.Resume(ctx, sid); err != nil {
					t.Fatalf("resume after the refused PATCH = %v; want success — the refusal exists precisely to keep the sandbox resumable", err)
				}
			})
		}

		// A RAISE is checked too, and this is not redundant: if the unit's quota row was
		// lost to an external change (the case the Rootfs subsystem's per-placement re-application exists
		// for) the guest can grow past the recorded cap, and then even a cap ABOVE the
		// recorded one can be at or below what the unit holds.
		t.Run("a RAISE that still lands under usage is refused", func(t *testing.T) {
			m, _, _, sid := newCapped(t, 128<<20) // usage past the recorded 64 MiB cap
			raise := int64(96 << 20)              // above the record, still under usage
			if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &raise}); !errors.Is(err, ErrDiskCapBelowUsage) {
				t.Fatalf("PATCH disk raise to %d with usage 128 MiB = %v; want ErrDiskCapBelowUsage (a raise is not automatically safe)", raise, err)
			}
		})

		// Dropping the cap only WIDENS what the unit may hold, so it is always safe and
		// must not even consult usage — a `N → 0` on a unit whose row is unreadable has to
		// keep working.
		t.Run("N → 0 is admitted without reading usage", func(t *testing.T) {
			m, st, rp, sid := newCapped(t, 128<<20)
			rp.mu.Lock()
			rp.usageErr = fmt.Errorf("injected unreadable quota row")
			rp.mu.Unlock()
			zero := int64(0)
			if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &zero}); err != nil {
				t.Fatalf("PATCH disk N → 0 = %v; want it ADMITTED without a usage read (clearing a cap cannot wedge anything)", err)
			}
			if rec, _ := st.GetSandbox(ctx, sid); rec.Limits.DiskByte != 0 {
				t.Fatalf("disk = %d; want 0", rec.Limits.DiskByte)
			}
			for _, c := range rp.Calls() {
				if strings.HasPrefix(c, "Usage:") {
					t.Fatalf("N → 0 consulted the provider (%q); dropping a cap needs no usage number", c)
				}
			}
		})

		// FAIL CLOSED: usage IS the decision here, and admitting a cap without knowing
		// usage risks a state with no exit through the API at all. The provider already
		// retries the transient wholesale-read failure, so a failure that reaches here is a
		// real "cannot tell".
		t.Run("an unreadable usage refuses the PATCH and writes nothing", func(t *testing.T) {
			m, st, rp, sid := newCapped(t, 0)
			rp.mu.Lock()
			rp.usageErr = fmt.Errorf("injected unreadable quota row")
			rp.mu.Unlock()
			before, _ := st.GetSandbox(ctx, sid)
			want := int64(32 << 20)
			err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &want})
			if err == nil {
				t.Fatal("PATCH with an unreadable usage was ADMITTED; resource admission requires failing closed — a blind cap can wedge the unit irreversibly")
			}
			if after, _ := st.GetSandbox(ctx, sid); after.Limits != before.Limits {
				t.Fatalf("fail-closed PATCH still wrote: %+v → %+v", before.Limits, after.Limits)
			}
		})

		// The check is GATED ON THE AXIS BEING WRITTEN. A PATCH that touches only TTLs must
		// not pay a backend round trip — and must not be refused because of a divergence it
		// does not touch. Driven with a usage that WOULD refuse a disk write.
		t.Run("a PATCH that does not write disk never consults usage", func(t *testing.T) {
			m, _, rp, sid := newCapped(t, 128<<20) // usage past the cap: a disk write would refuse
			idle := 30 * time.Minute
			if err := patchBinding(m, ctx, sid, store.Binding{IdleTimeout: &idle}); err != nil {
				t.Fatalf("TTL-only PATCH = %v; want it ADMITTED — it writes no cap, so the disk criterion has nothing to say", err)
			}
			for _, c := range rp.Calls() {
				if strings.HasPrefix(c, "Usage:") {
					t.Fatalf("a TTL-only PATCH consulted the provider (%q); an unrelated PATCH must not be exposed to a quota-row read failure", c)
				}
			}
		})
	})

	// A raise is a new promise, so it must clear admission (resource admission). The gate sums
	// from the store including paused records, so Σ changes at THIS commit — not at
	// the eventual resume.
	t.Run("a raise re-runs the admission gate", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{MemoryByte: 4 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 3 * gib}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		tooBig := 5 * gib
		if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &tooBig}); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("over-budget raise = %v; want ErrInsufficientCapacity", err)
		}
		// refused → the record is untouched.
		if rec, _ := st.GetSandbox(ctx, sid); rec.Limits.MemoryByte != 3*gib {
			t.Fatalf("memory = %d after a refused raise; want the original %d", rec.Limits.MemoryByte, 3*gib)
		}
		// a LOWERING needs no gate (Σ shrinks) and is always allowed.
		smaller := gib
		if err := patchBinding(m, ctx, sid, store.Binding{MemoryByte: &smaller}); err != nil {
			t.Fatalf("lowering refused: %v", err)
		}
	})

	// ...but a CPU-only raise is NOT a raise the gate can speak about. resource admission keeps CPU
	// out of the node accounting (`cpu_millis: 0` is always legal; neither budget
	// mentions it), so running admit for it can only produce a FALSE refusal: the node
	// below is full on BOTH budgets from another sandbox, and a PATCH asking for zero
	// additional bytes must still land.
	t.Run("a cpu-only raise does not run the gate", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 4 * gib, MemoryByte: 4 * gib})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: gib, MemoryByte: gib, CPUMillis: 500}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		// The disk budget goes full underneath it: uncapped layer bytes are MEASURED, not
		// promised, so the second term can grow after this sandbox was admitted until even
		// its own unchanged cap no longer fits (the same fixture the resume gate uses).
		rp.mu.Lock()
		rp.uncappedBytes = 3*gib + 1
		rp.mu.Unlock()
		// Proof the node is genuinely over budget: a one-byte DISK raise is refused.
		// Without this the subtest could pass on a node that was never full.
		oneMore := gib + 1
		if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &oneMore}); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("disk raise on a full node = %v; want ErrInsufficientCapacity (the fixture must really be exhausted)", err)
		}
		cpu := int64(2000)
		if err := patchBinding(m, ctx, sid, store.Binding{CPUMillis: &cpu}); err != nil {
			t.Fatalf("cpu-only raise = %v; want it ADMITTED — CPU is in neither node inequality (resource admission), so gating it can only yield a false 507", err)
		}
		rec, _ := st.GetSandbox(ctx, sid)
		if rec.Limits.CPUMillis != cpu {
			t.Fatalf("cpu = %d; want the raised %d persisted", rec.Limits.CPUMillis, cpu)
		}
		// and the axes the gate DOES own are untouched by the CPU edit.
		if rec.Limits.DiskByte != gib || rec.Limits.MemoryByte != gib {
			t.Fatalf("limits = %+v; want disk=%d memory=%d unchanged", rec.Limits, gib, gib)
		}
	})

	// `retention` is measured from UpdatedAt (health and reclamation), so PATCH must not bump it —
	// otherwise editing an unrelated cap silently extends an eviction deadline.
	t.Run("PATCH does not touch UpdatedAt", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 64 << 20}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		before, _ := st.GetSandbox(ctx, sid)
		v := int64(128 << 20)
		if err := patchBinding(m, ctx, sid, store.Binding{DiskByte: &v}); err != nil {
			t.Fatalf("update: %v", err)
		}
		after, _ := st.GetSandbox(ctx, sid)
		if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("UpdatedAt moved (%s → %s): a cap edit would silently extend the eviction deadline (health and reclamation)",
				before.UpdatedAt, after.UpdatedAt)
		}
	})

	t.Run("empty binding is a no-op", func(t *testing.T) {
		rp, rt := &fakeRootfs{}, &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{})
		sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := patchBinding(m, ctx, sid, store.Binding{}); err != nil {
			t.Fatalf("empty PATCH: %v", err)
		}
	})
}
