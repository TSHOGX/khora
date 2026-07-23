package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
)

// classifier truth table (§4.3 four quadrants) as pure logic against fakes:
// each quadrant from its L1 × L2 × activity inputs, plus the two "skip" cases
// (no live handle, uncertain L1). The D2 unreadable-idle guard gets its own test.

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
	seedBare(t, st) // §4.3 template FK
	rt := &fakeRuntime{
		adoptAlive: map[string]bool{id: adoptAlive},
	}
	if adoptErr != nil {
		rt.adoptErr = map[string]error{id: adoptErr}
	}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
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
	if err := st.CompareAndSetState(ctx, model.SandboxID(id), model.Creating, model.Running, nil); err != nil {
		t.Fatal(err)
	}
	if !noHandle {
		m.put(model.SandboxID(id), &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.1:9000"}})
	}
	return classifyFixture{m: m, st: st, rt: rt}
}

func classifyOf(t *testing.T, f classifyFixture, id string) Health {
	t.Helper()
	rec, err := f.st.GetSandbox(context.Background(), model.SandboxID(id))
	if err != nil {
		t.Fatal(err)
	}
	return f.m.classify(context.Background(), rec)
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

// D2 unreadable-idle guard: when L2 is DOWN, no idle_millis is available. The
// sandbox must classify by L1 (into transport-down / dead-recoverable) and
// NEVER into idle — even though the record's last-known activity might look
// stale. Structurally, idle is only read on the L2-up branch, so a down probe
// cannot yield idle regardless of any activity reading.
func TestClassifyUnreadableIdleGuard(t *testing.T) {
	// L2 down, L1 alive → must be transport-down, NOT idle (idle_timeout tiny so
	// a naive classifier tempted to read stale activity would say idle).
	f := newClassifyFixture(t, "sbx", 1*time.Millisecond, false, 0, true, nil, false)
	if got := classifyOf(t, f, "sbx"); got == HealthIdle {
		t.Fatalf("classify = idle on an L2-down sandbox (D2 violation: read idle from a failed probe)")
	} else if got != HealthTransportDown {
		t.Fatalf("classify = %s; want transport-down (L2 down + L1 alive)", got)
	}

	// L2 down, L1 dead → dead-recoverable, still never idle.
	f2 := newClassifyFixture(t, "sbx2", 1*time.Millisecond, false, 0, false, nil, false)
	if got := classifyOf(t, f2, "sbx2"); got == HealthIdle {
		t.Fatalf("classify = idle on a dead sandbox (D2 violation)")
	} else if got != HealthDeadRecoverable {
		t.Fatalf("classify = %s; want dead-recoverable", got)
	}
}

// uncertain L1 (Adopt errors: L1-alive but endpoint unrecoverable, §4.4-A) →
// Unknown, never a disposition (protect-on-uncertainty).
func TestClassifyUncertainL1IsUnknown(t *testing.T) {
	f := newClassifyFixture(t, "sbx", 10*time.Second, false, 0, false, errors.New("state probe transient"), false)
	if got := classifyOf(t, f, "sbx"); got != HealthUnknown {
		t.Fatalf("classify = %s; want unknown (uncertain L1 must not dispose)", got)
	}
}

// a store-`running` sandbox with NO live-map handle is reconcile's takeover
// concern (D4), not a health quadrant → Unknown (reaper skips it).
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
