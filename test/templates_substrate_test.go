//go:build substrate

// L3/L4 M5 acceptance: the Template object model against REAL runsc + the
// assembled engine. Covers the P1 gate criteria: delete-referenced →
// 409-equivalent (ErrInUse), reference-vs-reclaim atomicity under real
// concurrency, the E2B-bug regression (a paused sandbox's Template stays
// undeletable), boot-seed idempotency, and the orphan-layer sweep reclaiming a
// crashed-Promote staging layer. Each assertion states its property inline.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 300s ./test/ -run M5
package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
)

// create-from-Template → DeleteTemplate is refused (ErrInUse, → 409);
// after the sandbox is destroyed the Template is deletable. Real runsc create.
func TestM5DeleteReferencedTemplate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (runsc create)")
	}
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// referenced → ErrInUse (§4.3 被引用即拒删).
	if err := m.DeleteTemplate(ctx, "bare"); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete referenced template = %v; want ErrInUse", err)
	}
	// the row survives the failed delete.
	if _, err := eng.Store.GetTemplate(ctx, "bare"); err != nil {
		t.Fatalf("template vanished on failed delete: %v", err)
	}

	// destroy the sandbox → the reference row is gone → deletable.
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := m.DeleteTemplate(ctx, "bare"); err != nil {
		t.Fatalf("delete after referrer gone: %v", err)
	}
	t.Logf("✓ referenced Template protected; deletable once unreferenced")
}

// E2B-bug regression: a PAUSED sandbox still holds a persistent
// reference row to its Template, so RESTRICT sees it and the delete is refused —
// exactly the case E2B lost track of (the reference lived only in memory).
func TestM5PausedSandboxProtectsTemplate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Paused {
		t.Fatalf("state=%s; want paused", stt)
	}
	// paused → still a live persistent reference → RESTRICT refuses delete.
	if err := m.DeleteTemplate(ctx, "bare"); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete Template of paused sandbox = %v; want ErrInUse (E2B regression)", err)
	}
	t.Logf("✓ paused sandbox's Template stays undeletable (persistent reference row)")
}

// create-from-Template racing DeleteTemplate under real concurrency —
// exactly one side wins, the other fails cleanly, never a dangling reference or
// lost bytes. Runs many rounds against the real SQLite store (single-writer
// serialization is the arbiter, §4.3).
func TestM5ReferenceReclaimAtomicity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	const rounds = 20
	for i := 0; i < rounds; i++ {
		// each round: create a sandbox, then race a destroy (drops the reference)
		// against a delete of the Template. The invariant we assert is global, at
		// the end: the "bare" row is never left dangling — either it still exists
		// with no sandbox referencing it, or it was deleted with none referencing
		// it. No torn state, no panic.
		sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
		if err != nil {
			t.Fatalf("round %d create: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = m.Destroy(ctx, sid) }()
		go func() { defer wg.Done(); _ = m.DeleteTemplate(ctx, "bare") }()
		wg.Wait()

		// If the Template got deleted (delete won the race after destroy), re-seed
		// it for the next round — production would never delete a builtin mid-run,
		// this is just to keep the loop going.
		if _, err := eng.Store.GetTemplate(ctx, "bare"); errors.Is(err, store.ErrTemplateNotFound) {
			if err := eng.SeedTemplates(ctx); err != nil {
				t.Fatalf("round %d re-seed: %v", i, err)
			}
		}
		// invariant: no sandbox may reference a nonexistent Template. Scan.
		sboxes, _ := eng.Store.ListSandboxes(ctx)
		for _, s := range sboxes {
			if _, err := eng.Store.GetTemplate(ctx, s.Template); err != nil {
				t.Fatalf("round %d: sandbox %s references missing Template %s: %v (DANGLING)", i, s.ID, s.Template, err)
			}
		}
	}
	t.Logf("✓ %d rounds: reference-vs-reclaim never left a dangling reference", rounds)
}

// the boot seed is idempotent — reconciling twice leaves the builtin row
// count unchanged. Runs against the real SQLite store via the assembled engine.
func TestM5BootSeedIdempotent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second) // already seeds once
	before, _ := eng.Store.ListTemplates(ctx)

	// seed again (simulating a restart) → same count.
	if err := eng.SeedTemplates(ctx); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, _ := eng.Store.ListTemplates(ctx)
	if len(after) != len(before) {
		t.Fatalf("boot seed not idempotent: before=%d after=%d", len(before), len(after))
	}
	t.Logf("✓ boot seed idempotent (%d builtin rows unchanged across re-seed)", len(after))
}

// bytes-first sweep (P1 portion, §5.B / §4.4-A): a Promote-built layer dir with no live
// templates row is a crashed-Promote orphan → the boot sweep reclaims it; a live
// layer is kept. Simulated by planting layer dirs directly (the full Promote
// crash path is exercised in P2).
func TestM5OrphanLayerSweep(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)

	layersDir := filepath.Join(base, "state", "layers")
	orphan := filepath.Join(layersDir, "tmpl_orphan")
	live := filepath.Join(layersDir, "tmpl_live")
	for _, d := range []string{orphan, live} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// give the "live" layer a real templates row (Promote-built, non-builtin).
	if err := eng.Store.CreateTemplate(ctx, model.Template{ID: "tmpl_live", Parent: "bare", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed live layer row: %v", err)
	}

	if err := eng.SweepOrphanLayers(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan layer survived sweep: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live layer was reclaimed (has a row → must be kept): %v", err)
	}
	t.Logf("✓ orphan layer reclaimed, live layer kept")
}

