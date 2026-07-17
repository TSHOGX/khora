// Package lifecycle is the §4 mediator — the coordinator ABOVE the five
// subsystems, not a sixth peer. It owns the state machine and, per state,
// CALLS each subsystem's mechanism (§4 开头: 动词归 §4,机制归 §5). Subsystems
// never call each other — that is the start of a dependency cycle. Any
// "Rootfs pokes Runtime on resume" is rejected in review.
//
// The verbs create / pause / resume / destroy are here. Each is a serialized,
// CAS-guarded transition (§4.4-B: one transition lock per sandbox, verbs
// serialize on it, and inside the lock a compare-and-set precondition check
// rejects a verb whose source state is wrong). M1 wires the lock + CAS shape
// even though M1 is largely single-threaded, so M2 fills in real concurrency
// and the outbox (§4.4-C) without reshaping callers.
package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"engine/internal/model"
	"engine/internal/rootfs"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
)

// Deps are the mechanisms the mediator orchestrates. It holds the seams, not
// concrete implementations — swapping local_runsc / SQLite is registering an
// implementation (§1.3), not editing this file.
type Deps struct {
	Store   store.Store
	Runtime runtime.Provider
	Rootfs  rootfs.Provider
	// Dial builds a stateless transport client for a discovered endpoint
	// (§5.E: constructed per endpoint, discarded on resume).
	Dial func(transport.Endpoint) transport.Client

	// HealthTimeout bounds the ⑦ readiness gate (guest-runtime D5.1).
	HealthTimeout time.Duration
}

// Mediator holds per-sandbox transition locks (§4.4-B).
type Mediator struct {
	deps  Deps
	mu    sync.Mutex // guards locks map only
	locks map[model.SandboxID]*sync.Mutex

	// placements holds live placement handles per sandbox. In-memory and
	// non-authoritative: it is placement-local state (§3.3, rebound every
	// resume), NOT persistent SoT. M2's crash reconciliation (§4.4-A) rebuilds
	// the equivalent by scanning host resources against the authoritative store.
	placements map[model.SandboxID]*runtime.Placement
}

func New(deps Deps) *Mediator {
	if deps.HealthTimeout == 0 {
		deps.HealthTimeout = 30 * time.Second
	}
	return &Mediator{
		deps:       deps,
		locks:      make(map[model.SandboxID]*sync.Mutex),
		placements: make(map[model.SandboxID]*runtime.Placement),
	}
}

// lock returns the per-sandbox transition lock, creating it on first use
// (§4.4-B: all lifecycle verbs for the SAME sandbox serialize on it).
func (m *Mediator) lock(id model.SandboxID) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

// CreateSpec is the create verb input (§6.1 POST /v1/sandboxes). M1 ignores
// network/volumes (M4/M5); they are here to keep the API shape stable (§6.2
// create-time injection is a flow shape, not late-bound).
type CreateSpec struct {
	Template  model.TemplateID
	GuestUser string
}

// Create runs the cold-start chain for a fresh sandbox (guest-runtime D5.1,
// create prefix: build empty Overlay, then ①④⑤⑥⑦). Placeholder body — the
// M1 implementation fills the chain against local_runsc.
func (m *Mediator) Create(ctx context.Context, spec CreateSpec) (model.SandboxID, error) {
	return "", fmt.Errorf("lifecycle.Create: not implemented (M1 in progress)")
}

// Pause = quiesce to a consistent point, then cleanly stop the container
// (§4.2). The Overlay is already on the backend, so pause captures NOTHING;
// stopping the container releases RAM/CPU. CAS-guarded: requires `running`.
func (m *Mediator) Pause(ctx context.Context, id model.SandboxID) error {
	return fmt.Errorf("lifecycle.Pause: not implemented (M1 in progress)")
}

// Resume = cold-start a NEW container on the same Overlay artifact, new netns
// IP (§4.2 / §3.3). Same cold-start chain as create minus the empty-Overlay
// prefix (guest-runtime D5.1). CAS-guarded: requires `paused`. On failure,
// unwind half-built placement and stay `paused` (§4.4-A / D5.2 要点④).
func (m *Mediator) Resume(ctx context.Context, id model.SandboxID) error {
	return fmt.Errorf("lifecycle.Resume: not implemented (M1 in progress)")
}

// Destroy = terminal `destroyed` (§4.1). Transition (mark destroyed) is
// decoupled from byte reclamation (delete Overlay): the transition is the
// control-plane action, byte deletion M1 does synchronously (M2: async refcount
// sweep, §4.3/§4.4). Destroy is idempotent. Valid from running or paused.
func (m *Mediator) Destroy(ctx context.Context, id model.SandboxID) error {
	return fmt.Errorf("lifecycle.Destroy: not implemented (M1 in progress)")
}
