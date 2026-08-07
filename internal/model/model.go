// Package model holds the shared engine model (§3): sandbox identity,
// lifecycle state, and the three-state rootfs model. It is pure types with no
// dependency on any subsystem — every subsystem depends on model, model depends
// on none of them (§5: subsystems are peers over the shared model, never each
// other).
package model

import "time"

// SandboxID is the stable, opaque identity of a persistent sandbox (§3.3).
// It does NOT encode placement: after resume the IP / container / cgroup / node
// may all change, but the SandboxID is unchanged — same Overlay artifact, same
// sandbox (§3.3's iron rule).
type SandboxID string

// TemplateID is the opaque identity of an immutable read-only base (§3.1).
// Immutability is enforced by the read-only lower + refcount protection; the
// identity is opaque, not a content hash.
type TemplateID string

// OverlayID identifies the sandbox's writable delta layer as a first-class,
// ID-addressed, Template-decoupled artifact (§3.1.1). The host path where it
// materializes is a rootfs-provider implementation detail and never leaks above
// the Rootfs subsystem (§3.1.1: ID-addressed internally too, not just at the
// external API).
type OverlayID string

// Template is a first-class persistent object: an immutable, read-only base a
// sandbox cold-starts from (§3.1 / §4.3). It is the sharpest application of the
// uniform object model — lineage — so it is the first object to materialize the
// {derived refcount} pattern (§4.3):
//
//   - No State field. Template uses "existence IS the state" (§4.3): the row
//     existing = ready, deleting the row = destroyed, no intermediate state.
//     This is what FORCES bytes-first materialization (§5.B): with no not-ready
//     escape hatch, the layer bytes MUST be fully on the backend before the row
//     is inserted, or a crash leaves a row pointing at nonexistent bytes.
//   - No refcount field. "Is it deletable" is DERIVED (§4.3): it has no
//     referencing live rows (no sandbox references it, no child Template names it
//     as Parent). The store's FK RESTRICT (SQLite) / Go check (Memory) enforces
//     "referenced → cannot delete" as a contract promise, not a materialized
//     count.
type Template struct {
	// ID is the opaque identity (§3.1): "tmpl_<token>" for Promote-built,
	// the static directory name for builtin.
	ID TemplateID

	// Parent expresses lineage (§4.3 / §5.B multi-lowerdir): the Template this
	// one layers on top of. Empty = a base layer (no parent). A referencing
	// child protects its parent from deletion (FK RESTRICT, §4.3: references
	// point upward).
	Parent TemplateID

	// Builtin marks a platform-preseeded Template (boot-seed reconciled from a
	// static TemplatesDir directory, §4.3 existence-as-state / §4.4-A idempotent
	// reconcile) vs one produced by Promote. Both are
	// equally protected by RESTRICT — a referenced builtin is just as
	// undeletable as a referenced Promoted layer (§6 DELETE).
	Builtin bool

	CreatedAt time.Time
}

// VolumeID is the opaque identity of a Volume (§5.C): decoupled, ID-addressed
// storage whose lifecycle is independent of any sandbox — created before one,
// surviving after one, existing while nothing has it mounted.
//
// Like every other ID in the engine it is opaque and does NOT encode placement or
// a host path: the backend resolves the storage internally and only an unreadable
// source handle surfaces from it (§3.1.1 addressed by ID internally too / §6.2 no
// host paths on the wire).
type VolumeID string

