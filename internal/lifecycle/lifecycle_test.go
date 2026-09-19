package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/waitfor"
)

// The Deployment carries ONLY viaHostLocal: no second-hop uid, which is the state the
// via[] placement tests are about, plus the injected host-local predicate they need to
// reach it (see viaHostLocal). Tests that want a pinned uid build their own Deps.
func newTestMediator(t *testing.T, rp *fakeRootfs, rt *fakeRuntime, healthy bool) (*Mediator, *store.Memory) {
	st := store.NewMemory()
	seedBare(t, st) // health and reclamation template FK: the "bare" builtin row must pre-exist (boot seed)
	m := New(Deps{
		Store:         st,
		Runtime:       rt,
		Rootfs:        rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: healthy} },
		HealthTimeout: 500 * time.Millisecond,
		Deployment:    network.Deployment{HostLocal: viaHostLocal},
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

func TestSandboxCreateRetryReplaysBeforeDeploymentValidation(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	uid := 4711
	m, st := newTestMediator(t, rp, rt, true)
	m.deps.Deployment.SecondHopUID = &uid
	decl := viaDecl(t)
	key := model.IdempotencyKey("sandbox-deployment-replay")
	first, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key, Network: &decl})
	if err != nil {
		t.Fatalf("initial keyed create = %v", err)
	}
	// Simulate a restart under a deployment that no longer supplies the
	// second-hop uid. The durable object is still valid and must be replayable.
	m.deps.Deployment.SecondHopUID = nil
	replayed, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key, Network: &decl})
	if err != nil || replayed != first {
		t.Fatalf("same-key create after deployment change = %s, %v; want replay of %s", replayed, err, first)
	}
	// A changed request must conflict before the now-invalid deployment
	// declaration is evaluated.
	changed := CreateSpec{Template: "bare", IdempotencyKey: key, Network: &decl, Limits: model.Limits{MemoryByte: 1}}
	if _, err := m.Create(ctx, changed); model.ErrorClassOf(err) != model.ErrorIdempotencyConflict {
		t.Fatalf("changed keyed create after deployment change = %v; want idempotency_conflict", err)
	}
	if rec, err := st.GetSandbox(ctx, first); err != nil || rec.State != model.Running {
		t.Fatalf("replayed sandbox record = %+v, %v; want original running record", rec, err)
	}
}

func TestSandboxCreateValidationFailureTerminalizesIdempotentOperation(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true) // no second-hop uid: via[] is refused
	decl := viaDecl(t)
	key := model.IdempotencyKey("sandbox-invalid-deployment")
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key, Network: &decl}); err == nil {
		t.Fatal("invalid deployment declaration unexpectedly succeeded")
	}
	op, err := st.GetOperationByKey(ctx, "sandbox.create", key)
	if err != nil {
		t.Fatalf("read validation-failed operation: %v", err)
	}
	if op.State != model.OperationFailed || op.ErrorClass != model.ErrorInvalidArgument {
		t.Fatalf("validation-failed operation = state %q class %q; want failed/invalid_argument", op.State, op.ErrorClass)
	}
	if _, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key, Network: &decl}); model.ErrorClassOf(err) != model.ErrorInvalidArgument {
		t.Fatalf("same-key retry after validation failure = %v; want stable invalid_argument", err)
	}
}

func TestSandboxCreateRetryIgnoresChangedDefaultsWhenTTLWasOmitted(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)
	key := model.IdempotencyKey("sandbox-default-replay")
	first, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key})
	if err != nil {
		t.Fatalf("initial keyed create = %v", err)
	}
	firstRec, err := st.GetSandbox(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	// A restart may choose different defaults, but an omitted TTL means the
	// caller requested the deployment default, not the old materialized value.
	m.deps.DefaultIdleTimeout = 2 * time.Hour
	m.deps.DefaultRetention = 3 * time.Hour
	replayed, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key})
	if err != nil || replayed != first {
		t.Fatalf("same-key create after default change = %s, %v; want replay of %s", replayed, err, first)
	}
	got, err := st.GetSandbox(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdleTimeout != firstRec.IdleTimeout || got.Retention != firstRec.Retention {
		t.Fatalf("replayed sandbox binding changed: before=%s/%s after=%s/%s", firstRec.IdleTimeout, firstRec.Retention, got.IdleTimeout, got.Retention)
	}
}

func TestSandboxCreateSemanticHashIsStableForMountPermutations(t *testing.T) {
	a := CreateSpec{Template: "bare", Volumes: []model.Mount{
		{Volume: "vol_b", Path: "/z", Access: model.SharedReadOnly},
		{Volume: "vol_a", Path: "/a", Access: model.SharedReadWrite},
	}}
	b := CreateSpec{Template: a.Template, Volumes: []model.Mount{
		{Volume: "vol_a", Path: "/a/", Access: model.SharedReadWrite},
		{Volume: "vol_b", Path: "/z", Access: model.SharedReadOnly},
	}}
	hashA, err := model.RequestHash(semanticSandboxCreate(a, model.NetworkDecl{}))
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := model.RequestHash(semanticSandboxCreate(b, model.NetworkDecl{}))
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Fatalf("mount permutation hashes differ: %s vs %s", hashA, hashB)
	}
}

// TTL Binding materialization (resource admission / health and reclamation): a create that OMITS idle_timeout /
// retention stores the engine-wide default; a create that provides them stores
// the given values verbatim (including an explicit zero). The stored record
// always carries a concrete duration the reaper reads — never a sentinel.
func TestCreateMaterializesTTLBinding(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st) // health and reclamation template FK
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

func TestCreateRefusesPlacementWithoutAddress(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{invalidAddress: true}
	m, st := newTestMediator(t, rp, rt, true)

	if _, err := m.Create(ctx, CreateSpec{Template: "bare"}); err == nil {
		t.Fatal("Create succeeded with a placement that had no guest address")
	}
	recs, err := st.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("invalid provider output left %d sandbox record(s): %+v", len(recs), recs)
	}
	evs, err := st.UnpublishedEvents(ctx, allUnpublished)
	if err != nil {
		t.Fatalf("UnpublishedEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("invalid provider output published %d event(s): %+v", len(evs), evs)
	}
	if calls := strings.Join(rt.Calls(), ","); !strings.Contains(calls, "Destroy") {
		t.Fatalf("invalid placement was not destroyed; calls=%v", rt.Calls())
	}
}

// create: mount failure is the earliest cold-start failure ( before ), so the
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
	// mount failed before , so the runtime was never launched.
	if strings.Contains(strings.Join(rt.Calls(), ","), "Launch") {
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

// create : Health never succeeds → timeout → placement Destroyed, overlay
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
// that the loser never proceeds and never corrupts the state (per-sandbox transition serialization).
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

// NOTE — the destroy-vs-resume lock race (per-sandbox transition serialization) lives in
// stress_test.go's TestStressDestroyBlockedDuringResume, which is this same
// scenario run against BOTH store backends. It is deliberately not duplicated
// here.

// --- helpers -----------------------------------------------------------------

func seedPaused(t *testing.T, st *store.Memory, sid, oid, tmpl string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	// health and reclamation template FK: ensure the referenced Template row exists first. Idempotent
	// across repeat calls with the same tmpl (ErrConflict is fine → ignore).
	_ = st.CreateTemplate(ctx, model.Template{ID: model.TemplateID(tmpl), Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: now})
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
	waitfor.Must(t, "the condition", 2*time.Second, 5*time.Millisecond, cond)
}
