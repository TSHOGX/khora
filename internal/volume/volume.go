// Package volume is subsystem the Volume subsystem: decoupled, ID-addressed storage whose lifecycle is
// independent of any sandbox — it exists before one, outlives one, and exists while nothing
// has it mounted at all (the Volume subsystem).
//
// A SEPARATE subsystem with its own seam, deliberately not sharing the Rootfs subsystem's (the volume backend seam):
// rootfs.Provider materializes a LAYERED composition, volume.Backend materializes ONE
// self-sufficient unit — no lower stack, no lineage walk. They share substrate primitives
// (the internal/btrfs leaf), which is shared code and not a shared interface.
//
// The seam follows the runtime provider contract / the Rootfs subsystem convention: IDs cross it, host paths stay inside; policy
// and handles, never host primitives. seam_test.go enforces that structurally.
package volume

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrUnsupported is the typed refusal for a capability a backend does not report in
// Capabilities — a plain-directory backend asked to snapshot, or to enforce a cap it cannot.
// Typed because the API error contract maps it to a 400, which no retry or state change makes legal.
var ErrUnsupported = errors.New("volume: operation not supported by this backend")

// Capabilities is the PER-BACKEND capability query (the volume backend seam) — a property of the
// implementation, never of one unit.
//
// EVERY MEMBER IS A bool: a map[string]string feature table would carry mechanism names
// across the seam as VALUES, where no identifier-scanning guard can see them.
// seam_test.go asserts the bool-ness.
//
// Members need a CONSUMER (the extension boundary forbids speculative reservation): Snapshot/Clone gate the
// the control-plane API endpoints, Quota gates "a cap that cannot be enforced is refused" (resource admission).
var ErrUsageUnavailable = errors.New("volume: usage measurement unavailable")

type Capabilities struct {
	// Usage reports reliable per-unit and aggregate allocation measurements.
	Usage bool
	// Snapshot reports whether the backend can take a point-in-time copy of a
	// volume's bytes (the Volume subsystem: local = an atomic whole-tree snapshot of the unit).
	Snapshot bool
	// Clone reports whether the backend can produce an independent writable copy.
	// Same substrate path as Snapshot on the local arm; the two differ in the
	// semantic label, not the mechanism (snapshot lineage).
	Clone bool
	// Quota reports whether the backend can ENFORCE a per-volume capacity cap
	// (resource admission). False means a non-zero cap must be REFUSED with ErrUnsupported, not
	// accepted and ignored.
	Quota bool
}

// Materialized is a handle to one materialization of a volume: a host mount point ready to
// hand the runtime provider as an OCI bind source, exactly as the Rootfs subsystem hands over a merged
// rootfs (the Volume subsystem: the seam yields ONE thing).
type Materialized struct {
	// Volume is the ID this handle materializes — the identity half, required so Release
	// can resolve which volume a handle belongs to.
	Volume string

	// SourcePath is the host mount point, consumed ONLY by local_runsc when it writes the
	// OCI bind — the same exemption rootfs.Merged.MergedPath holds. Nothing above the Volume subsystem
	// interprets it, and the API error contract keeps it off the wire.
	SourcePath string

	// NO handle FIELD: members need a consumer (the extension boundary forbids speculative reservation).
	// the volume backend seam states the precise condition for adding one back.
}

// FileInfo is one entry's metadata as reported through Root — deliberately small and
// os-independent. It carries no host path: a Root addresses everything relative to the
// volume root, so an absolute name would be both useless and a leak.
type FileInfo struct {
	Name  string // base name within the listed/stat'ed directory
	Size  int64  // apparent size in bytes (0 for directories)
	Mode  fs.FileMode
	IsDir bool
	// Type preserves the entry kind under LSTAT; a symlink is never silently
	// presented as its target.
	Type EntryType
	// SymlinkTarget is populated only for symlink entries.
	SymlinkTarget string
	ModTime       time.Time
}

type EntryType string

const (
	EntryUnknown   EntryType = "unknown"
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "directory"
	EntrySymlink   EntryType = "symlink"
	EntryOther     EntryType = "other"
)

