package volume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"engine/internal/allocbytes"
	"engine/internal/btrfs"
)

// LocalBtrfs is the DEFAULT §5.C backend arm: one volume = one storage unit on the
// host, which the gofer proxies into a guest as an ordinary directory so POSIX
// comes free from the host kernel (§5.C / volume.md D0). It carries both a hard
// capacity cap and cheap atomic snapshots because the substrate attaches both to
// the SAME object — the strongest argument for modelling a volume as one unit.
//
// It reuses internal/btrfs UNCHANGED. That package is a shared substrate TOOL, not
// a seam: §5.B (Overlay upper+work caps) was its first consumer and §5.C is the
// second. Two subsystems importing one leaf is not "subsystems calling each other"
// (§4) — neither calls the OTHER.
type LocalBtrfs struct {
	// VolumesDir holds one storage unit per volume: VolumesDir/<volumeID>. A
	// sibling of the Overlay state root, never nested inside it, so each sweep
	// enumerates only its own subsystem's units.
	VolumesDir string

	// Btrfs materializes the per-volume capacity cap. Shared CODE with §5.B
	// (rootfs-overlay D6.4), not a shared interface.
	Btrfs btrfs.Client
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
// path (they differ in the semantic label, not the mechanism — volume.md D6.1a),
// and Quota is what lets §3.4 refuse a cap it cannot enforce instead of running
// uncapped.
func (b *LocalBtrfs) Capabilities() Capabilities {
	return Capabilities{Snapshot: true, Clone: true, Quota: true}
}

// unitPath is the storage unit for one volume. Derived from the ID, so enumeration
// recovers identity without any store read (§4: the backend never reads the store).
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

// CreateVolume allocates the unit and applies the cap in ONE backend operation.
//
// The cap is written HERE rather than left to a follow-up call by the caller,
// because a missing cap is SILENT AND UNDETECTABLE AFTERWARDS: Usage reads back
// "no cap" and 0 identically, so no reconcile, sweep or inspect can ever discover
// the divergence (volume.md D7.3). That forces the invariant to write time.
func (b *LocalBtrfs) CreateVolume(ctx context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	unit := b.unitPath(volumeID)
	if capBytes > 0 && !b.Btrfs.Available(ctx, b.VolumesDir) {
		// Refusing beats running uncapped (§3.4): a silently dropped cap leaves every
		// §2.3 blast-radius assertion trusting a limit that is not there.
		return fmt.Errorf("volume: %s requests a %d-byte cap but VolumesDir is not on a quota-capable filesystem — refusing to run uncapped (§3.4): %w",
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

// SetCapacity re-applies the cap on an existing unit (§6.1 PATCH write point). 0
// removes it, keeping the volume RECORD the single authority (§3.4).
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
// unit. See Backend.Usage: the LIMIT half is DIAGNOSTIC ONLY — the substrate maps
// "no cap" and 0 onto each other, so it cannot distinguish "never capped" from
// "deliberately uncapped" and must never be used to validate the record.
func (b *LocalBtrfs) Usage(ctx context.Context, volumeID string) (int64, int64, error) {
	if err := validateID(volumeID); err != nil {
		return 0, 0, err
	}
	unit := b.unitPath(volumeID)
	if b.Btrfs.IsSubvolume(ctx, unit) {
		used, limit, err := b.Btrfs.Usage(ctx, unit)
		if err == nil {
			return used, limit, nil
		}
		// Quotas unreadable (disabled on this fs, or a transient bulk-read failure):
		// fall through to the allocation walk rather than reporting 0, which a caller
		// would read as an empty volume and use to admit a cap lowering that then
		// wedges the unit permanently (volume.md D7.3).
	}
	used, err := allocbytes.Tree(unit)
	if err != nil {
		return 0, 0, fmt.Errorf("volume: measure usage of %s: %w", volumeID, err)
	}
	return used, 0, nil
}

// Materialize prepares the host mount point. CREATE-IF-ABSENT by contract
// (§5.C): runsc hard-fails on a missing bind source, so an open-only version would
// make a mis-swept sandbox permanently unresumable, whereas recreating the unit
// empty restarts cleanly (data lost, but recoverable). Safe to call concurrently
// for the same volume — CreateSubvolume is idempotent, which is what the whole
// shared-fanout design rests on.
//
// It does NOT re-apply a cap: the cap's authority is the volume record and its
// write points are create and PATCH, both of which reach the backend directly. A
// volume has no placement of its own, so there is no "re-materialize at next
// placement" moment to hang it on (§5.C — the mechanical reason volume caps are
// editable while unmounted, unlike sandbox caps).
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

// Release ends one materialization. A NO-OP on this arm, and that is a correct
// implementation of an explicit promise rather than a stub: the promise is "this
// materialization's source is no longer valid, THE BYTES SURVIVE, idempotent,
// tolerant of an already-gone target" (see Backend.Release for why the method is
// not called Unmount).
func (b *LocalBtrfs) Release(context.Context, *Materialized) error { return nil }

// AdoptMaterialized re-recognizes a still-present unit after a crash (§4.4-A). It
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

// OpenRoot returns the root-confined accessor over the unit (volume.md D5.1). The
// handle exposes no host path at all, which makes this the one materialization-class
// method needing no exemption in the structural guard.
func (b *LocalBtrfs) OpenRoot(_ context.Context, volumeID string) (Root, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	return NewLocalRoot(volumeID, b.unitPath(volumeID))
}

// DestroyVolume reclaims the unit's bytes. Idempotent (§4.4-A: reached from
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
	if err := b.Btrfs.DeleteSubvolume(ctx, unit); err != nil {
		return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
	}
	if err := os.RemoveAll(unit); err != nil {
		return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
	}
	return nil
}

// UncappedBytes reports bytes under no declared cap — the §5.C half of the §3.4
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
// An UNREADABLE quota falls through to measuring rather than being skipped:
// omitting real bytes would overcommit the node, and the measurement is the
// conservative direction the gate is supposed to err in.
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
			if err == nil {
				if limit > 0 {
					continue // capped → the cap term already counts it
				}
				// Uncapped unit: the quota is still the backend's own statement about
				// these bytes and it excludes extents physically shared with a source,
				// so prefer it over a walk (counts shared bytes at most once).
				total += used
				continue
			}
		}
		n, err := allocbytes.Tree(unit)
		if err != nil {
			return 0, fmt.Errorf("volume: measure volume %s: %w", e.Name(), err)
		}
		total += n
	}
	return total, nil
}

