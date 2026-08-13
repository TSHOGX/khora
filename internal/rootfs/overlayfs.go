package rootfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TSHOGX/warren/internal/allocbytes"
	"github.com/TSHOGX/warren/internal/btrfs"

	"github.com/TSHOGX/warren/internal/guestport"
)

// HostOverlayFS is the single-node rootfs provider (§5.B): it assembles the
// sandbox's writable Overlay artifact as a host kernel overlayfs — lower =
// Template, upper+work = the resident Overlay directory, merged = the rootfs
// handed to runsc (rootfs-overlay D0/D3). runsc runs --overlay2=none
// --file-access=shared and never manages the write layer.
//
// Host paths are LOCKED inside this subsystem (§3.1.1 "ID addressing applies internally too"):
// upper/work/merged all derive deterministically from the OverlayID under StateDir, so the
// handle is reconstructable from a store record on resume (OpenOverlay) without keeping it in
// memory. Above this subsystem an Overlay is only {ID, Template}.
//
// mount/umount are privileged (CAP_SYS_ADMIN) and copy-up files are root-owned (rootfs-overlay
// D5), so the orchestrator runs as root (ADR-0002). That privilege and the host paths stay
// sealed here, never in the mediator. The disk-CAPACITY cap (§3.4 / §5.B) is materialized here
// as a btrfs subvolume carrying an squota limit, via the internal/btrfs leaf package
// (rootfs-overlay D6.2/D6.4).
//
// The unit is StateDir/<id>, holding BOTH upper and work — one subvolume, hence
// ONE quota covering both. That is a correctness requirement, not tidiness
// (D6.3): overlayfs copy-up writes into workdir FIRST and only then renames into
// upper, so a quota on upper alone leaves copy-up as a path straight around the
// cap. Measured with both in the one subvolume: work/upper are invisible in the
// merged view and all copy-up traffic is charged.
type HostOverlayFS struct {
	// StateDir holds per-Overlay materialization: StateDir/<id>/{upper,work,merged}.
	StateDir string
	// TemplatesDir resolves a BUILTIN TemplateID to its read-only lower dir:
	// TemplatesDir/<template>. Builtin templates are static directories shipped
	// with the deployment (boot seed, §4.3); they are deployment assets, never swept.
	TemplatesDir string
	// LayersDir holds Promote-built layer directories: LayersDir/<templateID>.
	// These are the immutable read-only layers a Promote freezes (§5.B) — the
	// bytes-first artifact whose orphans (no live templates row) the sweep
	// reclaims (§5.B / §4.4-A). Distinct from TemplatesDir so the sweep never touches a
	// deployment-shipped builtin.
	LayersDir string

	// Btrfs materializes the §3.4 capacity cap (subvolume + squota). A shared
	// substrate TOOL, not a seam — §5.C imports the same package for per-volume
	// caps (rootfs-overlay D6.4).
	Btrfs btrfs.Client
}

// unitPath is the per-Overlay storage unit holding upper+work — the thing the
// capacity quota attaches to (D6.3, see HostOverlayFS).
func (p *HostOverlayFS) unitPath(id string) string { return filepath.Join(p.StateDir, id) }

// applyCap materializes the capacity Binding on an Overlay's storage unit (§3.4 /
// §5.B). Called from BOTH create and open, because the cap is re-applied at every
// placement from the current record rather than trusted to whatever the unit was
// left carrying (§3.4 Binding).
//
// A requested cap that CANNOT be enforced is a hard error, never a silent
// downgrade to "run uncapped": ignoring it would leave every §2.3 blast-radius
// assertion believing in a cap that does not exist. capByte 0 removes any
// lingering cap, keeping the record the single authority.
// THE STRICT-INEQUALITY GUARD BELOW IS THE MATERIALIZATION-SIDE HALF of the criterion the §6.1
// write point applies (lifecycle.ErrDiskCapBelowUsage). §3.4.5 requires it in both places
// because a Binding is durable while the facts its criterion reads are not — this one reads the
// unit's CURRENT usage — so "commit → the environment changes → re-materialize" reaches here
// with no write in between. Concretely: a quota row lost to an external change lets the guest
// grow past the recorded cap, and the next placement would write that cap UNDER the usage.
//
// §5.B CANNOT TAKE §5.C's ANSWER HERE: RepairVolumeCaps skips such a volume and leaves it
// uncapped, safe there because the §3.4 second term then measures its bytes. A placement has no
// such option — §5.B's rule is "refuse the placement, never silently degrade to running
// uncapped" — so this side REFUSES rather than skips, which is also the reversible direction
// (see ErrCapBelowUsage).
func (p *HostOverlayFS) applyCap(ctx context.Context, id string, capByte int64) error {
	if capByte == CapUnchanged {
		return nil // handle-only rebuild: touch no quota (see OpenOverlay)
	}
	unit := p.unitPath(id)
	if capByte <= 0 {
		// Uncapped by intent. If the unit is a subvolume (a cap was set before and
		// has since been cleared) drop the limit, so the record stays authoritative.
		//
		// No usage guard on this arm: clearing a limit can only WIDEN what the unit may
		// hold, so it cannot wedge anything. (On an ALREADY-wedged unit the clear itself
		// returns ENOSPC — measured — but that wedge predates this call and the error is
		// reported rather than hidden.)
		if p.Btrfs.IsSubvolume(ctx, unit) {
			if err := p.Btrfs.SetQuotaLimit(ctx, unit, 0); err != nil {
				return fmt.Errorf("rootfs: clear capacity cap on %s: %w", id, err)
			}
		}
		return nil
	}
	if !p.Btrfs.IsSubvolume(ctx, unit) {
		return fmt.Errorf("rootfs: overlay %s storage unit %s is not a btrfs subvolume — refusing to run with an unenforceable %d-byte cap (§3.4)", id, unit, capByte)
	}
	// FAIL CLOSED on an unreadable row. Writing the cap blind is the IRREVERSIBLE
	// direction, and a refused placement leaves the sandbox `paused` and retryable — so
	// the asymmetry decides it, not a preference. The cost is bounded because the
	// wholesale-failure mode this read has under concurrent churn is TRANSIENT and
	// btrfs.Client.Usage retries it (measured there: 8/300 first-attempt failures → 0/300
	// after retry). Note the failure never prints our own row, so there is no
	// parse-around: the read either answers or it does not.
	used, _, err := p.Btrfs.Usage(ctx, unit)
	if err != nil {
		return fmt.Errorf("rootfs: cannot read current usage of overlay %s, refusing to apply a %d-byte cap blind (a cap at or below usage wedges the unit irreversibly, §5.B): %w", id, capByte, err)
	}
	if capByte <= used {
		return fmt.Errorf("rootfs: %w: overlay %s holds %d byte(s) and the record's cap is %d — applying it would wedge the storage unit permanently (afterwards even truncate-to-zero and the quota write itself return ENOSPC, so the cap could be neither raised nor removed, and every future placement would fail). The sandbox stays recoverable: PATCH disk_byte strictly above %d (or to 0) and the next resume applies it, or Promote it and create a fresh sandbox",
			ErrCapBelowUsage, id, used, capByte, used)
	}
	if err := p.Btrfs.SetQuotaLimit(ctx, unit, capByte); err != nil {
		return fmt.Errorf("rootfs: apply capacity cap on %s: %w", id, err)
	}
	return nil
}

