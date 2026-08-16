package volume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/TSHOGX/khora/internal/allocbytes"
	"github.com/TSHOGX/khora/internal/btrfs"
)

// LocalBtrfs is the DEFAULT the Volume subsystem backend arm: one volume = one storage unit on the
// host, which the gofer proxies into a guest as an ordinary directory so POSIX
// semantics come directly from the host kernel. It carries both a hard
// capacity cap and cheap atomic snapshots because the substrate attaches both to
// the SAME object — the strongest argument for modelling a volume as one unit.
//
// It reuses internal/btrfs UNCHANGED. That package is a shared substrate TOOL, not
// a seam: the Rootfs subsystem (Overlay upper+work caps) was its first consumer and the Volume subsystem is the
// second. Two subsystems importing one leaf is not "subsystems calling each other"
// (the lifecycle) — neither calls the OTHER.
type LocalBtrfs struct {
	// VolumesDir holds one storage unit per volume: VolumesDir/<volumeID>. A
	// sibling of the Overlay state root, never nested inside it, so each sweep
	// enumerates only its own subsystem's units.
	VolumesDir string

	// Btrfs materializes the per-volume capacity cap. Shared CODE with the Rootfs subsystem
	// (the shared btrfs mechanism), not a shared interface.
	Btrfs btrfs.Client

	// mu guards deletedRows.
	mu sync.Mutex
	// deletedRows are quota-row ids recorded AT DELETE TIME, whose rows a deletion may
	// have leaked (the Volume subsystem / Volume quota cleanup). Recorded there because that is the last moment
	// the id is obtainable — see recordDeletedRow.
	deletedRows map[uint64]bool
}

// NewLocalBtrfs constructs the local arm, ensuring VolumesDir exists.
func NewLocalBtrfs(volumesDir string) (*LocalBtrfs, error) {
	if volumesDir == "" {
		return nil, fmt.Errorf("volume: VolumesDir is required")
	}
	if err := os.MkdirAll(volumesDir, 0o755); err != nil {
		return nil, fmt.Errorf("volume: create VolumesDir: %w", err)
	}
	return &LocalBtrfs{VolumesDir: volumesDir}, nil
}

func (b *LocalBtrfs) Name() string { return "local_btrfs" }

// Capabilities: this arm supports all three. Snapshot and Clone are one substrate
// path (they differ in the semantic label, not the mechanism — snapshot lineage),
// and Quota is what lets resource admission refuse a cap it cannot enforce instead of running
// uncapped.
func (b *LocalBtrfs) Capabilities() Capabilities {
	return Capabilities{Snapshot: true, Clone: true, Quota: true}
}

// unitPath is the storage unit for one volume. Derived from the ID, so enumeration
// recovers identity without any store read (the lifecycle: the backend never reads the store).
func (b *LocalBtrfs) unitPath(volumeID string) string {
	return filepath.Join(b.VolumesDir, volumeID)
}

// validateID refuses an ID that could escape VolumesDir or collide with path
// syntax. Volume IDs are engine-minted, so this is a structural guard against a
// future caller passing something else through, not input validation of a user
// string.
func validateID(volumeID string) error {
	if volumeID == "" {
		return fmt.Errorf("volume: empty volume id")
	}
	if strings.ContainsAny(volumeID, "/\\") || volumeID == "." || volumeID == ".." {
		return fmt.Errorf("volume: invalid volume id %q", volumeID)
	}
	return nil
}

// CreateVolume allocates the unit and applies the cap in ONE backend operation,
// never leaving it to a follow-up call by the caller: a missing cap is silent and
// undetectable afterwards (Backend.Usage / volume cap enforcement), which forces the
// invariant to write time.
func (b *LocalBtrfs) CreateVolume(ctx context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	unit := b.unitPath(volumeID)
	if capBytes > 0 && !b.Btrfs.Available(ctx, b.VolumesDir) {
		// Refusing beats running uncapped (resource admission): a silently dropped cap leaves every
		// the data-plane exposure boundary blast-radius assertion trusting a limit that is not there.
		return fmt.Errorf("volume: %s requests a %d-byte cap but VolumesDir is not on a quota-capable filesystem — refusing to run uncapped (resource admission): %w",
			volumeID, capBytes, ErrUnsupported)
	}
	if err := b.Btrfs.CreateSubvolume(ctx, unit); err != nil {
		return fmt.Errorf("volume: create unit %s: %w", volumeID, err)
	}
	if err := b.applyCap(ctx, volumeID, capBytes); err != nil {
		return err
	}
	return nil
}

