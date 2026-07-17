// Package store is the control-plane record seam (§1.1 / §4.4-C). It is a
// Behavior seam (§1.3): an interface with pluggable implementations. M1 ships
// an in-memory implementation; M2 swaps in an embedded single-node backend
// (SQLite / bbolt) WITHOUT changing this interface — that is the whole point of
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

	"engine/internal/model"
)

// ErrConflict is returned by CompareAndSetState when the current state is not
// the expected source state — the CAS lost (§4.4-B: pause vs resume racing, the
// later verb sees a state it didn't expect and is rejected).
var ErrConflict = errors.New("store: state precondition failed")

// ErrNotFound is returned when a sandbox record does not exist.
var ErrNotFound = errors.New("store: sandbox not found")

// Store is the authoritative control-plane record backend.
type Store interface {
	// CreateSandbox inserts a new sandbox record in state `creating`.
	CreateSandbox(ctx context.Context, s model.Sandbox) error

	// GetSandbox reads a sandbox record.
	GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error)

	// ListSandboxes returns all sandbox records.
	ListSandboxes(ctx context.Context) ([]model.Sandbox, error)

	// CompareAndSetState is the conditional write (§4.4-B): set the sandbox's
	// lifecycle-state to `next` only if it is currently `expected`, else return
	// ErrConflict. This is the mechanism the §4 mediator's transition lock uses
	// for CAS precondition checks; serialization happens in the mediator layer,
	// not by one subsystem calling another (§4 开头).
	CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle) error

	// DeleteSandbox removes a sandbox record (terminal `destroyed`, §4.1).
	DeleteSandbox(ctx context.Context, id model.SandboxID) error
}
