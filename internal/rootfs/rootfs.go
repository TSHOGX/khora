// Package rootfs is subsystem §5.B: Template (read-only lower) + the sandbox's
// writable Overlay artifact, assembled as a merged rootfs via the host kernel's
// overlayfs — runsc runs with --overlay2=none --file-access=shared and never
// manages the write layer itself (§2.5 / §5.B; rootfs-overlay D0/D3, validated
// on both substrate arches per ADR-0002).
//
// The rootfs provider is the §5.B storage seam (never "FS provider" — `fs` is the
// §5.E wire service, a different thing entirely, §1.1). Its abstraction exposes only
// "OverlayID + referenced Template + at-rest readable" and NEVER "it is a host
// upper path" (§3.1.1 对内也 ID 寻址) — the host path is locked inside this
// subsystem. Ships the single-node "resident upper directory" unit (D3);
// content-addressed diff layers are a later evolution behind the same seam.
package rootfs

import (
	"context"
	"errors"
)

// ErrCapBelowUsage is the §5.B placement-side refusal of a capacity cap that is not
// STRICTLY above the storage unit's current usage (§3.4 / rootfs-overlay D6.6).
//
// It is a REFUSED PLACEMENT rather than a wedged unit, and that is the whole point.
// Measured on this substrate: the cap is accepted with ZERO validation, and afterwards
// creating a file, `mkdir`, and even truncating an existing file to zero all return
// ENOSPC — while the quota write ITSELF returns ENOSPC, so the cap can then neither be
// raised nor removed. On a §5.B unit that is worse than on a volume: applyCap re-applies
// the record's cap at EVERY placement, and re-applying it to a wedged unit fails the same
// way, so the sandbox is permanently unresumable. `Mount`'s own fresh-workdir step
// (rm -rf work; mkdir work) fails with ENOSPC too, so even skipping the quota write would
// not get the sandbox back.
//
// So the two directions are not symmetric: refusing leaves a `paused` sandbox whose exits
// are ordinary API calls (PATCH the cap above usage, or to 0, and the next placement
// applies it; or Promote and recreate), while imposing leaves a unit no API call can ever
// unstick. `cap == usage` is included because it is ALSO a wedged state, measured: setting
// limit exactly equal to `referenced` is accepted and stored, but re-applying it and
// raising it both then return ENOSPC.
//
// It does NOT fire for a sandbox that filled its own cap the ordinary §3.4 way: squota
// stops the guest with ENOSPC while usage is still strictly BELOW the limit (measured
// 32 KiB of headroom), so re-applying that cap succeeds and this predicate never trips.
var ErrCapBelowUsage = errors.New("rootfs: capacity cap must be strictly greater than the storage unit's current usage")

// CapUnchanged is the OpenOverlay capByte sentinel for "rebuild the handle, do
// not touch the quota" (see OpenOverlay). Negative, so it can never collide with
// a real cap or with 0 = uncapped — the distinction matters because 0 is itself a
// meaningful instruction (remove the cap).
const CapUnchanged int64 = -1

// Overlay is the ID-addressed, Template-decoupled frozen layer (§3.1.1). Above
// this subsystem it is referenced by ID; the host materialization is private.
type Overlay struct {
	ID       string // OverlayID (opaque above §5.B)
	Template string // the immediate Template it references (§3.1.1 Binding; = Lower[0])

	// Lower is the resolved multi-lowerdir stack, LEAF-FIRST: the layered
	// Template's read-only lower stack expanded along its lineage (§5.B 分层
	// Template = multi-lowerdir). Lower[0] is the leaf (the referenced Template),
	// the tail its ancestors down to the base. len 1 for a base Template (no
	// parent) — the pre-Promote single-lower case, behaviorally unchanged.
	//
	// The MEDIATOR computes this by walking the authoritative `templates.parent`
	// chain (§4.3 lineage) and passes the ID stack across the seam — the store is
	// the single source of truth for lineage; the rootfs provider only maps each ID
	// to its layer dir (§4 subsystems don't read the store). IDs cross the seam,
	// host paths stay inside (§3.1.1).
	Lower []string
}

