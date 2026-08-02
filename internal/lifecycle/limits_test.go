package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/rootfs"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
)

// L1 tests for the §3.4 resource-limit Binding and the §4.3 death-cause
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
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		NodeCapacity:  capacity,
	})
	return m, st
}

// M6 acceptance 3 (L1 half) — the caps are a BINDING: create materializes them
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

	// materialized into the record (§3.4: SoT is the control-plane record).
	rec, _ := st.GetSandbox(ctx, sid)
	if rec.Limits != want {
		t.Fatalf("record limits = %+v; want %+v", rec.Limits, want)
	}
	// ...and pushed across the §5.A seam on the create placement. The capacity axis
	// must NOT be in the runtime spec — it goes to the FS provider (§3.4).
	if got := rt.launchLimits[0]; got.CPUMillis != 500 || got.MemoryByte != 192<<20 {
		t.Fatalf("launch limits = %+v; want cpu=500 mem=%d", got, 192<<20)
	}
	if !contains(rp.Calls(), fmt.Sprintf("CreateOverlay:%s;lower=bare;cap=%d", rec.Overlay, 64<<20)) {
		t.Fatalf("capacity cap did not reach the FS provider at create: %v", rp.Calls())
	}

	// Now the real assertion. Pause, EDIT the stored caps, resume: a Binding
	// re-materializes from the current record, so the new values must appear at the
	// next placement. A frozen-value implementation would replay the old ones.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	edited := model.Limits{CPUMillis: 1500, MemoryByte: 512 << 20, DiskByte: 128 << 20}
	st.SetLimitsForTest(sid, edited)

	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := rt.launchLimits[len(rt.launchLimits)-1]; got.CPUMillis != 1500 || got.MemoryByte != 512<<20 {
		t.Fatalf("resume replayed stale limits %+v; want the EDITED cpu=1500 mem=%d (§3.4 Binding, not frozen state)", got, 512<<20)
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

// §4.3 — the two dead quadrants are parallel, not nested: identical observations
// (L1 dead, persistent state intact), OPPOSITE dispositions, separated only by the
// death cause. Both crash-pause; only resource-terminated refuses to resume.
func TestDeathCauseSeparatesTheTwoParallelDeadQuadrants(t *testing.T) {
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
		{"undeterminable → resume (bounded-retry fallback)", runtime.CauseUnknown, HealthDeadRecoverable, model.CauseUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := &fakeRootfs{}
			rt := &fakeRuntime{
				adoptAlive: map[string]bool{},
				adoptCause: map[string]runtime.DeathCause{},
			}
			st := store.NewMemory()
			seedBare(t, st)
			// L2 down + L1 dead → the classifier consults Adopt for the cause.
			m := New(Deps{
				Store: st, Runtime: rt, Rootfs: rp,
				Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: false} },
				HealthTimeout: 200 * time.Millisecond,
			})
			// place a running sandbox with a live handle so classify resolves an endpoint
			sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 192 << 20}})
			if err == nil {
				t.Fatal("create should fail its health gate with an unhealthy client")
			}
			_ = sid
			// Build the running state directly: create cannot pass the ⑦ gate with an
			// unhealthy transport, and what we are testing is the DEAD path.
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

			// classify must carry the cause OUT with the quadrant (D4c).
			rec, _ := st.GetSandbox(ctx, sid2)
			h, gotCause := m.classify(ctx, rec)
			if h != tc.wantHealth {
				t.Fatalf("health = %s; want %s", h, tc.wantHealth)
			}
			if gotCause != tc.cause {
				t.Fatalf("classify dropped the cause: got %v want %v (D4c: read-and-discard loses it forever)", gotCause, tc.cause)
			}

			// disposition: crash-pause, persisting the cause in the SAME commit.
			dc := deathCauseOf(gotCause)
			if err := m.pauseLocked(ctx, sid2, "crash-paused", &dc); err != nil {
				t.Fatalf("crash-pause: %v", err)
			}
			after, _ := st.GetSandbox(ctx, sid2)
			if after.State != model.Paused {
				t.Fatalf("state = %s; want paused (both quadrants crash-pause)", after.State)
			}
			if after.DeathCause != tc.wantStored {
				t.Fatalf("stored death cause = %q; want %q", after.DeathCause, tc.wantStored)
			}

			// the disposition that actually differs.
			err = m.Resume(ctx, sid2)
			if tc.wantResume && errors.Is(err, ErrResourceTerminated) {
				t.Fatalf("resume was refused for a %s death; want it allowed (§4.3 dead-recoverable resumes)", tc.cause)
			}
			if !tc.wantResume {
				if !errors.Is(err, ErrResourceTerminated) {
					t.Fatalf("resume err = %v; want ErrResourceTerminated (§4.3: resuming re-triggers the cap = crash loop)", err)
				}
				stayed, _ := st.GetSandbox(ctx, sid2)
				if stayed.State != model.Paused {
					t.Fatalf("state = %s; want it to STAY paused", stayed.State)
				}
			}
		})
	}
}

// §4.3 bounded self-correction: an Unknown death resumes, and a SUCCESSFUL resume
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
	// crash-pause it with an Unknown cause (the §4.3 fallback case).
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

// M6 acceptance 8 (L1 half) — the §3.4 admission gate. Asserts the FORMULA,
// including the term that is easy to omit: measured layer bytes, which no
// per-sandbox cap covers under squota (rootfs-overlay D6.5).
func TestAdmissionGateEnforcesTheAccountingFormula(t *testing.T) {
	ctx := context.Background()
	const gib = 1 << 30

	t.Run("layer bytes count against the node budget", func(t *testing.T) {
		rp := &fakeRootfs{measuredBytes: 6 * gib} // Template/layer bytes on disk
		rt := &fakeRuntime{}
		m, _ := newLimitsMediator(t, rp, rt, NodeCapacity{DiskByte: 10 * gib})

		// 6 GiB of layers + a 5 GiB cap = 11 GiB > 10 GiB → refused. Were layer bytes
		// omitted from the sum, this would wrongly be admitted at 5 GiB.
		_, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 5 * gib}})
		if !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("create err = %v; want ErrInsufficientCapacity (6 GiB layers + 5 GiB cap > 10 GiB)", err)
		}
		// and NOTHING was built: no overlay, no placement (§4.4-A no half-build).
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
		rp.measuredBytes = 6 * gib
		rp.mu.Unlock()
		if err := m.Resume(ctx, sid); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("resume err = %v; want ErrInsufficientCapacity", err)
		}
		// §4.4-A: atomic return to the SOURCE state.
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Paused {
			t.Fatalf("state = %s; want it to stay paused (§4.4-A)", rec.State)
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
		// a huge DISK request is unaffected: the axes are not fungible (§3.4).
		if _, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: 900 * gib}}); err != nil {
			t.Fatalf("disk request refused against a RAM-only budget: %v", err)
		}
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

// compile-time: the fake still satisfies the seams after the M6 signature change.
var (
	_ rootfs.Provider  = (*fakeRootfs)(nil)
	_ runtime.Provider = (*fakeRuntime)(nil)
)
