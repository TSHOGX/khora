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

func newTestMediator(rp *fakeRootfs, rt *fakeRuntime, healthy bool) (*Mediator, *store.Memory) {
	st := store.NewMemory()
	m := New(Deps{
		Store:         st,
		Runtime:       rt,
		Rootfs:        rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: healthy} },
		HealthTimeout: 500 * time.Millisecond,
	})
	return m, st
}

func TestCreateHappyPath(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(rp, rt, true)

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

// A9 (create): cold-start failure reclaims half-built placement + empty Overlay
// and deletes the record → destroyed; no `failed` state.
func TestCreateUnwindOnLaunchFailure(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{failLaunch: true}
	m, st := newTestMediator(rp, rt, true)

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

// A9 (create ⑦): Health never succeeds → timeout → placement Destroyed, overlay
// reclaimed, record deleted.
func TestCreateUnwindOnHealthTimeout(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(rp, rt, false) // never healthy

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

// A9 (resume): cold-start failure reclaims half-built placement but stays
// `paused` (persistent Overlay untouched — NOT destroyed).
func TestResumeUnwindStaysPaused(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{failLaunch: true}
	m, st := newTestMediator(rp, rt, true)
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
	m, st := newTestMediator(rp, rt, true)
	seedPaused(t, st, "sbx_p", "ov_p", "bare")

	err := m.Pause(context.Background(), "sbx_p")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pause of paused sandbox = %v; want ErrConflict", err)
	}
}

// A10 (CAS): two concurrent resumes on a paused sandbox — the loser re-does CAS
// against the already-changed state and is rejected (ErrConflict); exactly one
// wins → running.
func TestResumeResumeCASShape(t *testing.T) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(rp, rt, true)
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

// A10 (lock): resume holds the transition lock for its whole cold-start; a
// concurrent destroy CANNOT acquire the lock and therefore cannot delete the
// Overlay mid-cold-start (§4.4-B closes the destroy-vs-resume race).
func TestDestroyBlockedDuringResumeColdStart(t *testing.T) {
	rp := &fakeRootfs{}
	gate := make(chan struct{})
	rt := &fakeRuntime{launchGate: gate}
	m, st := newTestMediator(rp, rt, true)
	seedPaused(t, st, "sbx_lock", "ov_lock", "bare")

	// start resume; it will block inside Launch (holding the lock).
	resumeDone := make(chan error, 1)
	go func() { resumeDone <- m.Resume(context.Background(), "sbx_lock") }()

	// wait until resume is inside Launch (lock held).
	waitFor(t, func() bool { return contains(rt.Calls(), "Launch:sbx_lock") })

	// fire destroy; it must BLOCK on the lock (cannot delete the Overlay).
	destroyDone := make(chan error, 1)
	go func() { destroyDone <- m.Destroy(context.Background(), "sbx_lock") }()

	// give destroy a chance to (wrongly) proceed; assert it did NOT delete yet.
	time.Sleep(150 * time.Millisecond)
	if contains(rp.Calls(), "DestroyOverlay:ov_lock") {
		t.Fatal("destroy deleted the Overlay while resume held the lock (race!)")
	}
	select {
	case <-destroyDone:
		t.Fatal("destroy completed while resume held the lock")
	default:
	}

	// release Launch → resume completes → running; then destroy proceeds.
	close(gate)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-destroyDone; err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// now the Overlay is destroyed and record gone.
	if !contains(rp.Calls(), "DestroyOverlay:ov_lock") {
		t.Errorf("Overlay not destroyed after destroy ran; calls=%v", rp.Calls())
	}
	if _, err := st.GetSandbox(context.Background(), "sbx_lock"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record should be gone after destroy; got %v", err)
	}
}

// --- helpers -----------------------------------------------------------------

func seedPaused(t *testing.T, st *store.Memory, sid, oid, tmpl string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(sid), Template: model.TemplateID(tmpl),
		Overlay: model.OverlayID(oid), State: model.Creating,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// creating → running → paused to reach a legal paused record.
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Creating, model.Running); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Running, model.Paused); err != nil {
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