// Volume is a first-class persistent object (§4.3), and its field list is
// SHORTER than it looks like it should be — each absence is a decision:
//
//   - No State field. Existence IS the state (§4.3), exactly as with Template:
//     the row existing = ready, deleting it = destroyed, no intermediate. That is
//     what FORCES bytes-first materialization (§5.C): with no not-ready escape
//     hatch the backend storage MUST be fully allocated before this row is
//     inserted, or a crash leaves a row naming storage that does not exist.
//   - No refcount field. "Is it deletable" is DERIVED (§4.3): no mount row
//     references it, no child volume names it as parent.
//   - NO AccessMode. It is a property of the MOUNT ROW, not of the volume, and
//     the reason is cardinality rather than taste: one volume can be shared-rw to
//     sandbox A and shared-ro to sandbox C AT THE SAME TIME (measured), so a
//     per-volume field could not carry it honestly (§5.C).
//   - No lease/owner field. An exclusive lease is not an object — it IS the
//     existence of an `exclusive` mount row (§5.C / volume.md D3/D4).
//
// The cap deliberately does NOT live in model.Limits: the three Limits axes are
// per-sandbox Binding materialized at each placement, while a volume cap is a
// property of the volume recorded here — a volume has no placement of its own
// (§3.4 / §5.C). It nonetheless counts in the FIRST term of the §3.4 node
// inequality, alongside sandbox caps: both are "upper bounds the engine has
// declared", so no third term is added.
type Volume struct {
	// ID is the opaque identity ("vol_<token>").
	ID VolumeID

	// Backend names which registered volume.Backend materializes this volume
	// (§5.C two arms). Recorded rather than inferred: the mediator must be able to
	// route a later operation to the SAME backend that allocated the bytes.
	Backend string

	// Parent expresses LINEAGE: the volume this one was snapshotted or cloned from.
	// Empty = not derived from anything. `REFERENCES volumes(id) ON DELETE RESTRICT`,
	// exactly like templates.parent (§4.3 lineage, references point upward).
	//
	// THE FK IS A CORRECTNESS ARGUMENT, NOT TIDINESS (volume.md D6.1a / D7.1). A quota
	// row leaks precisely when a unit that OWNS extents is deleted while a live sharer
	// still references them — and measured, "delete the sharers first, then the source"
	// leaks NOTHING. RESTRICT makes that order THE ONLY REACHABLE ONE: deleting a source
	// with a live descendant becomes a 409, so the leak is PREVENTED rather than
	// reclaimed. Free, and it introduces no new mechanism — it is §1.3's "add a join
	// table" (self-referential here) reusing the existing FK-violation predicate.
	//
	// An external implementation is the counter-example that pins this: it chose a
	// POINTER WITH NO CONSTRAINT (ADD COLUMN + index, no REFERENCES), so it had to
	// hand-write children checks in three places — and all three were RECLAMATION
	// guards, none a DELETE guard. The control-plane row therefore disappeared while the
	// bytes had to stay, and since every reclamation predicate was keyed on the source's
	// own record, deleting that record made the bytes permanently unfindable. Our
	// SweepOrphanVolumes would cover that case, but it SHOULD NOT HAVE TO.
	Parent VolumeID

	// CapByte is the per-volume capacity cap in bytes, 0 = uncapped (§5.C / §3.4).
	//
	// THIS RECORD IS THE AUTHORITY, always. The backend's read-back cannot serve:
	// the local substrate maps "no cap" and 0 onto each other in both directions, so
	// "never capped" and "deliberately uncapped" are byte-for-byte identical when
	// read from storage (volume.md D7.3). Any design that plans to reconcile the cap
	// against the backend afterwards is unimplementable on that substrate — which is
	// why the cap must be written in the same backend operation that creates the
	// bytes.
	CapByte int64

	CreatedAt time.Time
}

// AccessMode is how one MOUNT views a volume (§5.C). Three modes, and all three are
// materialized with NO special mechanism — the same host directory reaches one or N
// guests (measured; volume.md D1).
//
// IT IS A PROPERTY OF THE MOUNT ROW, NOT OF THE VOLUME, and the reason is
// cardinality rather than taste: one volume can be shared-rw to sandbox A and
// shared-ro to sandbox C AT THE SAME TIME (measured), so a per-volume field could not
// carry it honestly. That is why it appears here and on the mount row, and nowhere on
// model.Volume.
//
// The two halves are enforced in DIFFERENT places, and the split is load-bearing:
// READ-ONLY is enforced AT RUNTIME (a read-only bind ⇒ the guest's write gets EROFS,
// a clean POSIX refusal rather than a silent success), while EXCLUSIVITY is enforced
// in the CONTROL PLANE (the mount row itself). Both are decided before the backend is
// called, which is why the backend never learns the mode.
type AccessMode string

const (
	// Exclusive means no other mount row may coexist for this volume. The lease IS
	// this row's existence — there is no Lease object and no owner field (§5.C /
	// volume.md D3/D4).
	Exclusive AccessMode = "exclusive"
	// SharedReadOnly is a read-only fanout mount. The guest's writes get EROFS.
	SharedReadOnly AccessMode = "shared-ro"
	// SharedReadWrite is a shared writable mount. It guarantees POSIX semantics only
	// for multiple connections WITHIN one sandbox; CROSS-SANDBOX rw coherence is
	// explicitly UNDEFINED (§5.C). Gofer and sentry each cache, so shared mutable
	// consistency across an isolation boundary is unreliable in principle — the honest
	// position is to promise nothing rather than to build a fragile mechanism that
	// pretends to.
	SharedReadWrite AccessMode = "shared-rw"
)

// ReadOnly reports whether this mode materializes as a read-only bind.
func (a AccessMode) ReadOnly() bool { return a == SharedReadOnly }

// Valid reports whether a is one of the three defined modes. The schema asserts the
// same set, and that assertion is REQUIRED rather than belt-and-braces: the exclusive
// lease's partial index has a LITERAL STRING predicate, so a mis-cased value would
// make the index simply not apply and let two exclusive rows both insert — a silent
// failure of the one new invariant Volume introduces (§5.C).
func (a AccessMode) Valid() bool {
	switch a {
	case Exclusive, SharedReadOnly, SharedReadWrite:
		return true
	}
	return false
}

