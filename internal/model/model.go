// Package model holds the shared engine model (the sandbox object model): sandbox identity,
// lifecycle state, and the three-state rootfs model. It is pure types with no
// dependency on any subsystem — every subsystem depends on model, model depends
// on none of them (the subsystem boundaries: subsystems are peers over the shared model, never each
// other).
package model

import (
	"net/netip"
	"time"
)

// SandboxID is the stable, opaque identity of a persistent sandbox (the ID-reference model).
// It does NOT encode placement: after resume the IP / container / cgroup / node
// may all change, but the SandboxID is unchanged — same Overlay artifact, same
// sandbox (the ID-reference model's iron rule).
type SandboxID string

// TemplateID is the opaque identity of an immutable read-only base (the Template and Overlay model).
// Immutability is enforced by the read-only lower + refcount protection; the
// identity is opaque, not a content hash.
type TemplateID string

// OverlayID identifies the sandbox's writable delta layer as a first-class,
// ID-addressed, Template-decoupled artifact (the ID-addressed Overlay model). The host path where it
// materializes is a rootfs-provider implementation detail and never leaks above
// the Rootfs subsystem (the ID-addressed Overlay model: ID-addressed internally too, not just at the
// external API).
type OverlayID string

// Template is a first-class persistent object: an immutable, read-only base a
// sandbox cold-starts from (the Template and Overlay model / health and reclamation). It is the sharpest application of the
// uniform object model — lineage — so it is the first object to materialize the
// {derived refcount} pattern (health and reclamation):
//
//   - No State field. Template uses "existence IS the state" (health and reclamation): the row
//     existing = ready, deleting the row = destroyed, no intermediate state.
//     This is what FORCES bytes-first materialization (the Rootfs subsystem): with no not-ready
//     escape hatch, the layer bytes MUST be fully on the backend before the row
//     is inserted, or a crash leaves a row pointing at nonexistent bytes.
//   - No refcount field. "Is it deletable" is DERIVED (health and reclamation): it has no
//     referencing live rows (no sandbox references it, no child Template names it
//     as Parent). The store's FK RESTRICT (SQLite) / Go check (Memory) enforces
//     "referenced → cannot delete" as a contract promise, not a materialized
//     count.
type Template struct {
	// ID is the opaque identity (the Template and Overlay model): "tmpl_<token>" for Promote-built,
	// the static directory name for builtin.
	ID TemplateID

	// Parent expresses lineage (health and reclamation / the Rootfs subsystem multi-lowerdir): the Template this
	// one layers on top of. Empty = a base layer (no parent). A referencing
	// child protects its parent from deletion (FK RESTRICT, health and reclamation: references
	// point upward).
	Parent TemplateID

	// Builtin marks a platform-preseeded Template (boot-seed reconciled from a
	// static TemplatesDir directory, health and reclamation existence-as-state / atomic transition rollback idempotent
	// reconcile) vs one produced by Promote. Both are
	// equally protected by RESTRICT — a referenced builtin is just as
	// undeletable as a referenced Promoted layer (the external API DELETE).
	Builtin bool

	CreatedAt time.Time
}

// VolumeID is the opaque identity of a Volume (the Volume subsystem): decoupled, ID-addressed
// storage whose lifecycle is independent of any sandbox — created before one,
// surviving after one, existing while nothing has it mounted.
//
// Like every other ID in the engine it is opaque and does NOT encode placement or
// a host path: the backend resolves the storage internally and only an unreadable
// source handle surfaces from it (the ID-addressed Overlay model addressed by ID internally too / the API error contract no
// host paths on the wire).
type VolumeID string

