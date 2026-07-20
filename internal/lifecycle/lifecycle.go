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
	"crypto/rand"
	"encoding/hex"
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

	// placements + merged hold the live placement handle and mounted merged
	// rootfs per sandbox. In-memory and non-authoritative: placement-local state
	// (§3.3, rebound every resume), NOT persistent SoT. M2's crash reconciliation
	// (§4.4-A) rebuilds the equivalent by scanning host resources against the
	// authoritative store.
	live map[model.SandboxID]*liveState
}

// liveState is the mediator's in-memory handle to a running sandbox's
// placement-local resources (§3.3): the runtime placement + the mounted merged
// overlay. Discarded on pause, rebuilt on resume.
type liveState struct {
	placement *runtime.Placement
	merged    *rootfs.Merged
}

func New(deps Deps) *Mediator {
	if deps.HealthTimeout == 0 {
		deps.HealthTimeout = 30 * time.Second
	}
	return &Mediator{
		deps:  deps,
		locks: make(map[model.SandboxID]*sync.Mutex),
		live:  make(map[model.SandboxID]*liveState),
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
// create prefix: build empty Overlay, then ①④⑤⑥⑦). The transition lock is held
// for the whole chain (§4.4-B); on failure the half-built placement + empty
// Overlay are reclaimed and the record deleted → destroyed (§5 create exit).
func (m *Mediator) Create(ctx context.Context, spec CreateSpec) (model.SandboxID, error) {
	sid := model.SandboxID("sbx_" + randToken())
	oid := model.OverlayID("ov_" + randToken())

	l := m.lock(sid)
	l.Lock()
	defer l.Unlock()

	now := time.Now()
	rec := model.Sandbox{
		ID: sid, Template: spec.Template, Overlay: oid,
		State: model.Creating, GuestUser: spec.GuestUser,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.deps.Store.CreateSandbox(ctx, rec); err != nil {
		return "", fmt.Errorf("create: insert record: %w", err)
	}

	// ① build empty Overlay + run the cold-start chain.
	ov, err := m.deps.Rootfs.CreateOverlay(ctx, string(oid), string(spec.Template))
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid) // nothing built yet
		return "", fmt.Errorf("create: overlay: %w", err)
	}
	if err := m.coldStart(ctx, sid, ov, spec.GuestUser); err != nil {
		// create has no fallback state: reclaim half-built placement + empty
		// Overlay, delete the record → destroyed (§4.4-A / D5.2 要点④).
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid)
		return "", fmt.Errorf("create: cold start: %w", err)
	}

	// ⑦ readiness passed → flip creating→running (CAS).
	if err := m.deps.Store.CompareAndSetState(ctx, sid, model.Creating, model.Running); err != nil {
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid)
		return "", fmt.Errorf("create: commit running: %w", err)
	}
	return sid, nil
}

// Pause = quiesce, then cleanly stop the container (§4.2). The Overlay is
// already on the backend, so pause captures NOTHING; stopping releases RAM/CPU
// and the placement (§4.2 只占磁盘). CAS-guarded: requires `running`. Pause
// always lands in a legal `paused` (§4.4: no middle dirty state), so we CAS the
// state first, then release placement-local resources.
func (m *Mediator) Pause(ctx context.Context, id model.SandboxID) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Running, model.Paused); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	// release placement + unmount overlay; upper persists (§4.2 零捕获).
	ls := m.take(id)
	if ls != nil {
		if ls.placement != nil {
			_ = m.deps.Runtime.Stop(ctx, ls.placement)
		}
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	return nil
}

// Resume = cold-start a NEW container on the same Overlay artifact, new netns
// IP (§4.2 / §3.3). Same cold-start chain as create minus the empty-Overlay
// prefix (guest-runtime D5.1). CAS precondition: requires `paused`. The lock is
// held for the whole chain (§4.4-B) — the flip to `running` happens only at ⑦.
// On failure, unwind half-built placement and stay `paused` (§4.4-A / D5.2 ④).
func (m *Mediator) Resume(ctx context.Context, id model.SandboxID) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	// CAS precondition check (read side): must be paused. The actual state flip
	// is deferred to ⑦; the lock (not the CAS) holds off concurrent verbs for
	// the whole cold-start window (§4.4-B).
	if rec.State != model.Paused {
		return fmt.Errorf("resume: %w (state=%s, want paused)", store.ErrConflict, rec.State)
	}

	// ① re-open + re-mount the SAME Overlay artifact (resume prefix).
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), string(rec.Template))
	if err != nil {
		return fmt.Errorf("resume: open overlay: %w", err)
	}
	if err := m.coldStart(ctx, id, ov, rec.GuestUser); err != nil {
		// resume has a fallback state: reclaim half-built placement, stay paused
		// (persistent Overlay artifact untouched; §4.4-A).
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: cold start: %w", err)
	}

	// ⑦ readiness passed → flip paused→running (CAS).
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Paused, model.Running); err != nil {
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: commit running: %w", err)
	}
	return nil
}