// SetCapacity re-applies the cap on an existing unit (the control-plane API PATCH write point). 0
// removes it, keeping the volume RECORD the single authority (resource admission).
func (b *LocalBtrfs) SetCapacity(ctx context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	if _, err := os.Stat(b.unitPath(volumeID)); err != nil {
		return fmt.Errorf("volume: set capacity on %s: %w", volumeID, err)
	}
	return b.applyCap(ctx, volumeID, capBytes)
}

func (b *LocalBtrfs) applyCap(ctx context.Context, volumeID string, capBytes int64) error {
	unit := b.unitPath(volumeID)
	if capBytes < 0 {
		return fmt.Errorf("volume: negative cap %d for %s", capBytes, volumeID)
	}
	if capBytes > 0 && !b.Btrfs.IsSubvolume(ctx, unit) {
		return fmt.Errorf("volume: unit for %s cannot carry an enforceable %d-byte cap: %w", volumeID, capBytes, ErrUnsupported)
	}
	if capBytes == 0 && !b.Btrfs.IsSubvolume(ctx, unit) {
		return nil // a plain directory carries no cap to clear
	}
	if err := b.Btrfs.SetQuotaLimit(ctx, unit, capBytes); err != nil {
		return fmt.Errorf("volume: apply cap on %s: %w", volumeID, err)
	}
	return nil
}

// Usage reports the backend's own statement of usage plus the cap currently on the
// unit. The LIMIT half is DIAGNOSTIC ONLY — see Backend.Usage.
//
// A QUOTA-CAPABLE UNIT WHOSE ROW CANNOT BE READ IS AN ERROR, NEVER AN UNCAPPED ONE.
// Measured: `qgroup show -f <path>` enumerates EVERY row on the filesystem and
// applies the filter afterwards, so a subvolume deleted concurrently ANYWHERE on
// that filesystem fails the read wholesale — the error even names a row that is not
// ours ("cannot find the qgroup 0/21680"), 157 failures in 2142 reads under churn.
// The substrate's cleaner makes that churn asynchronous, so no caller can serialize
// against it.
//
// Answering `(walk, 0, nil)` there is a DEFINITE answer synthesized from a failure,
// which atomic transition rollback forbids a provider from producing, and it is wrong in BOTH halves:
// the walk counts file data only while the row also charges metadata extents (16 KiB
// on an EMPTY unit), so `used` comes back UNDER the substrate's own number and the
// cap write point's strict inequality would admit a cap the substrate then wedges the
// unit on (volume cap enforcement) — while limit 0 is indistinguishable from a deliberately
// uncapped volume. Returning the error is what keeps inspect's degraded response, the
// the control-plane API PATCH write point's fail-closed, and RepairVolumeCaps' draw-no-conclusion
// reachable at all.
//
// The walk stays for a PLAIN-DIRECTORY unit, which has no row to read and for which
// "no cap" is the truth rather than a guess.
func (b *LocalBtrfs) Usage(ctx context.Context, volumeID string) (int64, int64, error) {
	if err := validateID(volumeID); err != nil {
		return 0, 0, err
	}
	unit := b.unitPath(volumeID)
	if b.Btrfs.IsSubvolume(ctx, unit) {
		used, limit, err := b.Btrfs.Usage(ctx, unit)
		if err != nil {
			return 0, 0, fmt.Errorf("volume: read the quota row of %s: %w", volumeID, err)
		}
		return used, limit, nil
	}
	used, err := allocbytes.Tree(unit)
	if err != nil {
		return 0, 0, fmt.Errorf("volume: measure usage of %s: %w", volumeID, err)
	}
	return used, 0, nil
}

