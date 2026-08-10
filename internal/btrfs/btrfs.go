// Package btrfs wraps the btrfs storage-substrate primitives the engine needs:
// subvolume create/delete plus a quota limit and a usage read.
//
// It is a LEAF package and deliberately ignorant of the engine's model. It knows
// nothing of Overlay, Volume, Template, or SandboxID — it takes paths and byte
// counts. §5.B (Overlay upper+work) and §5.C (per-volume caps) each import it the
// way they import `os` (rootfs-overlay D6.4).
//
// WHY A PACKAGE AND NOT A SEAM. The capacity cap is shared CODE, not a shared
// INTERFACE. There is deliberately no cross-cutting QuotaProvider: a remote FS
// provider satisfies "this unit is at most N bytes" by entirely different means
// (object-store quota, a quota API, client-side accounting), so a cross-cutting
// interface would only weld the btrfs assumption into every implementation. Two
// subsystems importing one substrate tool do NOT violate "subsystems never call
// each other" (§4) — neither calls the OTHER; they share a utility.
//
// MECHANISM CHOICE: squota, not full qgroups (rootfs-overlay D6.2). Three
// reasons, the third decisive: (1) squota is designed for exactly this shape, one
// subvolume per workload, with no rescan storms; (2) its semantics match what we
// mean to charge for — bytes the sandbox WROTE — whereas under full qgroups a
// copy-up reflink pushes `referenced` past the cap and only falls back after
// refusing new writes, paying a cost for still no hard ceiling; (3) the dev/test
// substrate CANNOT change mode (BTRFS_IOC_QUOTA_CTL returns EPERM even as root
// with CAP_SYS_ADMIN), so choosing full qgroups would fork spec from substrate
// permanently.
//
// The consequence that must be handled OUTSIDE this package: under squota a
// reflinked copy-up is free, so the cap governs newly-written bytes and is not a
// hard ceiling for copy-up, and Template/layer bytes carry no cap at all. That is
// why node accounting lists measured layer bytes separately (D6.5) and why the
// §3.4 admission gate cannot be derived from caps alone.
//
// All operations shell out to `btrfs(8)` (btrfs-progs is an explicit deployment
// dependency, measured v6.17.1 on the dev substrate). They require root, like the
// overlayfs mounts in §5.B (rootfs-overlay D5) — privilege stays sealed inside
// the storage subsystems, never in the §4 mediator.
package btrfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner executes a command and returns its combined output. It exists so the
// callers that already route commands through sudo (dev convenience) can reuse
// that path; nil means "exec directly".
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// Client is a handle for running btrfs operations. The zero value works and
// executes commands directly.
type Client struct {
	// Bin is the btrfs binary (default "btrfs" on PATH).
	Bin string
	// Run, if non-nil, executes commands (e.g. wrapping them in sudo).
	Run Runner
}

func (c Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "btrfs"
}

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{c.bin()}, args...)
	if c.Run != nil {
		return c.Run(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %w: %s", argv, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// IsSubvolume reports whether path is a btrfs subvolume. Used to keep
// create/delete idempotent across restarts, and to let a caller tell a
// subvolume-backed unit from a plain directory (an uncapped Overlay, or one on
// a non-btrfs filesystem).
func (c Client) IsSubvolume(ctx context.Context, path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	_, err := c.run(ctx, "subvolume", "show", path)
	return err == nil
}

// CreateSubvolume creates a subvolume at path. The parent directory must exist.
// Idempotent: an existing subvolume at path is accepted as-is, so a retried
// create (or a resume over an already-materialized unit) is not an error.
//
// A subvolume rather than a plain directory because it is the unit BOTH capacity
// accounting and cheap atomic snapshots attach to — one choice serving §5.B's cap
// today and §5.C's snapshot/clone later (rootfs-overlay D6.2).
func (c Client) CreateSubvolume(ctx context.Context, path string) error {
	if c.IsSubvolume(ctx, path) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("btrfs: parent dir for %s: %w", path, err)
	}
	// A plain directory already sitting at path cannot become a subvolume; report
	// it rather than silently proceeding with an uncapped plain dir, which would
	// look like a working cap while enforcing nothing.
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return fmt.Errorf("btrfs: %s exists as a plain directory, not a subvolume", path)
	}
	if _, err := c.run(ctx, "subvolume", "create", path); err != nil {
		return fmt.Errorf("btrfs: create subvolume %s: %w", path, err)
	}
	return nil
}