// Usage reports one Overlay unit's usage and the cap on its storage. See the seam doc on
// rootfs.Provider for the contract; this is how the local btrfs adapter honors it:
//
//	unit IS a subvolume     → the qgroup row (referenced, max_referenced); an unreadable
//	                          row is an ERROR, never a synthesized zero-cap answer
//	unit is a plain dir     → allocated blocks of upper + work, limit 0 (a plain
//	                          directory has no row to read, and "no cap" is the TRUTH
//	                          there rather than a guess)
//
// The plain-directory arm enumerates `upper` and `work` BY NAME rather than walking the
// unit, for the same reason UncappedBytes does — see there. The subvolume arm needs no
// such care: `merged` is a mount point, so the extents visible through it belong to the
// layer dirs, not to this unit's qgroup.
func (p *HostOverlayFS) Usage(ctx context.Context, overlayID string) (int64, int64, error) {
	if err := validateSeg("overlayID", overlayID); err != nil {
		return 0, 0, err
	}
	unit := p.unitPath(overlayID)
	if p.Btrfs.IsSubvolume(ctx, unit) {
		used, limit, err := p.Btrfs.Usage(ctx, unit)
		if err != nil {
			return 0, 0, fmt.Errorf("rootfs: read the quota row of overlay %s: %w", overlayID, err)
		}
		return used, limit, nil
	}
	var used int64
	for _, sub := range []string{p.upperPath(overlayID), p.workPath(overlayID)} {
		n, err := blockBytes(sub)
		if err != nil {
			return 0, 0, fmt.Errorf("rootfs: measure usage of overlay %s: %w", overlayID, err)
		}
		used += n
	}
	return used, 0, nil
}

// NewHostOverlayFS constructs the provider, ensuring StateDir + LayersDir exist.
func NewHostOverlayFS(stateDir, templatesDir, layersDir string) (*HostOverlayFS, error) {
	if stateDir == "" || templatesDir == "" || layersDir == "" {
		return nil, fmt.Errorf("rootfs: StateDir, TemplatesDir and LayersDir are required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create StateDir: %w", err)
	}
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create LayersDir: %w", err)
	}
	return &HostOverlayFS{StateDir: stateDir, TemplatesDir: templatesDir, LayersDir: layersDir}, nil
}

func (p *HostOverlayFS) Name() string { return "host_overlayfs" }

// --- deterministic path derivation (host paths, private to this subsystem) ---

func (p *HostOverlayFS) upperPath(id string) string  { return filepath.Join(p.StateDir, id, "upper") }
func (p *HostOverlayFS) workPath(id string) string   { return filepath.Join(p.StateDir, id, "work") }
func (p *HostOverlayFS) mergedPath(id string) string { return filepath.Join(p.StateDir, id, "merged") }