// Materialize prepares the host mount point. CREATE-IF-ABSENT by contract
// (the Volume subsystem): runsc hard-fails on a missing bind source, so an open-only version would
// make a mis-swept sandbox permanently unresumable, whereas recreating the unit
// empty restarts cleanly (data lost, but recoverable). Safe to call concurrently
// for the same volume — CreateSubvolume is idempotent, which is what the whole
// shared-fanout design rests on.
//
// It does NOT re-apply a cap: the cap's authority is the volume record and its
// write points are create and PATCH, both of which reach the backend directly. A
// volume has no placement of its own, so there is no "re-materialize at next
// placement" moment to hang it on (the Volume subsystem — the mechanical reason volume caps are
// editable while unmounted, unlike sandbox caps).
//
// THAT LEAVES ONE GAP THE MEDIATOR CLOSES, named here because the dependency is
// otherwise invisible from this side. The fallback below yields a PLAIN DIRECTORY
// whenever CreateSubvolume fails on an absent path (VolumesDir not on a
// quota-capable filesystem, or a substrate refusal), and a plain directory cannot
// carry a cap at all — so a volume whose RECORD declares one is re-materialized
// UNCAPPED, undetectably (Backend.Usage / volume cap enforcement). Nothing on this path
// re-asserts it; Mediator.RepairVolumeCaps is what does, and it can, because a
// freshly recreated unit is EMPTY — the recorded cap is strictly above its usage,
// which is the condition that pass requires before writing one. (For a volume whose
// record says uncapped the fallback is simply correct: a plain directory is exactly
// what "no cap" looks like on this arm.)
func (b *LocalBtrfs) Materialize(ctx context.Context, volumeID string) (*Materialized, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	unit := b.unitPath(volumeID)
	if err := b.Btrfs.CreateSubvolume(ctx, unit); err != nil {
		// A plain directory already sitting there (an uncapped unit) is fine and
		// CreateSubvolume reports it as an error, so fall back to ensuring the dir.
		if _, statErr := os.Stat(unit); statErr != nil {
			if mkErr := os.MkdirAll(unit, 0o755); mkErr != nil {
				return nil, fmt.Errorf("volume: materialize %s: %w", volumeID, err)
			}
		}
	}
	return &Materialized{Volume: volumeID, SourcePath: unit}, nil
}

// Snapshot and Clone are ONE code path on this arm, which is what snapshot lineage
// concluded: the substrate implements both with the same whole-tree operation, and the
// difference is the semantic label a caller attaches, not the mechanism. Keeping them
// as two seam methods over one implementation is what lets a backend that supports only
// one of them say so through Capabilities.
func (b *LocalBtrfs) Snapshot(ctx context.Context, srcID, newID string, capBytes int64) error {
	return b.snapshotPath(ctx, srcID, newID, capBytes, "snapshot")
}

func (b *LocalBtrfs) Clone(ctx context.Context, srcID, newID string, capBytes int64) error {
	return b.snapshotPath(ctx, srcID, newID, capBytes, "clone")
}

// snapshotPath does the two-call sequence the substrate forces: copy the bytes, then set
// the cap. SETTING THE CAP IS PART OF THIS METHOD, not the caller's follow-up — see
// Backend.Snapshot for the measurement that forces it and for why `-i <qgroupid>` cannot
// collapse the two calls into one.
//
// The window between them cannot be closed here; it is covered by the mediator's
// in-flight keep-set plus the sweep's repair-state check.
func (b *LocalBtrfs) snapshotPath(ctx context.Context, srcID, newID string, capBytes int64, kind string) error {
	if err := validateID(srcID); err != nil {
		return err
	}
	if err := validateID(newID); err != nil {
		return err
	}
	if capBytes > 0 && !b.Btrfs.Available(ctx, b.VolumesDir) {
		return fmt.Errorf("volume: %s of %s requests a %d-byte cap but VolumesDir is not on a quota-capable filesystem: %w", kind, srcID, capBytes, ErrUnsupported)
	}
	src := b.unitPath(srcID)
	dst := b.unitPath(newID)
	if !b.Btrfs.IsSubvolume(ctx, src) {
		return fmt.Errorf("volume: %s: source %s is not a snapshottable storage unit: %w", kind, srcID, ErrUnsupported)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("volume: %s: %s already exists", kind, newID)
	}
	// A WRITABLE snapshot: no `-r`. That is the design path (the Volume subsystem — a snapshot yields an
	// ordinary writable volume), and it also sidesteps a substrate wrinkle: a read-only
	// unit on this progs version needs `property set ro false` before it can be deleted
	// at all, so test and cleanup code that ever creates one must carry that step
	// unconditionally.
	if err := b.Btrfs.Snapshot(ctx, src, dst); err != nil {
		return fmt.Errorf("volume: %s %s → %s: %w", kind, srcID, newID, err)
	}
	// THE SECOND CALL, and skipping it is the silent failure described above.
	if err := b.applyCap(ctx, newID, capBytes); err != nil {
		// Leave the bytes: they are registered in the mediator's in-flight set and the
		// record has not been inserted, so the sweep reclaims them as an orphan. Deleting
		// them here would race that.
		return fmt.Errorf("volume: %s %s → %s: set cap: %w", kind, srcID, newID, err)
	}
	return nil
}

