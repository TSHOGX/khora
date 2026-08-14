//go:build substrate

// L3 acceptance: a Volume as a first-class object with a lifecycle
// DECOUPLED from every sandbox — created before one exists, surviving after one is
// destroyed, its content readable and writable while nothing has it mounted.
// Requires root (storage units + quota ioctls) and a REAL btrfs state dir.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -p 1 -timeout 600s ./test/ -run Volume
//
// Each assertion states the property it proves inline. ONE DISCIPLINE UP FRONT
// (§5.C / volume.md D7.2): never assert "the sandbox is still alive". Measured, five
// separate ways of yanking a volume out from under a live guest killed NO sandbox —
// the container stayed running, a fresh exec returned 0, the runtime reported
// nothing. So that assertion is vacuously true, can never fail, and is worthless as
// a regression guard. What has to be asserted is the properties that would pass
// SILENTLY when broken.
package test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/orchestrator"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/test/harness"
)

// volumeEngine builds an engine whose state — and therefore its volume storage —
// lives on REAL btrfs. t.TempDir() is unusable: TMPDIR is /tmp = tmpfs here, where
// the storage units a capped volume needs cannot exist. Production has the same
// constraint (ADR-0002), so a tmpfs run would exercise a configuration the engine
// does not support.
func volumeEngine(t *testing.T, diskBudget int64) (*orchestrator.Engine, string) {
	t.Helper()
	base, stateDir := harness.BtrfsBase(t, "vol-e2e-", "per-volume caps need btrfs", cleanupUnitsAndOverlays)
	eng, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir:       base,
		HealthTimeout: 60 * time.Second,
		NodeDiskByte:  diskBudget,
	})
	return eng, stateDir
}

// cleanupUnitsAndOverlays and cleanupOverlaysOnly adapt the two cleanups below to
// harness.BtrfsBase's teardown callback. Two named funcs rather than closures at each
// call site because WHICH of the two a suite needs is a real distinction — a suite that
// creates no volume has no units to reclaim — and naming it keeps that visible.
func cleanupUnitsAndOverlays(stateDir string) {
	bc := btrfs.Client{}
	cleanupVolumeUnits(bc, stateDir)
	cleanupOverlaySubvolumes(bc, stateDir)
}

func cleanupOverlaysOnly(stateDir string) {
	cleanupOverlaySubvolumes(btrfs.Client{}, stateDir)
}

// cleanupVolumeUnits removes the volume storage units a run leaves behind. Needed
// because os.RemoveAll CANNOT delete a storage unit (rmdir(2) refuses one), so
// without this the scratch dir survives and the next run cannot tell "the engine
// leaked" from "my fixtures leaked".
func cleanupVolumeUnits(bc btrfs.Client, stateDir string) {
	ctx := context.Background()
	root := filepath.Join(stateDir, "volumes")
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if !bc.IsSubvolume(ctx, p) {
			_ = os.RemoveAll(p)
			continue
		}
		inner, _ := os.ReadDir(p)
		for _, ie := range inner {
			_ = os.RemoveAll(filepath.Join(p, ie.Name()))
		}
		_ = bc.DeleteSubvolume(ctx, p)
	}
}

// volumeUnitPath is a test-only peek past the §5.C seam (which keeps host paths
// private) so the assertions can inspect real storage. Deriving it here rather than
// asking the backend is deliberate: it means the test knows the layout
// independently, so a backend that silently stopped materializing anything would be
// caught rather than agreeing with the test.
func volumeUnitPath(stateDir string, id model.VolumeID) string {
	return filepath.Join(stateDir, "volumes", string(id))
}

