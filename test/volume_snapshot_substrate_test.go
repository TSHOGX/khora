//go:build substrate

// L3 acceptance: snapshot / clone against real storage, including the
// crash window and the quota-row leak. Requires root and real btrfs.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -p 1 -timeout 900s ./test/ -run VolumeSnapshot
package test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/waitfor"
	"github.com/TSHOGX/khora/test/harness"
)

// runCmd runs a host command and returns its combined output. Local to this file: the
// only thing it is used for is counting quota rows, which has no engine API by design
// (the engine locates rows by id, never by scanning — §5.C).
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// qgroupRowCount counts quota rows on the filesystem holding path — the number a leak
// moves. Used as a BASELINE COMPARISON rather than an absolute: this engine is not the
// only owner of the quota namespace on this box (measured, the dev filesystem carries
// external rows), so only the delta is meaningful.
func qgroupRowCount(t *testing.T, path string) int {
	t.Helper()
	bc := btrfs.Client{}
	mnt, err := bc.MountPoint(context.Background(), path)
	if err != nil {
		t.Fatalf("resolve mount point: %v", err)
	}
	// RETRIED, because `--sync` fails while a quota rescan is running and this suite's own
	// churn is what starts one: creating and deleting storage units in a loop leaves the
	// qgroup tree inconsistent, the kernel rescans, and `qgroup show --sync` exits 1 until
	// it finishes. Observed on this box (8799 rows on the dev filesystem, most of them
	// external and stale) as a one-off failure of the source-delete arm that passed on the
	// very next run.
	//
	// A t.Fatalf on the first error made a substrate hiccup indistinguishable from a quota
	// leak — the harness impersonating an engine defect, which is the one test bug this
	// suite cannot afford. The retry does not weaken the assertion: the caller compares two
	// counts, so a reading that never arrives fails below on its own.
	var out string
	var lastErr error
	ok := waitfor.Until(20*time.Second, 500*time.Millisecond, func() bool {
		out, lastErr = runCmd("btrfs", "qgroup", "show", "-re", "--raw", "--sync", mnt)
		return lastErr == nil
	})
	if !ok {
		t.Fatalf("qgroup show never succeeded within 20s (a rescan that never settles is a real "+
			"substrate problem, not a hiccup): %v", lastErr)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "0/") {
			n++
		}
	}
	return n
}

// TestVolumeSnapshotOfALiveWrittenVolume pins the live case: snapshotting a volume that
// a guest is actively writing must not disturb the writer, and the captured content must
// be a COMPLETE PREFIX of an append-only log.
//
// Crash-consistent is the promised level (§5.C) and it needs no fsync or freeze
// beforehand: sync makes no measurable difference, and freeze CANNOT COMBINE with a
// snapshot at all — creating a snapshot is itself a write to the filesystem just frozen,
// so it blocks uninterruptibly for the whole frozen window and lands only after thaw,
// capturing the post-thaw state. Zero consistency gain, and it wedges the guest.
func TestVolumeSnapshotOfALiveWrittenVolume(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })

	// An APPEND-ONLY LOG of monotonically increasing lines, written continuously. The
	// probe loop must outlast the whole run: a probe that ends on its own makes PID 1
	// exit and the container go `stopped`, which is INDISTINGUISHABLE from "this change
	// killed the sandbox".
	cl, err := eng.Mediator.Client(sid)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := cl.Start(ctx, transport.StartSpec{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "i=0; while [ $i -lt 100000 ]; do echo $i >> /data/log; i=$((i+1)); done; while true; do sleep 1; done"},
	})
	if err != nil {
		t.Fatalf("start the writer: %v", err)
	}
	defer func() {
		go func() {
			for range writer {
			}
		}()
	}()

	// Wait until the log is genuinely growing, so the snapshot lands mid-write rather
	// than before it starts.
	var lastLen int
	for i := 0; i < 200; i++ {
		out := guestSh(t, eng, sid, "n=0; while read L; do n=$((n+1)); done < /data/log; echo N:$n")
		if idx := strings.Index(out, "N:"); idx >= 0 {
			var n int
			_, _ = fmt.Sscanf(out[idx:], "N:%d", &n)
			if n > 50 && n > lastLen {
				break
			}
			lastLen = n
		}
	}

	// SNAPSHOT A LIVE, ACTIVELY WRITTEN VOLUME. No fsync, no freeze.
	const snapCap = 64 << 20
	snap, err := eng.Mediator.SnapshotVolume(ctx, vid, snapCap)
	if err != nil {
		t.Fatalf("snapshot a live volume: %v", err)
	}

	// The writer is UNDISTURBED: it keeps making progress after the snapshot.
	progressed := false
	beforeOut := guestSh(t, eng, sid, "n=0; while read L; do n=$((n+1)); done < /data/log; echo N:$n")
	for i := 0; i < 200; i++ {
		afterOut := guestSh(t, eng, sid, "n=0; while read L; do n=$((n+1)); done < /data/log; echo N:$n")
		if afterOut != beforeOut {
			progressed = true
			break
		}
	}
	if !progressed {
		t.Errorf("the guest's writes stopped making progress after the snapshot — a snapshot must not disturb the writer (§5.C, measured: zero write errors and unchanged throughput)")
	}

	// THE CAPTURE IS A COMPLETE PREFIX: every line 0..N-1 in order, with nothing missing
	// in the middle. That is exactly what crash-consistency should give, and a torn
	// capture would show up as a gap.
	var snapLog bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, snap, "log", &snapLog); err != nil {
		t.Fatalf("export the snapshot's log: %v", err)
	}
	lines := strings.Fields(snapLog.String())
	if len(lines) == 0 {
		t.Fatalf("the snapshot captured an empty log — it was taken before the writer produced anything, so this proves nothing")
	}
	for i, l := range lines {
		if l != fmt.Sprint(i) {
			t.Fatalf("the captured log is not a complete prefix: line %d is %q — a crash-consistent capture may be SHORT but must not have gaps", i, l)
		}
	}
	t.Logf("captured %d log lines as a complete prefix while the guest kept writing", len(lines))
}

