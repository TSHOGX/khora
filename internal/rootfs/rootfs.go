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
	Template string // the Template it references as read-only lower (§3.1.1 Binding)
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

	// CreateOverlay allocates an empty writable Overlay referencing a Template
	// (the create prefix of the cold-start chain, ① for create; guest-runtime
	// D5.1). Returns the Overlay by ID + private host materialization.
	CreateOverlay(ctx context.Context, overlayID, template string) (*Overlay, error)

	// OpenOverlay reconstructs the handle for an EXISTING Overlay artifact from
	// its ID + referenced Template (§3.1.1: Overlay is ID-addressed, so the
	// handle is rebuildable from the store record, not kept in memory). This is
	// the resume prefix — the artifact was created earlier and persists on the
	// backend across pause (§4.2 零捕获). It does NOT allocate new bytes.
	OpenOverlay(ctx context.Context, overlayID, template string) (*Overlay, error)

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
	// deployment ships (§8 boot seed): each subdirectory of TemplatesDir is a
	// platform-preseeded Template whose bytes are already on the backend. Returns
	// the directory names, which ARE the builtin TemplateIDs. bytes-first (§5.B)
	// is naturally satisfied — the dirs exist before the mediator seeds any row.
	// The host TemplatesDir path stays private to this subsystem (§3.1.1); only
	// the opaque IDs cross the seam.
	ListBuiltinTemplates(ctx context.Context) ([]string, error)

	// SweepOrphanLayers reclaims Promote-built layer directories with no live
	// `templates` row (§9 orphan-layer sweep). It is the §5.B twin of §4.4-A
	// placement reclamation: the mediator computes `live` (the layer IDs the
	// authoritative store still has a row for) and passes it in — the provider
	// enumerates only its OWN layer dirs and deletes any not in `live` and not in
	// `building` (§4 subsystems don't call the store). `building` is the
	// in-flight-Promote keep-set (§9): a synchronous Promote has its layer bytes
	// on disk BEFORE the row is inserted, so a concurrent sweep seeing "bytes, no
	// row" must NOT tear it down (the placement live-map's twin). At boot
	// `building` is empty — a crashed Promote's staging layer IS a true orphan and
	// is correctly reclaimed. Deployment-shipped builtin Template dirs live under
	// a different root and are NEVER candidates. Returns the count reclaimed.
	SweepOrphanLayers(ctx context.Context, live, building []string) (int, error)
}