// TestVolumeOutlivesItsSandbox pins the LIFECYCLE claim alone (§5.C: a volume is
// created before a sandbox and survives after one is destroyed, with its content
// byte-for-byte intact). Nothing is bound into the sandbox here on purpose — the
// mounted variant of the same claim is
// TestVolumeMountTwoConcurrentWritersBothReallyWrite and its neighbours in
// volume_mount_substrate_test.go.
func TestVolumeOutlivesItsSandbox(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := volumeEngine(t, 0)

	// ① The volume exists BEFORE any sandbox does.
	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	payload := bytes.Repeat([]byte("outlives-"), 32<<10) // 288 KiB
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "data/blob", bytes.NewReader(payload)); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	if _, err := os.Stat(volumeUnitPath(stateDir, vid)); err != nil {
		t.Fatalf("volume storage absent after create: %v", err)
	}

	// ② A sandbox comes and goes.
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := eng.Mediator.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy sandbox: %v", err)
	}
	if _, err := eng.Store.GetSandbox(ctx, sid); err != store.ErrNotFound {
		t.Fatalf("sandbox record survived destroy: %v", err)
	}

	// ③ The volume is still there, and its bytes are UNCHANGED. Byte-for-byte, not
	// "still readable": a destroy path that truncated or partially reclaimed the
	// volume would pass a mere existence check.
	var out bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "data/blob", &out); err != nil {
		t.Fatalf("export after the sandbox was destroyed: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("volume content changed across the sandbox's whole lifetime: %d bytes back, %d seeded", out.Len(), len(payload))
	}
	if _, err := eng.Store.GetVolume(ctx, vid); err != nil {
		t.Fatalf("volume record lost when the sandbox was destroyed: %v", err)
	}
}

// TestVolumeSweepReclaimsOnlyOrphans covers both the keep and the reclaim side in one
// pass.
//
// The POSITIVE control is what makes it a real test: asserting only "the live volume
// survived" would pass identically for a sweep that does NOTHING, so the same call
// must also reclaim a true orphan. The enumeration must also be
// per-directory rather than filesystem-wide — measured, a filesystem-level unit
// enumeration returns byte-identical results regardless of the directory given,
// including OTHER SOFTWARE's units on the node — so a sibling tree and an external
// unit under the enumeration root are checked untouched.
func TestVolumeSweepReclaimsOnlyOrphans(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := volumeEngine(t, 0)
	bc := btrfs.Client{}

	// A live volume, with content, so a mis-sweep is visible as data loss and not
	// only as a missing directory.
	live, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create live volume: %v", err)
	}
	const marker = "LIVE-VOLUME-CONTENT"
	if _, err := eng.Mediator.ImportVolume(ctx, live, "keep", strings.NewReader(marker)); err != nil {
		t.Fatalf("seed live volume: %v", err)
	}

	// A TRUE orphan: bytes with no row. Produced the way a crash produces one —
	// create the volume, then delete only its record.
	orphan, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create orphan volume: %v", err)
	}
	if err := eng.Store.DeleteVolume(ctx, orphan); err != nil {
		t.Fatalf("drop orphan row: %v", err)
	}
	orphanPath := volumeUnitPath(stateDir, orphan)
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("orphan storage missing before the sweep: %v", err)
	}

	// A sandbox, so the SIBLING tree (§5.B overlay units) is populated: the volume
	// sweep must not touch it.
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })
	overlayRoot := filepath.Join(stateDir, "overlays")
	siblingsBefore, err := os.ReadDir(overlayRoot)
	if err != nil {
		t.Fatalf("read sibling overlay root: %v", err)
	}
	if len(siblingsBefore) == 0 {
		t.Fatal("no sibling overlay units exist, so the sibling-tree assertion would pass vacuously")
	}

	// An EXTERNAL storage unit directly under the enumeration root — the shape a
	// filesystem-wide enumeration would wrongly claim. It has no `volumes` row, so a
	// sweep keyed on "no row ⇒ orphan" WILL delete it; that is correct and expected
	// (the root belongs to the engine). What must not happen is the volume sweep
	// reaching OUTSIDE its own root, which is what the sibling and the foreign unit
	// below check.
	foreignParent, err := os.MkdirTemp(filepath.Dir(stateDir), "foreign-")
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(foreignParent, "not-ours")
	if err := bc.CreateSubvolume(ctx, foreign); err != nil {
		t.Fatalf("create foreign unit: %v", err)
	}
	t.Cleanup(func() {
		_ = bc.DeleteSubvolume(ctx, foreign)
		_ = os.RemoveAll(foreignParent)
	})
	if err := os.WriteFile(filepath.Join(foreign, "theirs"), []byte("OTHER-SOFTWARE"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The sweep.
	if err := eng.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// POSITIVE control: the orphan is gone. Without this, everything below passes for
	// a no-op sweep.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("the orphan volume survived the sweep (stat err = %v) — a sweep that reclaims nothing also passes every negative assertion here", err)
	}

	// The live volume kept its bytes.
	var out bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, live, "keep", &out); err != nil {
		t.Errorf("live volume content unreadable after the sweep: %v", err)
	} else if out.String() != marker {
		t.Errorf("live volume content = %q, want %q", out.String(), marker)
	}

	// The SIBLING tree must be untouched, unit for unit.
	siblingsAfter, err := os.ReadDir(overlayRoot)
	if err != nil {
		t.Fatalf("read sibling overlay root after sweep: %v", err)
	}
	if len(siblingsAfter) != len(siblingsBefore) {
		t.Errorf("the volume sweep changed the sibling overlay tree: %d units before, %d after — enumeration must be per-directory, never filesystem-wide (§5.C)",
			len(siblingsBefore), len(siblingsAfter))
	}

	// And the foreign unit OUTSIDE the enumeration root is untouched, contents included.
	got, err := os.ReadFile(filepath.Join(foreign, "theirs"))
	if err != nil {
		t.Errorf("the volume sweep disturbed a storage unit outside its own root: %v", err)
	} else if string(got) != "OTHER-SOFTWARE" {
		t.Errorf("foreign unit content changed to %q", got)
	}
}

