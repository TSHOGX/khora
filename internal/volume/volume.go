// Package volume is subsystem §5.C: decoupled, ID-addressed storage whose
// lifecycle is independent of any sandbox — it exists before one, outlives one,
// and exists while nothing has it mounted at all (§5.C).
//
// It is a SEPARATE subsystem with its own seam, deliberately not sharing §5.B's
// (§5 preamble / volume.md D2): rootfs.Provider materializes a LAYERED
// composition (multi-lowerdir + upper/work + Promote), while volume.Backend
// materializes ONE self-sufficient storage unit — no composition, no lower stack,
// no lineage walk. The two share only the underlying substrate primitives (the
// internal/btrfs leaf), which is shared CODE and not a shared interface.
//
// The seam follows the §5.A / §5.B convention: IDs cross it, host paths stay
// inside; policy and handles, never host primitives. seam_test.go enforces that
// structurally rather than by review.
package volume

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrUnsupported is the typed refusal a backend returns for a capability it does
// NOT report in Capabilities — a plain-directory backend asked to snapshot, or
// asked to enforce a capacity cap it cannot enforce.
//
// It is typed rather than free-form because §6.2 maps it to a 400: the caller
// asked for something this backend cannot do, which no retry and no state change
// makes legal. The refusal must be LOUD: silently running uncapped would leave
// every §2.3 blast-radius assertion believing in a cap that does not exist, and
// silently skipping a snapshot would return success for an operation that did
// nothing.
var ErrUnsupported = errors.New("volume: operation not supported by this backend")

// Capabilities is the PER-BACKEND capability query (§5.C / volume.md D2). Scope
// is per-backend, never per-volume: the answer is a property of the
// implementation, not of one unit.
//
// EVERY MEMBER IS A bool, AND THAT IS LOAD-BEARING. A map[string]string feature
// table would carry MECHANISM NAMES ACROSS THE SEAM AS VALUES, where no
// identifier-scanning structural guard can see them — the same leak this package's
// seam_test.go exists to stop, arriving through a door the guard cannot watch.
// seam_test.go asserts the bool-ness.
//
// Members list only capabilities that have a CONSUMER (§1.3 forbids speculative
// reservation): Snapshot/Clone gate the §6.1 snapshot/clone endpoints, and Quota
// gates "a cap that cannot be enforced is refused, never silently dropped" (§3.4).
// A DirectIO-style bit has no consumer and would sit on the contract surface
// forever in the name of "we might need it".
type Capabilities struct {
	// Snapshot reports whether the backend can take a point-in-time copy of a
	// volume's bytes (§5.C: local = an atomic whole-tree snapshot of the unit).
	Snapshot bool
	// Clone reports whether the backend can produce an independent writable copy.
	// Same substrate path as Snapshot on the local arm; the two differ in the
	// semantic label, not the mechanism (volume.md D6.1a).
	Clone bool
	// Quota reports whether the backend can ENFORCE a per-volume capacity cap
	// (§3.4). False means a non-zero cap must be REFUSED with ErrUnsupported, not
	// accepted and ignored.
	Quota bool
}

// Materialized is a handle to one materialization of a volume: a host mount point
// ready to hand to the runtime provider as an OCI bind source, exactly as §5.B
// hands over a merged rootfs (§5.C: the seam yields ONE thing, a host mount point).
//
// Volume is required, not decoration: Release must be able to resolve which volume
// a handle belongs to.
type Materialized struct {
	// Volume is the ID this handle materializes. Above the seam a volume is ALWAYS
	// an ID; this is the identity half of the handle.
	Volume string

	// SourcePath is the host mount point, consumed ONLY by local_runsc when it
	// writes the OCI bind (the exact precedent, and the exact exemption, that
	// rootfs.Merged.MergedPath already holds). Nothing above §5.C interprets it,
	// and §6.2 keeps it off the wire — callers address volumes by ID.
	SourcePath string

	// handle is backend-private materialization state, UNEXPORTED on purpose
	// (runtime.Placement.handle is the precedent). The FUSE arm of a future remote
	// backend has to start and stop a process per materialization; without a place
	// to keep that, it would need a side table keyed by volume ID — precisely the
	// in-process handle registry volume.md D3.1 rejects. Keeping it unexported also
	// keeps it out of the structural guard's contract surface, because it is not
	// part of the contract.
	handle any
}

// SetHandle / Handle let a backend stash and recover its private materialization
// state without exposing it above the seam.
func (m *Materialized) SetHandle(h any) { m.handle = h }

// Handle returns the backend-private state set by SetHandle.
func (m *Materialized) Handle() any { return m.handle }