// layerPath resolves a lower-stack ID to its on-disk read-only layer dir. A
// layer is EITHER a Promote-built layer under LayersDir OR a deployment-shipped
// builtin under TemplatesDir; we prefer LayersDir (Promote layers) and fall back
// to TemplatesDir (builtins). Both roots are private to this subsystem (§3.1.1);
// the caller only ever sees the opaque ID.
func (p *HostOverlayFS) layerPath(id string) (string, error) {
	if lp := filepath.Join(p.LayersDir, id); dirExists(lp) {
		return lp, nil
	}
	if tp := filepath.Join(p.TemplatesDir, id); dirExists(tp) {
		return tp, nil
	}
	return "", fmt.Errorf("rootfs: layer %q not found under layers or templates", id)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// mountOptionsCap is the byte ceiling on ONE mount(2) options string. The classic mount
// path copies the options into a single page and TRUNCATES past it rather than refusing, so
// overrunning it does not fail cleanly — it mounts a lower stack that is missing its tail,
// or fails with an unrelated-looking error (rootfs-overlay D5.1 ⑩: "the classic mount path
// caps its options at a single page"). Escaping it needs the per-layer `lowerdir+=` API or
// passed descriptors, neither of which this provider uses.
//
// 4096 is the page size on every platform this engine supports (ADR-0002 fixes it to Linux
// on x86-64/arm64, both 4 KiB); reading it at runtime would buy nothing checkable.
const mountOptionsCap = 4096

// maxIDLen is the lower-stack ID width the depth guarantee assumes: "tmpl_" + 16 hex chars,
// which is what a Promote's ID generator produces.
//
// It bounds a PROMOTED layer's ID exactly and a BUILTIN's not at all — a builtin TemplateID
// is a directory name the deployment chose (ListBuiltinTemplates), so no constant here can
// bound it. That is precisely why Mount keeps its own exact check on the assembled string:
// this number makes the common case refusable at the gate, and Mount is what remains
// correct when a deployment ships a 200-character builtin name.
const maxIDLen = len("tmpl_") + 16

// MaxLowerDepth reports the deepest lower stack this provider can guarantee to mount.
//
// Worst case per entry: the longer of the two layer roots, a separator, and a full-width ID.
// The fixed overhead is the options string's other two paths plus its literal keys, both
// derived from StateDir. A stack at or under the returned depth therefore always fits
// mountOptionsCap, whichever root each layer resolves under.
func (p *HostOverlayFS) MaxLowerDepth(context.Context) (int, error) {
	root := p.LayersDir
	if len(p.TemplatesDir) > len(root) {
		root = p.TemplatesDir
	}
	// The widest an entry can be: root + "/" + ID, plus the ":" joining it to the next.
	perEntry := len(root) + 1 + maxIDLen + 1
	// upperPath and workPath are the longest StateDir-derived paths in the string.
	fixed := len("lowerdir=,upperdir=,workdir=") +
		len(filepath.Join(p.StateDir, strings.Repeat("x", maxIDLen), "upper")) +
		len(filepath.Join(p.StateDir, strings.Repeat("x", maxIDLen), "work"))
	if fixed >= mountOptionsCap {
		return 0, fmt.Errorf("rootfs: state dir is too long to mount any lower stack "+
			"(%d bytes of fixed mount options against a %d-byte cap)", fixed, mountOptionsCap)
	}
	depth := (mountOptionsCap - fixed) / perEntry
	if depth <= 0 {
		return 0, fmt.Errorf("rootfs: layer dir is too long to mount even one lower layer "+
			"(%d bytes per entry, %d bytes left of a %d-byte cap)",
			perEntry, mountOptionsCap-fixed, mountOptionsCap)
	}
	return depth, nil
}

// handle builds the above-subsystem Overlay view ({ID, Template, Lower}); host
// paths are derived on demand from the IDs, not stored (see rootfs.go).
func (p *HostOverlayFS) handle(overlayID string, lower []string) *Overlay {
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &Overlay{ID: overlayID, Template: tmpl, Lower: append([]string(nil), lower...)}
}

// CreateOverlay allocates an empty upper over a read-only lower stack (① create
// prefix). It does NOT mount.
//
// When a capacity cap is requested the storage unit is created as a btrfs
// SUBVOLUME so the quota has something to attach to; uncapped Overlays stay plain
// directories (no subvolume, nothing to reclaim beyond the bytes).
func (p *HostOverlayFS) CreateOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error) {
	if err := p.validateStack(overlayID, lower); err != nil {
		return nil, err
	}
	for _, id := range lower {
		if _, err := p.layerPath(id); err != nil {
			return nil, err
		}
	}
	if capByte > 0 {
		if !p.Btrfs.Available(ctx, p.StateDir) {
			return nil, fmt.Errorf("rootfs: overlay %s requests a %d-byte capacity cap but StateDir is not on btrfs — refusing to run uncapped (§3.4)", overlayID, capByte)
		}
		if err := p.Btrfs.CreateSubvolume(ctx, p.unitPath(overlayID)); err != nil {
			return nil, fmt.Errorf("rootfs: create capacity unit: %w", err)
		}
	}
	if err := os.MkdirAll(p.upperPath(overlayID), 0o755); err != nil {
		return nil, fmt.Errorf("rootfs: create upper: %w", err)
	}
	if err := p.applyCap(ctx, overlayID, capByte); err != nil {
		return nil, err
	}
	return p.handle(overlayID, lower), nil
}

// OpenOverlay reconstructs a handle for an existing artifact (resume prefix) and
// RE-APPLIES the capacity cap from the current record (§3.4 Binding: every
// placement re-materializes the cap rather than reading back a frozen value).
func (p *HostOverlayFS) OpenOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error) {
	if err := p.validateStack(overlayID, lower); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.upperPath(overlayID)); err != nil {
		return nil, fmt.Errorf("rootfs: Overlay %q upper missing (not created?): %w", overlayID, err)
	}
	if err := p.applyCap(ctx, overlayID, capByte); err != nil {
		return nil, err
	}
	return p.handle(overlayID, lower), nil
}

