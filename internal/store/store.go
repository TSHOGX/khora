// Package store is the control-plane record seam (§1.1 / §4.4-C). It is a
// Behavior seam (§1.3): an interface with pluggable implementations. Two live
// side by side behind it — an in-memory map (for fast unit tests) and the
// embedded single-node backend (SQLite, ADR-0004) — chosen by registration,
// not by reshaping callers; that is the whole point of a seam.
//
// The interface itself promises the minimal guarantees the algorithms need
// (§4.4-C): "lifecycle-state change + outbox append = single atomic commit".
// The CAS shape below gives the §4.4-B transition lock somewhere to land.
//
// NO-CAPABILITY-BRANCH RULE (§4.4-C, and it governs every method here): NEVER
// design the API so a backend reports whether it supports transactions and the
// algorithm branches on it. Each backend must DELIVER the promised atomicity by
// whatever means it has; a caller never learns which backend it is talking to,
// because that is how backend differences would leak into correctness.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"engine/internal/model"
)

// ErrConflict is returned by CompareAndSetState when the current state is not
// the expected source state — the CAS lost (§4.4-B: pause vs resume racing, the
// later verb sees a state it didn't expect and is rejected).
var ErrConflict = errors.New("store: state precondition failed")

// ErrNotFound is returned when a sandbox record does not exist.
var ErrNotFound = errors.New("store: sandbox not found")

// ErrTemplateNotFound is returned when a Template record does not exist —
// including the reference-side failure: inserting a sandbox (or child Template)
// that references a Template no concurrent transaction left in place (§4.3
// reference-reclaim atomicity: DELETE committed first → the referrer hits
// missing-parent).
var ErrTemplateNotFound = errors.New("store: template not found")

// ErrInUse is the §4.3 reference-protection verdict: a DeleteTemplate lost to a
// live referencing row (a sandbox pinning it, or a child Template naming it as
// parent). It is the store SEAM's contract promise (§4.3 / §1.3), NOT a SQLite
// feature leaking out: SQLite delivers it via FK RESTRICT, Memory via a Go scan
// — the two backends run one set of assertions. The API layer maps it to a clean
// 409 template in use (§6.1), never a raw 500.
var ErrInUse = errors.New("store: template in use")

// ErrVolumeNotFound is returned when a Volume record does not exist (§5.C). It is
// its OWN sentinel rather than a reuse of ErrNotFound ("sandbox not found") for the
// reason §6.2 states about the Volume verdicts generally: a 404 whose text names
// the wrong object sends the caller looking in the wrong place. The
// reference-protection verdicts are separate sentinels for the same reason — see
// below.
var ErrVolumeNotFound = errors.New("store: volume not found")

// The Volume reference verdicts (§6.2's four 409s). They are FOUR SEPARATE
// SENTINELS, not one, and that is a requirement rather than tidiness: each names a
// DIFFERENT object relationship, and ErrInUse's text says "template in use"
// verbatim — reusing it returns a 409 that names the wrong object, sending the
// caller to look at something unrelated.
//
// A storetest case asserts they stay mutually distinguishable, because the way this
// degrades is by someone later collapsing two of them that happen to map to the same
// status code.
var (
	// ErrVolumeInUse is verdict ①: deleting a volume that a live mount row holds
	// (mounts.volume RESTRICT). The Template-shaped case, on a different object.
	ErrVolumeInUse = errors.New("store: volume in use by a mount")

	// ErrSandboxHasMounts is verdict ②'s sandbox side: deleting a sandbox that a live
	// mount row holds (mounts.sandbox RESTRICT).
	//
	// A sentinel is required even though the FK-violation PREDICATE is generic
	// (isFKViolation is table- and column-agnostic): without a translation point in
	// DeleteSandbox the raw driver error reaches the API's default arm as a 500 rather
	// than a 409. The predicate is reusable; the translation is per-call-site.
	ErrSandboxHasMounts = errors.New("store: sandbox has live mounts")

	// ErrMountLeaseConflict is verdict ③: an attach lost the exclusive lease. It needs
	// its OWN predicate as well as its own sentinel — the FK-violation string match
	// correctly reports FALSE for a uniqueness or predicate failure, so one match
	// cannot cover all four verdicts.
	//
	// Unlike ①②④ this is a RETRYABLE state conflict: the caller may succeed later,
	// once the holder releases.
	ErrMountLeaseConflict = errors.New("store: volume already exclusively mounted")

	// ErrMountBindingInvalid is the §6.2 400 (NOT a 409): a single submitted binding
	// that CONTRADICTS ITSELF — the same destination twice, the same volume twice, or
	// one volume nested inside another's subtree.
	//
	// 400 because it is decidable WITHOUT reading any other row, and no retry or state
	// change can ever make it legal — which is exactly §6.2's 400 criterion. A sandbox
	// holds one complete binding at any moment and PATCH swaps whole binding for whole
	// binding, so there is no "add one mount to an existing set" operation for a
	// duplicate to be relative to. §6.2's four 409s do not grow.
	ErrMountBindingInvalid = errors.New("store: mount binding is self-contradictory")

	// ErrVolumeHasDescendants is verdict ②'s volume side: deleting a volume that a live
	// snapshot or clone names as its parent (volumes.parent RESTRICT).
	//
	// It is a DIFFERENT object relationship from ErrVolumeInUse (held by a mount row),
	// so it needs its own sentinel and its own text — this is D3's trap recurring on the
	// fourth FK path, and §6.1 requires the two to be distinguishable because the
	// caller's remedy differs: detach a mount vs delete a descendant.
	ErrVolumeHasDescendants = errors.New("store: volume has live snapshots or clones")
)