// FileInfo is one entry's metadata as reported through Root — a deliberately
// small, os-independent shape. It carries no host path: a Root handle addresses
// everything relative to the volume root (§5.C ID addressing), so an absolute
// name would be both useless to the caller and a leak.
type FileInfo struct {
	Name    string // base name within the listed/stat'ed directory
	Size    int64  // apparent size in bytes (0 for directories)
	Mode    fs.FileMode
	IsDir   bool
	ModTime time.Time
}

// Root is the ROOT-CONFINED content accessor (§5.C "operable while unmounted" /
// volume.md D5.1). Every path argument is relative to the volume root and
// out-of-root targets are UNREACHABLE — not an error the caller must remember to
// check, but a property of how the paths are resolved.
//
// It is STRICTER than Materialized, not looser: no member exposes a host path, so
// this is the one materialization-class part of the seam that needs no exemption
// in the structural guard.
//
// It is NOT a replacement for Materialize. That one must hand runsc a bindable
// host path; this one only moves bytes inside the engine process. Two different
// consumers (local_runsc vs the orchestrator's import/export endpoints), so two
// different methods.
//
// It also does NOT go through §5.E `fs`: that service exists only inside a guest
// and is reachable only through a LIVE placement, while an unmounted volume has no
// guest at all — and routing volume content through §5.E would create a
// volume → transport subsystem edge that internal/arch's empty allowlist rejects
// outright (volume.md D5.1).
//
// Concurrency: a Root is safe for concurrent use. Each operation resolves its own
// descriptor from the root, so there is no shared cursor to race on.
type Root interface {
	// Stat reports metadata for one path ("." = the volume root itself).
	Stat(name string) (FileInfo, error)
	// List enumerates one directory's entries (non-recursive).
	List(name string) ([]FileInfo, error)
	// ReadAt reads len(p) bytes from name starting at off, with io.Reader's
	// short-read convention (n < len(p) with io.EOF at end of file). Reading a
	// range rather than a whole file is what lets export STREAM (§6.1: bytes
	// stream, never a whole file buffered at either end).
	ReadAt(name string, p []byte, off int64) (int, error)
	// WriteAt writes p to name at off, creating the file (and NOT its parents) if
	// absent. The streaming half of import.
	WriteAt(name string, p []byte, off int64) (int, error)
	// Mkdir creates one directory. Parents must exist.
	Mkdir(name string, perm fs.FileMode) error
	// Remove deletes one file or EMPTY directory. Absent is not an error
	// (idempotent, matching every other reclamation path in the engine).
	Remove(name string) error
	// Rename moves within the same volume. Both paths are root-relative, so a
	// rename can never move bytes out of the volume.
	Rename(oldName, newName string) error
	// Close releases the root handle. Operations after Close fail.
	Close() error
}