// Mount assembles lower(Template)/upper/work → merged (① of the cold-start
// chain). A FRESH empty workdir is required per (re)mount — the kernel rejects
// or corrupts a reused work (rootfs-overlay D5). resume re-mounts the SAME
// upper (pause captured nothing, §4.2).
func (p *HostOverlayFS) Mount(_ context.Context, o *Overlay) (*Merged, error) {
	id := o.ID
	upper := p.upperPath(id)
	work := p.workPath(id)
	merged := p.mergedPath(id)

	// Resolve the lower stack to host dirs. overlayfs lowerdir is COLON-separated,
	// leaf-first (lowerdir=leaf:...:base) — the SAME leaf-first order as o.Lower
	// (§5.B multi-lowerdir: the new Template's lower stack = [the new layer, ...the
	// parent stack]).
	lowerDirs := make([]string, 0, len(o.Lower))
	for _, lid := range o.Lower {
		lp, err := p.layerPath(lid)
		if err != nil {
			return nil, fmt.Errorf("rootfs: mount lower: %w", err)
		}
		lowerDirs = append(lowerDirs, lp)
	}
	if len(lowerDirs) == 0 {
		return nil, fmt.Errorf("rootfs: mount overlay %s: empty lower stack", id)
	}

	// A fresh workdir each mount means EMPTIED, not RECREATED, and the difference is
	// load-bearing on a full unit rather than stylistic.
	//
	// `RemoveAll(work)` + `MkdirAll(work)` cannot resume a sandbox that filled its own
	// capacity cap: `mkdir` needs to allocate a metadata extent, and a full unit has only
	// tens of KiB of headroom (measured: a 16 MiB cap fills to 16728064 B, leaving 49152 B,
	// and the mkdir returns ENOSPC). Removing the directory frees nothing, because it was
	// already empty — so the sequence spends headroom it does not have to reach a state it
	// was already in. That made a sandbox which merely hit the ordinary §3.4 ENOSPC
	// PERMANENTLY UNRESUMABLE, breaking §4.2's promise that pause is a REVERSIBLE gate (the
	// same promise §3.4.4 pays a whole budget to keep). Emptying the CONTENTS keeps the
	// directory's existing inode, and the kernel then creates `work/work` itself — measured
	// to mount cleanly on a unit with that same 49 KiB of headroom.
	//
	// The overlayfs requirement is that workdir be EMPTY (the kernel rejects or corrupts a
	// reused one, rootfs-overlay D5); it is not that the directory be a new inode. Emptying
	// satisfies it exactly.
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("rootfs: mkdir %s: %w", d, err)
		}
	}
	if err := emptyDir(work); err != nil {
		return nil, fmt.Errorf("rootfs: clear stale work: %w", err)
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowerDirs, ":"), upper, work)
	// The EXACT limit, checked on the assembled string — MaxLowerDepth's number is a
	// worst-case guarantee, and this is what the kernel actually gets. Past the cap the
	// classic mount path truncates instead of refusing (mountOptionsCap), so without this
	// the failure is either a silently short lower stack or an error naming nothing.
	if len(opts) > mountOptionsCap {
		return nil, fmt.Errorf("rootfs: mount overlay %s: lower stack of %d layers needs %d "+
			"bytes of mount options, over the %d-byte limit (rootfs-overlay D5.1 ⑩)",
			id, len(lowerDirs), len(opts), mountOptionsCap)
	}
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return nil, fmt.Errorf("rootfs: mount overlay at %s (%s): %w", merged, opts, err)
	}
	return &Merged{MergedPath: merged}, nil
}

// Unmount tears down the merged overlay (pause / destroy). upper persists
// across pause (§4.2 zero-capture).
func (p *HostOverlayFS) Unmount(_ context.Context, m *Merged) error {
	if m == nil || m.MergedPath == "" {
		return nil
	}
	if err := syscall.Unmount(m.MergedPath, 0); err != nil {
		// MNT_DETACH fallback for a busy mount (lazy unmount, cleaned when refs drop).
		if err2 := syscall.Unmount(m.MergedPath, syscall.MNT_DETACH); err2 != nil {
			return fmt.Errorf("rootfs: unmount %s: %w (detach: %v)", m.MergedPath, err, err2)
		}
	}
	return nil
}

// DestroyOverlay reclaims upper+work bytes (terminal destroy, §4.1),
// synchronously. A failure here is retried by SweepOrphanOverlays, whose
// predicate is "no live sandbox row" — not refcount GC (model.md: the Overlay
// artifact is the degenerate case, referenced by exactly one sandbox row).
// Separate from placement teardown; the mediator invokes each (§4 subsystems
// don't call each other).
// The capacity unit is reclaimed along with the bytes: the subvolume AND its
// qgroup (a deleted subvolume otherwise leaves a stale `<under deletion>` qgroup
// row carrying the old limit — measured), so destroy leaves no quota residue
// behind (§4.4-A reclamation discipline).
func (p *HostOverlayFS) DestroyOverlay(ctx context.Context, o *Overlay) error {
	dir := p.unitPath(o.ID)
	// best-effort unmount first in case merged is still mounted.
	_ = syscall.Unmount(p.mergedPath(o.ID), syscall.MNT_DETACH)
	// Empty the unit's contents FIRST: `subvolume delete` requires the subvolume
	// itself, and its children (upper/work/merged dirs) are ordinary directories
	// inside it, so they go by RemoveAll. For an uncapped (plain-dir) Overlay this
	// single call is the whole reclamation.
	if p.Btrfs.IsSubvolume(ctx, dir) {
		for _, sub := range []string{p.upperPath(o.ID), p.workPath(o.ID), p.mergedPath(o.ID)} {
			if err := os.RemoveAll(sub); err != nil {
				return fmt.Errorf("rootfs: destroy overlay %s: clear %s: %w", o.ID, sub, err)
			}
		}
		if err := p.Btrfs.DeleteSubvolume(ctx, dir); err != nil {
			return fmt.Errorf("rootfs: destroy overlay %s: %w", o.ID, err)
		}
		// the (now-empty) unit dir itself
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("rootfs: destroy overlay %s: %w", o.ID, err)
		}
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("rootfs: destroy overlay %s: %w", o.ID, err)
	}
	return nil
}