// Event is one outbox row: a §4.1 lifecycle transition to publish to the event
// stream (created / paused / resumed / destroyed …). It is appended in the SAME
// atomic commit as the state change that produced it (§4.4-C) — a committed
// transition can never lack its event, nor an event lack its transition. The
// transition methods below take an *Event: the §4 mediator constructs it per
// transition semantics, carrying a transition-unique ID (§4.4-C). A nil Event
// means "this store mutation publishes nothing" (the transient creating-insert
// and the create-unwind delete are internal steps of a verb, not externally
// visible §4.1 transitions) — that is a mediator semantic choice, NOT a backend
// capability branch.
type Event struct {
	// Seq is the outbox's monotonic total order — the CURSOR a subscriber
	// resumes from (§6.1 GET /v1/events?after=). It is assigned BY THE BACKEND at
	// append; a caller-supplied value is ignored (the mediator does not own this
	// number, the store does). Zero on an Event being handed TO the store, and
	// always > 0 on one read back out.
	//
	// It exists because OccurredAt cannot serve: unix nanos have no tie-break at
	// equal nanos and the wall clock can be stepped backwards by NTP, so a
	// timestamp is not a total order and a timestamp cursor cannot be resumed
	// from unambiguously.
	Seq int64
	// ID is the transition-unique key subscribers dedup on (§4.4-C
	// at-least-once: a redelivered event carries the SAME ID). Backends key the
	// outbox row uniquely on it.
	ID string
	// Sandbox is which sandbox transitioned. Fine as an engine-internal field —
	// `sandbox` is a core atom, not a banned product concept (§1.1 vocabulary).
	Sandbox model.SandboxID
	// Kind is the transition name ("created" / "paused" / "resumed" /
	// "destroyed" / "auto-paused" / "crash-paused" …). The three pause-like Kinds
	// are distinct on purpose: "paused" = user pause, "auto-paused" = reaper idle
	// pause, "crash-paused" = INVOLUNTARY demote (§4.1 / §4.2) — which covers two
	// paths, not just death: a unit found dead (cause recorded alongside) and the
	// transport-down restart-budget escalation (reaper-health D4b), where L1 is
	// still ALIVE and so no cause is stamped. Whether a cause is present is what
	// separates them. All land state=paused, the Kind carries WHY. Free-form here;
	// the external event schema is a subscriber-side concern (§4.4-C: engine only
	// guarantees reliable write to a replayable stream).
	Kind string
	// State is the resulting lifecycle-state after the transition.
	State model.Lifecycle
	// OccurredAt is when the mediator constructed the transition.
	OccurredAt time.Time
}