// Mount is one row of the mount table: a FIRST-CLASS PERSISTENT OBJECT (§4.3), and
// structurally just a doubling of the `sandboxes.template` reference — two columns,
// each with ON DELETE RESTRICT, which is what gives both "a volume with a live mount
// row cannot be deleted" and "a sandbox with a live mount row cannot be deleted".
//
// The mount relationship is a BINDING (§3.4): what is mounted, where, under which
// mode is recorded here, and a resume RE-MOUNTS from the CURRENT table rather than
// restoring from frozen bytes — a volume is always the one real thing, never forked
// into a per-sandbox copy.
//
// Like Template and Volume it has NO state column and NO count column: existence is
// the state, and "is this volume exclusively held" is DERIVED from the rows' presence
// and access (§4.3). It is NOT a §4.1 lifecycle transition, so creating or deleting a
// mount row publishes NO event.
type Mount struct {
	// Sandbox is the sandbox this mount belongs to. REFERENCES sandboxes(id) ON
	// DELETE RESTRICT.
	Sandbox SandboxID
	// Volume is the volume mounted. REFERENCES volumes(id) ON DELETE RESTRICT.
	Volume VolumeID
	// Path is the guest destination, and it MUST be normalized before insert —
	// absolute, cleaned, not "/" (§5.C). `/data`, `/data/`, and `/data/../data` are one
	// mount point spelled three ways, and the UNIQUE(sandbox, path) assertion is a
	// literal string comparison, so an un-normalized path makes the assertion SILENTLY
	// not apply. Same trap as the CHECK beside the lease index.
	Path string
	// Access is how this mount views the volume. Per-ROW, never per-volume.
	Access AccessMode
}

// Lifecycle is the §4.1 state machine. Exactly four states — no `failed`
// intermediate state (§4.4: the absence is a conclusion of the model, not an
// omission), no independent generation counter (§3.3 uses Placement / §4.3 uses
// placement-IP-as-epoch instead).
type Lifecycle string

const (
	Creating  Lifecycle = "creating"
	Running   Lifecycle = "running"
	Paused    Lifecycle = "paused"
	Destroyed Lifecycle = "destroyed"
)

// Limits is the per-sandbox resource-limit Binding (§3.4). SoT is the
// control-plane record: the values are stable across resume and RE-MATERIALIZED
// at each placement from the record, never read back from a frozen placement.
//
// Three dimensions, three mechanisms, three DIFFERENT overrun consequences —
// they are deliberately NOT interchangeable knobs (§3.4):
//
//   - CPUMillis  → §5.A cgroup `cpu` controller. Overrun = the guest runs
//     slower, no error. A resource limit (fair sharing).
//   - DiskByte   → §5.B FS-layer quota on the Overlay upper+work (NOT cgroup —
//     cgroup cannot cap at-rest bytes; §3.4: capacity and I/O rate are two
//     orthogonal axes).
//     Overrun = the guest's write() gets ENOSPC and THE SANDBOX LIVES ON: a
//     data-plane event the control plane does not observe.
//   - MemoryByte → §5.A host memcg. Overrun TERMINATES the sandbox, so this one
//     is a SAFETY limit bounding blast radius (§2.3), not a fair-sharing limit:
//     the substrate offers no "guest process gets ENOMEM while the sandbox
//     survives" path, so it is a lifecycle event the control plane MUST see
//     (§4.3 resource-terminated).
//
// Zero on any dimension = unlimited (that dimension is not materialized).
//
// The disk-IO rate cap (§3.4's other rate-axis half) has NO field here on
// purpose, and that is a settled deferral rather than an open question: the
// mechanism is proven, the CONSUMER is what's missing, so adding the field now
// would be the speculative reservation §1.3 rejects. It joins additively when a
// consumer appears (one field here + one OCI section, no shape change) — and
// what joins is a RATE and nothing more: the whole-disk MAJ:MIN that `io.max`
// needs is DERIVED BY THE PROVIDER from its own state dir, never carried in this
// record and never crossing the §5.A seam. A device number is an irreducibly
// host-local fact, so putting it in the Binding would lift a host primitive into
// a cross-provider contract (§5.A); a remote provider expresses the same cap
// through its scheduler's equivalent instead.
type Limits struct {
	CPUMillis  int64 // 1000 = one full CPU
	MemoryByte int64
	DiskByte   int64
}