// SweepOrphanVolumes reclaims units with no live row (§4.4-A). See
// Backend.SweepOrphanVolumes for the contract and for why every safety requirement
// below is a HARD requirement — a mis-sweep has no detection path at all.
//
// ENUMERATION IS PER-DIRECTORY-ENTRY, deliberately: each VolumesDir subdirectory IS
// a unit named by its VolumeID. A filesystem-level unit enumeration is FORBIDDEN
// here (measured: its scope is the whole filesystem regardless of the directory
// given, and it returns other software's units on the same node). internal/btrfs is
// already the right shape for this; the requirement is not to "optimize" it away.
//
// A read or enumeration failure SKIPS the unit — it never concludes "no owner" from
// an empty or failed read (measured: under concurrent churn a bulk quota read fails
// wholesale and returns zero rows, which is exactly the condition a sweep runs in).
//
// Mid-sweep failure CONTINUES: the error is returned after the pass together with
// the count actually reclaimed, so one wedged unit cannot indefinitely shadow every
// other leak. The sweep is idempotent, so whatever failed is retried next pass.
func (b *LocalBtrfs) SweepOrphanVolumes(ctx context.Context, live, building []string) (int, error) {
	keep := make(map[string]bool, len(live)+len(building))
	for _, id := range live {
		keep[id] = true
	}
	for _, id := range building {
		keep[id] = true
	}
	ents, err := os.ReadDir(b.VolumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		// Enumeration failed: reclaim NOTHING. An empty listing must never be read as
		// "every unit is an orphan".
		return 0, fmt.Errorf("volume: sweep list units: %w", err)
	}
	reclaimed := 0
	var failed []string
	for _, e := range ents {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := b.DestroyVolume(ctx, e.Name()); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		reclaimed++
	}
	if len(failed) > 0 {
		return reclaimed, fmt.Errorf("volume: sweep reclaim unit(s): %s", strings.Join(failed, "; "))
	}
	return reclaimed, nil
}

// compile-time: LocalBtrfs satisfies Backend.
var _ Backend = (*LocalBtrfs)(nil)