// Binding is the mutable slice of a sandbox's §3.4 Binding — what §6.1 PATCH may
// rewrite. Each field is a POINTER so "not given" is distinguishable from "set to
// zero", which is load-bearing here: on these axes zero is itself a meaningful
// instruction (`0 = unlimited` for a cap, `retention: 0` = never evict), so a
// value-typed struct could not express "leave this alone" at all.
//
// Only fields the engine can actually re-materialize appear here. Template and
// Overlay are NOT Binding-mutable (changing either would make it a different
// sandbox, §3.3), and State is not here because this is not a transition — it is
// conditional ON the state (§4.4-B).
type Binding struct {
	CPUMillis   *int64
	MemoryByte  *int64
	DiskByte    *int64
	IdleTimeout *time.Duration
	Retention   *time.Duration
	// Network is the §5.D declaration, replaced WHOLE (§6.2: PATCH swaps one
	// complete binding for another; there is no field-level merge and no
	// "pending/unresolved" value).
	//
	// A POINTER for the same reason as the others — "not given" must be
	// distinguishable from "given" — but note what the distinction protects here.
	// The zero declaration is not a no-op, it is `{deny, [], [], []}`: the most
	// restrictive declaration there is. So a value type would make every unrelated
	// PATCH (raising a RAM cap, changing a TTL) silently reset the sandbox's network
	// to deny-everything.
	//
	// This is ALSO the field the `POST …/publish` hot-add writes through, carrying a
	// declaration the mediator produced by read-append under the transition lock. The
	// store does not know which of the two write points it is serving — both replace
	// the whole value — which is why the append has to happen above it (§5.D: two
	// concurrent publishes would otherwise lose one, and that is an engine bug rather
	// than the caller-side sharp edge §6.1 documents).
	Network *model.NetworkDecl
}

// IsEmpty reports whether the Binding asks for no change at all, which callers
// treat as a no-op rather than a write.
func (b Binding) IsEmpty() bool {
	return b.CPUMillis == nil && b.MemoryByte == nil && b.DiskByte == nil &&
		b.IdleTimeout == nil && b.Retention == nil && b.Network == nil
}

// Apply folds the requested changes onto a copy of the current values, returning
// what the record should hold. Kept here, next to the type, so both backends and
// the mediator agree on "omitted = unchanged" rather than each re-deriving it.
func (b Binding) Apply(cur model.Sandbox) (model.Limits, time.Duration, time.Duration, model.NetworkDecl) {
	lim, idle, ret, net := cur.Limits, cur.IdleTimeout, cur.Retention, cur.Network
	if b.CPUMillis != nil {
		lim.CPUMillis = *b.CPUMillis
	}
	if b.MemoryByte != nil {
		lim.MemoryByte = *b.MemoryByte
	}
	if b.DiskByte != nil {
		lim.DiskByte = *b.DiskByte
	}
	if b.IdleTimeout != nil {
		idle = *b.IdleTimeout
	}
	if b.Retention != nil {
		ret = *b.Retention
	}
	if b.Network != nil {
		// WHOLE replacement, and normalized on the way in so the record never holds a
		// nil array where §6.1 promises `[]`.
		net = b.Network.Normalize()
	}
	return lim, idle, ret, net
}

// marshalNetwork / unmarshalNetwork are the §5.D declaration's at-rest form.
//
// A FAILURE HERE IS RETURNED, NEVER SWALLOWED, and that is the point of having them
// as named helpers rather than inline calls. The declaration is a security-relevant
// field: reading it back as a zero value on a decode error would silently present a
// sandbox as `{deny, [], [], []}` — which looks safe, and is catastrophic in the one
// direction that matters, because the next placement would then generate a rule set
// from that empty declaration and QUIETLY REVOKE a publish the caller is relying on
// (or, on the read face, report a reachable set that is not the installed one).
// Failing the read is the honest outcome: the record is unreadable, so no answer is
// better than a plausible wrong one.
func marshalNetwork(d model.NetworkDecl) (string, error) {
	b, err := json.Marshal(d.Normalize())
	if err != nil {
		return "", fmt.Errorf("store: encode network declaration: %w", err)
	}
	return string(b), nil
}