// Volume is a first-class persistent object (health and reclamation), and its field list is
// SHORTER than it looks like it should be — each absence is a decision:
//
//   - No State field, no refcount field: existence-is-state and a derived refcount, the same
//     shape as Template above and forcing bytes-first materialization for the same reason
//     (health and reclamation / the Volume subsystem). "Is it deletable" = no mount row references it, no child volume names
//     it as parent.
//   - NO AccessMode, on cardinality grounds — see AccessMode below. It belongs to the MOUNT
//     ROW (the Volume subsystem).
//   - No lease/owner field. An exclusive lease is not an object — it IS the existence of an
//     `exclusive` mount row.
//
// The cap does NOT live in model.Limits: those three axes are per-sandbox Binding materialized
// at each placement, while a volume has no placement of its own (resource admission / the Volume subsystem). It still counts
// in the FIRST term of the resource admission node inequality alongside sandbox caps — both are "upper bounds
// the engine has declared" — so no third term is added.
type Volume struct {
	// ID is the opaque identity ("vol_<token>").
	ID VolumeID

	// Backend names which registered volume.Backend materializes this volume
	// (the Volume subsystem two arms). Recorded rather than inferred: the mediator must be able to
	// route a later operation to the SAME backend that allocated the bytes.
	Backend string

	// Parent expresses LINEAGE: the volume this one was snapshotted or cloned from.
	// Empty = not derived from anything. `REFERENCES volumes(id) ON DELETE RESTRICT`,
	// exactly like templates.parent (health and reclamation lineage, references point upward).
	//
	// The foreign key is a correctness constraint, not tidiness. A quota
	// row leaks precisely when a unit that OWNS extents is deleted while a live sharer
	// still references them — and measured, "delete the sharers first, then the source"
	// leaks NOTHING. RESTRICT makes that order THE ONLY REACHABLE ONE, so the leak is
	// PREVENTED rather than reclaimed.
	//
	// An external implementation is the counter-example that pins this: it chose a
	// POINTER WITH NO CONSTRAINT (ADD COLUMN + index, no REFERENCES), so it had to
	// hand-write children checks in three places — and all three were RECLAMATION
	// guards, none a DELETE guard. The control-plane row therefore disappeared while the
	// bytes had to stay, and since every reclamation predicate was keyed on the source's
	// own record, deleting that record made the bytes permanently unfindable.
	Parent VolumeID

	// CapByte is the per-volume capacity cap in bytes, 0 = uncapped (the Volume subsystem / resource admission).
	//
	// THIS RECORD IS THE AUTHORITY, always. The backend's read-back cannot serve:
	// the local substrate maps "no cap" and 0 onto each other in both directions, so
	// "never capped" and "deliberately uncapped" are byte-for-byte identical when
	// read from storage (volume cap enforcement). So no after-the-fact reconciliation against
	// the backend is implementable, and the cap must be written in the same backend
	// operation that creates the bytes.
	CapByte int64

	CreatedAt time.Time
}

// AccessMode is how one MOUNT views a volume (the Volume subsystem). Three modes, and all three are
// materialized with NO special mechanism — the same host directory reaches one or N
// guests (measured; Volume materialization).
//
// IT IS A PROPERTY OF THE MOUNT ROW, NOT OF THE VOLUME, on cardinality grounds: one
// volume can be shared-rw to sandbox A and shared-ro to sandbox C AT THE SAME TIME
// (measured), so a per-volume field could not carry it honestly.
//
// The two halves are enforced in DIFFERENT places, and the split is load-bearing:
// READ-ONLY is enforced AT RUNTIME (a read-only bind ⇒ the guest's write gets EROFS,
// a clean POSIX refusal rather than a silent success), while EXCLUSIVITY is enforced
// in the CONTROL PLANE (the mount row itself). Both are decided before the backend is
// called, which is why the backend never learns the mode.
type AccessMode string

const (
	// Exclusive means no other mount row may coexist for this volume. The lease IS
	// this row's existence — there is no Lease object and no owner field (the Volume subsystem /
	// the mount table.
	Exclusive AccessMode = "exclusive"
	// SharedReadOnly is a read-only fanout mount. The guest's writes get EROFS.
	SharedReadOnly AccessMode = "shared-ro"
	// SharedReadWrite is a shared writable mount. It guarantees POSIX semantics only
	// for multiple connections WITHIN one sandbox; CROSS-SANDBOX rw coherence is
	// explicitly UNDEFINED (the Volume subsystem) — gofer and sentry each cache, so shared mutable
	// consistency across an isolation boundary is unreliable in principle.
	SharedReadWrite AccessMode = "shared-rw"
)