// ReclaimQuotaRows implements the Volume subsystem / Volume storage recovery procedure for rows an earlier
// delete leaked. See Backend.ReclaimQuotaRows for why it is a correctness path.
//
// The procedure, in the measured order:
//
//  1. ONE sync per ROUND (not per unit): `filesystem sync` then `subvolume sync`. That
//     order is a measured finding — `subvolume sync` alone sleeps until the next
//     periodic commit, so its cost is load-dependent rather than fixed (runs at 7-16 s
//     and 19.1 s), while forcing the commit first returns in tens of ms (14-29 ms,
//     53 ms). The ~3 orders of magnitude is the point, not any one interval.
//  2. For each recorded id, READ THE LABEL and act on it: `<stale>` → destroy; absent →
//     done; `<under deletion>` → retry after a settle; `<squota space holder>` → LEAVE
//     for a later round (a live clone can hold one legitimately for its whole lifetime);
//     read failed → UNKNOWN, draw no conclusion.
//  3. A BOUNDED number of rounds (3), then leave the row and warn. NEVER spin on EBUSY
//     — that bound is normal operation, not an error path.
func (b *LocalBtrfs) ReclaimQuotaRows(ctx context.Context, ids []uint64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	mnt, err := b.Btrfs.MountPoint(ctx, b.VolumesDir)
	if err != nil {
		// No filesystem to address: nothing to reclaim, and guessing would be worse.
		return 0, fmt.Errorf("volume: reclaim quota rows: %w", err)
	}

	pending := append([]uint64(nil), ids...)
	reclaimed := 0
	var stuck []string
	const maxRounds = 3
	for round := 0; round < maxRounds && len(pending) > 0; round++ {
		// One sync per ROUND.
		if err := b.Btrfs.Sync(ctx, mnt); err != nil {
			// A failed sync means the labels may be stale, so this round would be reading
			// unreliable state. Stop rather than act on it; the next sweep retries.
			return reclaimed, fmt.Errorf("volume: reclaim quota rows: settle: %w", err)
		}
		var next []uint64
		for _, id := range pending {
			switch state := b.Btrfs.QuotaRowByID(ctx, id, mnt); state {
			case btrfs.RowAbsent:
				b.forgetDeletedRow(id) // reclaimed by the substrate itself; nothing to do
			case btrfs.RowStale:
				if err := b.Btrfs.DestroyQuotaRow(ctx, id, mnt); err != nil {
					// It said stale and still refused: keep it for a later round rather than
					// treating it as fatal.
					next = append(next, id)
					continue
				}
				reclaimed++
				b.forgetDeletedRow(id)
			case btrfs.RowUnderDeletion:
				next = append(next, id) // transient: another settle should clear it
			case btrfs.RowSpaceHolder:
				// A LIVE SHARER still references its extents. This is legitimate and can last
				// as long as that sharer does, so it is NOT retried within this call — it stays
				// recorded for a future sweep.
				stuck = append(stuck, fmt.Sprintf("0/%d: space holder (a live sharer still references its extents)", id))
			case btrfs.RowUnknown:
				// The read failed WHOLESALE, which happens under exactly the churn a sweep runs
				// in. Draw NO conclusion: keep it recorded and try again next sweep.
				stuck = append(stuck, fmt.Sprintf("0/%d: row state unreadable", id))
			case btrfs.RowLive:
				// The id belongs to a LIVE unit — either it was reused after our delete, or the
				// recorded id is wrong. Either way: never touch it. Clearing a live unit's limit
				// is the one operation that breaks an engine promise beyond data integrity and
				// has no EBUSY self-protection.
				b.forgetDeletedRow(id)
				stuck = append(stuck, fmt.Sprintf("0/%d: now belongs to a LIVE unit, left untouched", id))
			}
		}
		pending = next
	}
	for _, id := range pending {
		stuck = append(stuck, fmt.Sprintf("0/%d: still not reclaimable after %d rounds", id, maxRounds))
	}
	if len(stuck) > 0 {
		// Returned as information, not as a failure of the sweep: every entry here is a
		// row that stays recorded and is retried later.
		return reclaimed, fmt.Errorf("volume: quota rows left for a later round: %s", strings.Join(stuck, "; "))
	}
	return reclaimed, nil
}