// TestVolumeSnapshotCapIsEnforcedOnTheNewVolume is the one
// that catches the failure volume.md D7.3 calls silent and undetectable afterwards.
//
// The substrate gives a snapshot a BRAND-NEW UNCAPPED quota row (measured in four shapes
// and verified by writing 3× the source's cap into one), so an engine that recorded
// size=N and forgot to set it would let a writer exceed N with zero errors — and Usage's
// limit read cannot tell that apart from a deliberately uncapped volume.
func TestVolumeSnapshotCapIsEnforcedOnTheNewVolume(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)
	bc := btrfs.Client{}

	// A source with a LARGER cap, so an implementation that inherited would show up as
	// the wrong number rather than accidentally right.
	src, err := eng.Mediator.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := eng.Mediator.ImportVolume(ctx, src, "seed", strings.NewReader("SEEDED")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const snapCap = 8 << 20
	snap, err := eng.Mediator.SnapshotVolume(ctx, src, snapCap)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The cap is on the STORAGE, read through the substrate directly — the record
	// agreeing with itself proves nothing.
	_, limit, err := bc.Usage(ctx, volumeUnitPath(stateDir, snap))
	if err != nil {
		t.Fatalf("read the snapshot's quota row: %v", err)
	}
	if limit != snapCap {
		t.Fatalf("the snapshot's cap on storage = %d, want the DECLARED %d (the source had %d) — the substrate hands a snapshot an UNCAPPED row, so a missed cap here is invisible afterwards (volume.md D7.3)", limit, snapCap, 64<<20)
	}

	// AND IT IS ENFORCED: writing past the declared size fails. This is the behavioral
	// half — a cap that is recorded in the quota row but not enforced would pass the
	// check above.
	chunk := bytes.Repeat([]byte("z"), 1<<20)
	var landed int64
	var sawErr bool
	for i := 0; i < 32; i++ {
		n, err := eng.Mediator.ImportVolume(ctx, snap, fmt.Sprintf("fill%02d", i), bytes.NewReader(chunk))
		landed += n
		if err != nil {
			sawErr = true
			break
		}
	}
	if !sawErr {
		t.Fatalf("wrote %d bytes into an %d-byte snapshot with no error — the cap was declared but is NOT ENFORCED", landed, snapCap)
	}
	if landed > 2*snapCap {
		t.Errorf("landed %d bytes under an %d-byte cap — enforcement is far looser than the declared bound", landed, snapCap)
	}
	t.Logf("snapshot cap %d: writes failed after %d bytes landed", snapCap, landed)

	// The SOURCE is unaffected: the writer pays, in both directions (§5.C measured).
	if _, srcLimit, err := bc.Usage(ctx, volumeUnitPath(stateDir, src)); err != nil {
		t.Fatal(err)
	} else if srcLimit != 64<<20 {
		t.Errorf("the source's cap changed to %d, want %d", srcLimit, 64<<20)
	}
	var seed bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, src, "seed", &seed); err != nil {
		t.Errorf("the source's content is unreadable after filling its snapshot: %v", err)
	} else if seed.String() != "SEEDED" {
		t.Errorf("source content = %q", seed.String())
	}
}

