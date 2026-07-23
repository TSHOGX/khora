package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// SweepOrphanLayers is pure filesystem bookkeeping (no mount, no root), so it
// runs in the default unit suite: plant layer dirs, sweep, assert which survive.

func mkLayer(t *testing.T, layersDir, id string) string {
	t.Helper()
	dir := filepath.Join(layersDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc", "marker"), []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// §9: a layer dir with no live row and not building is an orphan → reclaimed; a
// live one is kept; an in-flight-Promote one (building) is protected even
// though it has no live row yet (bytes-first, §5.B).
func TestSweepOrphanLayers(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}

	liveDir := mkLayer(t, p.LayersDir, "tmpl_live")
	orphanDir := mkLayer(t, p.LayersDir, "tmpl_orphan")
	buildingDir := mkLayer(t, p.LayersDir, "tmpl_building")

	n, err := p.SweepOrphanLayers(ctx, []string{"tmpl_live"}, []string{"tmpl_building"})
	if err != nil {
		t.Fatalf("SweepOrphanLayers: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d; want 1 (only the orphan)", n)
	}
	if !exists(t, liveDir) {
		t.Error("live layer was reclaimed (has a live row → must be kept)")
	}
	if exists(t, orphanDir) {
		t.Error("orphan layer survived the sweep (no live row → must be reclaimed)")
	}
	if !exists(t, buildingDir) {
		t.Error("in-flight-Promote layer was reclaimed (building keep-set → must be protected)")
	}
}

// At boot the building keep-set is empty, so a crashed-Promote staging layer
// (bytes on disk, no row) is a TRUE orphan and is reclaimed (§9).
func TestSweepReclaimsCrashedPromoteStagingAtBoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	staging := mkLayer(t, p.LayersDir, "tmpl_crashed")

	// boot: no live rows, no building set.
	n, err := p.SweepOrphanLayers(ctx, nil, nil)
	if err != nil {
		t.Fatalf("SweepOrphanLayers: %v", err)
	}
	if n != 1 || exists(t, staging) {
		t.Fatalf("crashed-Promote staging layer not reclaimed at boot (n=%d exists=%v)", n, exists(t, staging))
	}
}

// A missing LayersDir → nothing to sweep, no error (defensive; NewHostOverlayFS
// creates it, but a manual removal must not crash the reaper).
func TestSweepMissingLayersDir(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	if err := os.RemoveAll(p.LayersDir); err != nil {
		t.Fatal(err)
	}
	n, err := p.SweepOrphanLayers(ctx, nil, nil)
	if err != nil || n != 0 {
		t.Fatalf("sweep of missing LayersDir = (%d, %v); want (0, nil)", n, err)
	}
}