// ReadOnly reports whether this mode materializes as a read-only bind.
func (a AccessMode) ReadOnly() bool { return a == SharedReadOnly }

// Valid reports whether a is one of the three defined modes. The schema asserts the
// same set, and that assertion is REQUIRED rather than belt-and-braces: the exclusive
// lease's partial index has a LITERAL STRING predicate, so a mis-cased value would
// make the index simply not apply and let two exclusive rows both insert — a silent
// failure of the one new invariant Volume introduces (the Volume subsystem).
func (a AccessMode) Valid() bool {
	switch a {
	case Exclusive, SharedReadOnly, SharedReadWrite:
		return true
	}
	return false
}

// Mount is one row of the mount table: a FIRST-CLASS PERSISTENT OBJECT (health and reclamation), and
// structurally just a doubling of the `sandboxes.template` reference — two columns,
// each with ON DELETE RESTRICT, which is what gives both "a volume with a live mount
// row cannot be deleted" and "a sandbox with a live mount row cannot be deleted".
//
// The mount relationship is a BINDING (resource admission): what is mounted, where, under which
// mode is recorded here, and a resume RE-MOUNTS from the CURRENT table rather than
// restoring from frozen bytes — a volume is always the one real thing, never forked
// into a per-sandbox copy.
//
// Like Template and Volume: existence-is-state, and "is this volume exclusively held" is
// DERIVED from the rows' presence and access (health and reclamation). It is NOT a lifecycle state machine lifecycle
// transition, so creating or deleting a mount row publishes NO event.
type Mount struct {
	// Sandbox is the sandbox this mount belongs to. REFERENCES sandboxes(id) ON
	// DELETE RESTRICT.
	Sandbox SandboxID
	// Volume is the volume mounted. REFERENCES volumes(id) ON DELETE RESTRICT.
	Volume VolumeID
	// Path is the guest destination, and it MUST be normalized before insert —
	// absolute, cleaned, not "/" (the Volume subsystem). `/data`, `/data/`, and `/data/../data` are one
	// mount point spelled three ways, and the UNIQUE(sandbox, path) assertion is a
	// literal string comparison, so an un-normalized path makes the assertion SILENTLY
	// not apply. Same trap as the CHECK beside the lease index.
	Path string
	// Access is how this mount views the volume. Per-ROW, never per-volume.
	Access AccessMode
}

// NetProto is the transport protocol a network declaration tuple names — the RECORD form.
//
// It is ALWAYS explicit, and that is a FORMAT requirement rather than an optional
// dimension: in the kernel mechanism that carries it, port matching itself depends
// on protocol matching, so `{cidr, port}` is not a well-formed tuple (the network declaration /
// network tuple validation).
//
// A separate type from the enforcement layer's protocol enum, exactly as DeathCause
// is separate from its provider-seam twin: the record shape and rule-rendering
// vocabulary must be able to change independently, and `model` depends on no
// subsystem.
type NetProto string

const (
	NetTCP NetProto = "tcp"
	NetUDP NetProto = "udp"
)