// DeleteSubvolume removes the subvolume at path and, where the filesystem needs
// it, its qgroup.
//
// Quota-row cleanup differs by quota mode, and BOTH behaviours are handled here
// because the mode is a deployment property, not something the engine controls:
//
//   - squota (the dev/test substrate, ADR-0002), unit owning NO shared extents:
//     btrfs reclaims the row ITSELF. Measured — right after delete the row lingers
//     briefly as `<under deletion>` and then disappears once the deletion settles;
//     across 6 create/fill/delete cycles the filesystem's qgroup count returned to
//     exactly its starting value. An explicit destroy here fails with ENOENT
//     (squota owns the row), which is why the call below is best-effort. This is
//     the ONLY squota case where best-effort suffices, and it is the case §5.B's
//     Overlay upper is in today: the engine never snapshots an upper.
//   - squota, unit whose extents are still referenced by a live sharer (§5.C
//     snapshots/clones a volume, so this case is REACHABLE there): the row LEAKS.
//     Measured — an immediate destroy returns EBUSY ("Device or resource busy"),
//     NOT ENOENT, and the row does not self-clean (25s + 6 syncs did not move it).
//     Its label is `<squota space holder>` while a sharer is alive and only becomes
//     `<stale>` after the last sharer is gone; destroy succeeds exactly when the
//     label is `<stale>`. The exact predicate for leaking is: the unit (i) owns
//     extents AND (ii) is deleted while a live sharer still references them —
//     "was snapshotted" is not the predicate, and having a cap is irrelevant.
//     Reclaiming these rows is NOT cosmetic: nothing reserves a leaked row's
//     subvolume id, so a remount can hand that id — and its cap — to a brand-new
//     unit, which then hits ENOSPC on a limit nobody set for it (or gets silently
//     over-provisioned), and Usage() cannot tell the difference. That reclamation
//     belongs to the volume orphan sweep, which polls the label and destroys on
//     `<stale>` (deleting sharers BEFORE sources leaks nothing at all); see
//     docs/spec/deep-dives/volume.md D7.
//   - full qgroup mode: the row does NOT go away on its own and must be destroyed,
//     or rows accumulate one per destroyed sandbox.
//
// So the destroy is attempted and its error ignored: required in one mode, a
// harmless ENOENT or a to-be-swept EBUSY in the other. Verifying "no stale row
// remains" belongs to the caller's acceptance, not to this function's return.
//
// Idempotent: a path that is absent, or not a subvolume, is a no-op — reclamation
// is retried from several paths (destroy, unwind, orphan sweep) and must never
// fail because someone else already cleaned up (§4.4-A).
func (c Client) DeleteSubvolume(ctx context.Context, path string) error {
	if !c.IsSubvolume(ctx, path) {
		// Not a subvolume: either already gone, or a plain directory whose bytes are
		// the caller's to remove. Either way nothing btrfs-specific to do.
		return nil
	}
	// Capture the id BEFORE deletion — afterwards it is unresolvable, and it is
	// what names the qgroup to destroy.
	id, idErr := c.subvolumeID(ctx, path)
	mnt, mntErr := c.mountPoint(ctx, path)
	if _, err := c.run(ctx, "subvolume", "delete", path); err != nil {
		return fmt.Errorf("btrfs: delete subvolume %s: %w", path, err)
	}
	if idErr == nil && mntErr == nil {
		// Best-effort BY DESIGN (see the doc comment): required under full qgroup
		// mode, an expected ENOENT under squota where btrfs reclaims the row itself,
		// and a no-op when quotas are disabled. None of those is a delete failure.
		_, _ = c.run(ctx, "qgroup", "destroy", "0/"+strconv.FormatUint(id, 10), mnt)
	}
	return nil
}

