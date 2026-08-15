package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// classifier truth table (health and reclamation health states) as pure logic against fakes:
// each health state from its L1 × L2 × activity inputs, plus the two "skip" cases
// (no live handle, uncertain L1).
//
// L1 / L2 here are the health and reclamation PROBE LAYERS (substrate liveness / transport Health),
// not a test-tier label.

// classifyFixture builds a mediator whose L2 (Health) and L1 (Adopt) verdicts
// are individually controllable, seeds a `running` record with the given
// idle_timeout, and (unless noHandle) installs a live-map handle so the
// classifier can resolve an endpoint.
type classifyFixture struct {
	m  *Mediator
	st *store.Memory
	rt *fakeRuntime
}

func newClassifyFixture(t *testing.T, id string, idleTimeout time.Duration, l2Healthy bool, idle time.Duration, adoptAlive bool, adoptErr error, noHandle bool) classifyFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st) // health and reclamation template FK
	rt := &fakeRuntime{
		adoptAlive: map[string]bool{id: adoptAlive},
	}
	if adoptErr != nil {
		rt.adoptErr = map[string]error{id: adoptErr}
	}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: l2Healthy, idle: idle} },
		HealthTimeout: time.Second,
	})
	// seed a running record with the desired TTL.
	now := time.Now()
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Creating, IdleTimeout: idleTimeout, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !noHandle {
		m.put(model.SandboxID(id), &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.1:9000"}})
	}
	return classifyFixture{m: m, st: st, rt: rt}
}

// classifyOf returns just the health state — these tests are the L1×L2×activity
// truth table. The death cause the classifier carries out ALONGSIDE the state
// (health and reclamation / death-cause propagation) is asserted in limits_test.go's
// TestDeathCauseSeparatesTheTwoParallelDeadHealthStates, which is where the cause
// actually matters.
func classifyOf(t *testing.T, f classifyFixture, id string) Health {
	t.Helper()
	rec, err := f.st.GetSandbox(context.Background(), model.SandboxID(id))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := f.m.classify(context.Background(), rec)
	return h
}

func TestClassifyHealthy(t *testing.T) {
	// L1 alive + L2 up + recent I/O (idle 1s < timeout 10s) → healthy.
	f := newClassifyFixture(t, "sbx", 10*time.Second, true, 1*time.Second, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got != HealthHealthy {
		t.Fatalf("classify = %s; want healthy", got)
	}
}

func TestClassifyIdle(t *testing.T) {
	// L1 alive + L2 up + idle (30s ≥ timeout 10s) → idle.
	f := newClassifyFixture(t, "sbx", 10*time.Second, true, 30*time.Second, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got != HealthIdle {
		t.Fatalf("classify = %s; want idle", got)
	}
}

func TestClassifyTransportDown(t *testing.T) {
	// L2 down but L1 alive (Adopt alive) → transport-down (transient).
	f := newClassifyFixture(t, "sbx", 10*time.Second, false, 0, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got != HealthTransportDown {
		t.Fatalf("classify = %s; want transport-down", got)
	}
}

func TestClassifyDeadRecoverable(t *testing.T) {
	// L2 down AND L1 dead (Adopt alive=false) → dead-recoverable.
	f := newClassifyFixture(t, "sbx", 10*time.Second, false, 0, false, nil, false)
	if got := classifyOf(t, f, "sbx"); got != HealthDeadRecoverable {
		t.Fatalf("classify = %s; want dead-recoverable", got)
	}
}

// health classification unreadable-idle guard: when L2 is DOWN there is no trustworthy idle reading,
// so the sandbox must classify by L1 alone and NEVER into idle.
//
// WHAT THIS PINS, PRECISELY: `classify` reads idle only on the L2-up branch, so with
// L2 down the fake's idle value is never consulted and this test cannot fail
// independently of TestClassifyTransportDown / TestClassifyDeadRecoverable. Its value
// is the change it would catch — adding a persisted last-activity field to the record
// and consulting it when the probe fails. A 1ms idle_timeout on an unreachable sandbox
// would then classify idle and auto-pause a sandbox whose substrate may be dead.
func TestClassifyUnreadableIdleGuard(t *testing.T) {
	// L2 down, L1 alive → must be transport-down, NOT idle (idle_timeout tiny so
	// a naive classifier tempted to read stale activity would say idle).
	f := newClassifyFixture(t, "sbx", 1*time.Millisecond, false, 0, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got == HealthIdle {
		t.Fatalf("classify = idle on an L2-down sandbox (health classification violation: read idle from a failed probe)")
	} else if got != HealthTransportDown {
		t.Fatalf("classify = %s; want transport-down (L2 down + L1 alive)", got)
	}

	// L2 down, L1 dead → dead-recoverable, still never idle.
	f2 := newClassifyFixture(t, "sbx2", 1*time.Millisecond, false, 0, false, nil, false)
	if got := classifyOf(t, f2, "sbx2"); got == HealthIdle {
		t.Fatalf("classify = idle on a dead sandbox (health classification violation)")
	} else if got != HealthDeadRecoverable {
		t.Fatalf("classify = %s; want dead-recoverable", got)
	}
}

// uncertain L1 (Adopt errors: L1-alive but endpoint unrecoverable, atomic transition rollback) →
// Unknown, never a disposition (protect-on-uncertainty).
func TestClassifyUncertainL1IsUnknown(t *testing.T) {
	f := newClassifyFixture(t, "sbx", 10*time.Second, false, 0, false, errors.New("state probe transient"), false)
	if got := classifyOf(t, f, "sbx"); got != HealthUnknown {
		t.Fatalf("classify = %s; want unknown (uncertain L1 must not dispose)", got)
	}
}

// a store-`running` sandbox with NO live-map handle is reconcile's takeover
// concern (health classification), not a health state → Unknown (reaper skips it).
func TestClassifyNoLiveHandleIsUnknown(t *testing.T) {
	f := newClassifyFixture(t, "sbx", 10*time.Second, true, 1*time.Second, true, nil, true /*noHandle*/)
	if got := classifyOf(t, f, "sbx"); got != HealthUnknown {
		t.Fatalf("classify = %s; want unknown (no live handle → reconcile's concern)", got)
	}
}

// zero idle_timeout = opted out of auto-pause: never idle, however long quiet.
func TestClassifyZeroIdleTimeoutNeverIdle(t *testing.T) {
	f := newClassifyFixture(t, "sbx", 0, true, 100*time.Hour, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got != HealthHealthy {
		t.Fatalf("classify = %s; want healthy (zero idle_timeout opts out of idle)", got)
	}
}