// NetTuple is `{cidr, port, proto}` (the network declaration): ONE tuple, not three independent
// options.
type NetTuple struct {
	// CIDR is the destination prefix, held PARSED and MASKED so the record carries a
	// canonical value rather than whatever text arrived.
	//
	// An INVALID (zero) prefix means ANY destination — meaningful only inside
	// `via[].match[]`, where it is the "transparent interception" value of this same
	// field and the generator emits exactly one clause fewer for it (the network declaration /
	// via intent forms).
	//
	// Only IP LITERALS ever become one: no domain names, refused on COST grounds
	// rather than feasibility (the IP-only boundary).
	//
	// NOTE ON THE JSON TAGS ON THIS TYPE: they serialize the RECORD, i.e. what the
	// store persists. They are NOT the wire schema. The API layer carries its own
	// wire type with `cidr` as a STRING, because netip.Prefix's own text
	// unmarshaling ACCEPTS a v4-mapped literal (verified) — which would route
	// straight past ClassV4Mapped, whose whole point is to refuse those
	// BEFORE any containment test (network substrate measurements). A record type that could be
	// decoded from untrusted input would make that class unenforceable.
	CIDR netip.Prefix `json:"cidr"`
	// Port is the destination port, and ZERO MEANS ANY PORT (the network declaration) — the second of this
	// tuple's two wildcards, alongside an invalid CIDR above.
	//
	// The record spelling is the ZERO VALUE while the WIRE spelling is an OMITTED field
	// (`omitempty` on both faces, see the orchestrator's tupleWire), and the mapping is
	// deliberately many-to-one in that direction only: an omitted `port` and an explicit
	// `"port": 0` both record as 0, and the read face renders 0 back as the omission. So
	// the wildcard has ONE recorded value and ONE emitted spelling, which is what keeps a
	// read-modify-write cycle from producing a body that differs from the one it read
	// (the control-plane API). A record holding 0 is therefore a CHOSEN value, not an absent one — the
	// distinction the network declaration requires `egress.default` to preserve.
	Port uint16 `json:"port,omitempty"`
	// Proto is mandatory (see NetProto).
	Proto NetProto `json:"proto"`
}

// AnyDest reports whether this tuple pins no destination address. The generator
// emits the destination clause IF AND ONLY IF this is false, and BOTH directions
// are load-bearing: keeping a cidr clause on a transparent-interception declaration
// blocks the traffic that declaration authorized, while dropping it on an
// explicit-endpoint declaration authorizes every OTHER address on that port
// (measured both ways, network substrate measurements).
func (t NetTuple) AnyDest() bool { return !t.CIDR.IsValid() || t.CIDR.Bits() == 0 }

// AnyPort reports whether this tuple pins no destination port, i.e. matches every
// port (the network declaration). It is AnyDest's twin, and a predicate with a NAME states the semantic
// where a bare `Port == 0` only implies it.
//
// Note the asymmetry with AnyDest: the generator must branch on AnyDest (an invalid
// prefix cannot be handed to a rule's address set at all), while the port wildcard needs
// no generator branch — 0 already IS the wildcard encoding in ruleset.Match, and the
// RENDERER emits `dport` iff it is non-zero. So no production caller needs this; its
// readers are the tests asserting the semantic, which keeps `Port == 0` from being
// re-derived by hand at a site that forgets what it means.
func (t NetTuple) AnyPort() bool { return t.Port == 0 }

// NetVia is one `egress.via[]` entry: flows matching any of Match go to Endpoint.
//
// WHAT SITS BEHIND THAT ENDPOINT, THE ENGINE DOES NOT KNOW AND DOES NOT ASK (the network declaration).
// It is an address; the engine installs a rule sending matched traffic there. So it
// is a declaration VALUE — never an engine constant, never an engine config key.
type NetVia struct {
	Match []NetTuple `json:"match"`
	// Endpoint is a HOST-LOCAL `ip:port`, and host-local is a CONTRACT requirement
	// rather than a convenience: the second hop — that process's own egress on the
	// sandbox's behalf — must be pinned by the KERNEL rather than by its own
	// discipline, and the kernel's reach stops at this host. Accepting a remote
	// endpoint would rest the L0 floor on that process's self-restraint, which is
	// precisely the confused deputy the second-hop floor rejects (second-hop pinning).
	Endpoint string `json:"endpoint"`
}

// NetPublish is one `ingress.publish[]` entry: a guest port exposed as a host
// `ip:port`. The explicit inbound exception, and the ONE hot-addable item — a pure
// host-side rule set, purely additive, touching no isolation unit (the network declaration).
type NetPublish struct {
	GuestPort uint16   `json:"guest_port"`
	Proto     NetProto `json:"proto"`
}

// NetEgressDefault is `egress.default` (the network declaration). Two values, and there is NO POSTURE
// ENUM: a posture is a COMBINATION of the four fields' values (`deny` + no allow +
// no via = "no egress"; `deny` + via = "only through this rendezvous"), and each
// enum would be one more coupling point between the engine and product semantics
// (the engine vocabulary boundary / the network declaration).
type NetEgressDefault string

