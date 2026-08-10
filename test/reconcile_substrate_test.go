//go:build substrate

// L4 §4.4-A startup crash-reconciliation acceptance against REAL runsc. Each
// test drives the assembled engine to a crash mid-transition (the fault
// hook), then builds a SECOND engine over the SAME store file + state dir — a
// faithful process restart — and runs Reconcile. Requires root.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 300s ./test/ -run Reconcile
package test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/orchestrator"
	"warren/internal/store"
	"warren/test/harness"
)

// crashSentinel is what the fault hook panics with; recoverCrash swallows
// exactly it so the test can continue as the "restarted" process.
type crashSentinel struct{ id model.SandboxID }

func recoverCrash(t *testing.T) {
	if r := recover(); r != nil {
		if _, ok := r.(crashSentinel); ok {
			return
		}
		t.Fatalf("unexpected panic: %v", r)
	}
}

// reconcileFixture sets up templates + shared store/state paths for a crash pair.
type reconcileFixture struct {
	tmpl      string
	stateDir  string
	storePath string
}

func newReconcileFixture(t *testing.T) reconcileFixture {
	t.Helper()
	repo := harness.RepoRoot(t)
	base := t.TempDir()
	tmpl := filepath.Join(base, "templates")
	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)
	return reconcileFixture{
		tmpl:      tmpl,
		stateDir:  filepath.Join(base, "state"),
		storePath: filepath.Join(base, "store.db"),
	}
}

func (f reconcileFixture) engine(t *testing.T, fault func(model.SandboxID)) *orchestrator.Engine {
	t.Helper()
	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: f.tmpl, StateDir: f.stateDir, StorePath: f.storePath,
		HealthTimeout:       30 * time.Second,
		FaultAfterColdStart: fault,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	// boot seed each start (idempotent) so the "bare" builtin row is present
	// for the FK before any create — mirrors production per-boot seeding.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		t.Fatalf("seed builtin templates: %v", err)
	}
	return eng
}

// crash mid-resume (placement half/fully built, store still `paused`) →
// restart → reconcile reclaims the orphan placement, upper intact, record stays
// paused. Also covers the `creating` orphan (→ record deleted, placement
// reclaimed).
func TestReconcileOrphanReclaim(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	f := newReconcileFixture(t)
	nsBefore := harness.CountEngineNetns(t)

	// --- pre-crash engine: create → pause, then resume that CRASHES at the fault
	// point (placement fully built, store still paused).
	var sid model.SandboxID
	func() {
		eng := f.engine(t, nil)
		defer func() { _ = eng.Store.(*store.SQLite).Close() }()
		var err error
		sid, err = eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := eng.Mediator.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
	}()

	// resume with the fault hook armed → panics after placement built.
	upper := filepath.Join(f.stateDir, "overlays")
	func() {
		eng := f.engine(t, func(id model.SandboxID) { panic(crashSentinel{id}) })
		defer func() { _ = eng.Store.(*store.SQLite).Close() }()
		defer recoverCrash(t)
		_ = eng.Mediator.Resume(ctx, sid) // crashes mid-resume
		t.Fatal("resume should have crashed at the fault hook")
	}()

	// store must still read `paused` (§4.4-C: the commit never happened, no torn
	// state) and the orphan netns is leaked (defer couldn't run).
	func() {
		eng := f.engine(t, nil)
		defer func() { _ = eng.Store.(*store.SQLite).Close() }()
		rec, err := eng.Store.GetSandbox(ctx, sid)
		if err != nil || rec.State != model.Paused {
			t.Fatalf("pre-reconcile: state=%v err=%v; want paused", rec.State, err)
		}
		if harness.CountEngineNetns(t) <= nsBefore {
			t.Fatalf("expected a leaked orphan netns before reconcile (before=%d now=%d)", nsBefore, harness.CountEngineNetns(t))
		}

		// --- restart: reconcile.
		if err := eng.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		// record still paused, orphan reclaimed, upper intact.
		rec2, _ := eng.Store.GetSandbox(ctx, sid)
		if rec2.State != model.Paused {
			t.Fatalf("post-reconcile state=%s; want paused (§4.4-A)", rec2.State)
		}
		if harness.CountEngineNetns(t) != nsBefore {
			t.Fatalf("orphan netns not reclaimed: before=%d after=%d", nsBefore, harness.CountEngineNetns(t))
		}
		up := filepath.Join(upper, string(rec2.Overlay), "upper")
		if _, err := os.Stat(up); err != nil {
			t.Fatalf("upper reclaimed by reconcile (must persist, §4.4-A): %v", err)
		}
		t.Logf("✓ orphan placement reclaimed, record stays paused, upper intact")
	}()
}