func unmarshalNetwork(s string) (model.NetworkDecl, error) {
	if s == "" {
		// Only reachable for a row this schema could not have written (the column is
		// NOT NULL with no default). Treated as corruption rather than as "no
		// declaration": an omitted `network` is normalized to an explicit
		// `{deny, [], [], []}` at the write point (§5.D), so an empty column means the
		// row was written by something that bypassed that normalization.
		return model.NetworkDecl{}, fmt.Errorf("store: sandbox row carries an EMPTY network declaration — " +
			"an omitted `network` is normalized to an explicit {deny, [], [], []} at the write point " +
			"(§5.D), so this row bypassed it. Refusing to read it as deny-everything, which would " +
			"silently revoke any publish the caller is relying on")
	}
	var d model.NetworkDecl
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return model.NetworkDecl{}, fmt.Errorf("store: decode network declaration: %w", err)
	}
	return d.Normalize(), nil
}

// Store is the authoritative control-plane record backend (§1.1 / §4.4-C). It is
// a Behavior seam (§1.3): the transition methods PROMISE "lifecycle-state change
// + outbox append = single atomic commit" and each backend DELIVERS it — SQLite
// via BEGIN/COMMIT, Memory via a single mutex over map+slice. The
// no-capability-branch rule in this file's package doc applies to every method
// below.
//
// # The outbox sequence contract (§4.4-C), which every backend must deliver
//
// Two promises, stated HERE rather than left to whatever the backend happens to
// do, because the whole point of invariant C is that the interface promises and
// the backend delivers — never that the algorithm asks the backend what it
// supports and branches:
//
//  1. TOTAL ORDER. Every appended Event gets a Seq that is unique and strictly
//     increasing in append order. Reads that return events return them in Seq
//     order.
//
//  2. NO VISIBILITY GAP. Once an event with Seq = N is VISIBLE to a reader, no
//     event with Seq < N may become visible afterwards. A subscriber that has
//     consumed through N and resumes from after=N has therefore not skipped
//     anything, which is the entire basis on which a cursor is safe to persist.
//
// Promise 2 is the non-obvious one, and it is a real defect class rather than a
// hypothetical: assign the sequence at INSERT but make the row visible at
// COMMIT, and two concurrent producers interleave — tx1 takes 5, tx2 takes 6,
// tx2 commits first, a reader consumes 6 and advances its cursor, and 5 then
// appears BEHIND an already-delivered later event and is never seen. Serializing
// the READERS (a lock around the drain) does not help; the ordering is broken on
// the PRODUCER side.
//
// SQLite delivers promise 2 by serializing write transactions outright (a second
// INSERT while another write transaction is open is refused with SQLITE_BUSY, so
// "took a lower sequence, committed later" cannot arise) and Memory by appending
// under the one mutex that also assigns the number. Neither is a licence to
// treat this as a backend quirk: a backend that permits genuinely concurrent
// write transactions (Postgres with a BIGSERIAL default) MUST add a watermark
// gate — publish a sequence as visible only once every lower sequence has
// committed — or it does not satisfy this interface. storetest asserts it.
type Store interface {
	// CreateSandbox inserts a new sandbox record in state `creating`, appending
	// ev in the same commit if non-nil (§4.4-C). Returns ErrConflict on a
	// duplicate ID.
	CreateSandbox(ctx context.Context, s model.Sandbox, ev *Event) error

	// GetSandbox reads a sandbox record.
	GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error)

	// ListSandboxes returns all sandbox records.
	ListSandboxes(ctx context.Context) ([]model.Sandbox, error)

	// CompareAndSetState is the conditional write (§4.4-B): set the sandbox's
	// lifecycle-state to `next` only if it is currently `expected`, else return
	// ErrConflict. ev (if non-nil) is appended to the outbox in the SAME commit
	// (§4.4-C). This is the mechanism the §4 mediator's transition lock uses for
	// CAS precondition checks; serialization happens in the mediator layer, not
	// by one subsystem calling another (§4: verbs belong to the mediator).
	//
	// cause (if non-nil) sets death_cause in the SAME commit. This is why the
	// death cause rides the EXISTING CAS path instead of getting its own write
	// point: §4.3 requires the crash-paused demotion and the death cause to be
	// ONE atomic commit, because the signal is volatile — after the demotion
	// commits, the placement can be torn down at any moment and the evidence is
	// unrecoverable, so a "write the cause just afterwards" window does not exist.
	// nil = leave death_cause untouched (every non-death transition). A non-nil
	// CauseNone CLEARS it (a successful resume: the sandbox is up, so its previous
	// death no longer explains anything).
	CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event, cause *model.DeathCause) error

	// DeleteSandbox removes a sandbox record (terminal `destroyed`, §4.1),
	// appending ev in the same commit if non-nil (§4.4-C). The outbox row
	// survives the record's deletion — it is a separate table/slice.
	DeleteSandbox(ctx context.Context, id model.SandboxID, ev *Event) error

	// UnpublishedEvents returns outbox rows not yet marked published, in Seq
	// order (oldest first). This is the PUBLISHER read side (§4.4-C) — it is
	// deliberately NOT part of the atomic-commit contract (not in the transition
	// commit); the publisher reads async, delivers, then marks. at-least-once: a
	// crash after deliver but before MarkPublished re-returns the same rows.
	UnpublishedEvents(ctx context.Context) ([]Event, error)

	// Events is the REPLAY read side backing §6.1 GET /v1/events: return at most
	// limit events with Seq > after, in Seq order, plus the window bounds
	// (earliest / latest Seq currently held, both 0 on an empty outbox).
	//
	// It reads the SAME rows as UnpublishedEvents over the SAME sequence — one
	// stream with two readers, not two stores of events. Delivery state is
	// deliberately invisible here: "has the in-process sink seen it" is not a fact
	// an external subscriber's replay should depend on.
	//
	// limit is REQUIRED to be bounded by the caller AND capped again server-side
	// at the API layer. The store honors what it is given; it does not invent a
	// cap, because a second caller of the seam would then inherit a different one
	// — the audited defect was exactly two read paths with mismatched caps, one of
	// which materialized the entire backlog.
	Events(ctx context.Context, after int64, limit int) (evs []Event, earliest, latest int64, err error)

	// MarkPublished marks one outbox row (by event ID) as delivered so it stops
	// coming back from UnpublishedEvents. Idempotent: marking an unknown or
	// already-marked ID is a no-op.
	MarkPublished(ctx context.Context, eventID string) error

	// UpdateBinding rewrites a sandbox's Binding fields (§3.4) — the create-time
	// write point's only sibling, backing §6.1 PATCH. It is CONDITIONAL on the
	// current lifecycle-state, the same shape as CompareAndSetState (§4.4-B): the
	// caller declares the state it decided against, and a mismatch is ErrConflict.
	// That is what keeps a concurrent resume/evict from landing between the
	// mediator's decision and this write.
	//
	// It does NOT change state, and it must NOT touch updated_at: `retention` is
	// measured from that column (the moment the sandbox entered `paused`, §4.3), so
	// bumping it here would silently extend an eviction deadline as a side effect of
	// editing an unrelated cap.
	//
	// cause follows CompareAndSetState's convention exactly (nil = leave untouched,
	// non-nil CauseNone = clear). A raise of the RAM cap passes &CauseNone so the
	// §4.3 resource-terminated refusal is lifted in the SAME commit as the cap that
	// lifts it — otherwise the documented remedy deadlocks: the guard reads
	// death_cause, and nothing else clears it before a successful resume, which the
	// guard itself prevents.
	UpdateBinding(ctx context.Context, id model.SandboxID, expected model.Lifecycle, b Binding, cause *model.DeathCause) error

	// --- Template CRUD (§4.3 uniform object model) ---------------------------
	//
	// Template is a first-class persistent object with DERIVED refcount and
	// existence-is-state (§4.3): no state column, no materialized count. These
	// methods are the store's slice of the §4.3 reference-reclaim atomicity
	// contract — the reference-protection promise (a referenced Template cannot be
	// deleted) is delivered here, identically by both backends (§1.3 seam).

	// CreateTemplate inserts a Template row. If Parent is non-empty it must
	// reference an existing Template (§4.3 lineage, references point upward): a
	// missing parent → ErrTemplateNotFound (the reference-side of the atomicity
	// race, §4.3). Returns ErrConflict on a duplicate ID. Bytes-first (§5.B): the
	// caller has already fully materialized the layer bytes before this insert —
	// existence-is-state has no not-ready escape hatch (§4.3).
	CreateTemplate(ctx context.Context, t model.Template) error

	// GetTemplate reads a Template row; ErrTemplateNotFound if absent.
	GetTemplate(ctx context.Context, id model.TemplateID) (model.Template, error)

	// ListTemplates returns all Template rows.
	ListTemplates(ctx context.Context) ([]model.Template, error)

	// DeleteTemplate removes a Template row iff nothing references it (§4.3 /
	// §6 DELETE). A live referencing row — a sandbox whose template = id, or a
	// child Template whose parent = id — makes it ErrInUse (RESTRICT). A missing
	// row → ErrTemplateNotFound. The reclaim of the layer's bytes is DECOUPLED
	// from this row delete (§4.4 / §5.B orphan sweep) — this only removes the
	// row; the now-orphaned bytes are reclaimed by the sweep, not here.
	DeleteTemplate(ctx context.Context, id model.TemplateID) error

	// --- Volume CRUD (§5.C / §4.3 uniform object model) -----------------------
	//
	// Volume is a first-class persistent object with existence-is-state and a
	// DERIVED refcount, exactly like Template (§4.3) — no state column, no
	// materialized count. See model.Volume for what is deliberately absent from the
	// row (access mode, lease, owner) and why.
	//
	// NONE of these methods take an *Event, and that is a semantic statement rather
	// than an omission: a Volume is not a sandbox, so its CRUD is not a §4.1
	// lifecycle transition and publishes nothing — the same rule Template CRUD
	// follows (§4.3 / §6.1 PATCH "does not emit an event").

	// CreateVolume inserts a Volume row. Returns ErrConflict on a duplicate ID.
	//
	// BYTES-FIRST (§5.C / §4.3): the caller has ALREADY allocated the backend storage
	// and written its cap before this insert. Existence-is-state offers no not-ready
	// state to park a half-built volume in, so a crash before this call leaves
	// ORPHAN BYTES (which the sweep reclaims) rather than a row naming storage that
	// does not exist.
	CreateVolume(ctx context.Context, v model.Volume) error

	// GetVolume reads a Volume row; ErrVolumeNotFound if absent.
	GetVolume(ctx context.Context, id model.VolumeID) (model.Volume, error)

	// ListVolumes returns all Volume rows. The §3.4 admission gate reads this to sum
	// Σ(volume caps) into the FIRST term of the node inequality — a volume cap is a
	// declared upper bound like a sandbox cap, so it is a like term rather than a new
	// one.
	ListVolumes(ctx context.Context) ([]model.Volume, error)

	// SetVolumeCapacity rewrites one volume's cap — the §6.1 PATCH write point, and
	// the only sibling of the create-time write.
	//
	// UNLIKE UpdateBinding IT IS NOT CONDITIONAL ON ANY LIFECYCLE STATE, and the
	// reason is mechanical, not a relaxation: a sandbox cap is re-materialized at the
	// next placement, so editing one on a `running` sandbox would leave the record
	// and the live unit disagreeing — whereas a VOLUME HAS NO PLACEMENT OF ITS OWN,
	// so "re-materialize next placement" would mean NEVER for an unmounted volume.
	// The cap therefore lands on the storage directly, there is nothing to defer, and
	// so there is no "pause it first" conflict to express (§5.C / §6.1 PATCH).
	//
	// The STRICT-inequality refusal ("lower below current usage") is NOT enforced
	// here: it needs the backend's usage reading, which the store cannot and must not
	// obtain (§4: the store holds records, the mediator asks subsystems). The mediator
	// evaluates it before calling this.
	//
	// ErrVolumeNotFound if the row is absent.
	SetVolumeCapacity(ctx context.Context, id model.VolumeID, capByte int64) error

	// DeleteVolume removes a Volume row. A missing row → ErrVolumeNotFound.
	//
	// IDEMPOTENT DELETE MUST NOT SWALLOW THAT (§6.2): the not-found verdict is
	// reported rather than folded into success. An external implementation folded
	// three distinct not-founds into `200 {"deleted": true}`, which makes a delete
	// that a guard REFUSED indistinguishable from one that succeeded — and the
	// reference-protection refusals that land on this same path are the whole
	// external signal those guards have.
	//
	// Byte reclamation is DECOUPLED from this row delete (§4.4 / §5.C orphan sweep):
	// this removes only the row, and the now-orphaned bytes are the sweep's job.
	//
	// Two distinct RESTRICT verdicts, and they must stay distinguishable (§6.1):
	// ErrVolumeInUse when a live mount row holds it, ErrVolumeHasDescendants when a live
	// snapshot/clone names it as parent. Both differ again from ErrInUse, whose text
	// names a Template.
	DeleteVolume(ctx context.Context, id model.VolumeID) error

	// --- Mount table (§5.C / §4.3) --------------------------------------------
	//
	// A mount row is a first-class persistent object, structurally the DOUBLING of
	// `sandboxes.template`: two columns, each ON DELETE RESTRICT. No state column, no
	// count column — "is this volume exclusively held" is DERIVED from the rows.
	//
	// Mount rows publish NO events: they are not §4.1 lifecycle transitions (§4.3, the
	// rule Template and Volume CRUD already follow).

	// CreateMounts inserts a sandbox's COMPLETE binding atomically, enforcing the
	// exclusive lease — the ONE new store invariant Volume introduces (§5.C).
	//
	// THE JUDGMENT AND THE INSERT MUST NOT BE SEPARABLE. A mediator-side pre-check
	// followed by an insert does NOT work, and this is a real measured TOCTOU rather
	// than a theoretical one: two concurrent creates mint two fresh SandboxIDs and so
	// hold two DISJOINT transition locks, meaning nothing serializes them against each
	// other — the only lock held at that instant is each sandbox's own. Measured: 8
	// racers, no scheduling hooks, broke the lease 2 times in 4000 iterations, and it
	// reproduces every time with an explicit scheduling point. This is the same reason
	// the admission gate needs its own mutex (a node-global invariant cannot be
	// enforced by per-sandbox locks).
	//
	// So the BACKEND arbitrates: one statement expressing the whole cross-row
	// predicate, serialized by the single authoritative store. This is NOT a new
	// mechanism — it is §4.3's reference-reclaim shape (one authoritative store
	// serializing judgment-and-action) applied to a second invariant. SQLite delivers
	// it with INSERT…SELECT…WHERE NOT EXISTS inside one transaction; Memory under the
	// one mutex it already holds. No new lock class on either side.
	//
	// The INVARIANT, stated precisely (§5.C): a volume has AT MOST ONE `exclusive`
	// mount row, AND that row does not coexist with ANY other mount row for that
	// volume. Note what it is NOT: "at most one rw row" would contradict
	// SharedReadWrite's whole fanout.
	//
	// ALL-OR-NOTHING across the whole binding: if any one mount loses the lease, NONE
	// is inserted. A sandbox holds one complete binding, so a partially-applied
	// binding is not a state the model has.
	//
	// Verdicts: ErrMountLeaseConflict when the lease is contended;
	// ErrMountBindingInvalid when the submitted binding contradicts itself (duplicate
	// destination, duplicate volume, or nesting); ErrNotFound / ErrVolumeNotFound when
	// a referenced sandbox or volume does not exist (the reference side of §4.3
	// atomicity). Paths MUST already be normalized by the caller — the uniqueness
	// assertions are literal string comparisons, so an un-normalized path makes them
	// silently not apply.
	CreateMounts(ctx context.Context, mounts []model.Mount) error

	// ReplaceMounts swaps one sandbox's COMPLETE binding for another — the write point
	// behind §6.1 `PATCH /v1/sandboxes/{id}/volumes`. An EMPTY `next` legitimately means
	// "detach everything" (§6.2: PATCH swaps whole bindings, there is no attach-one /
	// detach-one operation), so it is not a special case and not a no-op.
	//
	// THE REPLACEMENT IS ONE ATOMIC COMMIT, AND THAT IS A CONTRACT PROMISE, not an
	// implementation detail of one backend. §6.2 says a sandbox holds one complete
	// binding at EVERY INSTANT; two independent commits (delete, then insert) contradict
	// that literally, because between them the sandbox holds NO binding and that state is
	// observable. Note what the order cannot fix: the delete MUST precede the insert — a
	// re-submitted IDENTICAL binding would otherwise collide with the very rows it is
	// replacing, on both the lease predicate and UNIQUE(sandbox, path) — so the answer is
	// not to reorder the two statements but to put them in ONE commit.
	//
	// Three consequences, each a real defect of the two-commit shape rather than a
	// theoretical one:
	//
	//  1. A REFUSED replacement leaves the sandbox on its OLD binding. There is no
	//     "the new binding was rejected AND the old one is gone" outcome, so no
	//     compensating re-insert is needed — and a compensation path for a state the
	//     model denies is exactly the kind of code whose comment and behavior drift apart.
	//  2. THE EXCLUSIVE LEASE IS NEVER TRANSIENTLY RELEASED. The lease IS the row (§5.C),
	//     so a window with no rows is a window with no lease: a concurrent create could
	//     take a volume THIS replacement intended to keep, and the caller who asked to
	//     keep it would get a 409 for a lease it never gave up.
	//  3. A CONCURRENT ListMountsByVolume NEVER OBSERVES ZERO ROWS. That query is the
	//     write-regime predicate (§5.C direct write): "no mount rows" means "nobody has
	//     it mounted, a direct host-side write is safe", so the intermediate state would
	//     wrongly ADMIT a direct write against a volume a live sandbox is holding.
	//
	// THIS IS NOT A TRANSACTION ON THE SEAM. It is §4.4-C's discipline applied a second
	// time: the INTERFACE METHOD promises the atomicity and each backend honors it BY ITS
	// OWN MEANS (SQLite with one local transaction, Memory by replacing the rows under
	// the one mutex it already holds). There is ONE algorithm, and it must never branch
	// on "does this backend support transactions" — that would leak backend differences
	// into correctness, which §4.4-C rejects outright.
	//
	// Verdicts are exactly CreateMounts's, because the API maps them to the same status
	// codes: ErrMountLeaseConflict for a contended lease (409), ErrMountBindingInvalid
	// for a binding that contradicts itself (400) — including a row whose Sandbox is not
	// the `sandbox` being rebound, which would otherwise delete one sandbox's rows and
	// insert another's — ErrNotFound / ErrVolumeNotFound when an endpoint is absent.
	// Paths MUST already be normalized by the caller, same as CreateMounts.
	ReplaceMounts(ctx context.Context, sandbox model.SandboxID, next []model.Mount) error

	// ListMounts returns one sandbox's mount rows, ordered by path. This is what a
	// resume re-materializes from: the mount relationship is a Binding, so a resume
	// re-mounts per the CURRENT table rather than restoring frozen bytes (§3.4 / §5.C).
	ListMounts(ctx context.Context, sandbox model.SandboxID) ([]model.Mount, error)

	// ListMountsByVolume returns every mount row referencing one volume, in ANY
	// sandbox and regardless of that sandbox's state.
	//
	// The orphan sweep needs it as a live-mount check INDEPENDENT of any FK (§5.C
	// safety requirement): skip any volume appearing in ANY mount row, live or not. FK
	// RESTRICT protects the ROW; this protects the BYTES, which is a different thing
	// and the one a mis-sweep destroys.
	ListMountsByVolume(ctx context.Context, volume model.VolumeID) ([]model.Mount, error)

	// DeleteMounts removes ALL of one sandbox's mount rows. This is the LEASE RELEASE
	// POINT: the lease is the row's existence, so the row going away IS the release
	// (§5.C — there is no lease object to release separately).
	//
	// Idempotent (no rows → nil): destroy, unwind and evict all reach it.
	//
	// IT SURVIVES ReplaceMounts RATHER THAN COLLAPSING INTO IT, and the distinction is
	// semantic, not stylistic. Every remaining caller is a TEARDOWN: destroy, create's
	// unwind, evict, reconcile's cleanup. Those have no "next" binding — the sandbox
	// itself is going away — so `ReplaceMounts(id, nil)` would misdescribe them as a
	// rebind to the empty binding and would drag in the endpoint-existence verdicts a
	// teardown must not care about (it runs precisely when the row is about to vanish).
	// A rebind, conversely, must NEVER be expressed as this call plus CreateMounts: that
	// is the two-commit shape ReplaceMounts exists to forbid.
	//
	// CALLERS MUST CALL THIS BEFORE DeleteSandbox, and the ORDER IS REQUIRED rather
	// than stylistic (measured): RESTRICT fires IMMEDIATELY, so wrapping both deletes
	// in one transaction buys NO order-independence. And `defer_foreign_keys` is
	// REJECTED rather than untried: it does make the order irrelevant, but the
	// violation then surfaces at Commit() — and when Go's Tx.Commit() returns an error
	// SQLite has NOT rolled back, so on the pinned single connection the transaction
	// stays open and subsequent pooled queries run INSIDE it, seeing uncommitted state
	// no other handle can see. That is a store-level correctness risk, not a style
	// preference.
	DeleteMounts(ctx context.Context, sandbox model.SandboxID) error
}