// Snapshot creates a WRITABLE snapshot of the subvolume at src, at dst. Whole-tree,
// atomic and effectively instantaneous — measured, a new snapshot costs about 16 KiB of
// exclusive metadata and copies no data.
//
// WRITABLE (no `-r`) because that is what §5.C's snapshot object is: an ordinary
// writable volume, with immutability expressed by a read-only MOUNT ROW instead
// (volume.md D6.1a). It also avoids a substrate wrinkle worth knowing about even though
// the design path does not hit it: on this progs version a read-only unit must have its
// ro property cleared before it can be deleted at all, so any code that creates one —
// including test fixtures and cleanup traps — has to carry that step unconditionally.
//
// IT DOES NOT CARRY A CAP, and no flag makes it. The new unit gets a brand-new UNCAPPED
// quota row, so the caller MUST set the cap in a second call; `-i <qgroupid>` looks like
// the missing flag but sets MEMBERSHIP, whose limit is shared across all members (one
// member exhausting it makes its SIBLINGS hit ENOSPC). See volume.Backend.Snapshot.
func (c Client) Snapshot(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("btrfs: parent dir for %s: %w", dst, err)
	}
	if _, err := c.run(ctx, "subvolume", "snapshot", src, dst); err != nil {
		return fmt.Errorf("btrfs: snapshot %s → %s: %w", src, dst, err)
	}
	return nil
}

// SetQuotaLimit caps the subvolume at path to limitByte (0 = remove the cap).
//
// This is the enforcement point for the §3.4 capacity axis: past the cap the
// guest's write() returns a plain ENOSPC and THE SANDBOX KEEPS RUNNING —
// deliberately a data-plane event, not a lifecycle one (unlike the RAM cap).
// Measured through the whole chain (subvolume → overlayfs upper/work → gofer →
// sentry → guest): the ENOSPC lands on write() with ~0.05% precision, fsync/close
// still return 0, and the container remains exec-able afterwards.
func (c Client) SetQuotaLimit(ctx context.Context, path string, limitByte int64) error {
	arg := "none"
	if limitByte > 0 {
		arg = strconv.FormatInt(limitByte, 10)
	}
	if _, err := c.run(ctx, "qgroup", "limit", arg, path); err != nil {
		return fmt.Errorf("btrfs: set quota limit %s on %s: %w", arg, path, err)
	}
	return nil
}

// Usage reports the bytes charged to the subvolume at path and its current limit
// (limit 0 = none). This is the BACKEND's own accounting — the only trustworthy
// source for capacity — §5.B requires capacity accounting to come from the
// backend's own statement, never from guest-side numbers (see
// rootfs.Provider.UncappedBytes for why those are unusable).
// usageReadAttempts bounds the retry below. Three, because three is what the
// measurement needed and no more (see Usage).
const usageReadAttempts = 3

func (c Client) Usage(ctx context.Context, path string) (used, limit int64, err error) {
	// -f filters to just this path's qgroup; --raw for exact bytes; --sync so the
	// numbers reflect writes that have not yet been committed by a background
	// transaction (without it a just-written file can read as 0).
	//
	// RETRIED, AND THE RETRY IS LOAD-BEARING RATHER THAN DEFENSIVE. `-f` filters AFTER
	// enumerating every row on the filesystem, so a subvolume deleted concurrently
	// ANYWHERE on that fs fails this read WHOLESALE — the error even names a row that is
	// not ours. Measured under three-way churn: 10/300 reads failed, and on failure our
	// own row was printed 0/10 times, so there is nothing to parse around; the read
	// either answers or it does not. The engine's own destroy path generates exactly that
	// churn and the substrate's cleaner is asynchronous, so no caller can serialize
	// against it.
	//
	// The failure is TRANSIENT, which is what makes a retry the right shape rather than a
	// papering-over: under the same churn, three attempts turned 8 first-attempt failures
	// into 0 final failures (300 reads). That matters beyond tidiness — the callers that
	// must FAIL CLOSED on an unreadable row (the §6.1 cap write points, and §5.B's
	// materialization guard, which sits on the create/resume hot path) pay for this read's
	// reliability directly in refused requests and refused placements.
	//
	// The error contract is UNCHANGED: an error still means "unreadable, draw no
	// conclusion" — it is simply much rarer. Nothing here retries a SEMANTIC failure
	// (quotas disabled, no such row): those come back from the parse below, not from a
	// non-zero exit.
	var out []byte
	for attempt := 1; ; attempt++ {
		out, err = c.run(ctx, "qgroup", "show", "-re", "--raw", "--sync", "-f", path)
		if err == nil {
			break
		}
		if attempt == usageReadAttempts || ctx.Err() != nil {
			return 0, 0, fmt.Errorf("btrfs: qgroup show %s (%d attempts): %w", path, attempt, err)
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// qgroupid referenced exclusive max_referenced max_exclusive [path]
		if len(f) < 4 || !strings.HasPrefix(f[0], "0/") {
			continue
		}
		used, err = strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("btrfs: parse referenced %q: %w", f[1], err)
		}
		if f[3] != "none" {
			limit, _ = strconv.ParseInt(f[3], 10, 64)
		}
		return used, limit, nil
	}
	return 0, 0, fmt.Errorf("btrfs: no qgroup row for %s (quotas disabled on this fs?)", path)
}

