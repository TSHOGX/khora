// Package store is the control-plane record seam (the documented invariant / the documented invariant). It is a
// Behavior seam (the documented invariant): an interface with pluggable implementations. Two live
// side by side behind it — an in-memory map (for fast unit tests) and the
// embedded single-node backend (SQLite, the recorded design) — chosen by registration,
// not by reshaping callers.
//
// The interface itself promises the minimal guarantees the algorithms need
// (the documented invariant): "lifecycle-state change + outbox append = single atomic commit".
// The CAS shape below gives the the documented invariant transition lock somewhere to land.
//
// NO-CAPABILITY-BRANCH RULE (the documented invariant, and it governs every method here): NEVER
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

	"github.com/TSHOGX/khora/internal/model"
)

// ErrConflict is returned by CompareAndSetState when the current state is not
// the expected source state — the CAS lost (the documented invariant: pause vs resume racing, the
// later verb sees a state it didn't expect and is rejected).
var ErrConflict = errors.New("store: state precondition failed")

// ErrNotFound is returned when a sandbox record does not exist.
var ErrNotFound = errors.New("store: sandbox not found")

// ErrTemplateNotFound is returned when a Template record does not exist —
// including the reference-side failure: inserting a sandbox (or child Template)
// that references a Template no concurrent transaction left in place (the documented invariant
// reference-reclaim atomicity: DELETE committed first → the referrer hits
// missing-parent).
var ErrTemplateNotFound = errors.New("store: template not found")

// ErrInUse is the the documented invariant reference-protection verdict: a DeleteTemplate lost to a
// live referencing row (a sandbox pinning it, or a child Template naming it as
// parent). It is the store SEAM's contract promise (the documented invariant / the documented invariant), NOT a SQLite
// feature leaking out: SQLite delivers it via FK RESTRICT, Memory via a Go scan
// — the two backends run one set of assertions. The API layer maps it to a clean
// 409 template in use (the documented invariant), never a raw 500.
var ErrInUse = errors.New("store: template in use")

// ErrVolumeNotFound is returned when a Volume record does not exist (the documented invariant). Its
// OWN sentinel rather than a reuse of ErrNotFound ("sandbox not found"): a 404 whose
// text names the wrong object sends the caller looking in the wrong place (the documented invariant).
var ErrVolumeNotFound = errors.New("store: volume not found")

// The Volume reference verdicts (the documented invariant's four 409s). FOUR SEPARATE SENTINELS because each
// names a DIFFERENT object relationship and the caller's remedy differs; ErrInUse's text says
// "template in use" verbatim, so reusing it returns a 409 naming the wrong object. A storetest
// case asserts they stay mutually distinguishable — the degradation is someone later
// collapsing two that map to the same status code.
var (
	// ErrVolumeInUse is verdict ①: deleting a volume that a live mount row holds
	// (mounts.volume RESTRICT). The Template-shaped case, on a different object.
	ErrVolumeInUse = errors.New("store: volume in use by a mount")

	// ErrSandboxHasMounts is verdict ②'s sandbox side: deleting a sandbox that a live
	// mount row holds (mounts.sandbox RESTRICT).
	//
	// A sentinel is required even though the FK-violation predicate is generic
	// (isFKViolation is table- and column-agnostic): without a translation point in
	// DeleteSandbox the raw driver error reaches the API's default arm as a 500. The
	// predicate is reusable; the translation is per-call-site.
	ErrSandboxHasMounts = errors.New("store: sandbox has live mounts")

	// ErrMountLeaseConflict is verdict ③: an attach lost the exclusive lease. It needs its
	// OWN predicate as well as its own sentinel — the FK-violation string match correctly
	// reports FALSE for a uniqueness or predicate failure.
	//
	// Unlike ①②④ this is a RETRYABLE state conflict: the caller may succeed once the
	// holder releases.
	ErrMountLeaseConflict = errors.New("store: volume already exclusively mounted")

	// ErrMountBindingInvalid is the the documented invariant 400 (NOT a 409): a single submitted binding that
	// CONTRADICTS ITSELF — the same destination twice, the same volume twice, or one volume
	// nested inside another's subtree. 400 because it is decidable without reading any other
	// row and no retry can make it legal, which is the documented invariant's 400 criterion.
	ErrMountBindingInvalid = errors.New("store: mount binding is self-contradictory")

	// ErrVolumeHasDescendants is verdict ②'s volume side: deleting a volume that a live
	// snapshot or clone names as its parent (volumes.parent RESTRICT) — the documented invariant's trap recurring
	// on the fourth FK path, distinguishable from ErrVolumeInUse because the remedy differs
	// (delete a descendant vs detach a mount).
	ErrVolumeHasDescendants = errors.New("store: volume has live snapshots or clones")
)