// UncappedBytes reports the bytes this provider holds that NO declared capacity
// cap covers — the second term of the §3.4 inequality (rootfs-overlay D6.5).
// See the seam doc on rootfs.Provider for the contract; this is how the local
// btrfs adapter honors it:
//
//	layer dirs (LayersDir + TemplatesDir) → counted: read-only, charged to no
//	                                        sandbox, yet real disk
//	Overlay unit WITH a quota limit        → ZERO: its cap is already a committed
//	                                        promise the mediator counts
//	Overlay unit with NO limit            → upper + work, enumerated explicitly
//
// Two subtleties, both load-bearing:
//
// "Capped" is keyed on the qgroup LIMIT, not on IsSubvolume: applyCap can clear a limit to 0 and
// leave the subvolume in place, so a subvolume is not a reliable proxy for a cap.
//
// The uncapped branch enumerates upper and work BY NAME rather than walking the unit dir.
// `merged` lives inside the unit, and for a running sandbox that is the live overlayfs mount
// exposing lower ∪ upper — walking the unit would descend into it and count the whole Template
// stack a second time, already counted from TemplatesDir. (Usage's plain-dir arm inherits this.)
//
// Bytes come from the BACKEND: the qgroup for capped units, allocated blocks for
// everything else — never apparent size, never guest-side numbers (see
// blockBytes and rootfs.Provider.UncappedBytes).
func (p *HostOverlayFS) UncappedBytes(ctx context.Context) (int64, error) {
	var total int64

	// BOTH layer roots count. Omitting TemplatesDir is the exact undercount D6.2
	// warns about: it would overcommit the node by the whole Template library,
	// which on a fresh deployment is ALL of it.
	for _, root := range []string{p.LayersDir, p.TemplatesDir} {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, fmt.Errorf("rootfs: measure layers under %s: %w", root, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			n, err := blockBytes(filepath.Join(root, e.Name()))
			if err != nil {
				return 0, fmt.Errorf("rootfs: measure layer %s: %w", e.Name(), err)
			}
			total += n
		}
	}

	// Overlay units. A unit carrying a quota LIMIT is covered by the cap term and
	// must contribute nothing here; only an uncapped unit's bytes belong to this
	// term.
	units, err := os.ReadDir(p.StateDir)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("rootfs: measure overlays: %w", err)
	}
	for _, e := range units {
		if !e.IsDir() {
			continue
		}
		unit := p.unitPath(e.Name())
		if p.Btrfs.IsSubvolume(ctx, unit) {
			used, limit, err := p.Btrfs.Usage(ctx, unit)
			if err != nil {
				// Unreadable qgroup is an ERROR, not "assume uncapped". Enumerating here
				// billed a CAPPED unit's bytes on top of its own cap — the `limit > 0` skip
				// below is unreachable on this path — which §3.4.2's disjointness rule
				// forbids and which inflates the admission gate's left side into a wrongful
				// 507. The §5.C arm carries the same reasoning at length; the two must stay
				// identical, since one term's shape is the other's.
				return 0, fmt.Errorf("rootfs: measure overlay %s: %w", e.Name(), err)
			}
			if limit > 0 {
				continue // capped → the cap term already counts it (invariant 1)
			}
			// Subvolume with no limit: the qgroup is still the backend's own statement
			// about these bytes, and it excludes reflinked extents shared with the layer
			// (invariant 2), so prefer it over a walk.
			total += used
			continue
		}
		// GENUINELY not a quota unit: enumerate upper + work explicitly, never the unit
		// dir — `merged` lives in there too (see the doc comment).
		for _, sub := range []string{p.upperPath(e.Name()), p.workPath(e.Name())} {
			n, err := blockBytes(sub)
			if err != nil {
				return 0, fmt.Errorf("rootfs: measure overlay %s: %w", e.Name(), err)
			}
			total += n
		}
	}
	return total, nil
}

