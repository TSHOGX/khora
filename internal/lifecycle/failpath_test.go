package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// Provider-failure paths across the cold-start / teardown chain (atomic transition rollback). The
// existing tests fail at Mount and Launch; these cover the rest of the call sites,
// because atomic transition rollback ("a failed transition returns atomically to its source state") is
// a claim about EVERY failure point, not just the two that happened to get tests
// first. The shape asserted each time: the control plane lands in a legal resting
// state, and nothing persistent is destroyed that should have survived.

//	CreateOverlay fails — the EARLIEST failure after the record insert. The record
//
// must be deleted (no `failed` state, no orphan `creating` row), and the runtime
// must never be touched: the chain had not reached .
func TestCreateUnwindOnOverlayAllocationFailure(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{failCreateOverlay: true}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err == nil {
		t.Fatal("Create should have failed at CreateOverlay")
	}
	if _, gerr := st.GetSandbox(ctx, sid); !errors.Is(gerr, store.ErrNotFound) {
		t.Fatalf("record survived a failed overlay allocation: %v; want it deleted (atomic transition rollback: no `failed` state, no orphan `creating` row)", gerr)
	}
	if len(rt.Calls()) != 0 {
		t.Fatalf("runtime was called after CreateOverlay failed: %v; want nothing because the chain never reached launch", rt.Calls())
	}
	// And no record of ANY kind is left behind for the reaper to trip over.
	if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
		t.Fatalf("%d record(s) left after a failed create; want 0", len(recs))
	}
}

func TestDestroyRetainsRecordUntilPlacementReleaseSucceeds(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{failUnmount: true}
	rt := &fakeRuntime{failDestroy: true}
	m, st := newTestMediator(t, rp, rt, true)
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(ctx, sid); err == nil {
		t.Fatal("destroy hid teardown failure")
	}
	if _, err := st.GetSandbox(ctx, sid); err != nil {
		t.Fatalf("cleanup authority was deleted: %v", err)
	}
	placement, err := st.GetPlacement(ctx, sid)
	if err != nil || placement.State != model.PlacementStopping {
		t.Fatalf("missing teardown fence: %+v %v", placement, err)
	}
	if _, err := m.Endpoint(sid); err == nil {
		t.Fatal("stopping placement remains addressable")
	}
	for _, kind := range kindsFor(t, st, sid) {
		if kind == "destroyed" {
			t.Fatal("published terminal cleanup before teardown")
		}
	}
	rt.failDestroy = false
	rp.failUnmount = false
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSandbox(ctx, sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retry failed to complete cleanup: %v", err)
	}
}

func TestPauseRetainsWriterFenceWhenPlacementReleaseFails(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{failUnmount: true}
	rt := &fakeRuntime{failStop: true}
	m, st := newTestMediator(t, rp, rt, true)
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, sid); err == nil {
		t.Fatal("pause hid stop failure")
	}
	rec, err := st.GetSandbox(ctx, sid)
	if err != nil || rec.State != model.Paused {
		t.Fatalf("desired pause lost: %+v %v", rec, err)
	}
	placement, err := st.GetPlacement(ctx, sid)
	if err != nil || placement.State != model.PlacementStopping {
		t.Fatalf("missing writer fence: %+v %v", placement, err)
	}
	if _, err := m.Endpoint(sid); err == nil {
		t.Fatal("stopping placement remains addressable")
	}
	if err := m.Resume(ctx, sid); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("resume crossed unresolved teardown: %v", err)
	}
	rt.failStop = false
	rp.failUnmount = false
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatal(err)
	}
}