// Event is one outbox row: a the documented invariant lifecycle transition to publish to the event
// stream (created / paused / resumed / destroyed …). It is appended in the SAME
// atomic commit as the state change that produced it (the documented invariant) — a committed
// transition can never lack its event, nor an event lack its transition. The
// transition methods below take an *Event: the the documented invariant mediator constructs it per
// transition semantics, carrying a transition-unique ID (the documented invariant). A nil Event
// means "this store mutation publishes nothing" (the transient creating-insert
// and the create-unwind delete are internal steps of a verb, not externally
// visible the documented invariant transitions) — that is a mediator semantic choice, NOT a backend
// capability branch.
type Event struct {
	// Seq is the outbox's monotonic total order — the CURSOR a subscriber
	// resumes from (the documented invariant GET /v1/events?after=). It is assigned BY THE BACKEND at
	// append; a caller-supplied value is ignored (the mediator does not own this
	// number, the store does). Zero on an Event being handed TO the store, and
	// always > 0 on one read back out.
	//
	// OccurredAt cannot serve instead: unix nanos are not a total order (no tie-break at
	// equal nanos, and NTP can step the clock backwards) — see the outbox schema.
	Seq int64
	// ID is the transition-unique key subscribers dedup on (the documented invariant
	// at-least-once: a redelivered event carries the SAME ID). Backends key the
	// outbox row uniquely on it.
	ID string
	// Sandbox is which sandbox transitioned. Fine as an engine-internal field —
	// `sandbox` is a core atom, not a banned product concept (the documented invariant vocabulary).
	Sandbox model.SandboxID
	// Kind is the transition name ("created" / "paused" / "resumed" /
	// "destroyed" / "auto-paused" / "crash-paused" …). The three pause-like Kinds
	// are distinct on purpose: "paused" = user pause, "auto-paused" = reaper idle
	// pause, "crash-paused" = INVOLUNTARY demote (the documented invariant / the documented invariant) — which covers two
	// paths, not just death: a unit found dead (cause recorded alongside) and the
	// transport-down restart-budget escalation (reaper-health the documented invariantb), where L1 is
	// still ALIVE and so no cause is stamped. Whether a cause is present is what
	// separates them. All land state=paused, the Kind carries WHY.
	//
	// A TYPE rather than a bare string, and both backends assert the same closed set of
	// six — see model.EventKind. the documented invariant's "the event schema is a subscriber-side concern"
	// is about the WIRE shape and does not license writing a kind the engine never named.
	Kind model.EventKind
	// State is the resulting lifecycle-state after the transition.
	State model.Lifecycle
	// Cause is WHY, for the Kinds a subscriber cannot otherwise tell apart (the documented invariant). Empty on
	// every Kind with a single route. Today it separates `destroyed`'s two routes:
	// `requested` (the Destroy verb) from `retention-expired` (the reaper's evict of a paused
	// sandbox past `retention`). Exactly two, not three — the documented invariant's protect-on-uncertainty
	// means no health verdict ever destroys. See model.EventCause for why it must ride the
	// event rather than be read back from the record.
	Cause model.EventCause
	// OccurredAt is when the mediator constructed the transition.
	OccurredAt time.Time
}

