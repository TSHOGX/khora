package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

// TestSnapshotWritesTheCapInTheSameBackendCall is the unit-level guard for the failure
// the relevant internals page the documented invariant calls silent and undetectable afterwards.
//
// The substrate gives a snapshot a BRAND-NEW UNCAPPED quota row, so if the engine
// recorded size=N and left the cap to a follow-up, a writer could pour far more than N
// with zero errors and no reconcile could ever find the divergence (the limit read maps
// "never capped" and 0 onto each other). So the cap must cross the seam ON the operation
// that makes the bytes.
func TestSnapshotWritesTheCapInTheSameBackendCall(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	src, err := m.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatal(err)
	}

	const snapCap = 16 << 20
	snap, err := m.SnapshotVolume(ctx, src, snapCap)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The cap rode the SNAPSHOT call, not a later SetCapacity.
	var snapCall string
	for _, c := range vb.Calls() {
		if strings.HasPrefix(c, "Snapshot:") {
			snapCall = c
		}
		if strings.HasPrefix(c, "SetCapacity:"+string(snap)) {
			t.Errorf("the cap was applied by a SEPARATE SetCapacity call (%q) — it must ride the operation that creates the bytes, because a missed cap is silent and undetectable afterwards (the relevant internals page the documented invariant)", c)
		}
	}
	if snapCall == "" {
		t.Fatalf("the backend was never asked to snapshot: %v", vb.Calls())
	}
	if !strings.Contains(snapCall, fmt.Sprintf("cap=%d", snapCap)) {
		t.Errorf("the snapshot call did not carry the cap: %q", snapCall)
	}
	// The RECORD carries the declared cap and the lineage, and it is NOT the source's.
	rec, err := st.GetVolume(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CapByte != snapCap {
		t.Errorf("record cap = %d, want the DECLARED %d (the source's was %d — inheriting would silently double the node's declared claim at every snapshot)", rec.CapByte, snapCap, 64<<20)
	}
	if rec.Parent != src {
		t.Errorf("record parent = %q, want %q", rec.Parent, src)
	}
}

// TestSnapshotChecksCapabilityBeforeMovingBytes: the capability is asked BEFORE an ID is
// minted and before any bytes move (the relevant internals page the documented invariant). A late check leaves residue no sweep
// can classify, because the mediator allocates the ID.
func TestSnapshotChecksCapabilityBeforeMovingBytes(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	src, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := len(vb.Calls())
	vb.caps = volume.Capabilities{} // supports nothing

	if _, err := m.SnapshotVolume(ctx, src, 0); !errors.Is(err, ErrVolumeCapUnsupported) {
		t.Fatalf("snapshot on a backend without the capability = %v; want ErrVolumeCapUnsupported (→ 400)", err)
	}
	if got := vb.Calls(); len(got) != before {
		t.Errorf("the backend was called after the capability refusal: %v", got[before:])
	}
	vols, _ := st.ListVolumes(ctx)
	if len(vols) != 1 {
		t.Errorf("a refused snapshot left a record behind: %d volumes, want 1 (just the source)", len(vols))
	}
}

// TestSnapshotIsGatedByAdmission: the new volume's cap is a new promise, so it clears
// the the documented invariant gate — counted by its DECLARED cap even though the real bytes are
// metadata-level (~16 KiB measured).
func TestSnapshotIsGatedByAdmission(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	vb := newFakeVolume()
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
		NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
		Logf:         func(string, ...any) {},
	})
	src, err := m.CreateVolume(ctx, 60<<20)
	if err != nil {
		t.Fatal(err)
	}
	// 60 committed + 60 requested > 100.
	if _, err := m.SnapshotVolume(ctx, src, 60<<20); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("over-budget snapshot = %v; want ErrInsufficientCapacity — a snapshot's cap is a new declared promise (the documented invariant)", err)
	}
	// And `size` is REQUIRED while disk is metered, exactly as on create — the same rule
	// at its second endpoint, not a new one.
	if _, err := m.SnapshotVolume(ctx, src, 0); !errors.Is(err, ErrCapRequired) {
		t.Fatalf("uncapped snapshot on a metered node = %v; want ErrCapRequired", err)
	}
	// A snapshot that fits is admitted.
	if _, err := m.SnapshotVolume(ctx, src, 30<<20); err != nil {
		t.Errorf("in-budget snapshot: %v", err)
	}
}

