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
// materializes is an FS-provider implementation detail and never leaks above
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
// consumer appears (one field here + one OCI section, no shape change). What is
// still open is only where the whole-disk MAJ:MIN comes from (open-decisions §3).
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
// carries it today. Binding fields not yet consumed (mounted volumes, network
// policy) are omitted rather than stubbed — this record is a data shape, not a
// settled interface signature, so an unused field would be a speculative
// reservation (§1.3). (Contrast the runtime provider's SteeringSpec, which DOES
// exist now: it is a named member of the settled §5.A contract 3-tuple —
// "settled, not yet built".)
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
	// These EXIST here (unlike the still-omitted volume/network Binding) because
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