// Promote is bytes-first (the Rootfs subsystem / health and reclamation existence-is-state): the layer bytes are
// written BEFORE the row is inserted, so a failure at the row insert must leave an
// orphan the sweep collects — never a `templates` row pointing at bytes that do
// not exist.
//
// The asymmetry is the point. existence-is-state has no not-ready escape hatch, so
// a row is a promise that the bytes are there; the reverse (bytes with no row) is
// merely garbage.
func TestPromoteRowInsertFailureLeavesNoBrokenReadyRow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	rp := &fakeRootfs{}
	m := New(Deps{
		Store: &rowInsertFailingStore{Store: st}, Runtime: &fakeRuntime{}, Rootfs: rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedPaused(t, st, "sbx_bf", "ov_bf", "bare")

	_, err := m.Promote(ctx, "sbx_bf")
	if err == nil {
		t.Fatal("Promote should have failed at the templates-row insert")
	}
	// The bytes WERE written (bytes-first) — that is what makes the failure an
	// orphan rather than a lost promote.
	if !contains(rp.Calls(), "PromoteToTemplate:ov_bf->"+lastPromotedID(rp.Calls())) {
		t.Fatalf("expected the layer bytes to have been frozen before the row insert; calls=%v", rp.Calls())
	}
	// No row exists for the frozen layer: nothing claims those bytes are ready.
	tmpls, _ := st.ListTemplates(ctx)
	for _, tm := range tmpls {
		if !tm.Builtin {
			t.Fatalf("a non-builtin templates row (%s) exists after the insert failed — a row is a promise the bytes are ready (health and reclamation existence-is-state)", tm.ID)
		}
	}
	// The keep-set is released, so the boot sweep will see the layer as a TRUE
	// orphan and reclaim it. A stuck keep-set entry would protect the garbage forever.
	if ids := m.buildingLayerIDs(); len(ids) != 0 {
		t.Fatalf("buildingLayers still holds %v after a failed promote; the orphan would be protected from the sweep forever (atomic transition rollback)", ids)
	}
}

// lastPromotedID recovers the new layer id from the fake's call log (Promote mints
// it internally, so the test cannot know it in advance).
func lastPromotedID(calls []string) string {
	for _, c := range calls {
		if i := strings.Index(c, "PromoteToTemplate:"); i == 0 {
			if j := strings.Index(c, "->"); j >= 0 {
				return c[j+2:]
			}
		}
	}
	return ""
}

// rowInsertFailingStore fails ONLY CreateTemplate for a non-builtin row, which is
// exactly the bytes-first window: the layer bytes are already down, and the row
// insert is what fails. Everything else delegates.
type rowInsertFailingStore struct {
	store.Store
}

func (s *rowInsertFailingStore) CreateTemplate(ctx context.Context, t model.Template) error {
	if !t.Builtin {
		return errors.New("injected templates-row insert failure")
	}
	return s.Store.CreateTemplate(ctx, t)
}

// atomic transition rollback adopt, the L1-alive-but-not-serving crack. `Adopt` reports the isolation
// unit alive, but the L2 Health probe does not answer (a `created`-but-never-
// listening container). Per health and reclamation the takeover judgement is aligned with the cold
// start's  gate: L1 alive but L2 not answering is NOT an adoptable survivor — so
// it demotes to `paused`, recoverable by a later resume.
//
// The subtle half, and the reason this needs its own test: the demotion must record NO
// death cause (why, at the `notServing` branch in adoptOrDemote). A stamped cause is
// durable evidence a later reader would trust.
func TestAdoptDemotesL1AliveButNotServingWithoutInventingADeathCause(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// L1 says ALIVE; L2 (Health) never answers.
	rt := &fakeRuntime{
		adoptAlive: map[string]bool{"run_mute": true},
		// A cause is available from the probe, to prove the mediator does not
		// opportunistically persist it for a unit L1 called alive.
		adoptCause: map[string]runtime.DeathCause{"run_mute": runtime.CauseGone},
	}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: false} },
		HealthTimeout: time.Second,
	})
	seedRec(t, st, "run_mute", "ov_mute", model.Running)

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rec, err := st.GetSandbox(ctx, "run_mute")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	// Demoted, not adopted: an L1-alive unit that does not serve is not adoptable.
	if rec.State != model.Paused {
		t.Fatalf("state = %s; want paused — an L1-alive unit that fails the L2 readiness probe is not adoptable, matching the cold-start gate", rec.State)
	}
	// NO invented cause. This is the assertion the branch exists for.
	if rec.DeathCause != model.CauseNone {
		t.Fatalf("death cause = %q for a unit L1 reported ALIVE; want none — nothing died, and stamping a cause is the guessing the provider security contract forbids (it also becomes durable evidence a later reader trusts)", rec.DeathCause)
	}
	// Still reported as an involuntary pause so a subscriber can tell why.
	if got := kindsFor(t, st, "run_mute"); len(got) == 0 || got[len(got)-1] != "crash-paused" {
		t.Fatalf("events = %v; want the last to be `crash-paused` (FS-first pause and resume involuntary pause)", got)
	}
	// No live handle recorded — a dead endpoint in the live-map would make the
	// sandbox look addressable while nothing answers.
	if _, err := m.Endpoint("run_mute"); err == nil {
		t.Fatal("a non-serving survivor was recorded in the live-map; destroy/transport would then target a dead endpoint")
	}
}

// The FS-first pause and resume drain barrier must be re-opened when a pause FAILS. pauseLocked
// registers `defer undrain` BEFORE the CAS for this reason: a pause that loses its
// CAS has not frozen anything, so leaving the sandbox marked draining would make
// it refuse every mutating fs RPC forever — a permanently read-only sandbox, with
// no state anywhere recording why.
func TestFailedPauseReopensTheDrainBarrier(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)
	// Already `paused`, so Pause's CAS (which requires `running`) must fail.
	seedPaused(t, st, "sbx_wedge", "ov_wedge", "bare")

	if err := m.Pause(ctx, "sbx_wedge"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Pause of a paused sandbox = %v; want ErrConflict", err)
	}
	// The barrier must be open again: a mutating op can enter.
	release, ok := m.Enter("sbx_wedge", "fs.Write /root/after-failed-pause")
	if !ok {
		t.Fatal("the drain barrier stayed CLOSED after a failed pause — the sandbox would refuse every mutating fs RPC forever (FS-first pause and resume / the lifecycle contract: undrain must run even when the CAS loses)")
	}
	release()
}