// TestVolumeSnapshotSourceDeleteIsRefusedAndLeaksNoQuotaRow pins the lineage refusal: deleting
// a source with a live descendant is a 409, AND the filesystem's quota-row count returns
// to baseline once things are deleted in the order the FK forces.
//
// That is the point of the FK (volume.md D6.1a): a leak needs "delete the source while a
// live sharer references its extents", and RESTRICT makes that order UNREACHABLE through
// the API — so the leak is PREVENTED rather than reclaimed.
func TestVolumeSnapshotSourceDeleteIsRefusedAndLeaksNoQuotaRow(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)

	baseline := qgroupRowCount(t, stateDir)

	src, err := eng.Mediator.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Extents OWNED by the source: the leak predicate requires the deleted unit to own
	// extents, so an empty source would not exercise it (measured — a snapshotted but
	// EMPTY source does not leak).
	if _, err := eng.Mediator.ImportVolume(ctx, src, "data", bytes.NewReader(bytes.Repeat([]byte("o"), 4<<20))); err != nil {
		t.Fatalf("give the source extents: %v", err)
	}
	snap, err := eng.Mediator.SnapshotVolume(ctx, src, 32<<20)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// THE REFUSAL. Without it, this is exactly the deletion order that leaks.
	err = eng.Mediator.DeleteVolume(ctx, src)
	if err == nil {
		t.Fatal("deleting a source with a live snapshot SUCCEEDED — that is the order measured to leak a quota row, and RESTRICT is what makes it unreachable (volume.md D6.1a)")
	}
	if !strings.Contains(err.Error(), "snapshots or clones") {
		t.Errorf("the refusal does not name the lineage relationship: %v", err)
	}
	// And the refusal had NO side effect: the source is intact.
	var content bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, src, "data", &content); err != nil {
		t.Errorf("the refused delete damaged the source: %v", err)
	} else if content.Len() != 4<<20 {
		t.Errorf("source content is %d bytes, want %d", content.Len(), 4<<20)
	}

	// Now delete in the order the FK forces: DESCENDANT first, then the source.
	if err := eng.Mediator.DeleteVolume(ctx, snap); err != nil {
		t.Fatalf("delete the snapshot: %v", err)
	}
	if err := eng.Mediator.DeleteVolume(ctx, src); err != nil {
		t.Fatalf("delete the source after its descendant: %v", err)
	}
	// Give the substrate its bounded settle, then check the row count is back.
	if err := eng.ReclaimQuotaRows(ctx); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	after := qgroupRowCount(t, stateDir)
	if after > baseline {
		// Retry once through another reclamation round: the transition to reclaimable is
		// condition-triggered, so one round may legitimately be too early.
		if err := eng.ReclaimQuotaRows(ctx); err != nil {
			t.Fatalf("second reclaim: %v", err)
		}
		after = qgroupRowCount(t, stateDir)
	}
	if after > baseline {
		t.Errorf("quota rows went from %d to %d — deleting sharers BEFORE the source is measured to leak NOTHING, so a residue here means the order was not the one taken", baseline, after)
	} else {
		t.Logf("quota rows returned to baseline (%d → %d) after descendant-then-source deletion", baseline, after)
	}
}