// TestSnapshotLineageBlocksSourceDelete is the fourth FK path through the mediator, and
// the verdict must be the LINEAGE one rather than the mount one — the remedy differs.
func TestSnapshotLineageBlocksSourceDelete(t *testing.T) {
	ctx := context.Background()
	m, _, _ := volFixture(t)
	src, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.SnapshotVolume(ctx, src, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	err = m.DeleteVolume(ctx, src)
	if !errors.Is(err, store.ErrVolumeHasDescendants) {
		t.Fatalf("delete a source with a live snapshot = %v; want ErrVolumeHasDescendants", err)
	}
	if errors.Is(err, store.ErrVolumeInUse) {
		t.Errorf("the lineage verdict is indistinguishable from the mount verdict (the documented invariant: the remedy differs)")
	}
	// DESCENDANT FIRST, then the source — the order the FK forces, and the order measured
	// to leak no quota rows at all.
	if err := m.DeleteVolume(ctx, snap); err != nil {
		t.Fatalf("delete the snapshot: %v", err)
	}
	if err := m.DeleteVolume(ctx, src); err != nil {
		t.Errorf("delete the source after its descendant: %v", err)
	}
}

// TestRepairVolumeCapsReAssertsTheRecord is the the documented invariant repair-state pass: a volume whose
// record declares a cap while its storage carries none is a REPAIR state, not a legal
// one, and the direction is always RECORD → STORAGE.
//
// Adopting the storage's value into the record instead would silently RATIFY exactly the
// crash residue this exists to fix.
func TestRepairVolumeCapsReAssertsTheRecord(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	vid, err := m.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the crash residue: the record says capped, the storage says uncapped —
	// which is what a death between the snapshot ioctl and the cap write leaves.
	vb.mu.Lock()
	vb.appliedCaps[string(vid)] = 0
	vb.mu.Unlock()

	if err := m.RepairVolumeCaps(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	vb.mu.Lock()
	got := vb.appliedCaps[string(vid)]
	vb.mu.Unlock()
	if got != 32<<20 {
		t.Fatalf("after the repair pass the storage carries %d, want the RECORD's %d — this is the crash window between a snapshot and its cap write, and it is a repair state rather than a legal one (the relevant internals page the documented invariant)", got, 32<<20)
	}
	// The RECORD was not rewritten to match the storage: that direction would ratify the
	// residue.
	rec, _ := st.GetVolume(ctx, vid)
	if rec.CapByte != 32<<20 {
		t.Errorf("the repair changed the RECORD to %d — the record is the only authority (the documented invariant), so the storage is what moves", rec.CapByte)
	}

	// A DELIBERATELY uncapped volume is left alone: "record says 0, storage says 0" is
	// correct, not a divergence.
	unc, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RepairVolumeCaps(ctx); err != nil {
		t.Fatal(err)
	}
	vb.mu.Lock()
	uncCap := vb.appliedCaps[string(unc)]
	vb.mu.Unlock()
	if uncCap != 0 {
		t.Errorf("the repair pass imposed a cap of %d on a deliberately UNCAPPED volume", uncCap)
	}
}

// TestRepairVolumeCapsNeverImposesACapBelowUsage guards the one way the repair pass can
// be WORSE than doing nothing: re-asserting a recorded cap onto a unit that already holds
// more than that.
//
// Such a cap wedges the unit PERMANENTLY (measured, the relevant internals page the documented invariant), and this pass reaches
// the same backend call the documented invariant's write point guards, so without the check a recovery path is
// the way around ErrVolumeCapBelowUsage. RepairVolumeCaps's own comment has the measurement
// and why the input state is reachable rather than hypothetical.
//
// WHAT IS ASSERTED IS THAT IT SILENTLY PASSES, following the documented invariant's discipline: the property
// is "SetCapacity is not called", read from the backend's own call log, NOT "the volume
// still works". A wedge produces no errno on the control-plane side at all, so a liveness
// assertion would pass on the broken code.
func TestRepairVolumeCapsNeverImposesACapBelowUsage(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	const recordedCap = 32 << 20
	vid, err := m.CreateVolume(ctx, recordedCap)
	if err != nil {
		t.Fatal(err)
	}

	// The the documented invariant residue: the record promises a cap, the quota row carries none — and while
	// it carried none the writer poured past that cap.
	vb.mu.Lock()
	vb.appliedCaps[string(vid)] = 0
	vb.mu.Unlock()
	vb.used = recordedCap + (8 << 20)

	before := len(vb.Calls())
	if err := m.RepairVolumeCaps(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	for _, c := range vb.Calls()[before:] {
		if strings.HasPrefix(c, "SetCapacity:") {
			t.Fatalf("the repair pass called %q on a unit holding %d byte(s) — a cap at or below usage wedges the unit IRREVERSIBLY (afterwards even truncate-to-zero and the quota write itself return ENOSPC, so the cap can neither be removed nor raised), and this is the same predicate the documented invariant's write point refuses with ErrVolumeCapBelowUsage (the relevant internals page the documented invariant)", c, vb.used)
		}
	}
	// The RECORD is untouched either way: skipping is not "adopt the storage's value",
	// which would ratify the residue (the documented invariant — the record is the only authority).
	rec, err := st.GetVolume(ctx, vid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CapByte != recordedCap {
		t.Errorf("the skip rewrote the RECORD to %d, want the declared %d", rec.CapByte, recordedCap)
	}

	// EQUALITY IS ALSO SKIPPED — the write point's strict inequality, not >= (why, below).
	vb.used = recordedCap
	before = len(vb.Calls())
	if err := m.RepairVolumeCaps(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range vb.Calls()[before:] {
		if strings.HasPrefix(c, "SetCapacity:") {
			t.Fatalf("the repair pass called %q at cap == usage — that cap is accepted and stored but means FULL rather than exactly enough, so the boundary belongs on the skip side (the relevant internals page the documented invariant)", c)
		}
	}

	// AND THE PASS IS NOT DISABLED: once usage drops back under the recorded cap the very
	// next pass repairs the volume with no operator action — the skip is a deferral, not a
	// dead end (RepairVolumeCaps says why the interim uncapped state is acceptable).
	vb.used = 1 << 20
	if err := m.RepairVolumeCaps(ctx); err != nil {
		t.Fatal(err)
	}
	vb.mu.Lock()
	got := vb.appliedCaps[string(vid)]
	vb.mu.Unlock()
	if got != recordedCap {
		t.Errorf("after usage fell back under the cap the storage carries %d, want the RECORD's %d — the skip must be a deferral, not a permanent surrender of the cap", got, recordedCap)
	}
}

// TestReclaimQuotaRowsWiring: the mediator asks the backend for the ids it recorded at
// deletion time and hands them back to be reclaimed — never scanning for
// reclaimable-looking rows, whose failure mode is an uncapped live volume with zero
// signal (the documented invariant).
func TestReclaimQuotaRowsWiring(t *testing.T) {
	ctx := context.Background()
	m, _, vb := volFixture(t)
	vb.mu.Lock()
	vb.deletedRows[101] = true
	vb.deletedRows[102] = true
	vb.mu.Unlock()

	if err := m.ReclaimQuotaRows(ctx); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	var sawReclaim bool
	for _, c := range vb.Calls() {
		if strings.HasPrefix(c, "ReclaimQuotaRows:") {
			sawReclaim = true
			if !strings.Contains(c, "n=2") {
				t.Errorf("the reclamation round was handed %q; want the 2 recorded ids", c)
			}
		}
	}
	if !sawReclaim {
		t.Fatalf("the backend reclamation was never called: %v", vb.Calls())
	}
	if left := vb.DeletedRowIDs(ctx); len(left) != 0 {
		t.Errorf("%d recorded row(s) survived the round: %v", len(left), left)
	}
	// A round that finds nothing recorded does NOT call the backend — a backend with no
	// quota rows costs the sweep nothing (the documented invariant: no capability branch in the caller).
	before := len(vb.Calls())
	if err := m.ReclaimQuotaRows(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vb.Calls(); len(got) != before {
		t.Errorf("an empty reclamation round still called the backend: %v", got[before:])
	}
}

// TestSnapshotCrashWindowLeavesReclaimableBytes is the unit half of the snapshot crash window:
// a crash after the bytes exist but before the row is inserted leaves ORPHAN BYTES, and
// the cap is ALREADY WRITTEN because it rode the backend call rather than a follow-up
// the crash would have skipped.
func TestSnapshotCrashWindowLeavesReclaimableBytes(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	vb := newFakeVolume()
	var crashed model.VolumeID
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
		Logf: func(string, ...any) {},
		FaultAfterVolumeBytes: func(id model.VolumeID) {
			crashed = id
			panic("injected crash between the snapshot bytes and the row insert")
		},
	})
	src, err := m.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatal(err)
	}

	const snapCap = 16 << 20
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the injected crash did not fire")
			}
		}()
		_, _ = m.SnapshotVolume(ctx, src, snapCap)
	}()

	if crashed == "" {
		t.Fatal("no volume id was captured at the injection point")
	}
	// The BYTES exist...
	vb.mu.Lock()
	present := vb.units[string(crashed)]
	capOnStorage := vb.appliedCaps[string(crashed)]
	vb.mu.Unlock()
	if !present {
		t.Fatalf("the snapshot bytes do not exist at the crash point, so this window is not the one being tested")
	}
	// ...AND THE CAP IS ALREADY ON THEM. This is the property that makes the window
	// survivable: had the cap been left to a follow-up PATCH, the crash would have
	// skipped it and left a live UNCAPPED volume that nothing could detect afterwards.
	if capOnStorage != snapCap {
		t.Fatalf("the crashed snapshot's storage carries cap %d, want %d — the cap must be written by the SAME backend operation that made the bytes (the relevant internals page the documented invariant)", capOnStorage, snapCap)
	}
	// ...and NO row was inserted, so those bytes are an orphan the sweep reclaims.
	if _, err := st.GetVolume(ctx, crashed); err != store.ErrVolumeNotFound {
		t.Errorf("a row exists for the crashed snapshot (%v) — the insert is supposed to be after the injection point", err)
	}
	// A fresh boot's sweep reclaims them: the in-flight keep-set is empty after a
	// restart, so bytes with no row are a TRUE orphan.
	m2 := New(Deps{Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb, Logf: func(string, ...any) {}})
	if err := m2.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("post-crash sweep: %v", err)
	}
	var swept string
	for _, c := range vb.Calls() {
		if strings.HasPrefix(c, "SweepOrphanVolumes:") {
			swept = c
		}
	}
	if strings.Contains(swept, string(crashed)) {
		t.Errorf("the crashed snapshot was in the sweep's keep-set (%q) — after a restart it is a true orphan, since the in-flight set is empty", swept)
	}
}