// PromoteCostBytes forecasts what freezing this Overlay would add to the uncapped
// term (§3.4 / D6.5). See the seam doc on rootfs.Provider for the contract; this
// is how the local adapter honors it: PromoteToTemplate is a byte-for-byte copy of
// the upper into LayersDir, so the cost is the upper's projected size on disk.
//
// It deliberately does NOT reuse blockBytes. That one reports ALLOCATED blocks,
// which is right for measuring bytes already on disk but WRONG as a forecast of
// this copy: copyFile is an io.Copy and does not punch holes, so a sparse file
// with st_blocks=0 and st_size=512 MiB measures as 0 here and costs 512 MiB in the
// layer. Under-reporting is the one direction the gate cannot tolerate, so each
// regular file is projected at max(allocated, apparent) — apparent covers the
// sparse case, allocated covers a file whose blocks exceed its length.
//
// One deliberate overstatement, in the SAFE direction (seam contract point 2):
// hardlinks are counted per link (copyTree does not re-link, so each link really does
// become an independent file in the layer — this is the one place a per-link count is
// not a double charge).
//
// The source is at-rest by convention only — the mediator gates BEFORE quiescing —
// so a concurrent guest write makes this stale on return. That is stated at the
// seam; the number is a bound at the instant of admission, like the term it feeds.
func (p *HostOverlayFS) PromoteCostBytes(_ context.Context, o *Overlay, excludedMounts []string) (int64, error) {
	if o == nil {
		return 0, fmt.Errorf("rootfs: promote cost: nil overlay")
	}
	upper := p.upperPath(o.ID)
	// THE SAME exclusion set the freeze will use, built by the same function — otherwise
	// the forecast charges shadowed bytes the freeze is about to prune, refusing Promotes
	// against capacity that was never going to be consumed (promoteExclusions).
	excluded := promoteExclusions(excludedMounts)
	var total int64
	err := filepath.Walk(upper, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // raced with a destroy; a missing file costs nothing to copy
			}
			return err
		}
		if rel, relErr := filepath.Rel(upper, path); relErr == nil && rel != "." && excluded[filepath.Clean(rel)] {
			// Excluded: the freeze will not copy it, so it costs nothing. A DIRECTORY
			// prunes its whole subtree, matching copyTree's semantics exactly — which is
			// what makes "both calls use the same set" a real guarantee rather than two
			// implementations that happen to agree today.
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // dirs/symlinks/devices: no data bytes copied
		}
		size := info.Size()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if alloc := st.Blocks * 512; alloc > size {
				size = alloc
			}
		}
		total += size
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("rootfs: promote cost for overlay %s: %w", o.ID, err)
	}
	return total, nil
}

// blockBytes sums the ALLOCATED bytes of regular files under root — st_blocks×512,
// the host filesystem's own statement of what those files cost.
//
// Not apparent size (st_size): a sparse file's apparent size overstates its cost
// without bound (the spec's own example is a 512 MiB sparse file whose real
// allocation is 0), and in an admission gate overstating means refusing creates
// that would have fit. Allocated blocks are equal-or-better in every case — exact
// for ordinary files, correct for holes, and they reflect compression.
//
// This does NOT contradict §5.B's "never trust st_blocks": that rule bans
// GUEST-side numbers (see rootfs.Provider.UncappedBytes). Host-side st_blocks is
// the backend's own allocation statement, which is the source the rule points at.
//
// The measurement itself lives in the internal/allocbytes LEAF because §5.C needs the
// identical one for its own half of the §3.4 second term and subsystems must not import
// each other (§4; internal/guestport is the precedent). This wrapper stays so the
// reasoning above sits where §5.B's callers read it.
func blockBytes(root string) (int64, error) {
	return allocbytes.Tree(root)
}

// AdoptMerged reconstructs the merged handle for an Overlay still mounted after
// a crash (§4.4-A). The merged dir is deterministic from the ID; we confirm it
// is actually an overlay mount (kernel state held by the surviving container)
// before handing back the handle — a not-mounted merged means the container is
// not really live, so the caller should not adopt it.
func (p *HostOverlayFS) AdoptMerged(_ context.Context, o *Overlay) (*Merged, error) {
	merged := p.mergedPath(o.ID)
	mounted, err := isMountpoint(merged)
	if err != nil {
		return nil, fmt.Errorf("rootfs: adopt merged %s: %w", o.ID, err)
	}
	if !mounted {
		return nil, fmt.Errorf("rootfs: overlay %s not mounted (container not live)", o.ID)
	}
	return &Merged{MergedPath: merged}, nil
}