// TestVolumeCapWritePointOnRealStorage pins the cap write point against the real
// substrate: a raise re-clears admission, and a lowering to at-or-below current usage
// is refused with the STRICT inequality.
//
// The `== usage` case is the one worth the substrate run. Measured, the substrate
// ACCEPTS that cap and stores it, and the next write then fails — so it means FULL
// rather than "exactly enough", and a `>=` check would hand the caller a 200 plus a
// volume that can never take another byte (volume.md D7.3).
func TestVolumeCapWritePointOnRealStorage(t *testing.T) {
	ctx := context.Background()
	// A budget large enough that the gate is not what refuses anything here except in
	// the explicit over-budget sub-case.
	eng, stateDir := volumeEngine(t, 512<<20)

	vid, err := eng.Mediator.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create capped volume: %v", err)
	}
	// The cap reached the STORAGE, not just the record. Read through the substrate
	// directly: the record agreeing with itself proves nothing, and a cap that was
	// recorded but never applied is silent and undetectable afterwards.
	bc := btrfs.Client{}
	_, limit, err := bc.Usage(ctx, volumeUnitPath(stateDir, vid))
	if err != nil {
		t.Fatalf("read quota on the volume unit: %v", err)
	}
	if limit != 64<<20 {
		t.Fatalf("cap on real storage = %d, want %d — a cap that is recorded but not applied cannot be detected after the fact (volume.md D7.3)", limit, 64<<20)
	}

	// Write ~8 MiB, so "current usage" is a real number rather than zero.
	const written = 8 << 20
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "blob", bytes.NewReader(bytes.Repeat([]byte("u"), written))); err != nil {
		t.Fatalf("write into the volume: %v", err)
	}
	info, err := eng.Mediator.GetVolume(ctx, vid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	// UsedByte is a POINTER so that "unreadable" and "genuinely 0" stay distinct on the
	// §6.1 surface. Here nil is a real failure rather than an absent key: the volume was
	// just written to on a quota-capable arm, and the strict-inequality check this test
	// goes on to exercise cannot run at all without the number.
	if info.UsedByte == nil {
		t.Fatalf("usage came back unreadable after writing %d bytes — the strict-inequality check has nothing to compare against, and the cap write point would refuse every edit (fail closed)", written)
	}
	used := *info.UsedByte
	if used < written/2 {
		t.Fatalf("usage reads %d after writing %d bytes — the strict-inequality check depends on this number", used, written)
	}
	t.Logf("usage after writing %d bytes: %d", written, used)

	// Lower to BELOW usage → refused.
	if err := eng.Mediator.SetVolumeCapacity(ctx, vid, used/2); err == nil {
		t.Errorf("lowering the cap below usage was accepted — the substrate accepts it too, and the result is an irreversibly wedged volume (§5.C / volume.md D7.3)")
	} else if !strings.Contains(err.Error(), "strictly greater") {
		t.Errorf("refusal did not name the strict-inequality criterion: %v", err)
	}

	// Lower to EXACTLY usage → also refused. This is the case the substrate would
	// happily accept.
	if err := eng.Mediator.SetVolumeCapacity(ctx, vid, used); err == nil {
		t.Errorf("a cap EQUAL to usage was accepted — measured, the substrate stores it and the next write gets ENOSPC, so it means FULL rather than exactly enough")
	}

	// The refusals left the storage alone: the cap is still what it was.
	_, limit, err = bc.Usage(ctx, volumeUnitPath(stateDir, vid))
	if err != nil {
		t.Fatalf("re-read quota: %v", err)
	}
	if limit != 64<<20 {
		t.Errorf("a REFUSED cap change still altered the storage: limit now %d, want %d — the refusal exists precisely to keep the wedge from being reachable", limit, 64<<20)
	}

	// A raise within budget is accepted and reaches the storage.
	if err := eng.Mediator.SetVolumeCapacity(ctx, vid, 128<<20); err != nil {
		t.Fatalf("raise within budget: %v", err)
	}
	if _, limit, err = bc.Usage(ctx, volumeUnitPath(stateDir, vid)); err != nil {
		t.Fatalf("read quota after raise: %v", err)
	}
	if limit != 128<<20 {
		t.Errorf("cap after raise = %d, want %d", limit, 128<<20)
	}

	// A raise PAST the node budget is refused by the gate (§3.4: a write to a cap
	// changes Σ at this commit, so a raise re-clears admission).
	if err := eng.Mediator.SetVolumeCapacity(ctx, vid, 4<<30); err == nil {
		t.Errorf("a raise past the node budget was admitted — §3.4 requires a raise to re-clear the gate")
	}

	// A lowering to strictly above usage is fine, and needs no gate (Σ shrinks).
	if err := eng.Mediator.SetVolumeCapacity(ctx, vid, used*2); err != nil {
		t.Errorf("lowering to strictly above usage: %v", err)
	}
}