const (
	// NetDeny is the fail-closed side, and the value an omitted `network` normalizes
	// to (it is also exactly the minimal declaration — "no network").
	NetDeny NetEgressDefault = "deny"
	// NetAllow is "the public internet, MINUS the L0 floor and the internal deny-safe
	// tier" (the provider security contract) — emphatically not "everything": without that tier this value
	// hands the host's whole LAN to the sandbox (measured, network substrate measurements), i.e. it
	// would deliver an `allow` strictly wider than the provider security contract defines.
	NetAllow NetEgressDefault = "allow"
)

// NetEgress is the outbound half of the declaration: the three fields that change
// only at a resume boundary (the network declaration).
type NetEgress struct {
	// Default is MANDATORY whenever a `network` object is given, and not out of
	// strictness: if "unspecified" were read as some default value, a null in the
	// record could no longer be told apart from "the caller chose that value" (the network declaration,
	// the same shape as idle_timeout / retention).
	Default NetEgressDefault `json:"default"`
	Allow   []NetTuple       `json:"allow"`
	Via     []NetVia         `json:"via"`
}

// NetIngress is the inbound half: closed by default, opened item by item.
type NetIngress struct {
	Publish []NetPublish `json:"publish"`
}

// NetworkDecl is the network declaration's four-field reachability declaration, in its RECORD form —
// a Binding (resource admission) on the sandbox's own control-plane record.
//
// Every field uses IP / port / protocol vocabulary ONLY. The engine's enforcement
// evaluates nothing else: no domain names, no TLS, no application-layer protocol
// (the provider security contract / the IP-only boundary). The subsystem that owns the semantics is the network declaration
// (internal/network) — the TYPE lives here because the store persists it and
// `model` depends on no subsystem, exactly as with Mount and Limits.
type NetworkDecl struct {
	Egress  NetEgress  `json:"egress"`
	Ingress NetIngress `json:"ingress"`
}

// Normalize fills in the empty-slice spelling. the control-plane API requires the three arrays to
// ALWAYS appear as `[]` — never omitted, never null: they are DEFINITE values in the
// record, while the omit-when-unreadable rule (`death_cause`, `used_byte`) governs
// facts that cannot be READ, of which a declaration has none.
//
// Value receiver and copy-on-write: a read-modify-write cycle must not write
// through to the value it just read.
func (d NetworkDecl) Normalize() NetworkDecl {
	if d.Egress.Allow == nil {
		d.Egress.Allow = []NetTuple{}
	}
	if d.Egress.Via == nil {
		d.Egress.Via = []NetVia{}
	}
	if d.Ingress.Publish == nil {
		d.Ingress.Publish = []NetPublish{}
	}
	return d
}

// Clone returns a declaration that shares no backing array with the receiver.
//
// It exists because Normalize is NOT a copy: it replaces nil slices, and passes a
// non-nil slice through by identity. A store backend that hands out the record it
// holds therefore hands out the live arrays, and a caller mutating a returned
// declaration in place corrupts the store — on THAT BACKEND ONLY. SQLite cannot
// alias, because it re-decodes JSON on every read, so the same mediator bug is
// invisible against one backend and destructive against the other. the extension boundary wants one set
// of assertions over two backends, which requires them to agree on the read shape.
func (d NetworkDecl) Clone() NetworkDecl {
	out := d
	out.Egress.Allow = cloneSlice(d.Egress.Allow)
	out.Ingress.Publish = cloneSlice(d.Ingress.Publish)
	out.Egress.Via = cloneSlice(d.Egress.Via)
	// Each Via carries its own slice, so copying the outer one still leaves every
	// match list shared.
	for i := range out.Egress.Via {
		out.Egress.Via[i].Match = cloneSlice(d.Egress.Via[i].Match)
	}
	return out
}

// cloneSlice copies a slice while PRESERVING the nil-vs-empty distinction, which
// `append([]T(nil), s...)` does not: it returns nil for an empty input, and an empty
// declaration array that read back as nil would put a `null` on the wire where the control-plane API
// requires `[]` (the property that keeps the read face's output a legal write body).
func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