// Destroy = terminal `destroyed` (§4.1). The transition (mark destroyed) is
// decoupled from byte reclamation: M1 deletes the Overlay synchronously (M2:
// async refcount sweep, §4.3/§4.4). Valid from running or paused; idempotent
// (already-gone record → nil). The lock held here is the same one resume holds
// for its whole chain, so destroy CANNOT delete an Overlay mid-cold-start
// (§4.4-B: closes the destroy-vs-resume data race).
func (m *Mediator) Destroy(ctx context.Context, id model.SandboxID) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			return nil // idempotent
		}
		return fmt.Errorf("destroy: %w", err)
	}

	// tear down any live placement + mount (running case).
	ls := m.take(id)
	if ls != nil {
		if ls.placement != nil {
			_ = m.deps.Runtime.Destroy(ctx, ls.placement)
		}
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	// reclaim the Overlay artifact (byte deletion; §4.1 destroy).
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), string(rec.Template))
	if err == nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
	// delete the record → destroyed (M1 removes the row; no separate row).
	if err := m.deps.Store.DeleteSandbox(ctx, id); err != nil {
		return fmt.Errorf("destroy: delete record: %w", err)
	}
	return nil
}

// coldStart runs ①(mount)④(launch)⑤⑥⑦(health) for a given Overlay handle,
// shared by create and resume (guest-runtime D5.1). On success the live
// placement + merged are recorded in the mediator. On any error it returns
// without recording (the caller unwinds).
func (m *Mediator) coldStart(ctx context.Context, id model.SandboxID, ov *rootfs.Overlay, guestUser string) error {
	// ① mount lower(Template)/upper/work → merged.
	merged, err := m.deps.Rootfs.Mount(ctx, ov)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	// ④ launch runsc (sandboxd entrypoint), ⑤ guest boots, ⑥ IP discovered.
	pl, err := m.deps.Runtime.Launch(ctx, runtime.LaunchSpec{
		Sandbox:   string(id),
		Rootfs:    runtime.RootfsSource{MergedPath: merged.MergedPath},
		GuestUser: guestUser,
	})
	if err != nil {
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return fmt.Errorf("launch: %w", err)
	}
	// ⑦ readiness gate: poll Health RPC to first success (not bare TCP accept;
	// guest-runtime D5.2 要点①).
	if err := m.awaitHealthy(ctx, pl); err != nil {
		_ = m.deps.Runtime.Destroy(ctx, pl)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return fmt.Errorf("health gate: %w", err)
	}
	m.put(id, &liveState{placement: pl, merged: merged})
	return nil
}

// awaitHealthy polls the transport Health RPC until first success or timeout
// (⑦; the single readiness barrier, plan §5).
func (m *Mediator) awaitHealthy(ctx context.Context, pl *runtime.Placement) error {
	deadline := time.Now().Add(m.deps.HealthTimeout)
	cl := m.deps.Dial(transport.Endpoint(pl.TransportEndpoint))
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		err := cl.Health(hctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("readiness timeout after %s: %w", m.deps.HealthTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// unwind reclaims a half-built placement (§4.4-A: failure recycles placement,
// persistent Overlay artifact untouched). isCreate additionally destroys the
// just-built empty Overlay (create has no fallback state; resume keeps it).
func (m *Mediator) unwind(ctx context.Context, id model.SandboxID, ov *rootfs.Overlay, isCreate bool) {
	if ls := m.take(id); ls != nil {
		if ls.placement != nil {
			_ = m.deps.Runtime.Destroy(ctx, ls.placement)
		}
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	if isCreate && ov != nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
}

// put/take/peek manage the in-memory liveState map.
func (m *Mediator) put(id model.SandboxID, ls *liveState) {
	m.mu.Lock()
	m.live[id] = ls
	m.mu.Unlock()
}

func (m *Mediator) take(id model.SandboxID) *liveState {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.live[id]
	delete(m.live, id)
	return ls
}

// randToken returns a short opaque hex token for sandbox/overlay IDs. Opaque =
// not derived from placement (§3.3) and safe as a single path segment (§5.B
// validation rejects separators).
func randToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