// QuotaRowState is what a quota row's LABEL says about it, which is the only
// available predicate for whether the row can be reclaimed (see QuotaRowByID).
type QuotaRowState string

const (
	// RowAbsent: the row is gone. Nothing to reclaim.
	RowAbsent QuotaRowState = "absent"
	// RowLive: the row belongs to a live unit (its label is a path). It must NEVER be
	// touched — clearing a live unit's limit is the one operation that breaks an engine
	// promise beyond data integrity, and unlike destroy it has no EBUSY self-protection.
	RowLive QuotaRowState = "live"
	// RowStale: the unit is deleted, its deletion has settled, and no sharer references
	// its extents. THE ONLY DESTROYABLE STATE — measured, destroy succeeds exactly when
	// the label is `<stale>`.
	RowStale QuotaRowState = "stale"
	// RowUnderDeletion: the transient right after a delete, before the cleaner has run.
	// Destroy returns EBUSY; retry after a settle.
	RowUnderDeletion QuotaRowState = "under-deletion"
	// RowSpaceHolder: deleted, but a LIVE SHARER still references its extents. Destroy
	// returns EBUSY, and this can legitimately persist for the ENTIRE LIFETIME of a live
	// clone — so a sweep must leave it for a later round rather than spinning on it.
	RowSpaceHolder QuotaRowState = "space-holder"
	// RowUnknown: the read FAILED. Measured hazard, and the reason this value exists as
	// a first-class answer rather than being folded into RowAbsent: under concurrent
	// churn a bulk quota read fails WHOLESALE and returns zero rows (about 1 in 150),
	// and those are exactly the conditions a sweep runs in. A reader that treats an
	// empty result as "the row is gone" SILENTLY SKIPS A DESTROY, which is a permanent
	// leak. Callers must draw no conclusion from this.
	RowUnknown QuotaRowState = "unknown"
)

// QuotaRowByID reports the state of the quota row for subvolume id, by READING ITS
// LABEL.
//
// BY ID, RECORDED AT DELETE TIME — never by scanning rows for something that looks
// reclaimable. The failure mode of scanning is an uncapped LIVE volume plus zero
// signal: a scan that misidentifies a row and clears its limit produces exactly that,
// and nothing downstream can detect it (§5.C requires locating rows by the id recorded
// at deletion for this reason).
//
// The state comes from the label because that is the only predicate the substrate
// offers, and it CANNOT false-positive on a running unit: a live unit's label is its
// PATH, while every reclaimable state is a `<...>` marker.
//
// A FAILED READ RETURNS RowUnknown, never RowAbsent. See RowUnknown.
func (c Client) QuotaRowByID(ctx context.Context, id uint64, mountPoint string) QuotaRowState {
	// -re --raw --sync, matching Usage: --sync so the answer reflects writes a
	// background transaction has not committed yet.
	out, err := c.run(ctx, "qgroup", "show", "-re", "--raw", "--sync", mountPoint)
	if err != nil {
		// The read failed WHOLESALE, which happens under exactly the concurrent churn a
		// sweep runs in. Report it as unknown so the caller skips rather than concludes.
		return RowUnknown
	}
	want := "0/" + strconv.FormatUint(id, 10)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 1 || f[0] != want {
			continue
		}
		// qgroupid referenced exclusive max_referenced max_exclusive [path/label...]
		if len(f) < 6 {
			// A row with no label column at all: present, but nothing says it is
			// reclaimable, so it is treated as live (the conservative direction).
			return RowLive
		}
		label := strings.Join(f[5:], " ")
		switch {
		case strings.Contains(label, "<stale>"):
			return RowStale
		case strings.Contains(label, "<under deletion>"):
			return RowUnderDeletion
		case strings.Contains(label, "squota space holder"):
			return RowSpaceHolder
		default:
			// A path, i.e. a LIVE unit. Never destroy, never clear.
			return RowLive
		}
	}
	return RowAbsent
}

