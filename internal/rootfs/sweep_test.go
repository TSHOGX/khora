package rootfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"engine/internal/btrfs"
)

// SweepOrphanLayers is pure filesystem bookkeeping (no mount, no root), so it
// runs in the default unit suite: plant layer dirs, sweep, assert which survive.
//
// SweepOrphanOverlays joins it there for the UNCAPPED unit, which is a plain
// directory: its reclamation is a RemoveAll and needs neither root nor btrfs. The
// btrfs dependency is cut at the btrfs.Client.Runner seam (see noBtrfs) rather
// than by shelling out to a binary that may not exist, so the test asserts our
// logic and not the substrate's presence. The CAPPED (subvolume + quota) unit is
// inherently substrate-only and is covered by the L2 resource tests.

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

// noBtrfs is a btrfs.Client whose every query FAILS, which is exactly what an
// uncapped deployment looks like from the provider's side: IsSubvolume reports
// false, so a unit is a plain directory and its reclamation is a RemoveAll. Cuts
// the substrate dependency at the Runner seam rather than at a `btrfs` binary
// that may not be installed, so these tests exercise our logic either way.
func noBtrfs() btrfs.Client {
	return btrfs.Client{Run: func(_ context.Context, _ []string) ([]byte, error) {
		return nil, fmt.Errorf("btrfs unavailable (test stub)")
	}}
}

// mkUnit plants an uncapped Overlay storage unit (StateDir/<id> holding
// upper+work, D6.3) with a file in it, and returns the unit path.
func mkUnit(t *testing.T, p *HostOverlayFS, id string) string {
	t.Helper()
	for _, sub := range []string{p.upperPath(id), p.workPath(id)} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p.upperPath(id), "marker"), []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	return p.unitPath(id)
}

// §5.B / §4.4-A: a layer dir with no live row and not building is an orphan →
// reclaimed; a live one is kept; an in-flight-Promote one (building) is protected
// even though it has no live row yet (bytes-first, §5.B).
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
// (bytes on disk, no row) is a TRUE orphan and is reclaimed (§5.B / §4.4-A).
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

// --- SweepOrphanOverlays: the §4.4-A unit sweep (§4.4's licensed retry) ---

// The three properties in one pass, because they must hold SIMULTANEOUSLY — a
// sweep that reclaimed the orphan by reclaiming everything would pass three
// separate single-unit tests:
//
//	orphan  (no row, not in flight) → RECLAIMED. This is the leak §4.4 licenses
//	                                 and nothing collected: destroy dropped
//	                                 DestroyOverlay's error, then deleted the row.
//	live    (row present)           → KEPT. Reclaiming it would delete a running
//	                                 sandbox's writable layer.
//	building (create in flight)     → KEPT. Bytes are down but the row is not yet
//	                                 in the caller's `live` snapshot; the sweep
//	                                 must not tear it out from under the create.
func TestSweepOrphanOverlays(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	p.Btrfs = noBtrfs()

	liveUnit := mkUnit(t, p, "ov_live")
	orphanUnit := mkUnit(t, p, "ov_orphan")
	buildingUnit := mkUnit(t, p, "ov_building")

	n, err := p.SweepOrphanOverlays(ctx, []string{"ov_live"}, []string{"ov_building"})
	if err != nil {
		t.Fatalf("SweepOrphanOverlays: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d; want 1 (only the orphan)", n)
	}
	if !exists(t, liveUnit) {
		t.Error("live Overlay unit was reclaimed (has a live sandbox row → must be kept)")
	}
	if exists(t, orphanUnit) {
		t.Error("orphan Overlay unit survived the sweep (no live row → must be reclaimed; this is the §4.4 licensed retry)")
	}
	if !exists(t, buildingUnit) {
		t.Error("in-flight-create Overlay unit was reclaimed (building keep-set → must be protected)")
	}
}

// At boot the in-flight keep-set is empty, so a unit whose record is already gone
// is a TRUE orphan and is reclaimed — the exact post-deletion residue that had no
// collector (§4.4-A). Also pins that the uncapped path is genuinely
// btrfs-independent: with every btrfs query failing the unit still goes away.
func TestSweepOrphanOverlaysReclaimsPostDeletionResidueAtBoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	p.Btrfs = noBtrfs()

	residue := mkUnit(t, p, "ov_record_already_deleted")

	// boot: no rows, nothing in flight.
	n, err := p.SweepOrphanOverlays(ctx, nil, nil)
	if err != nil {
		t.Fatalf("SweepOrphanOverlays: %v", err)
	}
	if n != 1 || exists(t, residue) {
		t.Fatalf("post-deletion Overlay residue not reclaimed at boot (n=%d exists=%v)", n, exists(t, residue))
	}
}

// A live-set entry naming a unit that does not exist is not an error, and a
// missing StateDir → nothing to sweep (defensive: NewHostOverlayFS creates it,
// but a manual removal must not crash the reaper — same contract as the sibling).
func TestSweepOrphanOverlaysMissingStateDir(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	p.Btrfs = noBtrfs()

	if err := os.RemoveAll(p.StateDir); err != nil {
		t.Fatal(err)
	}
	n, err := p.SweepOrphanOverlays(ctx, []string{"ov_gone"}, nil)
	if err != nil || n != 0 {
		t.Fatalf("sweep of missing StateDir = (%d, %v); want (0, nil)", n, err)
	}
}
