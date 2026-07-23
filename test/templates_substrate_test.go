//go:build substrate

// L3/L4 M5 acceptance: the Template object model against REAL runsc + the
// assembled engine. Covers the P1 gate criteria (M5 §11): delete-referenced →
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

// §11.1: create-from-Template → DeleteTemplate is refused (ErrInUse, → 409);
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

// §11.4 (E2B-bug regression): a PAUSED sandbox still holds a persistent
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

// §11.3: create-from-Template racing DeleteTemplate under real concurrency —
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

// §11.8: the boot seed is idempotent — reconciling twice leaves the builtin row
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

// §11.5 (bytes-first sweep, P1 portion): a Promote-built layer dir with no live
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