// Binding is the mutable slice of a sandbox's the documented invariant Binding — what the documented invariant PATCH may
// rewrite. Each field is a POINTER so "not given" is distinguishable from "set to
// zero", which is load-bearing here: on these axes zero is itself a meaningful
// instruction (`0 = unlimited` for a cap, `retention: 0` = never evict), so a
// value-typed struct could not express "leave this alone" at all.
//
// Only fields the engine can actually re-materialize appear here. Template and
// Overlay are NOT Binding-mutable (changing either would make it a different
// sandbox, the documented invariant), and State is not here because this is not a transition — it is
// conditional ON the state (the documented invariant).
type Binding struct {
	CPUMillis   *int64
	MemoryByte  *int64
	DiskByte    *int64
	IdleTimeout *time.Duration
	Retention   *time.Duration
	// Network is the the documented invariant declaration, replaced WHOLE (the documented invariant: PATCH swaps one
	// complete binding for another; there is no field-level merge and no
	// "pending/unresolved" value).
	//
	// A POINTER carries more weight here than on the others: the zero declaration is not a
	// no-op but `{deny, [], [], []}`, the most restrictive there is, so a value type would
	// make every unrelated PATCH (raising a RAM cap, changing a TTL) silently reset the
	// sandbox's network to deny-everything.
	//
	// Also the field `POST …/publish` writes through, carrying a declaration the mediator
	// produced by read-append under the transition lock. The store cannot tell the two write
	// points apart — both replace the whole value — so the append happens above it, two
	// concurrent publishes otherwise losing one (the documented invariant).
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
		// nil array where the documented invariant promises `[]`.
		net = b.Network.Normalize()
	}
	return lim, idle, ret, net
}

// marshalNetwork / unmarshalNetwork are the the documented invariant declaration's at-rest form.
//
// A FAILURE HERE IS RETURNED, NEVER SWALLOWED: zero-valuing a decode error presents the
// sandbox as `{deny, [], [], []}`, which looks safe and is catastrophic in the one direction
// that matters — the next placement generates a rule set from that empty declaration and
// quietly revokes a publish the caller relies on.
func marshalNetwork(d model.NetworkDecl) (string, error) {
	b, err := json.Marshal(d.Normalize())
	if err != nil {
		return "", fmt.Errorf("store: encode network declaration: %w", err)
	}
	return string(b), nil
}

func unmarshalNetwork(s string) (model.NetworkDecl, error) {
	if s == "" {
		// Corruption, not "no declaration": the column is NOT NULL with no default and an
		// omitted `network` is normalized to an explicit `{deny, [], [], []}` at the write
		// point (the documented invariant), so an empty column means the row bypassed that normalization.
		return model.NetworkDecl{}, errors.New("store: sandbox row carries an empty network declaration, bypassing the the documented invariant write-point normalization; refusing to read it as deny-everything")
	}
	var d model.NetworkDecl
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return model.NetworkDecl{}, fmt.Errorf("store: decode network declaration: %w", err)
	}
	return d.Normalize(), nil
}