// ListBuiltinTemplates enumerates the static Template directories under
// TemplatesDir (boot seed, §4.3). Each subdirectory name is a builtin TemplateID;
// its bytes are the read-only lower shipped with the deployment (bytes-first is
// inherent — the dir exists before any row). The host TemplatesDir path never
// leaves this subsystem — only the opaque IDs (§3.1.1). A missing TemplatesDir
// is treated as "no builtins" (empty), not an error: a deployment may ship only
// Promote-built Templates.
func (p *HostOverlayFS) ListBuiltinTemplates(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(p.TemplatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("rootfs: list templates: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// SweepOrphanOverlays reclaims Overlay storage units under StateDir with no live
// `sandboxes` row (§4.4-A reclamation discipline). See the seam doc on
// rootfs.Provider for the contract and for why `building` is a separate keep-set
// from `live`; this is how the local adapter honors it:
//
//	each StateDir subdirectory IS a unit, named by its OverlayID (the paths are
//	derived from the ID, so enumeration recovers the identity without a store
//	read), and one not in either keep-set goes through DestroyOverlay.
//
// Failure policy is SKIP-AND-LOG-SHAPED (§4.4-A): a unit that will not delete
// does not abort the pass over the others — the error is returned after the loop
// completes, along with the count actually reclaimed, so one wedged subvolume
// cannot indefinitely shadow every other leak. The sweep is idempotent, so
// whatever failed is retried next pass. A missing StateDir → nothing to sweep.
func (p *HostOverlayFS) SweepOrphanOverlays(ctx context.Context, live, building []string) (int, error) {
	keep := make(map[string]bool, len(live)+len(building))
	for _, id := range live {
		keep[id] = true
	}
	for _, id := range building {
		keep[id] = true
	}
	entries, err := os.ReadDir(p.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("rootfs: sweep list overlay units: %w", err)
	}
	reclaimed := 0
	var failed []string
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		// Orphan: bytes with no live row and no create in flight. Either destroy
		// deleted the record after DestroyOverlay failed (§4.4 licenses exactly
		// that, deferring the retry to a sweep — this one), or the process died
		// between the two. Either way the row is gone for good, so nothing above
		// will ever ask about these bytes again and they are safe to drop.
		//
		// A handle is enough to name the unit: DestroyOverlay derives every path
		// from the ID, so the lower stack is irrelevant here — the unit is being
		// deleted, not mounted.
		if err := p.DestroyOverlay(ctx, &Overlay{ID: e.Name()}); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		reclaimed++
	}
	if len(failed) > 0 {
		return reclaimed, fmt.Errorf("rootfs: sweep reclaim overlay unit(s): %s", strings.Join(failed, "; "))
	}
	return reclaimed, nil
}

// SweepOrphanLayers reclaims Promote-built layer dirs under LayersDir with no
// live `templates` row (§5.B / §4.4-A). It reuses the §4.4-A reverse-reconcile shape: the
// authoritative set (`live` + `building`) is passed in, the provider diffs it
// against what is on disk and deletes the difference. `building` protects an
// in-flight Promote whose bytes are down but whose row is not yet inserted
// (bytes-first, §5.B) — without it a concurrent sweep would tear the layer out
// from under the Promote. A missing LayersDir → nothing to sweep.
func (p *HostOverlayFS) SweepOrphanLayers(_ context.Context, live, building []string) (int, error) {
	keep := make(map[string]bool, len(live)+len(building))
	for _, id := range live {
		keep[id] = true
	}
	for _, id := range building {
		keep[id] = true
	}
	entries, err := os.ReadDir(p.LayersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("rootfs: sweep list layers: %w", err)
	}
	reclaimed := 0
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		// orphan: a layer dir with no live row and not being built → reclaim its
		// bytes (§5.B / §4.4-A). The row-vs-bytes decoupling means the row is already gone
		// (DELETE template) or never landed (crashed Promote); either way the
		// bytes are safe to drop.
		if err := os.RemoveAll(filepath.Join(p.LayersDir, e.Name())); err != nil {
			return reclaimed, fmt.Errorf("rootfs: sweep reclaim layer %s: %w", e.Name(), err)
		}
		reclaimed++
	}
	return reclaimed, nil
}

// emptyDir removes everything INSIDE dir, keeping dir itself. See Mount for why the
// distinction matters: on a unit at its capacity cap, recreating the directory needs a
// metadata extent the quota has no room for, while emptying it needs none. An absent dir is
// not an error — the caller has just ensured it exists, and "nothing inside" is the
// requested state either way.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// isMountpoint reports whether path is a mount point by comparing its device
// number to its parent's (a mount boundary changes st_dev). Cheap, no /proc
// parse, correct for overlayfs.
func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev, nil
}

// validateStack keeps the overlay ID and every lower-stack ID to a safe single
// path segment (no traversal / separators), and requires a non-empty stack.
func (p *HostOverlayFS) validateStack(overlayID string, lower []string) error {
	if err := validateSeg("overlayID", overlayID); err != nil {
		return err
	}
	if len(lower) == 0 {
		return fmt.Errorf("rootfs: empty lower stack for overlay %q", overlayID)
	}
	for _, id := range lower {
		if err := validateSeg("lower", id); err != nil {
			return err
		}
	}
	return nil
}

// validateSeg rejects an empty or path-unsafe single segment (also used by
// PromoteToTemplate for the new layer ID).
func validateSeg(name, v string) error {
	if v == "" {
		return fmt.Errorf("rootfs: empty %s", name)
	}
	if strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
		return fmt.Errorf("rootfs: unsafe %s %q", name, v)
	}
	return nil
}

// promoteExcludedPaths is the minimal excluded-path set (§5.B / §3.1 Promote;
// rootfs-overlay D5): engine-injected guest-runtime paths that must NOT be
// captured into a Promoted layer. Today these are the sandboxd transport portal
// files, written upper-relative (the guest daemon writes them under /tmp, which
// is the upper's tmp/). These MUST track the guest daemon's own paths in
// cmd/sandboxd — a rename there without a matching edit here leaks a runtime
// file into a frozen layer, and there is no compile coupling to catch it
// (separate binaries). It is the STATIC half of the exclusion set; a Promote's
// volume mount destinations are merged in per-call by promoteExclusions.
var promoteExcludedPaths = []string{
	"tmp/.sandboxd-transport.pid", // transport child pidfile (§5.E.1)
	// Crash-loop sentinel: only a `testguest` build reads it
	// (cmd/sandboxd/faultinject_testguest.go), but the exclusion is
	// unconditional — capturing it would mint a Template whose every sandbox is
	// born unable to serve, and one list entry is cheaper than that failure.
	"tmp/.sandboxd-crashloop",
}

// promoteExcludedPrefixes excludes engine-injected debris by BASE-NAME prefix,
// for the cases where the exact path is not knowable in advance.
//
// The streaming upload's temp file is the reason this exists: it must be created in
// the CALLER'S destination directory (rename(2) cannot cross filesystems, and /tmp
// is a separate tmpfs), so it can appear anywhere the caller writes. A crash
// mid-upload strands one, and an exact-path list cannot cover "anywhere". Freezing
// that debris into a Template would ship it to every sandbox created from it.
//
// The prefix comes from internal/guestport, imported by BOTH binaries, so a rename
// cannot silently drift the way the exact-path list above can.
var promoteExcludedPrefixes = []string{
	guestport.UploadTempPrefix,
}

