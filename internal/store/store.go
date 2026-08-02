// Package store is the control-plane record seam (§1.1 / §4.4-C). It is a
// Behavior seam (§1.3): an interface with pluggable implementations. Two live
// side by side behind it — an in-memory map (for fast unit tests) and the
// embedded single-node backend (SQLite, ADR-0004) — chosen by registration,
// not by reshaping callers; that is the whole point of a seam.
//
// The interface itself promises the minimal guarantees the algorithms need
// (§4.4-C): "lifecycle-state change + outbox append = single atomic commit".
// The CAS shape below gives the §4.4-B transition lock somewhere to land.
// NEVER design the API so a backend reports whether
// it supports transactions and the algorithm branches on it — that leaks
// backend differences into correctness (§4.4-C).
package store

import (
	"context"
	"errors"
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
// 引用-回收原子性: DELETE committed first → the referrer hits missing-parent).
var ErrTemplateNotFound = errors.New("store: template not found")

// ErrInUse is the §4.3 reference-protection verdict: a DeleteTemplate lost to a
// live referencing row (a sandbox pinning it, or a child Template naming it as
// parent). It is the store SEAM's contract promise (§4.3 INV-1 / §1.3), NOT a
// SQLite feature leaking out: SQLite delivers it via FK RESTRICT, Memory via a
// Go scan — the two backends run one set of assertions (§4.4-C 铁律). The API
// layer maps it to a clean 409 template in use (§6.1), never a raw 500.
var ErrInUse = errors.New("store: template in use")

// Event is one outbox row: a §4.1 lifecycle transition to publish to the event
// stream (created / paused / resumed / destroyed …). It is appended in the SAME
// atomic commit as the state change that produced it (§4.4-C) — a committed
// transition can never lack its event, nor an event lack its transition. The
// transition methods below take an *Event: the §4 mediator constructs it per
// transition semantics, carrying a transition-unique ID (§4.4-C). A nil Event
// means "this store mutation publishes nothing" (the transient creating-insert
// and the create-unwind delete are internal steps of a verb, not externally
// visible §4.1 transitions) — that is a mediator semantic choice, NOT a backend
// capability branch (§4.4-C 铁律).
type Event struct {
	// ID is the transition-unique key subscribers dedup on (§4.4-C
	// at-least-once: a redelivered event carries the SAME ID). Backends use it
	// as the outbox row's primary key.
	ID string
	// Sandbox is which sandbox transitioned. Fine as an engine-internal field —
	// `sandbox` is a core atom, not a banned product concept (§1.1 vocabulary).
	Sandbox model.SandboxID
	// Kind is the transition name ("created" / "paused" / "resumed" /
	// "destroyed" / "auto-paused" / "crash-paused" …). The three pause-like Kinds
	// are distinct on purpose: "paused" = user pause, "auto-paused" = reaper idle
	// pause, "crash-paused" = involuntary demote of a dead unit at reconcile
	// (§4.1 / §4.2) — all land state=paused, the Kind carries WHY. Free-form here;
	// the external event schema is a subscriber-side concern (§4.4-C: engine only
	// guarantees reliable write to a replayable stream).
	Kind string
	// State is the resulting lifecycle-state after the transition.
	State model.Lifecycle
	// OccurredAt is when the mediator constructed the transition.
	OccurredAt time.Time
}

// Store is the authoritative control-plane record backend (§1.1 / §4.4-C). It is
// a Behavior seam (§1.3): the transition methods PROMISE "lifecycle-state change
// + outbox append = single atomic commit" and each backend DELIVERS it — SQLite
// via BEGIN/COMMIT, Memory via a single mutex over map+slice. NEVER add a method
// that reports whether the backend supports transactions so the algorithm can
// branch on it — that leaks backend differences into correctness (§4.4-C 铁律).
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
	// by one subsystem calling another (§4 开头).
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

	// UnpublishedEvents returns outbox rows not yet marked published, oldest
	// first. This is the PUBLISHER read side (§4.4-C) — it is deliberately NOT
	// part of the atomic-commit contract (not in the transition commit); the
	// publisher reads async, delivers, then marks. at-least-once: a crash after
	// deliver but before MarkPublished re-returns the same rows.
	UnpublishedEvents(ctx context.Context) ([]Event, error)

	// MarkPublished marks one outbox row (by event ID) as delivered so it stops
	// coming back from UnpublishedEvents. Idempotent: marking an unknown or
	// already-marked ID is a no-op.
	MarkPublished(ctx context.Context, eventID string) error

	// --- Template CRUD (§4.3 uniform object model / M5) ---------------------
	//
	// Template is a first-class persistent object with DERIVED refcount and
	// existence-is-state (§4.3): no state column, no materialized count. These
	// methods are the store's slice of the §4.3 引用-回收原子性 contract — the
	// reference-protection promise (a referenced Template cannot be deleted) is
	// delivered here, identically by both backends (§1.3 seam).

	// CreateTemplate inserts a Template row. If Parent is non-empty it must
	// reference an existing Template (lineage, §4.3 INV-2 引用向上): a missing
	// parent → ErrTemplateNotFound (the reference-side of the atomicity race,
	// §4.3). Returns ErrConflict on a duplicate ID. Bytes-first (§5.B): the
	// caller has already fully materialized the layer bytes before this insert —
	// existence-is-state has no not-ready escape hatch (§4.3 INV-3).
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
}