// Lifecycle is the lifecycle state machine. Exactly four states — no `failed`
// intermediate state (the transition contract: the absence is a conclusion of the model, not an
// omission), no independent revision counter (the ID-reference model uses Placement / health and reclamation uses
// placement-IP-as-epoch instead).
type Lifecycle string

const (
	Creating  Lifecycle = "creating"
	Running   Lifecycle = "running"
	Paused    Lifecycle = "paused"
	Destroyed Lifecycle = "destroyed"
)

// EventKind is the lifecycle state machine transition-event kind. EXACTLY SIX, and the set is closed:
// every the lifecycle state machine transition emits one of these and nothing else emits at all.
//
// Typed rather than left as a string because the lifecycle state machine states the closure and nothing
// enforced it: the six spellings lived as literals in two packages, so a typo'd
// `crash_paused` would compile, persist, and reach subscribers as a seventh kind. The
// store schema asserts the same set, for the reason `mounts.access` does — a value the
// schema cannot write is a value no reader has to handle.
//
// Note there is no `evict` kind: eviction reaches destroyLocked and emits Destroyed.
// The three PAUSED kinds differ in WHY, which is what a subscriber needs to tell an
// operator's pause from the reaper's idle sweep from a crash (health and reclamation). `destroyed`'s two
// routes are separated the OTHER way, by EventCause below.
type EventKind string

const (
	EventCreated     EventKind = "created"
	EventPaused      EventKind = "paused"
	EventAutoPaused  EventKind = "auto-paused"
	EventCrashPaused EventKind = "crash-paused"
	EventResumed     EventKind = "resumed"
	EventDestroyed   EventKind = "destroyed"
)

// EventKinds is the closed set, for the schema check and for any consumer that must
// enumerate rather than hard-code.
var EventKinds = []EventKind{
	EventCreated, EventPaused, EventAutoPaused, EventCrashPaused, EventResumed, EventDestroyed,
}

// EventCause is WHY a transition of a given EventKind happened, for the kinds a
// subscriber cannot otherwise tell apart (the transactional outbox). Empty = not applicable, and most
// events carry nothing here: the field is populated only where one kind has more than
// one route to it, so a subscriber that ignores it reads the stream exactly as before.
//
// A FIELD rather than more kinds: EventKind states a closed set of six, and a seventh spelling
// (`evicted`) would make every existing subscriber's exhaustive match wrong. `crash-paused`
// already uses this shape, its two routes separated by whether a cause of death was recorded.
//
// NOT DeathCause, and the two must never be merged: DeathCause is why an isolation UNIT was
// found gone, this is why a RECORD reached a terminal. A shared enum would hold members
// meaningless to each side.
//
// For `destroyed` this field is the ONLY carrier the fact ever has — destroy deletes the record,
// so a GET after that event is 404 by contract, at any later time. Every other kind leaves a
// readable record, making their causes a shortcut; this one's is irreplaceable.
type EventCause string

const (
	// CauseNotApplicable is the zero value: this kind has one route, so there is
	// nothing to disambiguate.
	CauseNotApplicable EventCause = ""
	// CauseRequested = an operator asked for this transition (the Destroy verb).
	// Someone meant it.
	CauseRequested EventCause = "requested"
	// CauseRetentionExpired = the reaper evicted a sandbox left `paused` past its
	// `retention` (health and reclamation). A TTL fired while nobody was looking, which is a different
	// fact for a subscriber than a deliberate delete — and the reason it must ride the
	// event is that the record is gone by the time anyone could ask.
	CauseRetentionExpired EventCause = "retention-expired"
)

// EventCauses is the closed set, for the schema check and for any consumer that must
// enumerate rather than hard-code. CauseNotApplicable is excluded: it is the ABSENCE of
// a cause, expressed as NULL in the store rather than as a member.
var EventCauses = []EventCause{CauseRequested, CauseRetentionExpired}

// Valid reports whether c is a real cause. The empty value is NOT valid here — it means
// "no cause", which callers express by passing nothing at all.
func (c EventCause) Valid() bool {
	for _, v := range EventCauses {
		if c == v {
			return true
		}
	}
	return false
}