// The single-node host paths (upper/work/merged) are NOT carried on this struct:
// they derive deterministically from ID under the rootfs provider's StateDir (see
// HostOverlayFS.upperPath/workPath), so the provider recomputes them on demand
// and nothing above §5.B ever sees them (§3.1.1 对内也 ID 寻址). upper/work must
// share one overlayfs-capable FS and one quota unit for the later disk-capacity
// cap (D6.3).

// Merged is a handle to an assembled merged rootfs, ready to hand to the
// runtime provider as an opaque RootfsSource (§5.A: provider doesn't know how
// it was assembled). Only the Rootfs subsystem constructs/destructs it.
type Merged struct {
	MergedPath string // the overlayfs merged dir; consumed by local_runsc only
}

// Provider is the §5.B rootfs-provider storage seam.
type Provider interface {
	Name() string

	// CreateOverlay allocates an empty writable Overlay over a read-only lower
	// STACK (the create prefix of the cold-start chain, ① for create;
	// guest-runtime D5.1). lower is leaf-first: lower[0] is the immediate
	// Template, the tail its ancestors (§5.B multi-lowerdir). The mediator
	// resolved it from the authoritative lineage (§4.3); len 1 = a base Template.
	// Returns the Overlay by ID + private host materialization.
	//
	// capByte is the §3.4 disk-CAPACITY cap (Binding), applied to the upper+work
	// storage unit. It crosses the seam as a PLAIN BYTE COUNT — the caller states
	// intent, the provider chooses the mechanism (local: btrfs subvolume + squota;
	// a remote rootfs provider would use an object-store quota or its own accounting).
	// This is why capacity is NOT a cross-cutting QuotaProvider interface: such an
	// interface would weld the btrfs assumption into every implementation (§5.B /
	// rootfs-overlay D6.4). 0 = uncapped.
	CreateOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error)

	// OpenOverlay reconstructs the handle for an EXISTING Overlay artifact from
	// its ID + resolved lower STACK (§3.1.1: Overlay is ID-addressed, so the
	// handle is rebuildable from the store record, not kept in memory; the
	// mediator re-resolves the lineage each time). This is the resume prefix —
	// the artifact was created earlier and persists on the backend across pause
	// (§4.2 零捕获). It does NOT allocate new bytes.
	//
	// capByte RE-APPLIES the capacity Binding from the current record (§3.4): the
	// cap is re-materialized at each placement rather than trusted to whatever the
	// storage unit was left carrying, so an edited cap takes effect on resume and a
	// quota lost to an external change is restored. 0 = uncapped, which also
	// REMOVES any cap the unit still carries (the record is the authority).
	//
	// CapUnchanged means "reconstruct the handle only, touch no quota" — for the
	// callers that are NOT materializing a placement (destroy, a Promote byte-read,
	// orphan reclamation, re-adopting a live survivor). Those must not pass 0: a
	// handle rebuild is not a statement that the sandbox is uncapped, and clearing a
	// live sandbox's quota mid-teardown or mid-adopt would silently un-cap it.
	OpenOverlay(ctx context.Context, overlayID string, lower []string, capByte int64) (*Overlay, error)

	// Mount assembles lower(Template)/upper/work into a merged rootfs (① of the
	// cold-start chain). Called by the §4 mediator at create/resume; the mount
	// lifecycle belongs to this subsystem (§5.B / rootfs-overlay D4). resume
	// re-mounts the SAME Overlay artifact — pause captured nothing (§4.2).
	Mount(ctx context.Context, o *Overlay) (*Merged, error)

	// Unmount tears down the merged overlay (pause / destroy path). The upper
	// directory persists on the backend across pause (§4.2 零捕获).
	Unmount(ctx context.Context, m *Merged) error

	// DestroyOverlay reclaims the upper+work bytes (destroy terminal state,
	// §4.1), synchronously; a failure is retried asynchronously by
	// SweepOrphanOverlays (§4.4-A orphan-unit sweep, not refcount GC). Separate
	// from placement teardown — the mediator invokes each (§4: subsystems don't
	// call each other).
	DestroyOverlay(ctx context.Context, o *Overlay) error

	// AdoptMerged reconstructs the merged-mount handle for an Overlay that is
	// STILL mounted after a control-plane crash (§4.4-A 另一半). The overlayfs
	// mount is kernel state — it survives the orchestrator process, held by the
	// still-running container — so on Adopt the mediator recovers the handle
	// rather than re-mounting. The host path stays private (§3.1.1); the returned
	// Merged is opaque above this subsystem. It does NOT mount — a NOT-mounted
	// overlay is a caller error (that sandbox's container is not actually live).
	AdoptMerged(ctx context.Context, o *Overlay) (*Merged, error)

	// ListBuiltinTemplates enumerates the static Template directories the
	// deployment ships (boot seed — existence-as-state §4.3, idempotent reconcile
	// §4.4-A): each subdirectory of TemplatesDir is a
	// platform-preseeded Template whose bytes are already on the backend. Returns
	// the directory names, which ARE the builtin TemplateIDs. bytes-first (§5.B)
	// is naturally satisfied — the dirs exist before the mediator seeds any row.
	// The host TemplatesDir path stays private to this subsystem (§3.1.1); only
	// the opaque IDs cross the seam.
	ListBuiltinTemplates(ctx context.Context) ([]string, error)

	// PromoteToTemplate freezes a sandbox's at-rest Overlay upper into a NEW
	// read-only layer directory addressed by newTemplateID (§5.B Promote / §3.1
	// Promote). The caller (mediator) has already quiesced the sandbox to a
	// consistent at-rest point (equivalent to a pause, §4.2) and torn down its
	// mount, so the upper is a plain host dir tree safe to read (§2.5 / §5.B
	// 只读 at-rest). It COPIES the upper into the layer dir, EXCLUDING the
	// engine-injected runtime paths that must not be captured (§5.B / §3.1
	// Promote; excluded paths detailed in rootfs-overlay D5: today the two
	// sandboxd portal sentinel files). It does NOT insert any store row
	// and does NOT wire lineage — that is the mediator's bytes-first sequel
	// (write bytes here → insert the templates row after, §4.3 INV-3). A crash
	// after this returns but before the row insert leaves an orphan layer the
	// sweep reclaims. Overlay carries the source's OverlayID; newTemplateID names
	// the frozen layer dir under LayersDir.
	//
	// excludedMounts are THIS sandbox's Volume mount destinations, and they are a
	// PER-PROMOTE PARAMETER rather than a static package list because a mount
	// destination is per-sandbox: a package-level list has no way to know them
	// (rootfs-overlay D5). Passing them in is also the architecturally cleaner route —
	// the §4 mediator already holds the mount table and already calls Promote, so §5.B
	// NEVER has to ask §5.C anything and no subsystem edge appears (§4).
	//
	// WHAT THEY ARE ACTUALLY FOR, because the obvious guess is wrong: they do NOT
	// create the empty placeholder. That is the DEFAULT outcome — measured, after
	// `runsc create` and before any guest code runs, the destination is already an
	// ordinary empty directory, mode 0755, with zero overlayfs xattrs (and when the
	// Template itself provides the destination the upper stays pristine). What they do
	// is PRUNE PRE-EXISTING BYTES THE MOUNT SHADOWS: bytes written into the upper
	// before the volume was bound over that path are invisible to every guest, yet
	// they are still on disk, still in the upper, and would be copied into the frozen
	// layer in full AND counted in both the sandbox's capacity cap and the cost
	// forecast (rootfs-overlay D5).
	//
	// Paths follow the existing convention: relative to the upper, leading slash
	// stripped. NESTED destinations prune the LEAF SUBTREE and accept an empty
	// intermediate chain — settled in rootfs-overlay D5, because pruning only the leaf
	// would leave ghost intermediates in the layer while excluding a top-level
	// component would discard real guest data that legitimately lives beside the mount.
	PromoteToTemplate(ctx context.Context, o *Overlay, newTemplateID string, excludedMounts []string) error

	// PromoteCostBytes reports how many bytes a PromoteToTemplate of THIS Overlay
	// would ADD to the UncappedBytes term (§3.4 / rootfs-overlay D6.5) — the
	// forecast the mediator gates a Promote on, since a frozen layer is charged to
	// no sandbox cap and so grows the node's uncapped total.
	//
	// It is asked BEFORE the source is quiesced, so it is an UPPER BOUND, not an
	// exact count — a promise-time gate wants the conservative direction anyway
	// (§3.4 refuses on the strongest available statement). Two consequences the
	// contract fixes rather than leaves to each implementation:
	//
	//  1. It must NOT under-report what its own PromoteToTemplate will cost. An
	//     implementation whose freeze does not preserve sparseness must project the
	//     APPARENT size of a sparse file, not its allocation — otherwise a
	//     hole-punched upper measures near zero and costs its full length once
	//     copied, and the gate's bound points the wrong way. Symmetrically, an
	//     implementation whose freeze is a reflink/snapshot may legitimately report
	//     ~0: the question is the DELTA its own mechanism adds, not the upper's size.
	//  2. Excluded paths (the engine-injected runtime debris PromoteToTemplate
	//     drops) need not be subtracted. Counting them overstates, which is the
	//     same direction as the pre-quiesce timing.
	//
	// Same shape as UncappedBytes (§4: mediator asks, provider answers): IDs cross
	// the seam, host paths stay inside, the store is never read. A remote provider
	// answers from its own backend's accounting — nothing here names a directory
	// walk or a filesystem.
	//
	// A caller that has not yet quiesced the sandbox must treat the result as stale
	// the moment it returns: the guest can keep writing. That residual is bounded by
	// the sandbox's own remaining capacity headroom and is the same "true at the
	// instant of admission" property the whole second term has (D6.5 残留).
	// excludedMounts MUST BE THE SAME SET PromoteToTemplate is given. Not a
	// consistency nicety — a forecast that counts shadowed bytes the freeze will not
	// copy is conservative in DIRECTION but KNOWN WRONG in value, so it refuses
	// Promotes against capacity that was never going to be consumed. The two calls
	// share one set so they cannot drift.
	PromoteCostBytes(ctx context.Context, o *Overlay, excludedMounts []string) (int64, error)

	// Usage reports the backend's OWN statement of ONE Overlay unit's usage and the
	// cap currently on its storage — the §5.B twin of volume.Backend.Usage.
	//
	// It is not a convenience: the §6.1 PATCH write point forces it (§3.4). A cap edit
	// must refuse "lower the cap below current usage", the criterion is a STRICT
	// inequality (see ErrCapBelowUsage), and the mediator cannot evaluate that without
	// the per-sandbox number. UncappedBytes cannot answer it — a CAPPED unit contributes
	// ZERO there BY CONTRACT (the two §3.4 terms are disjoint), which is exactly the
	// unit a cap edit is about.
	//
	// BY ID, NOT BY *Overlay HANDLE, and the difference is behavioral rather than
	// stylistic: a handle is only obtainable through OpenOverlay, which WRITES the quota.
	// Running a placement-shaped call to read one number would re-apply a cap as a side
	// effect of a query. Every host path stays inside (§3.1.1) — the unit is derivable
	// from the ID.
	//
	// THE limit RESULT IS DIAGNOSTIC ONLY — NEVER USE IT TO VALIDATE, for the same reason
	// as on the §5.C side: the local substrate maps "no cap" and 0 onto each other in BOTH
	// directions, so "never capped" and "deliberately uncapped" read back byte-for-byte
	// identical (volume.md D7.3). THE SANDBOX RECORD IS ALWAYS THE AUTHORITY (§3.4).
	//
	// AN UNREADABLE QUOTA ROW IS AN ERROR, NEVER A ZERO-CAP ANSWER. Synthesizing
	// "(measured walk, 0, nil)" from a failed read is wrong in both halves and dangerous
	// in one: a walk counts file data only while the row ALSO counts metadata extents
	// (16 KiB on an EMPTY unit, measured), so the synthesized `used` comes back UNDER the
	// substrate's own number — and the strict inequality would then admit a cap the
	// substrate wedges the unit on. Callers handle the error: the write point FAILS
	// CLOSED, and inspect-shaped readers degrade their answer.
	//
	// Same shape as the rest of this seam (§4: mediator asks, provider answers): the
	// provider measures only its OWN storage and never reads the store. It reports what
	// the BACKEND says, never what a guest says (see UncappedBytes for why guest-side
	// numbers are unusable).
	Usage(ctx context.Context, overlayID string) (used, limit int64, err error)

	// UncappedBytes reports the total bytes this provider currently holds in its
	// storage THAT NO DECLARED CAPACITY CAP COVERS — the second term of the §3.4
	// node-capacity inequality (rootfs-overlay D6.5).
	//
	// The contract is a PREDICATE, not an enumeration, and that is what makes it
	// provider-neutral: "bytes under no cap" is answerable by a rootfs provider
	// backed by an object store or a network FS (each asks its own backend) without
	// the contract naming subvolumes, quotas, or directory walks. Two invariants:
	//
	//  1. DISJOINT from the cap term: a byte covered by a declared cap is NOT
	//     reported here, because the mediator already counts that cap. Concretely a
	//     CAPPED Overlay unit contributes ZERO — adding its current usage on top of
	//     its own promise charges the same bytes twice, and its usage is a
	//     PREDICTION where the cap is already an upper bound.
	//  2. At most ONCE for physically shared bytes: a reflinked copy-up shares
	//     extents with the layer it came from, and squota does not charge them to the
	//     sandbox unit, so the layer side counts that extent exactly once. The
	//     property that FORCES this term to exist (D6.2) is the same one that makes
	//     the two terms naturally non-overlapping.
	//
	// So the term is "already-consumed capacity to subtract from the budget", not a
	// forecast — which is why a promise-time gate may legitimately read it (§3.4:
	// take the strongest available statement about each object — the cap where one
	// exists, the measurement where none does).
	//
	// Same shape as SweepOrphanLayers (§4: mediator asks, provider answers): the
	// provider measures only its OWN storage and never reads the store.
	//
	// It reports what the BACKEND says its bytes cost, never what a guest says — the
	// sentry FABRICATES st_blocks (a 512 MiB sparse file reads as 536870912 blocks
	// in-guest while the host's true allocation is 0), so guest-side numbers are
	// unusable for capacity accounting: §5.B requires the number to come from the
	// backend's own statement. That bans GUEST-side numbers only — the host-side
	// allocated-block count IS the backend's statement, and is the source it means.
	UncappedBytes(ctx context.Context) (int64, error)

	// SweepOrphanOverlays reclaims Overlay STORAGE UNITS with no live `sandboxes`
	// row (§4.4-A reclamation discipline) — the §5.B *unit* twin of
	// SweepOrphanLayers, which sweeps only layers. It carries the retry §4.4
	// licenses: destroy DISCARDS DestroyOverlay's error and deletes the record
	// anyway ("删字节失败不阻塞、不回退状态转换…由孤儿单元 sweep(§4.4-A 反向对账,
	// 与 refcount GC 不是一回事)异步重试"), and this is the ONLY thing that revisits
	// the unit afterwards — SweepOrphanLayers scans only LayersDir and the periodic
	// reconcile iterates only store RECORDS, so with the row gone there is no record
	// left to hang reclamation off. The crash-BEFORE-deletion half is separate and
	// covered elsewhere (the mediator reclaims a record's orphan Overlay during
	// §4.4-A reconcile); this is the POST-deletion half.
	//
	// Same contract as the sibling (§4: mediator asks, provider answers): the
	// mediator computes `live` — every OverlayID the AUTHORITATIVE store still has
	// a sandbox row for, in ANY state — and the provider enumerates only its OWN
	// storage and deletes each unit in neither keep-set. The provider never reads
	// the store.
	//
	// TWO keep-sets, and `building` is NOT redundant here even though create
	// inserts its record BEFORE it allocates the unit. Write ORDER is not what
	// protects a unit: `live` is a SNAPSHOT, and the provider's enumeration
	// necessarily happens after it, so a create whose row lands after the snapshot
	// presents as bytes on disk with no entry in `live` — indistinguishable from an
	// orphan, and reclaimed out from under a sandbox that is about to run. That is
	// the same "bytes exist, row not yet in hand" hazard `building` guards for
	// Promote, reached by a different route (a stale keep-set rather than
	// bytes-first ordering), so `building` is the in-flight-CREATE keep-set: an
	// OverlayID whose materialization the mediator has begun and not yet settled.
	// One argument would be sufficient only if the store read could be ordered
	// AFTER the enumeration, which this call shape cannot express.
	//
	// The mediator should therefore snapshot `building` as LATE as possible, right
	// before this call, so the set is current; a create that begins entirely after
	// both snapshots is a residual window this shape shares with the sibling and
	// closing it fully would require serializing the sweep against create in the
	// mediator. At boot `building` is empty — every unit without a record is a true
	// orphan and is correctly reclaimed.
	//
	// THAT RESIDUAL WINDOW IS NOT HYPOTHETICAL, AND §5.C DELIBERATELY DOES NOT SHARE
	// THIS SHAPE. Under this signature, volume.Backend destroyed committed data: a
	// volume whose ENTIRE creation fell between the mediator's `live` read and its
	// `building` read was in neither set, so the backend deleted the bytes of a fully
	// committed volume (measured: 3 of 6 snapshots destroyed under 458 overlapping
	// sweeps). §5.C therefore split into "backend enumerates, mediator decides", which
	// closes the window by ordering rather than locking — see volume.Backend.ListUnitIDs.
	//
	// This side keeps the simpler shape DELIBERATELY, and the asymmetry has a reason
	// rather than being an oversight: an Overlay's `sandboxes` row is inserted BEFORE its unit is
	// allocated, so only the snapshot SKEW is exposed here, whereas volume creation is
	// bytes-first and exposes the whole create. The window is correspondingly far
	// narrower — but "narrower" is not "closed", so if this ever needs hardening, the
	// §5.C split is the shape to copy and the measurement above is the evidence.
	//
	// Reclamation goes through the full DestroyOverlay path rather than a directory
	// delete, because a CAPPED unit is a btrfs subvolume plus a quota row and the two
	// are reclaimed by different calls (btrfs.Client.DeleteSubvolume handles both
	// quota modes). Note this cannot be tested by directory existence alone —
	// `rm -rf` does remove the tree, and what it leaves behind is the quota row.
	// Returns the count reclaimed.
	SweepOrphanOverlays(ctx context.Context, live, building []string) (int, error)

	// SweepOrphanLayers reclaims Promote-built layer directories with no live
	// `templates` row (orphan-layer sweep, §5.B / §4.4-A). It is the §5.B twin of §4.4-A
	// placement reclamation: the mediator computes `live` (the layer IDs the
	// authoritative store still has a row for) and passes it in — the provider
	// enumerates only its OWN layer dirs and deletes any not in `live` and not in
	// `building` (§4 subsystems don't call the store). `building` is the
	// in-flight-Promote keep-set (§5.B / §4.4-A): a synchronous Promote has its layer bytes
	// on disk BEFORE the row is inserted, so a concurrent sweep seeing "bytes, no
	// row" must NOT tear it down (the placement live-map's twin). At boot
	// `building` is empty — a crashed Promote's staging layer IS a true orphan and
	// is correctly reclaimed. Deployment-shipped builtin Template dirs live under
	// a different root and are NEVER candidates. Returns the count reclaimed.
	SweepOrphanLayers(ctx context.Context, live, building []string) (int, error)
}
