package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
)

func newTestMediator(t *testing.T, rp *fakeRootfs, rt *fakeRuntime, healthy bool) (*Mediator, *store.Memory) {
	st := store.NewMemory()
	seedBare(t, st) // §4.3 template FK: the "bare" builtin row must pre-exist (boot seed)
	m := New(Deps{
		Store:         st,
		Runtime:       rt,
		Rootfs:        rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: healthy} },
		HealthTimeout: 500 * time.Millisecond,
	})
	return m, st
}

func TestCreateHappyPath(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := st.GetSandbox(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if rec.State != model.Running {
		t.Fatalf("state = %s; want running", rec.State)
	}
}

// TTL Binding materialization (§3.4 / §4.3): a create that OMITS idle_timeout /
// retention stores the engine-wide default; a create that provides them stores
// the given values verbatim (including an explicit zero). The stored record
// always carries a concrete duration the reaper reads — never a sentinel.
func TestCreateMaterializesTTLBinding(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st) // §4.3 template FK
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{},
		Volume:             newFakeVolume(),
		Dial:               func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout:      500 * time.Millisecond,
		DefaultIdleTimeout: 15 * time.Minute,
		DefaultRetention:   6 * time.Hour,
	})

	// omitted → engine default materialized.
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("Create (default TTL): %v", err)
	}
	rec, _ := st.GetSandbox(ctx, sid)
	if rec.IdleTimeout != 15*time.Minute || rec.Retention != 6*time.Hour {
		t.Fatalf("default TTL not materialized: idle=%s retention=%s; want 15m/6h", rec.IdleTimeout, rec.Retention)
	}

	// explicit values honored verbatim.
	idle, ret := 3*time.Minute, 90*time.Minute
	sid2, err := m.Create(ctx, CreateSpec{Template: "bare", IdleTimeout: &idle, Retention: &ret})
	if err != nil {
		t.Fatalf("Create (explicit TTL): %v", err)
	}
	rec2, _ := st.GetSandbox(ctx, sid2)
	if rec2.IdleTimeout != idle || rec2.Retention != ret {
		t.Fatalf("explicit TTL not honored: idle=%s retention=%s; want 3m/90m", rec2.IdleTimeout, rec2.Retention)
	}

	// explicit zero honored (NOT overridden by default): a caller opting a
	// sandbox out of a deadline.
	zero := time.Duration(0)
	sid3, err := m.Create(ctx, CreateSpec{Template: "bare", IdleTimeout: &zero})
	if err != nil {
		t.Fatalf("Create (zero idle): %v", err)
	}
	rec3, _ := st.GetSandbox(ctx, sid3)
	if rec3.IdleTimeout != 0 {
		t.Fatalf("explicit zero idle overridden: %s; want 0", rec3.IdleTimeout)
	}
	if rec3.Retention != 6*time.Hour {
		t.Fatalf("omitted retention not defaulted alongside explicit-zero idle: %s; want 6h", rec3.Retention)
	}
}

// create: cold-start failure reclaims half-built placement + empty Overlay
// and deletes the record → destroyed; no `failed` state.
func TestCreateUnwindOnLaunchFailure(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{failLaunch: true}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err == nil {
		t.Fatal("Create should have failed")
	}
	// record deleted (no failed state).
	if _, err := st.GetSandbox(context.Background(), sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record should be deleted, got %v", err)
	}
	// unwind reclaimed the half-built mount + destroyed the empty overlay.
	calls := strings.Join(rp.Calls(), ",")
	if !strings.Contains(calls, "Unmount:") {
		t.Errorf("mount not reclaimed on unwind; calls=%v", rp.Calls())
	}
	if !strings.Contains(calls, "DestroyOverlay:") {
		t.Errorf("empty overlay not destroyed on create unwind; calls=%v", rp.Calls())
	}
}

// create: mount failure is the earliest cold-start failure (① before ④), so the
// runtime is never launched; unwind still destroys the empty Overlay and deletes
// the record → destroyed, no `failed` state.
func TestCreateUnwindOnMountFailure(t *testing.T) {
	rp, rt := &fakeRootfs{failMount: true}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err == nil {
		t.Fatal("Create should have failed at mount")
	}
	// record deleted (no failed state).
	if _, err := st.GetSandbox(context.Background(), sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record should be deleted, got %v", err)
	}
	// mount failed before ④, so the runtime was never launched.
	if len(rt.Calls()) != 0 {
		t.Errorf("runtime should not be launched when mount fails; calls=%v", rt.Calls())
	}
	// the empty overlay is still reclaimed on unwind; nothing to unmount.
	calls := strings.Join(rp.Calls(), ",")
	if strings.Contains(calls, "Unmount:") {
		t.Errorf("nothing to unmount when mount never succeeded; calls=%v", rp.Calls())
	}
	if !strings.Contains(calls, "DestroyOverlay:") {
		t.Errorf("empty overlay not destroyed on mount-fail unwind; calls=%v", rp.Calls())
	}
}