// DeletedRowIDs returns the ids recorded at deletion time and not yet reclaimed.
func (b *LocalBtrfs) DeletedRowIDs(context.Context) []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]uint64, 0, len(b.deletedRows))
	for id := range b.deletedRows {
		out = append(out, id)
	}
	return out
}

// recordDeletedRow / forgetDeletedRow track ids whose row a delete may have leaked.
//
// RECORDED AT DELETE TIME, because that is the last moment the id is obtainable: after
// the delete the path is unresolvable, and the id is the only handle on a row the
// delete may have left behind. the Volume subsystem requires locating rows this way rather than by
// scanning, because a scan that misidentifies a row yields an uncapped live volume with
// zero signal.
//
// In-memory, so a crash loses the knowledge that we were watching a row — never the ROW
// ITSELF, which stays on the filesystem, and the ids only ever drive an idempotent
// reclaim. Persisting the list would mean a new store table whose only consumer is a
// substrate-specific cleanup, which the extension boundary argues against.
func (b *LocalBtrfs) recordDeletedRow(id uint64) {
	b.mu.Lock()
	if b.deletedRows == nil {
		b.deletedRows = map[uint64]bool{}
	}
	b.deletedRows[id] = true
	b.mu.Unlock()
}

func (b *LocalBtrfs) forgetDeletedRow(id uint64) {
	b.mu.Lock()
	delete(b.deletedRows, id)
	b.mu.Unlock()
}

// Release ends one materialization. A NO-OP on this arm, and that is a correct
// implementation of Backend.Release's four-part promise rather than a stub — see
// there for the promise and for why the method is not called Unmount.
func (b *LocalBtrfs) Release(context.Context, *Materialized) error { return nil }

// AdoptMaterialized re-recognizes a still-present unit after a crash (atomic transition rollback). It
// does NOT create — an absent unit means there is nothing to adopt, and reporting
// that is what lets the mediator tell "recover the handle" from "materialize
// afresh".
func (b *LocalBtrfs) AdoptMaterialized(_ context.Context, volumeID string) (*Materialized, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	unit := b.unitPath(volumeID)
	if fi, err := os.Stat(unit); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("volume: adopt %s: unit not present", volumeID)
	}
	return &Materialized{Volume: volumeID, SourcePath: unit}, nil
}

// OpenRoot returns the root-confined accessor over the unit. The handle exposes no host
// path at all, which makes this the one materialization-class
// method needing no exemption in the structural guard.
func (b *LocalBtrfs) OpenRoot(_ context.Context, volumeID string) (Root, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	return NewLocalRoot(volumeID, b.unitPath(volumeID))
}

// DestroyVolume reclaims the unit's bytes. Idempotent (atomic transition rollback: reached from
// delete, unwind and the sweep).
//
// The contents are emptied FIRST because a unit delete addresses the unit itself
// while its children are ordinary directories inside it. For an uncapped
// (plain-directory) volume the single RemoveAll is the whole reclamation.
func (b *LocalBtrfs) DestroyVolume(ctx context.Context, volumeID string) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	unit := b.unitPath(volumeID)
	if !b.Btrfs.IsSubvolume(ctx, unit) {
		if err := os.RemoveAll(unit); err != nil {
			return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
		}
		return nil
	}
	ents, err := os.ReadDir(unit)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("volume: destroy %s: list contents: %w", volumeID, err)
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(unit, e.Name())); err != nil {
			return fmt.Errorf("volume: destroy %s: clear %s: %w", volumeID, e.Name(), err)
		}
	}
	// Capture the quota-row id BEFORE the delete: afterwards the path is unresolvable
	// and this id is the only handle on a row the delete may leak (the Volume subsystem — rows are
	// located by the id recorded at deletion, never by scanning). A failure to read it
	// is not fatal to the delete; it only means the reclaim path cannot watch this row.
	if id, idErr := b.Btrfs.SubvolumeID(ctx, unit); idErr == nil {
		b.recordDeletedRow(id)
	}
	if err := b.Btrfs.DeleteSubvolume(ctx, unit); err != nil {
		return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
	}
	if err := os.RemoveAll(unit); err != nil {
		return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
	}
	return nil
}