// PromoteToTemplate freezes the source Overlay's at-rest upper into a new
// read-only layer dir under LayersDir (§5.B Promote). bytes-first: this writes
// ONLY bytes — the caller inserts the templates row afterward (§4.3 INV-3). The
// copy excludes the engine-injected runtime paths (§5.B / §3.1; rootfs-overlay D5).
// The caller must have quiesced + unmounted the sandbox first, so upper is a stable
// plain dir tree
// (§2.5). The layer is built at a staging path then atomically renamed into
// place, so a crash mid-copy never leaves a half-written layer at the final ID
// (a staging dir is a plain orphan the sweep reclaims).
//
// Freezing a layer INCREASES the "bytes under no cap" term of the §3.4 node
// inequality (rootfs-overlay D6.5), so the mediator gates a Promote on
// PromoteCostBytes before calling this — the forecast of what this copy will add.
// This function does not gate anything itself: it is a mechanism (§4).
func (p *HostOverlayFS) PromoteToTemplate(_ context.Context, o *Overlay, newTemplateID string, excludedMounts []string) error {
	if err := validateSeg("newTemplateID", newTemplateID); err != nil {
		return err
	}
	upper := p.upperPath(o.ID)
	if _, err := os.Stat(upper); err != nil {
		return fmt.Errorf("rootfs: promote: source upper missing: %w", err)
	}
	dst := filepath.Join(p.LayersDir, newTemplateID)
	if dirExists(dst) {
		return fmt.Errorf("rootfs: promote: layer %q already exists", newTemplateID)
	}
	// stage under a sibling temp dir on the SAME FS (LayersDir) so the final
	// rename is atomic. A crash before the rename leaves the staging dir; it is
	// NOT at the final ID, so no row will ever reference it → sweep reclaims it.
	staging := dst + ".staging"
	_ = os.RemoveAll(staging)
	if err := copyTree(upper, staging, promoteExclusions(excludedMounts)); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("rootfs: promote: copy upper: %w", err)
	}
	if err := os.Rename(staging, dst); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("rootfs: promote: finalize layer: %w", err)
	}
	return nil
}

// promoteExclusions merges the STATIC engine-injected paths with THIS Promote's
// volume mount destinations, and it is the single place the two are combined so the
// freeze and the forecast cannot be given different sets.
//
// The two halves exist for different reasons and neither replaces the other: the
// static list drops engine-injected runtime debris, while the per-Promote destinations
// prune the pre-existing bytes a mount shadows (rootfs.Provider.PromoteToTemplate says
// what for; rootfs-overlay D5 argues it).
//
// Destinations arrive as absolute guest paths and are converted to the upper-relative,
// leading-slash-stripped form the existing convention uses. NESTING falls out of
// copyTree's semantics: an excluded directory skips its whole subtree, so a nested
// destination prunes THE LEAF SUBTREE and leaves an empty intermediate chain — the
// settled choice (D5).
func promoteExclusions(excludedMounts []string) map[string]bool {
	excluded := make(map[string]bool, len(promoteExcludedPaths)+len(excludedMounts))
	for _, e := range promoteExcludedPaths {
		excluded[filepath.Clean(e)] = true
	}
	for _, dest := range excludedMounts {
		rel := strings.TrimPrefix(filepath.Clean("/"+strings.TrimSpace(dest)), "/")
		if rel == "" || rel == "." {
			// A destination of "/" would exclude the entire upper. It cannot arise (the
			// mount-path validation rejects "/"), and silently excluding everything is the
			// worst possible response to it, so it is dropped rather than honored.
			continue
		}
		excluded[rel] = true
	}
	return excluded
}

// copyTree recursively copies src into dst, skipping paths (relative to src,
// cleaned) present in excluded. Preserves mode bits and symlinks. dst is created.
// Files are copied byte-for-byte (the layer is a plain read-only dir tree, §5.B).
//
// Byte-for-byte is deliberate, and `cp --reflink` is a TRAP here — the obvious
// "make Promote cheap" optimisation leaks a btrfs qgroup row on EVERY Promote,
// and the leak outlives the layer it came from. Measured on the squota substrate:
// a reflink INTO a unit does not leak (copy-up's shape — the unit does not own
// those extents), but a reflink OUT of one does: deleting the source afterwards
// leaves a `<squota space holder>` row that never self-cleans, and a leaked row
// does not reserve its own subvolume id, so a later remount can hand that id and
// its stale cap to a brand-new unit (see internal/btrfs.DeleteSubvolume and
// docs/spec/deep-dives/volume.md D7). Reflink here would need the sweep-side
// reclamation path built first.
func copyTree(src, dst string, excluded map[string]bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." && excluded[filepath.Clean(rel)] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Prefix exclusion, for debris whose exact path is not knowable in advance
		// (see promoteExcludedPrefixes). Matched on the BASE NAME, since an upload
		// temp file can be left in any directory the caller writes to.
		if rel != "." && !info.IsDir() {
			base := filepath.Base(rel)
			for _, pre := range promoteExcludedPrefixes {
				if strings.HasPrefix(base, pre) {
					return nil
				}
			}
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			// skip devices/sockets/pipes — a frozen read-only layer holds only a
			// normal file tree (an engine-injected socket is excluded anyway).
			return nil
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