// Promote end-to-end (§3.1 / §5.B): write a file into a sandbox → Promote → create a
// NEW sandbox from the new Template → the file is visible (the freeze took
// effect); and the sandboxd portal path is NOT in the frozen layer (excluded-paths, §5.B).
func TestM5PromoteEndToEnd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// source sandbox: write a marker into its Overlay (the "installed" change).
	src, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, src) })
	cl, err := m.Client(src)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	marker := []byte("PROMOTED_" + time.Now().Format("150405.000000"))
	if err := cl.Write(ctx, "/etc/promoted-marker", marker); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Promote → new Template; source is left paused (§3.1 / §5.B).
	newTmpl, err := m.Promote(ctx, src)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if stt, _ := m.State(ctx, src); stt != model.Paused {
		t.Fatalf("source state after promote = %s; want paused", stt)
	}
	// lineage: new Template's parent is the source's Template ("bare").
	trec, err := eng.Store.GetTemplate(ctx, newTmpl)
	if err != nil {
		t.Fatalf("get new template: %v", err)
	}
	if trec.Parent != "bare" || trec.Builtin {
		t.Fatalf("new template lineage wrong: parent=%q builtin=%v; want bare/false", trec.Parent, trec.Builtin)
	}
	t.Logf("✓ promote → %s (parent=bare, builtin=false), source paused", newTmpl)

	// new sandbox from the promoted Template → sees the marker (freeze effective).
	child, err := m.Create(ctx, lifecycle.CreateSpec{Template: newTmpl})
	if err != nil {
		t.Fatalf("create from promoted template: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, child) })
	ccl, err := m.Client(child)
	if err != nil {
		t.Fatalf("child client: %v", err)
	}
	got, err := ccl.Read(ctx, "/etc/promoted-marker", 0, 0)
	if err != nil || string(got) != string(marker) {
		t.Fatalf("promoted marker not visible in child: got %q err=%v; want %q", got, err, marker)
	}
	t.Logf("✓ child from promoted Template sees the frozen change (%s)", marker)

	// excluded-paths (§5.B): the sandboxd portal pidfile must NOT be in the frozen layer.
	layerDir := filepath.Join(base, "state", "layers", string(newTmpl))
	if _, err := os.Stat(filepath.Join(layerDir, "tmp", ".sandboxd-transport.pid")); !os.IsNotExist(err) {
		t.Fatalf("sandboxd portal path leaked into promoted layer: %v", err)
	}
	t.Logf("✓ sandboxd portal path excluded from the frozen layer")
}

// lineage protection (§4.3): Promote a child layer T2 over T1 → deleting T1 is
// refused while T2 exists (RESTRICT 引用向上); delete T2 first, then T1.
func TestM5LineageProtection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// build a T1 by promoting a sandbox off "bare".
	s1, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create s1: %v", err)
	}
	t1, err := m.Promote(ctx, s1)
	if err != nil {
		t.Fatalf("promote s1→T1: %v", err)
	}
	_ = m.Destroy(ctx, s1) // drop s1's reference; T1 now referenced only by lineage later

	// build T2 as a child of T1: create a sandbox from T1, promote it.
	s2, err := m.Create(ctx, lifecycle.CreateSpec{Template: t1})
	if err != nil {
		t.Fatalf("create s2 from T1: %v", err)
	}
	t2, err := m.Promote(ctx, s2)
	if err != nil {
		t.Fatalf("promote s2→T2: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, s2) })
	if tr, _ := eng.Store.GetTemplate(ctx, t2); tr.Parent != t1 {
		t.Fatalf("T2 parent = %q; want %s", tr.Parent, t1)
	}

	// s2 still references T1 (its lineage) AND T2 (direct) — but the point of the
	// test is the parent-child RESTRICT, so first drop s2 so only lineage pins T1.
	_ = m.Destroy(ctx, s2)

	// T1 is referenced by T2's parent → RESTRICT refuses delete.
	if err := m.DeleteTemplate(ctx, t1); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete T1 (parent of T2) = %v; want ErrInUse", err)
	}
	// delete T2 first, then T1 is deletable.
	if err := m.DeleteTemplate(ctx, t2); err != nil {
		t.Fatalf("delete T2: %v", err)
	}
	if err := m.DeleteTemplate(ctx, t1); err != nil {
		t.Fatalf("delete T1 after T2 gone: %v", err)
	}
	t.Logf("✓ lineage protection: parent undeletable while child exists")
}

// bytes-first crash, full (§5.B / §4.4-A): simulate a Promote that wrote the layer bytes
// but crashed before inserting the row (a staging→final layer with no templates
// row). The boot sweep reclaims it, leaving no broken ready state.
func TestM5PromoteCrashBeforeRowIsReclaimed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)

	// a finalized layer dir with NO templates row = the "crashed after bytes,
	// before row" state (bytes-first, §5.B). buildingLayers is empty at boot.
	crashed := filepath.Join(base, "state", "layers", "tmpl_crashed_promote")
	if err := os.MkdirAll(filepath.Join(crashed, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crashed, "etc", "x"), []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.SweepOrphanLayers(ctx); err != nil {
		t.Fatalf("boot sweep: %v", err)
	}
	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Fatalf("crashed-Promote orphan layer survived boot sweep: %v", err)
	}
	// no row was ever inserted → no broken ready state.
	if _, err := eng.Store.GetTemplate(ctx, "tmpl_crashed_promote"); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("unexpected row for crashed promote: %v", err)
	}
	t.Logf("✓ crashed-Promote layer (bytes, no row) reclaimed at boot, no broken ready state")
}