// Backend is the §5.C storage seam: one pluggable backend per storage form, each
// yielding the SAME one thing — a host mount point (volume.md D0/D2). The local
// arm is a host directory with a capacity cap and cheap atomic snapshots; a future
// remote arm (S3 / NFS) is a host-side FUSE mount point the backend maintains, and
// the two are INDISTINGUISHABLE above this seam.
//
// A SHAPE RULE RUNS THROUGH THE WHOLE INTERFACE: "allocate", "set the cap" and
// "place" are THREE actions and must not be compressed into one call (volume.md
// D2). Merging them leaves all three of creation, capacity and the admission term
// homeless — DestroyVolume loses its counterpart, "leave the quota alone" needs a
// sentinel value, and an uncapped volume's bytes have nobody to report them. The
// rule was FALSIFIED WITH A SECOND BACKEND rather than reasoned about: the
// test-only plain-directory arm in this package runs the same shared contract
// suite as the local arm, and a merged shape fails in each of those three places.
// That is why the second arm is not filler — it is this seam's only falsification
// instrument, and it must stay.
type Backend interface {
	// Name identifies the backend for registration and diagnostics.
	Name() string

	// Capabilities reports what this backend can do (see Capabilities).
	//
	// TIMING IS LOAD-BEARING: the mediator MUST ask BEFORE it allocates an ID or
	// moves a byte. An external implementation maps "backend does not support this"
	// at its HTTP boundary while the error is born deep inside — so the 400 arrives
	// only after the source has been fetched, flushed and materialized. Combined
	// with "the mediator allocates the new volume's ID" (below), a late capability
	// check leaves a window whose side effects cannot be classified by any sweep.
	Capabilities() Capabilities

	// CreateVolume allocates the backend storage for volumeID and applies capBytes
	// in the SAME operation.
	//
	// capBytes crosses as a PLAIN BYTE COUNT: the caller states intent, the backend
	// picks the mechanism (local: a quota on the storage unit; a remote backend: its
	// own accounting or a backend quota API). That is why capacity is not a
	// cross-cutting interface — such an interface would weld one mechanism into
	// every implementation (§5.C / rootfs-overlay D6.4).
	//
	// 0 = uncapped. A non-zero cap on a backend whose Capabilities().Quota is false
	// is ErrUnsupported — REFUSED, never silently downgraded to "run uncapped"
	// (§3.4: a cap that cannot be enforced makes every §2.3 assertion believe in a
	// cap that does not exist).
	//
	// bytes-first (§4.3 existence-is-state): the caller inserts the `volumes` row
	// only AFTER this returns, so the unit named by volumeID must be fully
	// materialized here. A crash in between leaves ORPHAN BYTES the sweep reclaims,
	// never a row pointing at storage that does not exist.
	CreateVolume(ctx context.Context, volumeID string, capBytes int64) error

	// SetCapacity re-applies the capacity cap of an EXISTING volume (§6.1 PATCH).
	//
	// It is its own method rather than a parameter on a placement entry point, and
	// that is the only correct placement: a volume's cap MUST be editable while
	// nothing has it mounted, which a placement entry point cannot express at all.
	// (rootfs.Provider needs a CapUnchanged sentinel precisely because its cap intent
	// rides OpenOverlay, a call that is also doing something else; here the call IS
	// the intent, so no sentinel exists or is needed.)
	//
	// 0 removes the cap. ErrUnsupported for a non-zero cap without Quota capability.
	SetCapacity(ctx context.Context, volumeID string, capBytes int64) error

	// Usage reports the backend's OWN statement of one volume's usage and the cap
	// currently on its storage.
	//
	// It is not an optional convenience — the cap write point forces it (§5.C): a
	// PATCH must refuse "lower the cap below current usage", the criterion is a
	// STRICT inequality, and the engine cannot evaluate that without reading current
	// usage. inspect reports usage through the same call.
	//
	// THE limit RESULT IS DIAGNOSTIC ONLY — NEVER USE IT TO VALIDATE. The local
	// substrate maps "no cap" and 0 onto each other in BOTH directions, so "never
	// capped" and "deliberately uncapped" read back byte-for-byte identical
	// (volume.md D7.3). THE VOLUME RECORD IS ALWAYS THE AUTHORITY. Any design that
	// plans to "verify afterwards that the cap was set correctly" is unimplementable
	// on this substrate, which is the other half of why setting the cap is forced
	// into the write itself.
	Usage(ctx context.Context, volumeID string) (used, limit int64, err error)

	// Materialize prepares a host mount point for volumeID, ready to bind into a
	// guest.
	//
	// CONTRACT IS CREATE-IF-ABSENT, not open-only (§5.C, measured): runsc fails hard
	// when a bind source is missing rather than creating it, so an open-only
	// contract would make a mis-swept sandbox PERMANENTLY unresumable, where
	// create-if-absent restarts cleanly (all data lost, but recoverable). This is
	// also where "a volume's bytes exist on demand" lives.
	//
	// CONCURRENCY IS PART OF THE CONTRACT: the same volume may be materialized
	// CONCURRENTLY any number of times. The entire shared-fanout design rests on
	// that (measured: two sandboxes rw-binding one source both succeed, and the
	// second is genuinely writable rather than silently downgraded) — without it the
	// AccessMode enum would collapse to Exclusive alone.
	//
	// It does NOT take an access mode, for two independent reasons and the second is
	// fatal: (1) the mode changes nothing the backend does — the host directory is
	// identical in all three, ro is enforced at RUNTIME and exclusivity in the
	// CONTROL PLANE, both decided before this call; (2) the CARDINALITY is wrong —
	// one volume can be shared-rw to sandbox A and shared-ro to sandbox C at the
	// same time, so the mode is per-MOUNT-ROW while this call is per-VOLUME.
	Materialize(ctx context.Context, volumeID string) (*Materialized, error)

	// Release ends ONE materialization (local: a no-op; a remote arm: unmount the
	// FUSE point).
	//
	// IT IS NOT CALLED Unmount, deliberately: that word names a mechanism the
	// DEFAULT arm does not have, and naming a seam after one implementation's
	// mechanism is exactly how a seam grows into that implementation's shape. The
	// contract is a PROMISE — this materialization's source is no longer valid, the
	// BYTES SURVIVE, it is idempotent, and it tolerates a target that has already
	// gone. A local no-op is therefore a correct implementation of an explicit
	// promise (the position runtime.Stop is in), not a smell. The smell would be one
	// name carrying two undefined contracts ("unmount, or nothing").
	Release(ctx context.Context, m *Materialized) error

	// AdoptMaterialized re-recognizes a still-live materialization after a
	// control-plane crash (§4.4-A) — rootfs.AdoptMerged's twin. It does not create:
	// a volume whose materialization is gone is not adoptable, and saying so is what
	// lets the mediator tell "recover the handle" from "materialize afresh".
	AdoptMaterialized(ctx context.Context, volumeID string) (*Materialized, error)

	// OpenRoot returns the root-confined content accessor for volumeID, usable
	// WHILE UNMOUNTED (§5.C / volume.md D5.1). See Root.
	//
	// This is what makes "the volume's lifecycle is decoupled from any sandbox"
	// true in SUBSTANCE rather than only in the record layer: seeding a volume from
	// scratch (before any sandbox exists) and reading a volume after the last
	// sandbox is gone both have no other route.
	OpenRoot(ctx context.Context, volumeID string) (Root, error)

	// DestroyVolume reclaims one volume's backend bytes (terminal). IDEMPOTENT:
	// §4.4-A reaches it from several paths (delete, unwind, the orphan sweep) and it
	// must never fail because someone else already cleaned up.
	DestroyVolume(ctx context.Context, volumeID string) error

	// UncappedBytes reports the bytes this backend holds THAT NO DECLARED CAP
	// COVERS — the §5.C half of the §3.4 inequality's second term, the same shape
	// rootfs.Provider.UncappedBytes has on the §5.B side. There is no third term:
	// volume caps and sandbox caps are like terms on the same left-hand side (both
	// are "upper bounds the engine has declared"), even though a volume cap is
	// recorded on the volume rather than in some sandbox's Binding.
	//
	// THIS IS A REAL GAP, NOT DECORATION: without it every uncapped volume's bytes
	// are INVISIBLE to the admission gate, and a node can be filled by a pile of
	// uncapped volumes without the gate ever refusing once.
	//
	// Two invariants, identical to the §5.B side: a CAPPED volume contributes ZERO
	// (its cap is already counted by the mediator, and adding its usage on top would
	// bill the same bytes twice), and physically shared bytes count at most once.
	// The number comes from the BACKEND's own allocation statement, never from a
	// guest and never from apparent size.
	UncappedBytes(ctx context.Context) (int64, error)

	// SweepOrphanVolumes reclaims volume storage UNITS with no live `volumes` row
	// (§4.4-A reverse reconciliation). Same shape as every reverse reconcile: the
	// mediator reads the AUTHORITATIVE store to compute the keep-sets and hands them
	// in; the backend enumerates only its OWN storage and deletes the difference
	// (§4: subsystems never read the store).
	//
	// Its twin is SweepOrphanOverlays, NOT SweepOrphanLayers, and the difference is
	// structural: a layer is a read-only base SHARED by lineage and refcount ("does
	// any row still reference it"), while a volume — like an Overlay unit — is a
	// SELF-SUFFICIENT unit ("does this unit still have a live owner"). Volumes are
	// the latter, so the sweep is per-unit.
	//
	// `building` is MORE necessary here than on the Overlay side. There it guards
	// only a stale keep-set (the row is inserted first); volume creation is
	// BYTES-FIRST (the ID must name the unit before any row exists), so a perfectly
	// healthy in-flight create presents as "bytes, no row" EVEN WITH A FRESH
	// keep-set. Snapshot/Clone are two more producers of the same window.
	//
	// MID-SWEEP FAILURE IS CONTINUE, NOT ABORT: this is a best-effort reclamation
	// path, and one stuck unit must not leave every other leak in place. It returns
	// the count actually reclaimed.
	//
	// THE SAFETY REQUIREMENTS ARE PART OF THE CONTRACT (§5.C), because every one of
	// them fails SILENTLY when violated — a mis-sweep has NO detection path at all
	// (measured: five ways of yanking a volume out from under a live guest killed no
	// sandbox, the control plane saw nothing, and in three of them the guest's writes
	// KEPT RETURNING SUCCESS into an unlinked inode):
	//
	//  1. ENUMERATE BY PROBING EACH DIRECTORY ENTRY. Never use a filesystem-level
	//     unit enumeration: its scope is the whole filesystem rather than the given
	//     directory (measured: run in the volumes dir and in a sibling dir it returns
	//     byte-identical results, including units belonging to other software on the
	//     node).
	//  2. SKIP ON ANY ENUMERATION OR READ ERROR — NEVER treat an empty result as
	//     "no owner". Measured: under concurrent churn a bulk quota-row read fails
	//     WHOLESALE and returns zero rows, and those are exactly the conditions a
	//     sweep runs in.
	//  3. The mediator additionally passes every volume that appears in ANY mount
	//     row (live or not) inside `live`, which is a check independent of any FK.
	SweepOrphanVolumes(ctx context.Context, live, building []string) (int, error)
}