// Valid reports whether k is one of the six.
func (k EventKind) Valid() bool {
	for _, v := range EventKinds {
		if k == v {
			return true
		}
	}
	return false
}

// Limits is the per-sandbox resource-limit Binding (resource admission). SoT is the
// control-plane record: the values are stable across resume and RE-MATERIALIZED
// at each placement from the record, never read back from a frozen placement.
//
// Three dimensions, three mechanisms, three DIFFERENT overrun consequences —
// they are deliberately NOT interchangeable knobs (resource admission):
//
//   - CPUMillis  → the runtime provider contract cgroup `cpu` controller. Overrun = the guest runs
//     slower, no error. A resource limit (fair sharing).
//   - DiskByte   → the Rootfs subsystem FS-layer quota on the Overlay upper+work (NOT cgroup —
//     cgroup cannot cap at-rest bytes; resource admission: capacity and I/O rate are two
//     orthogonal axes).
//     Overrun = the guest's write() gets ENOSPC and THE SANDBOX LIVES ON: a
//     data-plane event the control plane does not observe.
//   - MemoryByte → the runtime provider contract host memcg. Overrun TERMINATES the sandbox, so this one
//     is a SAFETY limit bounding blast radius (the data-plane exposure boundary), not a fair-sharing limit:
//     the substrate offers no "guest process gets ENOMEM while the sandbox
//     survives" path, so it is a lifecycle event the control plane MUST see
//     (health and reclamation resource-terminated).
//
// Zero on any dimension = unlimited (that dimension is not materialized).
//
// The disk-IO rate cap has NO field here: a settled deferral, the mechanism being proven and
// the CONSUMER missing (the extension boundary). It joins additively as a RATE alone — the whole-disk MAJ:MIN
// `io.max` needs is DERIVED BY THE PROVIDER, a device number being irreducibly host-local, so
// carrying one here would lift a host primitive into a cross-provider contract (the runtime provider contract).
type Limits struct {
	CPUMillis  int64 // 1000 = one full CPU
	MemoryByte int64
	DiskByte   int64
}

// DeathCause is why a sandbox's isolation unit is gone (health and reclamation). It is an ABSTRACT
// ENUM, never an errno or a cgroup file: local_runsc concludes it from its own
// memcg, a remote provider from its scheduler's termination reason, and the lifecycle
// mediator only ever sees these three values (the provider security contract / the runtime provider contract Adopt contract). That
// is what makes resource-terminated work across providers instead of being a
// host-local hack.
//
// It is persisted on the sandbox record because the signal is VOLATILE — it
// lives in placement-local state and vanishes when the placement is torn down,
// so "ask again later" does not exist. It is written in the SAME commit as the
// crash-paused demotion CAS (health and reclamation / the transactional outbox).
type DeathCause string

const (
	// CauseNone is the resting value for a sandbox that has not been observed
	// dead. Not a death cause — the absence of one.
	CauseNone DeathCause = ""
	// CauseUnknown = the unit is gone but WHY could not be determined. A
	// first-class legal value, NOT an error (health and reclamation): the disposition is to treat it
	// as dead-recoverable, which a caller may then resume. NO resume-attempt bound,
	// and none is needed — self-correction is STRUCTURAL: if it really was the RAM
	// cap the same workload re-triggers termination after that resume, this time in
	// the control plane's plain view, and THAT observation records the real cause,
	// after which the persisted fact refuses further resumes. The engine never
	// resumes a dead-recoverable sandbox itself (the lifecycle state machine lists no such automatic
	// transition), so the retry is a caller's choice, not an engine loop. Providers
	// MUST report this rather than guess (the provider security contract: no guessing).
	CauseUnknown DeathCause = "unknown"
	// CauseGone = the unit died for a reason that is NOT a resource cap (sentry
	// crash, external kill, clean guest exit). Disposition: dead-recoverable →
	// resume (health and reclamation).
	CauseGone DeathCause = "gone"
	// CauseResourceExceeded = the unit was terminated for exceeding a resource
	// cap (today: the RAM safety limit, whole-group). Disposition:
	// resource-terminated → do NOT resume (health and reclamation) — resuming re-runs the same
	// workload into the same cap, which is a crash loop.
	CauseResourceExceeded DeathCause = "resource-exceeded"
)