// create ⑦: Health never succeeds → timeout → placement Destroyed, overlay
// reclaimed, record deleted.
func TestCreateUnwindOnHealthTimeout(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, false) // never healthy

	sid, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("Create should fail at health gate; got %v", err)
	}
	if _, err := st.GetSandbox(context.Background(), sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record should be deleted, got %v", err)
	}
	if !strings.Contains(strings.Join(rt.Calls(), ","), "Destroy") {
		t.Errorf("placement not destroyed on health-timeout unwind; calls=%v", rt.Calls())
	}
}

// resume: cold-start failure reclaims half-built placement but stays
// `paused` (persistent Overlay untouched — NOT destroyed).
func TestResumeUnwindStaysPaused(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{failLaunch: true}
	m, st := newTestMediator(t, rp, rt, true)
	ctx := context.Background()

	// seed a paused sandbox directly.
	seedPaused(t, st, "sbx_x", "ov_x", "bare")

	err := m.Resume(ctx, "sbx_x")
	if err == nil {
		t.Fatal("Resume should have failed")
	}
	rec, gerr := st.GetSandbox(ctx, "sbx_x")
	if gerr != nil {
		t.Fatalf("GetSandbox: %v", gerr)
	}
	if rec.State != model.Paused {
		t.Fatalf("state = %s; want paused (resume unwind keeps source state)", rec.State)
	}
	// resume unwind must NOT destroy the persistent Overlay.
	if strings.Contains(strings.Join(rp.Calls(), ","), "DestroyOverlay:") {
		t.Errorf("resume unwind destroyed the persistent Overlay; calls=%v", rp.Calls())
	}
}

// CAS precondition: pause requires running.
func TestPauseRequiresRunning(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)
	seedPaused(t, st, "sbx_p", "ov_p", "bare")

	err := m.Pause(context.Background(), "sbx_p")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pause of paused sandbox = %v; want ErrConflict", err)
	}
}

// Two concurrent resumes on a paused sandbox: exactly one wins → running, and
// the loser is cleanly rejected. The loser blocks on the transition lock, then
// fails Resume's READ-side precondition check (state is `running` by then, not
// `paused`) — which is why the rejection reads "want paused" rather than coming
// from the store's CAS. Both spellings wrap store.ErrConflict; the property is
// that the loser never proceeds and never corrupts the state (§4.4-B).
func TestResumeResumeCASShape(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)
	seedPaused(t, st, "sbx_r", "ov_r", "bare")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = m.Resume(context.Background(), "sbx_r") }(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case errors.Is(e, store.ErrConflict) || strings.Contains(e.Error(), "want paused"):
			conflicts++
		default:
			t.Fatalf("unexpected resume error: %v", e)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d; want exactly 1 each", wins, conflicts)
	}
	rec, _ := st.GetSandbox(context.Background(), "sbx_r")
	if rec.State != model.Running {
		t.Fatalf("final state = %s; want running", rec.State)
	}
}

// NOTE — the destroy-vs-resume lock race (§4.4-B) lives in
// stress_test.go's TestStressDestroyBlockedDuringResume, which is this same
// scenario run against BOTH store backends. It is deliberately not duplicated
// here.

// --- helpers -----------------------------------------------------------------

func seedPaused(t *testing.T, st *store.Memory, sid, oid, tmpl string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	// §4.3 template FK: ensure the referenced Template row exists first. Idempotent
	// across repeat calls with the same tmpl (ErrConflict is fine → ignore).
	_ = st.CreateTemplate(ctx, model.Template{ID: model.TemplateID(tmpl), Builtin: true, CreatedAt: now})
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(sid), Template: model.TemplateID(tmpl),
		Overlay: model.OverlayID(oid), State: model.Creating,
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// creating → running → paused to reach a legal paused record.
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Creating, model.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Running, model.Paused, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