// DeathCause is why a sandbox's isolation unit is gone (§4.3). It is an ABSTRACT
// ENUM, never an errno or a cgroup file: local_runsc concludes it from its own
// memcg, a remote provider from its scheduler's termination reason, and the §4
// mediator only ever sees these three values (§2.4 / §5.A Adopt contract). That
// is what makes resource-terminated work across providers instead of being a
// host-local hack.
//
// It is persisted on the sandbox record because the signal is VOLATILE — it
// lives in placement-local state and vanishes when the placement is torn down,
// so "ask again later" does not exist. It is written in the SAME commit as the
// crash-paused demotion CAS (§4.3 / §4.4-C).
type DeathCause string

const (
	// CauseNone is the resting value for a sandbox that has not been observed
	// dead. Not a death cause — the absence of one.
	CauseNone DeathCause = ""
	// CauseUnknown = the unit is gone but WHY could not be determined. A
	// first-class legal value, NOT an error (§4.3): the disposition is to treat it
	// as dead-recoverable, which a caller may then resume. Self-correction is
	// STRUCTURAL rather than counted: if it really was the RAM cap, the same
	// workload re-triggers termination after that resume — this time in the control
	// plane's plain view — and THAT observation records the real cause, after which
	// the persisted fact refuses further resumes. There is no resume-attempt bound
	// and none is needed: the engine never resumes a dead-recoverable sandbox
	// itself (§4.1 lists no such automatic transition), so the retry is a caller's
	// explicit choice, not an engine loop. Providers MUST report this rather than
	// guess (§2.4: no guessing).
	CauseUnknown DeathCause = "unknown"
	// CauseGone = the unit died for a reason that is NOT a resource cap (sentry
	// crash, external kill, clean guest exit). Disposition: dead-recoverable →
	// resume (§4.3).
	CauseGone DeathCause = "gone"
	// CauseResourceExceeded = the unit was terminated for exceeding a resource
	// cap (today: the RAM safety limit, whole-group). Disposition:
	// resource-terminated → do NOT resume (§4.3) — resuming re-runs the same
	// workload into the same cap, which is a crash loop.
	CauseResourceExceeded DeathCause = "resource-exceeded"
)

// Sandbox is the persistent unit (§3.2): the durable record the control plane
// owns. Placement is deliberately NOT a field here — it is rebound every resume
// and lives with the runtime provider, not in the identity record (§3.3).
//
// Minimal shape by one rule: a field exists here only when this record actually
// carries it. The mount topology is consumed but lives in its own table (§5.C)
// keyed by sandbox, not as a field here — a sandbox's volume set is many-to-many
// and mutable independently of the identity record. The network declaration
// (§5.D) is not carried at all yet and is omitted rather than stubbed, since an
// unused field would be a speculative reservation (§1.3). (Contrast the runtime provider's
// SteeringSpec, which DOES exist: it is a named member of the settled §5.A
// contract 3-tuple — "settled, not yet built".)
type Sandbox struct {
	ID       SandboxID
	Template TemplateID
	Overlay  OverlayID // the writable delta this sandbox owns (§3.1.1)
	State    Lifecycle

	// No guest-identity field: the sandbox runs as guest root by default and the
	// engine does not model guest users (§2.2 — guest identity is not a
	// host-isolation pillar). "Run as user X" is a guest-native concern (su/sudo),
	// not engine state (§3.2.1).

	// IdleTimeout and Retention are the lifecycle TTL — per-sandbox Binding
	// fields (§3.4: SoT is the control-plane record, stable across resume,
	// materialized each placement), consumed by the reaper (§4.3):
	//   - IdleTimeout: a `running` sandbox idle (no I/O) longer than this →
	//     auto-pause (§4.1 automatic transition).
	//   - Retention: a `paused` sandbox older than this → evict (reaper's
	//     destroy of a paused sandbox, §4.1).
	// These EXIST here (unlike the still-omitted network Binding) because
	// the reaper is a live consumer — a justified model growth, not a
	// speculative reservation (§1.3). A create that omits them gets the
	// engine-wide default resolved at create time, so the stored value is always
	// the concrete duration the reaper reads (default site: lifecycle.Deps).
	IdleTimeout time.Duration
	Retention   time.Duration

	// Limits is the resource-limit Binding (§3.4), materialized into this record
	// at create and RE-APPLIED at every placement from the record — that is what
	// makes it a Binding rather than placement state: a resume re-derives the caps
	// from here instead of reading back a frozen value (§3.4).
	Limits Limits

	// DeathCause records WHY this sandbox's isolation unit was last found dead
	// (§4.3). Written in the SAME commit as the crash-paused CAS that demoted it
	// (§4.4-C; store.CompareAndSetState states why no later window exists). It is
	// what separates the two otherwise-identical §4.3 dispositions:
	// dead-recoverable (resume it) vs resource-terminated (do NOT). Cleared on a
	// successful resume — a sandbox that came back up is no longer explained by
	// its previous death.
	DeathCause DeathCause

	CreatedAt time.Time
	UpdatedAt time.Time
}