// Sandbox is the persistent unit (the sandbox process model): the durable record the control plane
// owns. Placement is deliberately NOT a field here — it is rebound every resume
// and lives with the runtime provider, not in the identity record (the ID-reference model).
//
// Minimal shape by one rule: a field exists here only when this record actually
// carries it. The mount topology is consumed but lives in its own table (the Volume subsystem)
// keyed by sandbox, not as a field here — a sandbox's volume set is many-to-many
// and mutable independently of the identity record. The network declaration
// (the network declaration) IS carried, as a Binding — see the Network field below.
type Sandbox struct {
	ID       SandboxID
	Template TemplateID
	Overlay  OverlayID // the writable delta this sandbox owns (the ID-addressed Overlay model)
	State    Lifecycle

	// No guest-identity field: the sandbox runs as guest root by default and the
	// engine does not model guest users (the guest identity model — guest identity is not a
	// host-isolation pillar). "Run as user X" is a guest-native concern (su/sudo),
	// not engine state (the product-neutral sandbox boundary).

	// IdleTimeout and Retention are the lifecycle TTL — per-sandbox Binding
	// fields (resource admission: SoT is the control-plane record, stable across resume,
	// materialized each placement), consumed by the reaper (health and reclamation):
	//   - IdleTimeout: a `running` sandbox idle (no I/O) longer than this →
	//     auto-pause (the lifecycle state machine automatic transition).
	//   - Retention: a `paused` sandbox older than this → evict (reaper's
	//     destroy of a paused sandbox, the lifecycle state machine).
	// These EXIST here because the reaper is a live consumer — a justified model
	// growth, not a speculative reservation (the extension boundary). A create that omits them gets the
	// engine-wide default resolved at create time, so the stored value is always
	// the concrete duration the reaper reads (default site: lifecycle.Deps).
	IdleTimeout time.Duration
	Retention   time.Duration

	// Limits is the resource-limit Binding (resource admission), materialized into this record
	// at create and RE-APPLIED at every placement from the record — that is what
	// makes it a Binding rather than placement state: a resume re-derives the caps
	// from here instead of reading back a frozen value (resource admission).
	Limits Limits

	// Network is the network declaration reachability declaration — a Binding, exactly like Limits
	// and for the same reason: every placement regenerates its rule set from THIS
	// record rather than reading back anything frozen, so an edit made while paused
	// takes effect at the next resume and a crash-surviving rule set can be rebuilt
	// from here (resource admission / atomic transition rollback).
	//
	// An OPAQUE JSON DOCUMENT rather than normalized columns, because that is what the field
	// IS: a whole-value Binding with two write points that each replace or append to the whole
	// document (the control-plane API PATCH swaps whole binding for whole binding, `POST …/publish` appends one
	// item). Nothing queries INTO it, so columns would buy no query and cost a table per
	// repeated field. Contrast the mount table, normalized because the exclusive lease is a
	// CROSS-ROW predicate the store must arbitrate (the Volume subsystem).
	//
	// THE ZERO VALUE IS NOT A VALID DECLARATION. An omitted `network` normalizes to
	// an explicit `{deny, [], [], []}` AT THE WRITE POINT (the network declaration), so every record
	// holds a chosen value and "a null cannot be told from a choice" is structurally
	// impossible. A record read back with an empty Default means the row predates
	// that write point, which is a bug rather than a state to interpret.
	Network NetworkDecl

	// DeathCause records WHY this sandbox's isolation unit was last found dead
	// (health and reclamation). Written in the SAME commit as the crash-paused CAS that demoted it
	// (the transactional outbox; store.CompareAndSetState states why no later window exists). It is
	// what separates the two otherwise-identical health and reclamation dispositions:
	// dead-recoverable (resume it) vs resource-terminated (do NOT). Cleared on a
	// successful resume — a sandbox that came back up is no longer explained by
	// its previous death.
	DeathCause DeathCause

	CreatedAt time.Time
	UpdatedAt time.Time
}