// Store is the authoritative control-plane record backend (the documented invariant / the documented invariant) and a Behavior seam
// (the documented invariant). The package doc's no-capability-branch rule applies to every method below.
//
// # The outbox sequence contract (the documented invariant)
//
// Two promises stated HERE rather than left to the backend:
//
//  1. TOTAL ORDER. Every appended Event gets a unique Seq, strictly increasing in append
//     order, and reads return events in Seq order.
//
//  2. NO VISIBILITY GAP. Once Seq = N is visible to a reader, no event with Seq < N may become
//     visible afterwards — the entire basis on which a cursor is safe to persist.
//
// Promise 2 is the non-obvious one: assign the sequence at INSERT but make the row visible at
// COMMIT and two producers interleave — tx1 takes 5, tx2 takes 6, tx2 commits first, a reader
// consumes 6 and advances, and 5 appears behind an already-delivered event and is never seen.
// Serializing the READERS does not help; the break is on the producer side.
//
// SQLite delivers it by serializing write transactions outright, Memory by appending under the
// mutex that assigns the number. A backend permitting genuinely concurrent write transactions
// (Postgres with BIGSERIAL) MUST add a watermark gate — a sequence becomes visible only once
// every lower one has committed — or it does not satisfy this interface. storetest asserts it.
type Store interface {
	// CreateSandbox inserts a new sandbox record in state `creating`, appending
	// ev in the same commit if non-nil (the documented invariant). Returns ErrConflict on a
	// duplicate ID.
	CreateSandbox(ctx context.Context, s model.Sandbox, ev *Event) error

	// GetSandbox reads a sandbox record.
	GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error)

	// ListSandboxes returns all sandbox records.
	ListSandboxes(ctx context.Context) ([]model.Sandbox, error)

	// CompareAndSetState is the conditional write (the documented invariant): set the sandbox's
	// lifecycle-state to `next` only if it is currently `expected`, else return
	// ErrConflict. ev (if non-nil) is appended to the outbox in the SAME commit
	// (the documented invariant). This is the mechanism the the documented invariant mediator's transition lock uses for
	// CAS precondition checks; serialization happens in the mediator layer, not
	// by one subsystem calling another (the documented invariant: verbs belong to the mediator).
	//
	// cause (if non-nil) sets death_cause in the SAME commit. This is why the
	// death cause rides the EXISTING CAS path instead of getting its own write
	// point: the documented invariant requires the crash-paused demotion and the death cause to be
	// ONE atomic commit, because the signal is volatile — after the demotion
	// commits, the placement can be torn down at any moment and the evidence is
	// unrecoverable, so a "write the cause just afterwards" window does not exist.
	// nil = leave death_cause untouched (every non-death transition). A non-nil
	// CauseNone CLEARS it (a successful resume: the sandbox is up, so its previous
	// death no longer explains anything).
	CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event, cause *model.DeathCause) error

	// DeleteSandbox removes a sandbox record (terminal `destroyed`, the documented invariant),
	// appending ev in the same commit if non-nil (the documented invariant). The outbox row
	// survives the record's deletion — it is a separate table/slice.
	DeleteSandbox(ctx context.Context, id model.SandboxID, ev *Event) error

	// UnpublishedEvents returns at most limit outbox rows not yet marked
	// published, in Seq order (oldest first). This is the PUBLISHER read side
	// (the documented invariant) — it is deliberately NOT part of the atomic-commit contract (not in
	// the transition commit); the publisher reads async, delivers, then marks.
	// at-least-once: a crash after deliver but before MarkPublished re-returns the
	// same rows.
	//
	// limit is REQUIRED and bounded by the caller, for the same reason Events' is. A bound is
	// safe for the drain because delivery is strictly in Seq order and stops at the first
	// failure, so a pass can never usefully act on more than one batch; whatever is left is
	// still there next tick.
	//
	// A caller that needs the backlog's SIZE must use CountUnpublished, never len() of this
	// result.
	UnpublishedEvents(ctx context.Context, limit int) ([]Event, error)

	// CountUnpublished reports how many outbox rows are not yet marked published.
	//
	// Separate from the drain read per the documented invariant's operability requirement ③ (observability must
	// not share a source with the loop being observed), and because a bounded read cannot
	// answer it: len(UnpublishedEvents(ctx, limit)) saturates at limit, reporting a small
	// constant at precisely the moment the backlog is large.
	CountUnpublished(ctx context.Context) (int64, error)

	// Events is the REPLAY read side backing the documented invariant GET /v1/events: return at most
	// limit events with Seq > after, in Seq order, plus the window bounds
	// (earliest / latest Seq currently held, both 0 on an empty outbox).
	//
	// It reads the SAME rows as UnpublishedEvents over the SAME sequence — one
	// stream with two readers, not two stores of events. Delivery state is
	// deliberately invisible here: "has the in-process sink seen it" is not a fact
	// an external subscriber's replay should depend on.
	//
	// limit is REQUIRED to be bounded by the caller AND capped again server-side at the API
	// layer. The store honors what it is given and does not invent a cap: a second caller of
	// the seam would inherit a different one, and the audited defect was two read paths with
	// mismatched caps, one materializing the entire backlog.
	Events(ctx context.Context, after int64, limit int) (evs []Event, earliest, latest int64, err error)

	// MarkPublished marks one outbox row (by event ID) as delivered so it stops
	// coming back from UnpublishedEvents. Idempotent: marking an unknown or
	// already-marked ID is a no-op.
	MarkPublished(ctx context.Context, eventID string) error

	// TrimOutbox deletes at most limit DELIVERED outbox rows whose delivery is older
	// than cutoff, oldest first, and returns how many it deleted. It is the mechanism
	// behind the documented invariant's retention window; the WINDOW'S VALUE belongs to the deployment
	// and 0 (never trim) is the engine's default, so a deployment that sets nothing
	// never reaches this.
	//
	// Four properties, each with a wrong answer that looks reasonable (the documented invariant argues each):
	//
	//   - AGE IS MEASURED FROM DELIVERY (published_at), never occurrence. Counterexample: a
	//     halted stream resumed days later delivers its backlog with fresh delivery times but
	//     ancient occurred_at, so ageing on the latter trims it at first delivery.
	//   - DELIVERED ROWS ONLY. Must NOT consult the publisher's halt latch to achieve it —
	//     the halted head is undelivered, so the predicate already stops below it.
	//   - THE NEWEST ROW IS NEVER TRIMMED, even when it qualifies: an empty table reads 0/0,
	//     indistinguishable from "nothing ever happened", so a stale cursor gets a silent
	//     short page instead of the documented invariant's 410. One retained row keeps the bounds truthful.
	//   - BOUNDED BY limit, like the drain read: the delete runs on a possibly-serialized write
	//     path, and an unbounded one holds it while every state transition waits.
	//
	// Trimming advances the window's lower bound, which is what makes the documented invariant's 410 reachable.
	TrimOutbox(ctx context.Context, cutoff time.Time, limit int) (int, error)

	// UpdateBinding rewrites a sandbox's Binding fields (the documented invariant) — the create-time
	// write point's only sibling, backing the documented invariant PATCH. It is CONDITIONAL on the
	// current lifecycle-state, the same shape as CompareAndSetState (the documented invariant): the
	// caller declares the state it decided against, and a mismatch is ErrConflict.
	// That is what keeps a concurrent resume/evict from landing between the
	// mediator's decision and this write.
	//
	// It does NOT change state, and it must NOT touch updated_at: `retention` is
	// measured from that column (the moment the sandbox entered `paused`, the documented invariant), so
	// bumping it here would silently extend an eviction deadline as a side effect of
	// editing an unrelated cap.
	//
	// cause follows CompareAndSetState's convention (nil = leave untouched, non-nil CauseNone
	// = clear). A raise of the RAM cap passes &CauseNone so the the documented invariant resource-terminated
	// refusal is lifted in the SAME commit as the cap lifting it; otherwise the documented
	// remedy deadlocks, the guard reading a death_cause that only a successful resume — which
	// the guard itself prevents — would clear.
	UpdateBinding(ctx context.Context, id model.SandboxID, expected model.Lifecycle, b Binding, cause *model.DeathCause) error

	// --- Template CRUD (the documented invariant uniform object model) ---------------------------
	//
	// See model.Template for the shape (existence-is-state, derived refcount). These
	// methods are the store's slice of the the documented invariant reference-reclaim atomicity contract —
	// the reference-protection promise (a referenced Template cannot be deleted) is
	// delivered here, identically by both backends (the documented invariant seam).

	// CreateTemplate inserts a Template row. If Parent is non-empty it must
	// reference an existing Template (the documented invariant lineage, references point upward): a
	// missing parent → ErrTemplateNotFound (the reference-side of the atomicity
	// race, the documented invariant). Returns ErrConflict on a duplicate ID. Bytes-first (the documented invariant): the
	// caller has already fully materialized the layer bytes before this insert —
	// existence-is-state has no not-ready escape hatch (the documented invariant).
	CreateTemplate(ctx context.Context, t model.Template) error

	// GetTemplate reads a Template row; ErrTemplateNotFound if absent.
	GetTemplate(ctx context.Context, id model.TemplateID) (model.Template, error)

	// ListTemplates returns all Template rows.
	ListTemplates(ctx context.Context) ([]model.Template, error)

	// DeleteTemplate removes a Template row iff nothing references it (the documented invariant /
	// the documented invariant DELETE). A live referencing row — a sandbox whose template = id, or a
	// child Template whose parent = id — makes it ErrInUse (RESTRICT). A missing
	// row → ErrTemplateNotFound. The reclaim of the layer's bytes is DECOUPLED
	// from this row delete (the documented invariant / the documented invariant orphan sweep) — this only removes the
	// row; the now-orphaned bytes are reclaimed by the sweep, not here.
	DeleteTemplate(ctx context.Context, id model.TemplateID) error

	// --- Volume CRUD (the documented invariant / the documented invariant uniform object model) -----------------------
	//
	// See model.Volume for the row's shape and for what is deliberately absent from it
	// (access mode, lease, owner).
	//
	// NONE of these methods take an *Event, and that is a semantic statement rather
	// than an omission: a Volume is not a sandbox, so its CRUD is not a the documented invariant
	// lifecycle transition and publishes nothing — the same rule Template CRUD
	// follows (the documented invariant / the documented invariant PATCH "does not emit an event").

	// CreateVolume inserts a Volume row. Returns ErrConflict on a duplicate ID.
	//
	// BYTES-FIRST (the documented invariant / the documented invariant): the caller has ALREADY allocated the backend storage and
	// written its cap before this insert. Existence-is-state offers no not-ready state to park
	// a half-built volume in, so a crash before this call leaves ORPHAN BYTES for the sweep
	// rather than a row naming storage that does not exist.
	CreateVolume(ctx context.Context, v model.Volume) error

	// GetVolume reads a Volume row; ErrVolumeNotFound if absent.
	GetVolume(ctx context.Context, id model.VolumeID) (model.Volume, error)

	// ListVolumes returns all Volume rows. The the documented invariant admission gate reads this to sum
	// Σ(volume caps) into the FIRST term of the node inequality — a volume cap is a
	// declared upper bound like a sandbox cap, so it is a like term rather than a new
	// one.
	ListVolumes(ctx context.Context) ([]model.Volume, error)

	// SetVolumeCapacity rewrites one volume's cap — the the documented invariant PATCH write point, and
	// the only sibling of the create-time write.
	//
	// NOT CONDITIONAL ON ANY LIFECYCLE STATE, unlike UpdateBinding, for a mechanical reason:
	// a volume has no placement of its own, so "re-materialize at the next placement" would
	// mean NEVER for an unmounted volume. The cap lands on the storage directly, leaving no
	// deferral and so no "pause it first" conflict to express (the documented invariant / the documented invariant PATCH).
	//
	// The STRICT-inequality refusal ("lower below current usage") is NOT enforced
	// here: it needs the backend's usage reading, which the store cannot and must not
	// obtain (the documented invariant: the store holds records, the mediator asks subsystems). The mediator
	// evaluates it before calling this.
	//
	// ErrVolumeNotFound if the row is absent.
	SetVolumeCapacity(ctx context.Context, id model.VolumeID, capByte int64) error

	// DeleteVolume removes a Volume row. A missing row → ErrVolumeNotFound.
	//
	// IDEMPOTENT DELETE MUST NOT SWALLOW THAT (the documented invariant): an external implementation folded three
	// distinct not-founds into `200 {"deleted": true}`, making a delete a guard REFUSED
	// indistinguishable from one that succeeded — and the reference-protection refusals landing
	// on this path are the whole external signal those guards have.
	//
	// Byte reclamation is DECOUPLED from this row delete (the documented invariant / the documented invariant orphan sweep).
	//
	// Two distinct RESTRICT verdicts, and they must stay distinguishable (the documented invariant):
	// ErrVolumeInUse when a live mount row holds it, ErrVolumeHasDescendants when a live
	// snapshot/clone names it as parent. Both differ again from ErrInUse, whose text
	// names a Template.
	DeleteVolume(ctx context.Context, id model.VolumeID) error

	// --- Mount table (the documented invariant / the documented invariant) --------------------------------------------
	//
	// See model.Mount for the row's shape. Mount rows publish NO events: they are not
	// the documented invariant lifecycle transitions (the documented invariant, the rule Template and Volume CRUD already follow).

	// CreateMounts inserts a sandbox's COMPLETE binding atomically, enforcing the
	// exclusive lease — the ONE new store invariant Volume introduces (the documented invariant).
	//
	// THE JUDGMENT AND THE INSERT MUST NOT BE SEPARABLE — a measured TOCTOU: two concurrent
	// creates mint two fresh SandboxIDs and so hold two DISJOINT transition locks, with nothing
	// serializing them. Measured: 8 racers, no scheduling hooks, broke the lease 2 times in
	// 4000 iterations. A node-global invariant cannot be enforced by per-sandbox locks, the same
	// reason the admission gate needs its own mutex.
	//
	// So the BACKEND arbitrates: one statement expressing the whole cross-row predicate —
	// the documented invariant's reference-reclaim shape on a second invariant, delivered by SQLite with
	// INSERT…SELECT…WHERE NOT EXISTS in one transaction and by Memory under its existing mutex.
	//
	// The INVARIANT (the documented invariant): a volume has AT MOST ONE `exclusive` mount row, and that row does
	// not coexist with ANY other row for that volume. Not "at most one rw row", which would
	// contradict SharedReadWrite's fanout.
	//
	// ALL-OR-NOTHING across the binding: if one mount loses the lease, none is inserted, since a
	// partially-applied binding is not a state the model has.
	//
	// Verdicts: ErrMountLeaseConflict for a contended lease; ErrMountBindingInvalid for a
	// binding contradicting itself (duplicate destination, duplicate volume, nesting);
	// ErrNotFound / ErrVolumeNotFound for an absent endpoint (the documented invariant's reference side). Paths MUST
	// arrive normalized — the uniqueness assertions are literal string comparisons, so an
	// un-normalized path makes them silently not apply.
	CreateMounts(ctx context.Context, mounts []model.Mount) error

	// ReplaceMounts swaps one sandbox's COMPLETE binding for another — the write point
	// behind the documented invariant `PATCH /v1/sandboxes/{id}/volumes`. An EMPTY `next` legitimately means
	// "detach everything" (the documented invariant: PATCH swaps whole bindings, there is no attach-one /
	// detach-one operation), so it is not a special case and not a no-op.
	//
	// THE REPLACEMENT IS ONE ATOMIC COMMIT, as a contract promise rather than one backend's
	// detail. Reordering cannot substitute — the delete MUST precede the insert, or a
	// re-submitted identical binding collides with the rows it is replacing on both the lease
	// predicate and UNIQUE(sandbox, path). the documented invariant states the three consequences two independent
	// commits cannot hold (the original binding survives a refusal; the lease is not
	// transiently released; ListMountsByVolume never reads zero rows).
	//
	// NOT A TRANSACTION ON THE SEAM — the documented invariant's discipline again: the method promises atomicity
	// and each backend honors it by its own means.
	//
	// Verdicts are exactly CreateMounts's, mapping to the same status codes:
	// ErrMountLeaseConflict (409), ErrMountBindingInvalid (400) — including a row whose Sandbox
	// is not the one being rebound, which would delete one sandbox's rows and insert another's —
	// ErrNotFound / ErrVolumeNotFound for an absent endpoint. Paths arrive normalized.
	ReplaceMounts(ctx context.Context, sandbox model.SandboxID, next []model.Mount) error

	// ListMounts returns one sandbox's mount rows, ordered by path. This is what a
	// resume re-materializes from: the mount relationship is a Binding, so a resume
	// re-mounts per the CURRENT table rather than restoring frozen bytes (the documented invariant / the documented invariant).
	ListMounts(ctx context.Context, sandbox model.SandboxID) ([]model.Mount, error)

	// ListMountsByVolume returns every mount row referencing one volume, in ANY
	// sandbox and regardless of that sandbox's state.
	//
	// The orphan sweep needs it as a live-mount check INDEPENDENT of any FK (the documented invariant
	// safety requirement): skip any volume appearing in ANY mount row, live or not. FK
	// RESTRICT protects the ROW; this protects the BYTES, which is a different thing
	// and the one a mis-sweep destroys.
	ListMountsByVolume(ctx context.Context, volume model.VolumeID) ([]model.Mount, error)

	// DeleteMounts removes ALL of one sandbox's mount rows. This is the LEASE RELEASE
	// POINT: the lease is the row's existence, so the row going away IS the release
	// (the documented invariant — there is no lease object to release separately).
	//
	// Idempotent (no rows → nil): destroy, unwind and evict all reach it.
	//
	// SEPARATE FROM ReplaceMounts: every caller is a TEARDOWN (destroy, create's unwind, evict,
	// reconcile's cleanup) with no "next" binding, so `ReplaceMounts(id, nil)` would drag in
	// endpoint-existence verdicts a teardown must not care about. Conversely a rebind must NEVER
	// be this call plus CreateMounts — the two-commit shape ReplaceMounts forbids.
	//
	// CALLERS MUST CALL THIS BEFORE DeleteSandbox, and the order is required (measured): RESTRICT
	// fires IMMEDIATELY, so wrapping both deletes in one transaction buys no order-independence.
	// `defer_foreign_keys` is REJECTED rather than untried — it does make the order irrelevant,
	// but the violation then surfaces at Commit(), and when Go's Tx.Commit() returns an error
	// SQLite has NOT rolled back: on the pinned single connection the transaction stays open and
	// subsequent pooled queries run INSIDE it, seeing uncommitted state no other handle can see.
	DeleteMounts(ctx context.Context, sandbox model.SandboxID) error
}