// Root is the root-confined content accessor used while a Volume is unmounted.
// Every path is relative to the volume root and out-of-root targets are unreachable
// — a property of how paths resolve, not an error the caller must remember to check. No
// member exposes a host path, so it is the one materialization-class part of the seam
// needing no exemption in the structural guard.
//
// Not a replacement for Materialize (that hands runsc a bindable host path; this only moves
// bytes inside the engine process — two consumers, two methods), and not routed through the transport data plane
// `fs`: that service is reachable only through a live placement, and the route would create a
// volume → transport edge that the architecture guard rejects outright.
//
// Concurrency: safe for concurrent use, each operation resolving its own descriptor.
type Root interface {
	// Stat reports metadata for one path ("." = the volume root itself).
	Stat(name string) (FileInfo, error)
	// Lstat reports the final directory entry itself, preserving symlink type
	// and target rather than following it.
	Lstat(name string) (FileInfo, error)
	// List enumerates one directory's entries (non-recursive).
	List(name string) ([]FileInfo, error)
	// ReadAt reads len(p) bytes from name starting at off, with io.Reader's
	// short-read convention (n < len(p) with io.EOF at end of file). Reading a
	// range rather than a whole file is what lets export STREAM (the control-plane API: bytes
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

// Backend is the Volume subsystem storage seam: one backend per storage form, each yielding the SAME one
// thing — a host mount point that the runtime provider can bind. The local arm is a host directory with a cap
// and cheap atomic snapshots; a remote arm (S3 / NFS) would be a host-side FUSE point, and
// the two are indistinguishable above this seam.
//
// A SHAPE RULE RUNS THROUGH THE WHOLE INTERFACE: "allocate", "set the cap" and "place" are
// THREE actions and must not be compressed into one call (the volume backend seam) — merging them
// leaves creation, capacity and the admission term all homeless. FALSIFIED WITH A SECOND
// BACKEND rather than reasoned about: the plain-directory arm runs the same
// contract suite, and a merged shape fails in each of those three places (see PlainDir).
type Backend interface {
	// Name identifies the backend for registration and diagnostics.
	Name() string

	// Capabilities reports what this backend can do.
	//
	// TIMING IS LOAD-BEARING: the mediator MUST ask BEFORE it allocates an ID or moves a byte.
	// An external implementation would map "unsupported" at its HTTP boundary while the error
	// is born deep inside, so the 400 arrives only after the source is fetched and
	// materialized — leaving a window whose side effects no sweep can classify.
	Capabilities() Capabilities

	// CreateVolume allocates the backend storage for volumeID and applies capBytes in the SAME
	// operation. capBytes crosses as a PLAIN BYTE COUNT: the caller states intent, the backend
	// picks the mechanism — which is why capacity is not a cross-cutting interface, as that
	// would weld one mechanism into every implementation (the shared btrfs mechanism).
	//
	// 0 = uncapped. A non-zero cap without Capabilities().Quota is ErrUnsupported — refused,
	// never downgraded to "run uncapped" (resource admission).
	//
	// bytes-first (health and reclamation existence-is-state): the `volumes` row is inserted only after this
	// returns, so the unit must be fully materialized here. A crash in between leaves orphan
	// bytes the sweep reclaims, never a row pointing at storage that does not exist.
	CreateVolume(ctx context.Context, volumeID string, capBytes int64) error

	// SetCapacity re-applies an EXISTING volume's cap (the control-plane API PATCH). Its own method because a
	// cap must be editable while nothing has the volume mounted, which a placement entry point
	// cannot express. (rootfs.Provider needs a CapUnchanged sentinel precisely because its cap
	// intent rides OpenOverlay; here the call IS the intent.)
	//
	// 0 removes the cap. ErrUnsupported for a non-zero cap without Quota capability.
	SetCapacity(ctx context.Context, volumeID string, capBytes int64) error

	// Usage returns ErrUsageUnavailable when Capabilities.Usage is false.
	// Otherwise it reports the backend's own statement of one volume's usage and the cap on its
	// storage. The cap write point forces it (the Volume subsystem): a PATCH refuses on a STRICT inequality
	// against current usage. inspect reports usage through the same call.
	//
	// THE limit RESULT IS DIAGNOSTIC ONLY — NEVER USE IT TO VALIDATE. The local substrate maps
	// "no cap" and 0 onto each other in both directions, so "never capped" and "deliberately
	// uncapped" read back identical (volume cap enforcement). The record is always the authority, and
	// any design planning to "verify afterwards that the cap was set" is unimplementable here.
	Usage(ctx context.Context, volumeID string) (used, limit int64, err error)

	// Materialize prepares a host mount point for volumeID, ready to bind into a guest.
	//
	// CREATE-IF-ABSENT, not open-only (the Volume subsystem, measured): runsc fails hard on a missing bind
	// source rather than creating it, so open-only would make a mis-swept sandbox PERMANENTLY
	// unresumable where create-if-absent restarts cleanly (data lost, sandbox recoverable).
	//
	// CONCURRENCY IS PART OF THE CONTRACT: one volume may be materialized concurrently any
	// number of times. The shared-fanout design rests on it (measured: two sandboxes rw-binding
	// one source both succeed, the second genuinely writable) — without it AccessMode would
	// collapse to Exclusive alone.
	//
	// No access mode, and the second reason is fatal: (1) the mode changes nothing the backend
	// does — ro is enforced at RUNTIME, exclusivity in the CONTROL PLANE, both decided before
	// this call; (2) the CARDINALITY is wrong — one volume can be shared-rw to A and shared-ro
	// to C at once, so the mode is per-MOUNT-ROW while this call is per-VOLUME.
	Materialize(ctx context.Context, volumeID string) (*Materialized, error)

	// Release ends ONE materialization (local: a no-op; a remote arm: unmount the FUSE point).
	//
	// NOT called Unmount, deliberately: that names a mechanism the default arm does not have,
	// and naming a seam after one implementation's mechanism is how a seam grows into that
	// implementation's shape. The contract is a PROMISE — this source is no longer valid, the
	// bytes survive, it is idempotent, and it tolerates a target already gone. A local no-op is
	// a correct implementation of an explicit promise (runtime.Stop's position), not a smell.
	Release(ctx context.Context, m *Materialized) error

	// AdoptMaterialized re-recognizes a still-live materialization after a
	// control-plane crash (atomic transition rollback) — rootfs.AdoptMerged's twin. It does not create:
	// a volume whose materialization is gone is not adoptable, and saying so is what
	// lets the mediator tell "recover the handle" from "materialize afresh".
	AdoptMaterialized(ctx context.Context, volumeID string) (*Materialized, error)

	// Snapshot creates newID as a point-in-time copy of srcID's bytes; Clone makes an
	// independent writable copy. Both produce AN ORDINARY WRITABLE VOLUME with no read-only
	// marker (snapshot lineage) — same object shape, same local substrate path, differing in
	// the semantic label a caller attaches. Immutability is expressed with a shared-ro MOUNT
	// ROW, the same cardinality argument that keeps AccessMode off the volume.
	//
	// THE NEW ID IS ALLOCATED BY THE MEDIATOR, not minted here: bytes a backend named itself
	// cannot have been in the in-flight keep-set before they existed, so a crash midway leaves
	// bytes under a name no sweep can classify.
	//
	// capBytes IS NEITHER OPTIONAL NOR INHERITED, because there is NOTHING TO INHERIT
	// (measured, volume cap enforcement): the substrate gives a snapshot a brand-new UNCAPPED quota row
	// in all four shapes tested. Verified by WRITING, not by reading a number — 96 MiB (3× the
	// source's cap) landed in full on an uncapped snapshot.
	//
	// So setting the cap is a MANDATORY step of this method, necessarily a SECOND call, never
	// left to the caller to PATCH: a miss is undetectable afterwards (see Usage). The window it
	// creates ("bytes exist, cap not yet set") is covered by the mediator holding the keep-set
	// across both calls and by the sweep treating "record exists, row uncapped" as a REPAIR
	// state rather than a legal one.
	//
	// `snapshot -i <qgroupid>` is EXPLICITLY EXCLUDED as a way to make this one call: it sets
	// MEMBERSHIP, not a cap — the new unit's row stays uncapped while the parent's limit is
	// enforced across ALL members, so one member exhausting it makes its SIBLINGS hit ENOSPC.
	// Looks correct in a single-volume test, takes out neighbours in production.
	//
	// Without the corresponding capability: ErrUnsupported, asked before any ID or byte moves.
	Snapshot(ctx context.Context, srcID, newID string, capBytes int64) error
	Clone(ctx context.Context, srcID, newID string, capBytes int64) error

	// ReclaimQuotaRows reclaims quota rows LEAKED by earlier deletions (Volume quota cleanup).
	//
	// A CORRECTNESS PATH, NOT TIDINESS: a leaked row does not reserve its own unit id, so a
	// remount can hand that id — along with its cap — to a brand-new unit, which then hits
	// ENOSPC on a limit nobody set for it (measured: a unit that never had a limit hit ENOSPC
	// at 3.3 MB) or is silently over-provisioned. Undetectable from the inside: the limit reads
	// back looking exactly like the unit's own.
	//
	// The predicate is the row's LABEL, polled: the transition is CONDITION-triggered, so the
	// first attempt succeeds once the sharer is gone and sleeping-and-hoping is the wrong shape.
	//
	// ids are those recorded AT DELETION TIME. Never scan for reclaimable-looking rows (the Volume subsystem) —
	// a scan that misidentifies one produces an uncapped live volume with zero signal.
	//
	// Rows not yet reclaimable are LEFT for a later round: a `<squota space holder>` can
	// legitimately resist destruction for a live clone's whole lifetime, so it is not an error
	// path and must not spin on EBUSY.
	ReclaimQuotaRows(ctx context.Context, ids []uint64) (int, error)

	// DeletedRowIDs reports the quota-row ids recorded at deletion time and not yet reclaimed —
	// ReclaimQuotaRows' input. On the backend because it is substrate-specific residue rather
	// than engine state: a backend with no quota rows returns nothing and the path costs it
	// nothing, with no capability branch in the caller (the extension boundary).
	DeletedRowIDs(ctx context.Context) []uint64

	// OpenRoot returns the root-confined content accessor for volumeID, usable WHILE UNMOUNTED
	// by design. This is what makes "the volume's lifecycle is decoupled from any
	// sandbox" true in substance rather than only in the record layer: seeding a volume before
	// any sandbox exists, and reading one after the last is gone, have no other route.
	OpenRoot(ctx context.Context, volumeID string) (Root, error)

	// DestroyVolume reclaims one volume's backend bytes (terminal). IDEMPOTENT:
	// atomic transition rollback reaches it from several paths (delete, unwind, the orphan sweep) and it
	// must never fail because someone else already cleaned up.
	DestroyVolume(ctx context.Context, volumeID string) error

	// UncappedBytes returns ErrUsageUnavailable when Capabilities.Usage is false.
	// Otherwise it reports the bytes this backend holds THAT NO DECLARED CAP COVERS — the Volume subsystem
	// half of the resource admission inequality's second term, the same shape as rootfs.Provider's. No third
	// term: volume caps and sandbox caps are like terms on the same left-hand side. Without it,
	// uncapped volumes are invisible to the admission gate and a node can be filled by a pile
	// of them without the gate refusing once.
	//
	// Two invariants, identical to the Rootfs subsystem side: a CAPPED volume contributes ZERO, and
	// physically shared bytes count at most once. The number is the BACKEND's own allocation
	// statement, never a guest's and never apparent size.
	UncappedBytes(ctx context.Context) (int64, error)

	// ListUnitIDs enumerates the volume IDs this backend holds storage for — the ENUMERATION
	// HALF of orphan reclamation, the mediator deciding and calling DestroyVolume per orphan.
	//
	// The split was forced by a MEASURED DEFECT in the combined `Sweep(live, building)` shape:
	// a volume whose whole creation fit between the mediator's two reads appeared in NEITHER
	// set, so the backend deleted a fully committed volume's bytes (3 of 6 snapshots, under 458
	// overlapping sweeps). The Overlay sweep keeps that shape because its row is inserted
	// BEFORE the unit, exposing only the snapshot skew; VOLUMES ARE BYTES-FIRST, so the exposed
	// interval is the entire create. Splitting closes it by ordering rather than locking, the
	// mediator re-checking each candidate after enumeration (the volume backend seam).
	//
	// ENUMERATION MUST BE PER-DIRECTORY-ENTRY (the Volume subsystem), never a filesystem-level unit enumeration
	// — that returns byte-identical results from a sibling directory, including units belonging
	// to other software on the node (measured).
	//
	// An enumeration error is returned and the pass reclaims NOTHING. An empty list must never
	// come from a failed read: a caller cannot tell "no units" from "the read failed" (measured
	// — under a sweep's concurrent churn, a bulk quota read fails wholesale and returns zero).
	ListUnitIDs(ctx context.Context) ([]string, error)
}