// TestVolumeCapsCountAgainstTheNodeBudget pins Σ(volume caps) on real storage: a pile
// of volumes cannot fill the node while the gate refuses nothing, and a metered disk
// axis makes a volume cap mandatory.
//
// The failure it guards is IMMEDIATE oversell (§3.4): an uncapped volume contributes
// 0 to the left side of the inequality while free to consume all of the right, so
// omitting Σ(volume caps) does not undercount slightly — it makes the inequality
// describe a device other than the one being written to.
func TestVolumeCapsCountAgainstTheNodeBudget(t *testing.T) {
	ctx := context.Background()
	const budget = 200 << 20
	eng, _ := volumeEngine(t, budget)

	// A metered axis requires a cap: 400, not a silently substituted default.
	if _, err := eng.Mediator.CreateVolume(ctx, 0); err == nil {
		t.Errorf("an uncapped volume was admitted on a metered node — §3.4 requires the cap")
	} else if !strings.Contains(err.Error(), "cap is required") {
		t.Errorf("refusal did not name the missing cap: %v", err)
	}

	// Fill the budget with volumes alone.
	var admitted int
	for i := 0; i < 8; i++ {
		if _, err := eng.Mediator.CreateVolume(ctx, 40<<20); err != nil {
			t.Logf("volume %d refused (expected once the budget is spent): %v", i+1, err)
			break
		}
		admitted++
	}
	if admitted == 8 {
		t.Fatalf("all 8 × 40 MiB volumes were admitted against a %d-byte budget — Σ(volume caps) is not in the first term, which oversells the node on the spot (§3.4)", int64(budget))
	}
	t.Logf("admitted %d × 40 MiB volumes against a %d MiB budget", admitted, budget>>20)

	// And the refusal is the CAPACITY verdict, distinct from "you must declare a cap".
	_, err := eng.Mediator.CreateVolume(ctx, 40<<20)
	if err == nil {
		t.Fatal("a volume was admitted past the budget")
	}
	if !strings.Contains(err.Error(), "insufficient node capacity") {
		t.Errorf("over-budget refusal = %v; want the capacity verdict", err)
	}

	// A SANDBOX now has to see the volumes' caps too: they are like terms on one
	// left-hand side, not two independent budgets (§3.4, no third term).
	_, err = eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{DiskByte: 100 << 20},
	})
	if err == nil {
		t.Errorf("a 100 MiB sandbox was admitted while volumes already hold %d MiB of the %d MiB budget — volume caps and sandbox caps are like terms (§3.4)",
			int64(admitted)*40, int64(budget>>20))
	} else {
		t.Logf("sandbox correctly refused against volume-held budget: %v", err)
	}
}

