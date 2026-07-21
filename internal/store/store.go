// Package store is the control-plane record seam (§1.1 / §4.4-C). It is a
// Behavior seam (§1.3): an interface with pluggable implementations. M1 ships
// an in-memory implementation; M2 swaps in an embedded single-node backend
// (SQLite, ADR-0004) WITHOUT changing this interface — that is the whole point of
// a seam (register an implementation, don't reshape the callers).
//
// The interface itself must promise the minimal guarantees the algorithms need
// (§4.4-C): "lifecycle-state change + outbox append = single atomic commit" is
// an M2 addition, but the CAS shape below is fixed in M1 so the §4.4-B transition
// lock has somewhere to land. NEVER design the API so a backend reports whether
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
	CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event) error

	// DeleteSandbox removes a sandbox record (terminal `destroyed`, §4.1),
	// appending ev in the same commit if non-nil (§4.4-C). The outbox row
	// survives the record's deletion — it is a separate table/slice.
	DeleteSandbox(ctx context.Context, id model.SandboxID, ev *Event) error

	// UnpublishedEvents returns outbox rows not yet marked published, oldest
	// first. This is the PUBLISHER read side (§4.4-C) — it is deliberately NOT
	// part of the atomic-commit contract (not in the transition commit); the
	// publisher reads async, delivers, then marks. at-least-once: a crash after
	// deliver but before MarkPublished re-returns the same rows (B2).
	UnpublishedEvents(ctx context.Context) ([]Event, error)

	// MarkPublished marks one outbox row (by event ID) as delivered so it stops
	// coming back from UnpublishedEvents. Idempotent: marking an unknown or
	// already-marked ID is a no-op.
	MarkPublished(ctx context.Context, eventID string) error
}