// a `running` sandbox whose container SURVIVES a control-plane crash →
// restart → reconcile probes it alive → Adopt rebuilds the handle at the SAME
// IP (not a resume, no new epoch); the adopted sandbox is addressable again
// (destroy tears the live container down cleanly). Contrast: a `running` whose
// container is DEAD → CAS-demoted to paused.
func TestReconcileAdoptSurvivor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	f := newReconcileFixture(t)

	// --- pre-crash: create → running, capture its endpoint, then "crash" the
	// control plane WITHOUT stopping the container (just drop the engine handle).
	var sid model.SandboxID
	var epBefore string
	func() {
		eng := f.engine(t, nil)
		defer func() { _ = eng.Store.(*store.SQLite).Close() }()
		var err error
		sid, err = eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		epBefore, err = eng.Mediator.Endpoint(sid)
		if err != nil {
			t.Fatalf("endpoint: %v", err)
		}
		// NB: no Destroy — the container keeps running past this engine's death.
	}()

	// --- restart: reconcile must re-adopt the survivor.
	eng := f.engine(t, nil)
	t.Cleanup(func() {
		_ = eng.Mediator.Destroy(ctx, sid)
		_ = eng.Store.(*store.SQLite).Close()
	})
	if err := eng.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, _ := eng.Store.GetSandbox(ctx, sid)
	if rec.State != model.Running {
		t.Fatalf("survivor demoted: state=%s; want running (§4.4-A adopt)", rec.State)
	}
	epAfter, err := eng.Mediator.Endpoint(sid)
	if err != nil {
		t.Fatalf("adopted sandbox not addressable: %v", err)
	}
	if epAfter != epBefore {
		t.Fatalf("adopt changed IP %s → %s; want SAME (not a resume, §4.4-A)", epBefore, epAfter)
	}
	// addressable: a fresh client reaches the guest.
	cl, err := eng.Mediator.Client(sid)
	if err != nil {
		t.Fatalf("client after adopt: %v", err)
	}
	if _, err := cl.Health(ctx); err != nil {
		t.Fatalf("adopted endpoint health: %v", err)
	}
	t.Logf("✓ survivor re-adopted at same IP %s, addressable", epAfter)

	// destroy tears the live container down cleanly (no leak) — proves the handle
	// was truly rebuilt.
	if err := eng.Mediator.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy adopted: %v", err)
	}
	if _, err := eng.Store.GetSandbox(ctx, sid); err == nil {
		t.Fatal("record present after destroy of adopted sandbox")
	}
	t.Logf("✓ destroy of adopted sandbox clean (no leak)")
}

// after reconcile there is no engine residue, and a second reconcile is a
// no-op (idempotent); no `failed` intermediate state ever appears.
func TestReconcileNoResidueIdempotent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	f := newReconcileFixture(t)
	nsBefore := harness.CountEngineNetns(t)

	// create a `creating`-crash orphan: fault hook on CREATE leaves a fully built
	// placement while the store still reads `creating`.
	var sid model.SandboxID
	func() {
		eng := f.engine(t, func(id model.SandboxID) { sid = id; panic(crashSentinel{id}) })
		defer func() { _ = eng.Store.(*store.SQLite).Close() }()
		defer recoverCrash(t)
		_, _ = eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
		t.Fatal("create should have crashed at the fault hook")
	}()
	if harness.CountEngineNetns(t) <= nsBefore {
		t.Fatalf("expected a leaked orphan netns from crashed create")
	}

	eng := f.engine(t, nil)
	defer func() { _ = eng.Store.(*store.SQLite).Close() }()

	// pre-reconcile the record is `creating` (torn? no — §4.4-C keeps it clean).
	if rec, _ := eng.Store.GetSandbox(ctx, sid); rec.State != model.Creating {
		t.Fatalf("crashed-create record state=%s; want creating", rec.State)
	}

	if err := eng.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// creating → deleted; no residue.
	if _, err := eng.Store.GetSandbox(ctx, sid); err == nil {
		t.Fatal("creating record survived reconcile (should be destroyed)")
	}
	if harness.CountEngineNetns(t) != nsBefore {
		t.Fatalf("engine netns residue after reconcile: before=%d after=%d", nsBefore, harness.CountEngineNetns(t))
	}
	// no `failed` state anywhere.
	list, _ := eng.Store.ListSandboxes(ctx)
	for _, r := range list {
		if r.State != model.Running && r.State != model.Paused {
			t.Fatalf("record %s in non-resting state %s (no failed state allowed, §4.4)", r.ID, r.State)
		}
	}
	// second reconcile: no-op.
	if err := eng.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile 2 (idempotent): %v", err)
	}
	if harness.CountEngineNetns(t) != nsBefore {
		t.Fatalf("second reconcile changed netns count")
	}
	t.Logf("✓ no residue, no failed state, reconcile idempotent")
}