// UncappedBytes reports bytes under no declared cap — the Volume subsystem half of the resource admission
// second term (see Backend.UncappedBytes for the contract). How this arm honors it:
//
//	unit WITH a cap   → ZERO: its cap is already a committed promise the mediator
//	                    counts, and adding usage on top bills the same bytes twice
//	unit with NO cap  → its measured allocation
//
// "Capped" is keyed on the LIMIT, not on whether the unit is a quota-capable
// storage unit: SetCapacity can clear a limit to 0 and leave the unit in place, so
// the unit kind is not a reliable proxy for a cap.
//
// An UNREADABLE quota row is an ERROR, never a fallback to measuring: on the
// failure path the `limit > 0` skip below is UNREACHABLE, so the walk ran for
// CAPPED units too, whose correct contribution here is ZERO — the same bytes
// billed twice, which the node admission budgets's disjointness rule forbids, at the magnitude of the
// whole unit and strictly ONE-directional, so it produces wrongful 507s on a live
// admission path. The aggregate therefore stays all-or-nothing: refusing admission
// visibly is safer than silently understating the node total. Same rule as Backend.Usage
// and atomic transition rollback; this was the one place that guessed.
//
// The read itself is retried inside the btrfs client (transient failure under
// concurrent churn), so an error reaching here means it stayed unreadable.
func (b *LocalBtrfs) UncappedBytes(ctx context.Context) (int64, error) {
	ents, err := os.ReadDir(b.VolumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("volume: measure volumes: %w", err)
	}
	var total int64
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		unit := b.unitPath(e.Name())
		if b.Btrfs.IsSubvolume(ctx, unit) {
			used, limit, err := b.Btrfs.Usage(ctx, unit)
			if err != nil {
				// Unreadable, NOT unlimited: see the doc comment. A walk here would bill a
				// capped unit's bytes on top of its own cap.
				return 0, fmt.Errorf("volume: measure volume %s: %w", e.Name(), err)
			}
			if limit > 0 {
				continue // capped → the cap term already counts it
			}
			// Uncapped unit: the quota is still the backend's own statement about these
			// bytes and it excludes extents physically shared with a source, so prefer it
			// over a walk (counts shared bytes at most once).
			total += used
			continue
		}
		// GENUINELY not a storage unit with a quota row (the plain-directory case), so a
		// walk is the only statement available and is the right one — this is not the
		// fallback removed above.
		n, err := allocbytes.Tree(unit)
		if err != nil {
			return 0, fmt.Errorf("volume: measure volume %s: %w", e.Name(), err)
		}
		total += n
	}
	return total, nil
}

// ListUnitIDs enumerates the volume IDs this arm holds storage for (see
// Backend.ListUnitIDs for why enumeration and deciding are split).
//
// PER-DIRECTORY-ENTRY, deliberately: each VolumesDir subdirectory IS a unit named by its
// VolumeID, so enumeration recovers identity without any store read. A filesystem-level
// unit enumeration is FORBIDDEN here — measured, its scope is the whole filesystem
// regardless of the directory given, and it returns other software's units on the same
// node. The requirement is not to "optimize" it away.
//
// A read error is RETURNED, never flattened into an empty list (Backend.ListUnitIDs).
func (b *LocalBtrfs) ListUnitIDs(_ context.Context) ([]string, error) {
	ents, err := os.ReadDir(b.VolumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no directory yet: genuinely no units
		}
		return nil, fmt.Errorf("volume: enumerate units: %w", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// compile-time: LocalBtrfs satisfies Backend.
var _ Backend = (*LocalBtrfs)(nil)
