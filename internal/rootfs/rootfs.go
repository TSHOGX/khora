// Package rootfs is subsystem the Rootfs subsystem: Template (read-only lower) + the sandbox's writable
// Overlay artifact, assembled as a merged rootfs via the host kernel's overlayfs — runsc
// runs with --overlay2=none --file-access=shared and never manages the write layer itself
// (the substrate requirements / the Rootfs subsystem; overlayfs constraints/rootfs storage).
//
// The rootfs provider is the Rootfs subsystem storage seam (never "FS provider" — `fs` is the transport data plane wire
// service, the engine vocabulary boundary). It exposes "OverlayID + referenced Template + at-rest readable" and never
// "it is a host upper path" (the ID-addressed Overlay model); host paths stay locked inside this subsystem. Ships
// the single-node resident-upper-directory unit (rootfs storage).
package rootfs

import (
	"context"
	"errors"
)

// ErrCapBelowUsage refuses a capacity cap that is not STRICTLY above the storage unit's
// current usage (resource admission / the quota-lowering constraint).
//
// The asymmetry is the reason: imposing such a cap wedges the unit permanently — the quota
// write itself then returns ENOSPC, so the cap can be neither raised nor removed, and
// placement re-applies the record's cap every time, leaving the sandbox unresumable.
// Refusing leaves a `paused` sandbox with ordinary exits (PATCH the cap, or Promote and
// recreate). `cap == usage` is included because it wedges the same way. It never fires on a
// sandbox that merely filled its own cap: squota stops the guest while usage is still
// strictly below the limit.
var ErrCapBelowUsage = errors.New("rootfs: capacity cap must be strictly greater than the storage unit's current usage")

// CapUnchanged is the OpenOverlay capByte sentinel for "rebuild the handle, do
// not touch the quota" (see OpenOverlay). Negative, so it can never collide with
// a real cap or with 0 = uncapped — the distinction matters because 0 is itself a
// meaningful instruction (remove the cap).
const CapUnchanged int64 = -1

// Overlay is the ID-addressed, Template-decoupled frozen layer (the ID-addressed Overlay model). Above
// this subsystem it is referenced by ID; the host materialization is private.
type Overlay struct {
	ID       string // OverlayID (opaque above the Rootfs subsystem)
	Template string // the immediate Template it references (the ID-addressed Overlay model Binding; = Lower[0])

	// Lower is the resolved multi-lowerdir stack, LEAF-FIRST: Lower[0] is the referenced
	// Template, the tail its ancestors down to the base; len 1 for a base Template.
	//
	// The MEDIATOR computes it by walking the authoritative `templates.parent` chain (health and reclamation
	// lineage) and passes the ID stack across the seam — the provider only maps each ID to
	// its layer dir (the lifecycle subsystems don't read the store).
	Lower []string
}

// Host paths (upper/work/merged) are deliberately absent from Overlay: they derive from ID
// under the provider's StateDir, so nothing above the Rootfs subsystem ever sees them (the ID-addressed Overlay model). upper/work
// must share one overlayfs-capable FS and one quota unit (rootfs storage).

// Merged is a handle to an assembled merged rootfs, handed to the runtime provider as an
// opaque RootfsSource (the runtime provider contract). Only this subsystem constructs or destructs one.
type Merged struct {
	MergedPath string // the overlayfs merged dir; consumed by local_runsc only
}

