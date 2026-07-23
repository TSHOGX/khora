// Package rootfs is subsystem §5.B: Template (read-only lower) + the sandbox's
// writable Overlay artifact, assembled as a merged rootfs via the host kernel's
// overlayfs — runsc runs with --overlay2=none --file-access=shared and never
// manages the write layer itself (§2.5 / §5.B, ECS-validated; rootfs-overlay
// D0/D3).
//
// The FS Provider is the §5.B storage seam. Its abstraction exposes only
// "OverlayID + referenced Template + at-rest readable" and NEVER "it is a host
// upper path" (§3.1.1 对内也 ID 寻址) — the host path is locked inside this
// subsystem. Ships the single-node "resident upper directory" unit (D3);
// content-addressed diff layers are a later evolution behind the same seam.
package rootfs

import "context"

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
	// the single source of truth for lineage; the FS provider only maps each ID
	// to its layer dir (§4 subsystems don't read the store). IDs cross the seam,
	// host paths stay inside (§3.1.1).
	Lower []string
}

// The single-node host paths (upper/work/merged) are NOT carried on this struct:
// they derive deterministically from ID under the FS provider's StateDir (see
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

// Provider is the §5.B FS-provider seam.
type Provider interface {
	Name() string

	// CreateOverlay allocates an empty writable Overlay over a read-only lower
	// STACK (the create prefix of the cold-start chain, ① for create;
	// guest-runtime D5.1). lower is leaf-first: lower[0] is the immediate
	// Template, the tail its ancestors (§5.B multi-lowerdir). The mediator
	// resolved it from the authoritative lineage (§4.3); len 1 = a base Template.
	// Returns the Overlay by ID + private host materialization.
	CreateOverlay(ctx context.Context, overlayID string, lower []string) (*Overlay, error)

	// OpenOverlay reconstructs the handle for an EXISTING Overlay artifact from
	// its ID + resolved lower STACK (§3.1.1: Overlay is ID-addressed, so the
	// handle is rebuildable from the store record, not kept in memory; the
	// mediator re-resolves the lineage each time). This is the resume prefix —
	// the artifact was created earlier and persists on the backend across pause
	// (§4.2 零捕获). It does NOT allocate new bytes.
	OpenOverlay(ctx context.Context, overlayID string, lower []string) (*Overlay, error)

	// Mount assembles lower(Template)/upper/work into a merged rootfs (① of the
	// cold-start chain). Called by the §4 mediator at create/resume; the mount
	// lifecycle belongs to this subsystem (§5.B / rootfs-overlay D4). resume
	// re-mounts the SAME Overlay artifact — pause captured nothing (§4.2).
	Mount(ctx context.Context, o *Overlay) (*Merged, error)

	// Unmount tears down the merged overlay (pause / destroy path). The upper
	// directory persists on the backend across pause (§4.2 零捕获).
	Unmount(ctx context.Context, m *Merged) error

	// DestroyOverlay reclaims the upper+work bytes (destroy terminal state,
	// §4.1). Deletes synchronously; a refcount-driven async sweep (§4.3) is the
	// eventual form. Separate from placement teardown — the mediator invokes each
	// (§4: subsystems don't call each other).
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
	PromoteToTemplate(ctx context.Context, o *Overlay, newTemplateID string) error

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