// TestVolumeSnapshotCrashBetweenBytesAndRowIsRepaired pins the crash window: a crash in the
// load-bearing window leaves reclaimable bytes, and — the half that matters — the cap is
// ALREADY on them, because it rode the same backend operation rather than a follow-up the
// crash would have skipped.
//
// It also exercises the boot REPAIR pass: a volume whose record declares a cap while its
// storage carries none is a repair state, not a legal one, and the engine re-asserts the
// record's value.
func TestVolumeSnapshotCrashBetweenBytesAndRowIsRepaired(t *testing.T) {
	ctx := context.Background()
	base, stateDir := harness.BtrfsBase(t, "vol-crash-", "snapshot caps ride a btrfs operation", cleanupUnitsAndOverlays)
	bc := btrfs.Client{}
	storePath := filepath.Join(base, "store.db")

	// Engine 1, with the crash injected between the snapshot bytes and the row insert.
	var crashed model.VolumeID
	eng1, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir: base, StorePath: storePath, HealthTimeout: 60 * time.Second,
		FaultAfterVolumeBytes: func(id model.VolumeID) {
			crashed = id
			panic("injected crash between the snapshot bytes and the row insert")
		},
	})
	src, err := eng1.Mediator.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	const snapCap = 8 << 20
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the injected crash did not fire")
			}
		}()
		_, _ = eng1.Mediator.SnapshotVolume(ctx, src, snapCap)
	}()
	if crashed == "" {
		t.Fatal("no volume id captured at the injection point")
	}
	unit := volumeUnitPath(stateDir, crashed)
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("the snapshot bytes do not exist at the crash point: %v", err)
	}
	// THE CAP IS ALREADY ON THE STORAGE. Had it been left to a follow-up PATCH, this
	// crash would have skipped it and left a live UNCAPPED volume nothing could detect.
	_, limit, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("read the crashed snapshot's quota row: %v", err)
	}
	if limit != snapCap {
		t.Fatalf("the crashed snapshot's storage carries cap %d, want %d — the cap must ride the operation that makes the bytes (volume.md D7.3)", limit, snapCap)
	}
	// And NO row was inserted.
	if _, err := eng1.Store.GetVolume(ctx, crashed); err != store.ErrVolumeNotFound {
		t.Fatalf("a row exists for the crashed snapshot (%v)", err)
	}

	// A RESTART reclaims those bytes as a true orphan: the in-flight keep-set is empty
	// after a restart, so bytes with no row are unambiguous. harness.Engine runs the boot
	// sweep in production order, so no explicit call is needed here.
	eng2, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir: base, StorePath: storePath, HealthTimeout: 60 * time.Second,
	})
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("the crashed snapshot's bytes survived the restart (%v) — with no row and an empty in-flight set they are a true orphan", err)
	}

	// THE REPAIR PASS, on the other residue shape: a volume whose record declares a cap
	// while its storage carries none. Manufactured directly, because that is what a death
	// between the two substrate calls leaves if the cap write is ever moved out.
	victim, err := eng2.Mediator.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatalf("create the repair victim: %v", err)
	}
	if err := bc.SetQuotaLimit(ctx, volumeUnitPath(stateDir, victim), 0); err != nil {
		t.Fatalf("clear the cap behind the engine's back: %v", err)
	}
	if _, limit, _ := bc.Usage(ctx, volumeUnitPath(stateDir, victim)); limit != 0 {
		t.Fatalf("precondition: the cap should read as cleared, got %d", limit)
	}
	if err := eng2.RepairVolumeCaps(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	_, limit, err = bc.Usage(ctx, volumeUnitPath(stateDir, victim))
	if err != nil {
		t.Fatal(err)
	}
	if limit != 32<<20 {
		t.Fatalf("after the repair pass the storage carries %d, want the RECORD's %d — 'record capped, storage uncapped' is a REPAIR state, not a legal one (volume.md D7.3)", limit, 32<<20)
	}
	// And the repair moved the STORAGE, not the record: adopting the storage's value
	// would silently ratify the residue.
	rec, err := eng2.Store.GetVolume(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CapByte != 32<<20 {
		t.Errorf("the repair rewrote the RECORD to %d — the record is the only authority (§3.4), so the storage is what moves", rec.CapByte)
	}
}

// TestVolumeSnapshotConcurrentWithSweep guards the in-flight keep-set on the
// snapshot/clone path: those are two more producers of the "bytes exist, no row yet"
// window, so a concurrent sweep must not reclaim them.
func TestVolumeSnapshotConcurrentWithSweep(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)

	src, err := eng.Mediator.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := eng.Mediator.ImportVolume(ctx, src, "data", bytes.NewReader(bytes.Repeat([]byte("s"), 2<<20))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Snapshot repeatedly while sweeping repeatedly. Each snapshot passes through the
	// window, so overlapping many of them with many sweeps is what makes the race
	// reachable rather than hypothetical.
	var wg sync.WaitGroup
	const rounds = 6
	snaps := make(chan model.VolumeID, rounds)
	errs := make(chan error, rounds)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			id, err := eng.Mediator.SnapshotVolume(ctx, src, 32<<20)
			if err != nil {
				errs <- err
				return
			}
			snaps <- id
		}
		close(snaps)
		close(errs)
	}()
	sweeps := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	for {
		select {
		case <-done:
			select {
			case err := <-errs:
				if err != nil {
					t.Fatalf("a snapshot failed during concurrent sweeps: %v", err)
				}
			default:
			}
			t.Logf("ran %d sweeps across %d snapshots", sweeps, rounds)
			if sweeps == 0 {
				t.Fatal("no sweep ran during the snapshots, so this proved nothing")
			}
			// Every snapshot that got a record still has its bytes AND its cap.
			bc := btrfs.Client{}
			n := 0
			for id := range snaps {
				if _, err := os.Stat(volumeUnitPath(stateDir, id)); err != nil {
					t.Errorf("snapshot %s was reclaimed during a concurrent sweep: %v — the in-flight keep-set spans the bytes-then-row window", id, err)
					continue
				}
				if _, limit, err := bc.Usage(ctx, volumeUnitPath(stateDir, id)); err == nil && limit != 32<<20 {
					t.Errorf("snapshot %s carries cap %d, want %d", id, limit, 32<<20)
				}
				n++
			}
			if n == 0 {
				t.Fatal("no snapshot survived to be checked")
			}
			return
		default:
			if err := eng.SweepOrphanVolumes(ctx); err != nil {
				t.Fatalf("sweep during snapshots: %v", err)
			}
			sweeps++
		}
	}
}