// Provider is the Rootfs subsystem rootfs-provider storage seam.
type Provider interface {
	Name() string

	// CreateOverlay allocates an empty writable Overlay over a read-only lower STACK (the
	// create prefix of the cold-start chain, ; the cold-start sequence). lower is leaf-first.
	//
	// capByte is the resource admission disk-capacity cap, applied to the upper+work unit. It crosses the
	// seam as a PLAIN BYTE COUNT: the caller states intent, the provider picks the mechanism
	// — which is why capacity is not a cross-cutting QuotaProvider interface, since that
	// would weld the btrfs assumption into every implementation (the shared btrfs mechanism).
	// 0 = uncapped.
	CreateOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error)

	// OpenOverlay reconstructs the handle for an EXISTING Overlay from its ID + resolved
	// lower stack — the resume prefix. The handle is rebuildable from the record rather than
	// held in memory (the ID-addressed Overlay model), and the artifact survives pause (FS-first pause and resume). Allocates no bytes.
	//
	// capByte RE-APPLIES the record's cap at each placement rather than trusting whatever
	// the unit was left carrying, so an edited cap takes effect on resume and a quota lost
	// to an external change is restored. 0 = uncapped, and also REMOVES any cap still on the
	// unit (the record is the authority).
	//
	// CapUnchanged means "rebuild the handle, touch no quota" — for callers not materializing
	// a placement (destroy, a Promote byte-read, orphan reclamation, adopting a survivor).
	// They must not pass 0 instead: a handle rebuild is not a statement that the sandbox is
	// uncapped, and it would silently un-cap a live one.
	OpenOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error)

	// Mount assembles lower/upper/work into a merged rootfs ( of the cold-start chain). The
	// mount lifecycle belongs to this subsystem (rootfs seam ownership). Resume re-mounts the SAME
	// artifact — pause captured nothing (FS-first pause and resume).
	Mount(ctx context.Context, o *Overlay) (*Merged, error)

	// MaxLowerDepth reports the longest lower STACK this provider can actually mount, so the
	// mediator can refuse a too-deep lineage BEFORE acting on it. Mount enforces the real
	// limit itself; this is the number that lets a refusal happen at the gate instead.
	//
	// It crosses the seam as a PLAIN COUNT for the same reason capByte does: the ceiling is a
	// function of host path lengths and ID width, both private to this subsystem (the ID-addressed Overlay model), so
	// the mediator cannot compute it and must not learn its inputs.
	//
	// The value is an UNDER-ESTIMATE where a stack's real cost varies (mixed ID widths, two
	// layer roots of different lengths): a provider reports the depth it can guarantee for a
	// worst-case stack, so a stack at or under it always mounts. 0 = no limit.
	MaxLowerDepth(ctx context.Context) (int, error)

	// Unmount tears down the merged overlay (pause / destroy). The upper persists across pause.
	Unmount(ctx context.Context, m *Merged) error

	// DestroyOverlay reclaims the upper+work bytes synchronously (the lifecycle state machine); a failure is retried
	// by SweepOrphanOverlays. Separate from placement teardown — the mediator invokes each (the lifecycle).
	DestroyOverlay(ctx context.Context, o *Overlay) error

	// AdoptMerged reconstructs the handle for an Overlay STILL mounted after a control-plane
	// crash (atomic transition rollback). The overlayfs mount is kernel state held by the still-running container,
	// so Adopt recovers the handle rather than re-mounting. It does NOT mount: a not-mounted
	// overlay is a caller error, since that sandbox's container is not actually live.
	AdoptMerged(ctx context.Context, o *Overlay) (*Merged, error)

	// ListBuiltinTemplates enumerates the static Template directories the deployment ships
	// (boot seed — existence-as-state health and reclamation, idempotent under atomic transition rollback). Returns the directory
	// names, which ARE the builtin TemplateIDs; bytes-first is naturally satisfied, the dirs
	// existing before any row. TemplatesDir itself stays private (the ID-addressed Overlay model).
	ListBuiltinTemplates(ctx context.Context) ([]string, error)

	// PromoteToTemplate freezes a sandbox's at-rest Overlay upper into a NEW read-only layer
	// dir addressed by newTemplateID (the Rootfs subsystem / the Template and Overlay model Promote). The mediator has already quiesced
	// the sandbox and torn down its mount, so the upper is a plain host tree safe to read. It
	// copies the upper into the layer dir, excluding the engine-injected runtime paths that
	// must not be captured (rootfs mount constraints). It inserts NO store row and wires no lineage —
	// that is the mediator's bytes-first sequel (health and reclamation INV-3), and a crash in between leaves an
	// orphan layer the sweep reclaims.
	//
	// excludedMounts are this sandbox's Volume mount destinations, per-Promote rather than a
	// package list because a destination is per-sandbox — and because the mediator already
	// holds the mount table, so the Rootfs subsystem never has to ask the Volume subsystem anything (the lifecycle).
	//
	// What they are FOR, since the obvious guess is wrong: not creating the empty placeholder
	// (that is the default outcome, measured). They PRUNE PRE-EXISTING BYTES THE MOUNT
	// SHADOWS — bytes written to the upper before the volume was bound over that path, which
	// are invisible to every guest yet would be frozen into the layer and counted in both the
	// cap and the cost forecast (rootfs mount constraints).
	//
	// Paths are relative to the upper, leading slash stripped. Nested destinations prune the
	// LEAF SUBTREE and accept an empty intermediate chain (rootfs mount constraints): pruning only
	// the leaf leaves ghost intermediates, excluding a top-level component discards real
	// guest data living beside the mount.
	PromoteToTemplate(ctx context.Context, o *Overlay, newTemplateID string, excludedMounts []string) error

	// PromoteCostBytes reports how many bytes a PromoteToTemplate of this Overlay would ADD
	// to the UncappedBytes term (resource admission / uncapped rootfs accounting) — the forecast a Promote is
	// gated on, since a frozen layer is charged to no sandbox cap.
	//
	// Asked before the source is quiesced, so it is an UPPER BOUND (resource admission refuses on the
	// strongest available statement). Two obligations the contract fixes rather than leaving
	// to each implementation:
	//
	//  1. It must not UNDER-report what its own PromoteToTemplate will cost. A freeze that
	//     does not preserve sparseness must project a sparse file's APPARENT size, or a
	//     hole-punched upper measures near zero and costs its full length once copied. A
	//     reflink/snapshot freeze may legitimately report ~0 — the question is the delta its
	//     own mechanism adds.
	//  2. Excluded paths need not be subtracted: counting them overstates, the same direction
	//     as the pre-quiesce timing.
	//
	// excludedMounts MUST be the same set PromoteToTemplate is given — not a nicety, since a
	// forecast counting shadowed bytes the freeze will not copy refuses Promotes against
	// capacity that was never going to be consumed. The result is stale the moment it returns
	// (the guest keeps writing), bounded by that sandbox's remaining headroom (rootfs storage).
	PromoteCostBytes(ctx context.Context, o *Overlay, excludedMounts []string) (int64, error)

	// Usage reports the backend's own statement of ONE Overlay unit's usage and the cap on
	// its storage — the Rootfs subsystem twin of volume.Backend.Usage. The control-plane API PATCH write point forces
	// it: a cap edit refuses on a STRICT inequality against current usage (ErrCapBelowUsage),
	// and UncappedBytes cannot answer that, since a capped unit contributes zero there by
	// contract.
	//
	// BY ID, not by *Overlay handle, and behaviorally so: a handle comes only from
	// OpenOverlay, which WRITES the quota — so a handle-shaped read would re-apply a cap as
	// a side effect of a query.
	//
	// The limit result is DIAGNOSTIC ONLY, never a validation input: the local substrate maps
	// "no cap" and 0 onto each other in both directions, so "never capped" and "deliberately
	// uncapped" read back identical (volume cap enforcement). The record is always the authority.
	//
	// AN UNREADABLE QUOTA ROW IS AN ERROR, NEVER A ZERO-CAP ANSWER. A synthesized walk counts
	// file data only, while the row also counts metadata extents (16 KiB on an empty unit,
	// measured), so `used` would come back under the substrate's own number and the strict
	// inequality would admit a cap that wedges the unit. The write point fails closed;
	// inspect-shaped readers degrade their answer.
	Usage(ctx context.Context, overlayID string) (used, limit int64, err error)

	// UncappedBytes reports the bytes this provider holds THAT NO DECLARED CAPACITY CAP
	// COVERS — the second term of the resource admission node-capacity inequality (uncapped rootfs accounting).
	// A PREDICATE rather than an enumeration, which is what keeps it answerable by an
	// object-store or network-FS provider without naming subvolumes or directory walks.
	// Two invariants:
	//
	//  1. DISJOINT from the cap term: a capped Overlay unit contributes ZERO. Adding its
	//     usage on top of its own promise charges the same bytes twice, and the cap is
	//     already an upper bound where usage is only a prediction.
	//  2. At most ONCE for physically shared bytes: a reflinked copy-up shares extents with
	//     its layer and squota does not charge them to the sandbox unit, so the layer side
	//     counts that extent exactly once.
	//
	// So it is already-consumed capacity to subtract from the budget, not a forecast — which
	// is why a promise-time gate may read it (resource admission takes the cap where one exists, the
	// measurement where none does).
	//
	// It reports what the BACKEND says, never what a guest says: the sentry fabricates
	// st_blocks (a 512 MiB sparse file reads as 536870912 blocks in-guest against a true
	// allocation of 0). The host-side allocated-block count IS the backend's statement.
	UncappedBytes(ctx context.Context) (int64, error)

	// SweepOrphanOverlays reclaims Overlay STORAGE UNITS with no live `sandboxes` row — the
	// unit twin of SweepOrphanLayers. It carries the retry the transition contract licenses: destroy discards
	// DestroyOverlay's error and deletes the record anyway, and once the row is gone this is
	// the only thing that revisits the unit (SweepOrphanLayers scans only LayersDir, and
	// periodic reconcile iterates store RECORDS). The crash-BEFORE-deletion half belongs to
	// atomic transition rollback reconcile instead.
	//
	// The mediator computes `live` — every OverlayID with a sandbox row in ANY state — and the
	// provider deletes each of its own units in neither keep-set, never reading the store (the lifecycle).
	//
	// `building` is NOT redundant despite create inserting its row before allocating the unit:
	// `live` is a SNAPSHOT and enumeration necessarily follows it, so a create whose row lands
	// after the snapshot looks exactly like an orphan. It is therefore the in-flight-create
	// keep-set, and the mediator should snapshot it as late as possible.
	//
	// THAT RESIDUAL WINDOW IS NOT HYPOTHETICAL, AND the Volume subsystem DELIBERATELY DOES NOT SHARE THIS
	// SHAPE: under this signature a volume whose whole creation fell between the two reads was
	// in neither set and its committed bytes were deleted (measured: 3 of 6 snapshots under 458
	// overlapping sweeps). the Volume subsystem split into "backend enumerates, mediator decides" — see
	// volume.Backend.ListUnitIDs. This side keeps the simpler shape because an Overlay's row is
	// inserted BEFORE its unit exists, so only the snapshot skew is exposed, not the whole
	// create. Narrower, not closed: the Volume subsystem split is the shape to copy if it needs hardening.
	//
	// Reclamation goes through DestroyOverlay rather than a directory delete: a capped unit is
	// a subvolume plus a quota row, reclaimed by different calls. So it cannot be tested by
	// directory existence — `rm -rf` removes the tree and leaves the quota row.
	SweepOrphanOverlays(ctx context.Context, live, building []string) (int, error)

	// SweepOrphanLayers reclaims Promote-built layer dirs with no live `templates` row (the Rootfs subsystem /
	// atomic transition rollback). Same shape as the sibling: the mediator computes `live`, the provider deletes
	// its own layer dirs in neither set. `building` is the in-flight-Promote keep-set — a
	// synchronous Promote has bytes on disk before the row is inserted, so "bytes, no row"
	// must not be torn down. At boot `building` is empty, and a crashed Promote's staging layer
	// is correctly a true orphan. Builtin Template dirs live under a different root and are
	// never candidates.
	SweepOrphanLayers(ctx context.Context, live, building []string) (int, error)
}