// TestVolumeSeedAndReadBackWithoutAnySandbox pins the sandbox-free half of
// import → export: on a node that has NEVER had a sandbox, the bytes come back
// byte-for-byte.
//
// This is the SUBSTANCE of "the volume's lifecycle is decoupled from any sandbox"
// (§5.C) rather than the record-layer version of it: an object whose content can only
// move through a sandbox is only independent on paper. The other half — a sandbox
// mounting the seeded volume and seeing the same bytes — is
// TestVolumeRegimeSeedWithoutASandboxThenMountThenReadAfterDestroy.
func TestVolumeSeedAndReadBackWithoutAnySandbox(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := volumeEngine(t, 0)

	recs, err := eng.Store.ListSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("this node already has %d sandbox(es); the property under test is that none is needed", len(recs))
	}

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	// A payload big enough to cross several stream chunks in both directions, so the
	// streaming path is exercised rather than a single buffer.
	payload := make([]byte, 3<<20)
	for i := range payload {
		payload[i] = byte(i * 7 % 251)
	}
	n, err := eng.Mediator.ImportVolume(ctx, vid, "nested/dir/seed.bin", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("import wrote %d of %d bytes", n, len(payload))
	}
	var out bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "nested/dir/seed.bin", &out); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("round trip mismatch: %d bytes back, %d written", out.Len(), len(payload))
	}

	// The storage unit is on disk, and a SECOND export reads the same bytes — export is
	// repeatable, not a one-shot drain of the unit.
	if _, err := os.Stat(volumeUnitPath(stateDir, vid)); err != nil {
		t.Fatalf("volume storage missing: %v", err)
	}
	var again bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "nested/dir/seed.bin", &again); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if !bytes.Equal(again.Bytes(), payload) {
		t.Errorf("second export differs from the first")
	}
}

// TestVolumeSweepAtBootPrecedesStorageCreation pins the ORDERING §5.C calls
// load-bearing rather than housekeeping: the orphan-volume sweep runs before anything
// creates a storage unit.
//
// The hazard is specific (volume.md D7.1): a leaked quota row reserves no unit id, so
// a restart can hand that id — ALONG WITH ITS CAP — to a brand-new volume, which then
// meets ENOSPC on a limit nobody set for it, or is silently over-provisioned, with the
// engine unable to tell from the inside. Sweeping only on the periodic tick leaves a
// window in which exactly that inheritance happens.
func TestVolumeSweepAtBootPrecedesStorageCreation(t *testing.T) {
	ctx := context.Background()
	base, stateDir := harness.BtrfsBase(t, "vol-boot-", "the boot sweep acts on storage units", cleanupUnitsAndOverlays)
	bc := btrfs.Client{}
	storePath := filepath.Join(base, "store.db")

	// First engine: create a volume, then drop only its row — the residue a crash
	// between byte reclamation and the row delete leaves behind.
	eng1, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir: base, StorePath: storePath, HealthTimeout: 60 * time.Second,
	})
	vid, err := eng1.Mediator.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if err := eng1.Store.DeleteVolume(ctx, vid); err != nil {
		t.Fatalf("drop row: %v", err)
	}
	unit := volumeUnitPath(stateDir, vid)
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("orphan unit missing: %v", err)
	}

	// Second engine over the SAME store and storage — a process restart. harness.Engine
	// runs the boot sweep in production order, so the orphan must be gone by the time
	// it returns, WITHOUT any explicit sweep call from this test.
	eng2, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir: base, StorePath: storePath, HealthTimeout: 60 * time.Second,
	})
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("the orphan volume unit survived boot (stat err = %v) — the sweep must run BEFORE anything can create a storage unit, or a restart can hand its id and cap to a new volume (§5.C / volume.md D7.1)", err)
	}

	// The freshly booted engine can still create a capped volume, and its cap is its
	// own. This is the property the ordering protects: a new volume must not inherit
	// anything from a reclaimed one.
	fresh, err := eng2.Mediator.CreateVolume(ctx, 16<<20)
	if err != nil {
		t.Fatalf("create after boot sweep: %v", err)
	}
	_, limit, err := bc.Usage(ctx, volumeUnitPath(stateDir, fresh))
	if err != nil {
		t.Fatalf("read quota on the fresh volume: %v", err)
	}
	if limit != 16<<20 {
		t.Errorf("the fresh volume's cap is %d, want %d — an inherited cap is exactly the failure the boot-ordering prevents, and the engine cannot detect it from the inside", limit, 16<<20)
	}
}