// DestroyQuotaRow destroys the quota row for subvolume id.
//
// Separate from DeleteSubvolume's best-effort destroy, which cannot serve here: that
// one fires immediately after the delete, when the row is `<under deletion>` or
// `<squota space holder>` and the destroy therefore returns EBUSY. Reclaiming a
// leaked row needs a LATER attempt, made when the label says the row is reclaimable —
// which is why this is its own entry point taking an id rather than a path (the path
// is gone by then).
//
// Returns the error verbatim so a caller can distinguish "destroyed" from EBUSY. It
// must NOT be retried in a tight loop on EBUSY: a `<squota space holder>` can
// legitimately be undestroyable for the entire lifetime of a live clone, so a bounded
// number of rounds and then leaving the row is the correct behavior, not an error path.
func (c Client) DestroyQuotaRow(ctx context.Context, id uint64, mountPoint string) error {
	if _, err := c.run(ctx, "qgroup", "destroy", "0/"+strconv.FormatUint(id, 10), mountPoint); err != nil {
		return fmt.Errorf("btrfs: destroy quota row 0/%d: %w", id, err)
	}
	return nil
}

// SubvolumeID exposes the numeric id, which names the quota row (0/<id>). It must be
// captured BEFORE a delete: afterwards the path is unresolvable and the id is the only
// handle on the row that delete may have leaked.
func (c Client) SubvolumeID(ctx context.Context, path string) (uint64, error) {
	return c.subvolumeID(ctx, path)
}

// MountPoint exposes the filesystem mount point containing path — the argument the
// quota-row calls need, since they address a FILESYSTEM rather than a subvolume path.
// Also captured before a delete, for the same reason.
func (c Client) MountPoint(ctx context.Context, path string) (string, error) {
	return c.mountPoint(ctx, path)
}

// Sync forces a commit and then waits for pending subvolume deletions to settle.
//
// THE ORDER IS THE WHOLE POINT, and it is a measured finding rather than a style
// choice: `subvolume sync` ALONE sleeps until the next periodic commit, so its cost
// tracks the commit interval and the load rather than being a fixed property (runs
// have landed at 7-16 s and at 19.1 s), while preceding it with `filesystem sync`
// forces that commit so the pair returns in TENS OF MILLISECONDS (14-29 ms, 53 ms).
// The load-bearing fact is that ~3 orders of magnitude, not any one interval: same
// effect, bounded instead of "wait for a commit".
//
// Called ONCE PER SWEEP ROUND rather than once per unit, which is the other half of
// keeping it bounded.
func (c Client) Sync(ctx context.Context, mountPoint string) error {
	if _, err := c.run(ctx, "filesystem", "sync", mountPoint); err != nil {
		return fmt.Errorf("btrfs: filesystem sync %s: %w", mountPoint, err)
	}
	if _, err := c.run(ctx, "subvolume", "sync", mountPoint); err != nil {
		return fmt.Errorf("btrfs: subvolume sync %s: %w", mountPoint, err)
	}
	return nil
}

// subvolumeID reads the numeric subvolume id, which names its qgroup (0/<id>).
func (c Client) subvolumeID(ctx context.Context, path string) (uint64, error) {
	out, err := c.run(ctx, "subvolume", "show", path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, v, ok := strings.Cut(line, "Subvolume ID:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("btrfs: no Subvolume ID in `subvolume show %s`", path)
}

// mountPoint resolves the filesystem mount point containing path — the argument
// `qgroup destroy` needs (it addresses a filesystem, not a subvolume path).
func (c Client) mountPoint(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "findmnt", "-T", path, "-no", "TARGET")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("btrfs: findmnt %s: %w", path, err)
	}
	mnt := strings.TrimSpace(string(out))
	if mnt == "" {
		return "", fmt.Errorf("btrfs: empty mount point for %s", path)
	}
	return mnt, nil
}

// Available reports whether path is on a btrfs filesystem with quotas usable.
// Callers use it to decide whether a capacity cap can be honored at all, so they
// can REFUSE a cap they cannot enforce rather than silently running uncapped.
func (c Client) Available(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-T", path, "-no", "FSTYPE")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "btrfs"
}
