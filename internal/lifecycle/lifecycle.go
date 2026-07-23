// Package lifecycle is the §4 mediator — the coordinator ABOVE the five
// subsystems, not a sixth peer. It owns the state machine and, per state,
// CALLS each subsystem's mechanism (§4 开头: 动词归 §4,机制归 §5). Subsystems
// never call each other — that is the start of a dependency cycle. Any
// "Rootfs pokes Runtime on resume" is rejected in review.
//
// The verbs create / pause / resume / destroy are here. Each is a serialized,
// CAS-guarded transition (§4.4-B: one transition lock per sandbox, verbs
// serialize on it, and inside the lock a compare-and-set precondition check
// rejects a verb whose source state is wrong). Every transition commits its
// state change and its outbox event in one atomic store op (§4.4-C).
package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
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

	// DefaultIdleTimeout / DefaultRetention are the engine-wide TTL defaults
	// (§4.3 / §1.1 single-binary config) applied when a CreateSpec omits them.
	// Conservative numeric knobs (reaper-health D5): they bound no invariant's
	// shape, only detection latency, and stay tunable. Zero here falls back to
	// the package defaults (defaultIdleTimeout / defaultRetention) so a caller
	// that leaves Deps unset still gets a sane, non-zero TTL rather than a
	// sandbox that auto-pauses instantly.
	DefaultIdleTimeout time.Duration
	DefaultRetention   time.Duration

	// Logf reports per-item reconciliation outcomes that are skipped rather than
	// fatal (§4.4-A startup reconcile is skip-and-log: one un-adoptable sandbox
	// must not abort recovery of the rest). Defaults to log.Printf; tests inject a
	// capturing sink.
	Logf func(format string, args ...any)

	// FaultAfterColdStart is a TEST-ONLY injection hook: if non-nil it runs
	// right after coldStart records the live placement but BEFORE the committing
	// CAS to `running`. A test panics/exits from it to simulate the orchestrator
	// crashing mid-transition — leaving a fully-built placement while the store
	// still reads the SOURCE state (`paused` for resume / `creating` for create).
	// This manufactures the §4.4-A crash scenarios (orphan half-build + surviving
	// container with lost handle) that Reconcile must clean up. nil in production;
	// it carries NO production semantics.
	FaultAfterColdStart func(id model.SandboxID)
}

// Mediator holds per-sandbox transition locks (§4.4-B).
type Mediator struct {
	deps  Deps
	mu    sync.Mutex // guards locks map only
	locks map[model.SandboxID]*sync.Mutex

	// live holds the per-sandbox liveState (placement handle + mounted merged
	// rootfs). In-memory and non-authoritative: placement-local state (§3.3,
	// rebound every resume), NOT persistent SoT. Crash reconciliation (§4.4-A)
	// rebuilds the equivalent by scanning host resources against the authoritative
	// store.
	live map[model.SandboxID]*liveState

	// buildingLayers is the in-flight-Promote keep-set: TemplateIDs whose
	// layer bytes are being written to the backend but whose `templates` row has
	// not been inserted yet (bytes-first, §5.B). The orphan-layer sweep must skip
	// these — "bytes on disk, no row" is a Promote in progress, not an orphan
	// (the placement live-map's twin, §4.4-A). Empty until Promote lands (P2); a
	// crashed Promote's staging layer is correctly absent here at boot, so it is
	// reclaimed as a true orphan. Guarded by mu.
	buildingLayers map[model.TemplateID]bool
}

// liveState is the mediator's in-memory handle to a running sandbox's
// placement-local resources (§3.3): the runtime placement + the mounted merged
// overlay. Discarded on pause, rebuilt on resume.
type liveState struct {
	placement *runtime.Placement
	merged    *rootfs.Merged
}

// TTL defaults (§4.3 / reaper-health D5): conservative, revisable numeric knobs
// noted at their config site. They bound no invariant's shape — only how long
// an idle `running` / a `paused` sandbox lingers before the reaper acts. A
// create that omits them, and a Deps that leaves the Default* fields zero, both
// resolve to these.
const (
	defaultIdleTimeout = 30 * time.Minute // running idle → auto-pause
	defaultRetention   = 24 * time.Hour   // paused → evict
)

func New(deps Deps) *Mediator {
	if deps.HealthTimeout == 0 {
		deps.HealthTimeout = 30 * time.Second
	}
	if deps.DefaultIdleTimeout == 0 {
		deps.DefaultIdleTimeout = defaultIdleTimeout
	}
	if deps.DefaultRetention == 0 {
		deps.DefaultRetention = defaultRetention
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return &Mediator{
		deps:           deps,
		locks:          make(map[model.SandboxID]*sync.Mutex),
		live:           make(map[model.SandboxID]*liveState),
		buildingLayers: make(map[model.TemplateID]bool),
	}
}

// lock returns the per-sandbox transition mutex, creating it on first use
// (§4.4-B: all lifecycle verbs for the SAME sandbox serialize on it).
//
// KNOWN LIMITATION: entries are never removed, even after destroy — the map
// grows by one mutex per distinct SandboxID ever seen (a slow leak under high
// churn). Not a correctness bug; deleting on destroy races a verb blocked on
// acquiring the same lock (it would let a blocked verb and a fresh verb serialize
// on DIFFERENT mutexes), so it needs care. Tracked in open-decisions §4.
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

// tryLock attempts to acquire a sandbox's transition lock WITHOUT blocking
// (§4.4-B). Returns (mutex, true) with the lock HELD on success — the caller
// must Unlock it. Returns (nil, false) if a verb already holds it: for the
// reaper and periodic reconcile, a held lock means "a verb is driving this
// sandbox (in-flight resume / user pause / …)", so we SKIP it this round rather
// than misclassify it (reaper-health D3: an in-flight resume has L2 down and
// would classify dead-recoverable; skipping is the single-flight guard, adding
// no §4.1 state). This is the ONLY non-blocking acquire; every user verb takes
// the lock blocking.
func (m *Mediator) tryLock(id model.SandboxID) (*sync.Mutex, bool) {
	l := m.lock(id)
	if l.TryLock() {
		return l, true
	}
	return nil, false
}

// CreateSpec is the create verb input (§6.1 POST /v1/sandboxes). Network/volume
// Binding inputs are deliberately OMITTED until those subsystems land — omitted,
// not stubbed (same rule as model.Sandbox; §1.3 no speculative reservation). The
// §6.2 create-time injection flow shape holds regardless of when they arrive.
type CreateSpec struct {
	Template model.TemplateID

	// IdleTimeout / Retention are the OPTIONAL TTL Binding inputs (§3.4 / §4.3;
	// §6.1 POST /v1/sandboxes idle_timeout?/retention?). nil = omitted → the
	// engine-wide default (Deps.Default*) is materialized into the stored record
	// at create time, so the reaper always reads a concrete duration, never a
	// sentinel. A non-nil pointer is honored verbatim (including an explicit
	// zero, which the reaper treats as "no such deadline" — see the reaper's
	// TTL handling).
	IdleTimeout *time.Duration
	Retention   *time.Duration
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
		State: model.Creating,
		// Materialize the TTL Binding once, at create (§3.4: Binding is stable
		// across resume; the stored value is the concrete duration the reaper
		// reads). Omitted inputs resolve to the engine-wide default here.
		IdleTimeout: m.deps.DefaultIdleTimeout,
		Retention:   m.deps.DefaultRetention,
		CreatedAt:   now, UpdatedAt: now,
	}
	if spec.IdleTimeout != nil {
		rec.IdleTimeout = *spec.IdleTimeout
	}
	if spec.Retention != nil {
		rec.Retention = *spec.Retention
	}
	// The transient creating-insert is an internal step of the create verb, not
	// an externally-visible §4.1 transition — it publishes nothing (nil Event).
	// The "created" event is emitted only when create COMPLETES → running below.
	if err := m.deps.Store.CreateSandbox(ctx, rec, nil); err != nil {
		return "", fmt.Errorf("create: insert record: %w", err)
	}

	// ① build empty Overlay over the resolved lineage lower stack (§5.B
	// multi-lowerdir) + run the cold-start chain.
	lower, err := m.resolveLower(ctx, spec.Template)
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: resolve lineage: %w", err)
	}
	ov, err := m.deps.Rootfs.CreateOverlay(ctx, string(oid), lower)
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: overlay: %w", err)
	}
	if err := m.coldStart(ctx, sid, ov); err != nil {
		// create has no fallback state: reclaim half-built placement + empty
		// Overlay, delete the record → destroyed (§4.4-A / D5.2 要点④). The
		// sandbox was never running, so this unwind-delete publishes nothing.
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
		return "", fmt.Errorf("create: cold start: %w", err)
	}

	// fault injection: simulate a crash after the placement is fully built but
	// before the committing CAS — store still reads `creating` (§4.4-A create var).
	if m.deps.FaultAfterColdStart != nil {
		m.deps.FaultAfterColdStart(sid)
	}

	// ⑦ readiness passed → flip creating→running (CAS) + emit "created" in the
	// same commit (§4.4-C).
	if err := m.deps.Store.CompareAndSetState(ctx, sid, model.Creating, model.Running, newEvent(sid, "created", model.Running)); err != nil {
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
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
	if err := m.pauseLocked(ctx, id, "paused"); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	return nil
}

// pauseLocked commits a running→paused transition with the given event Kind and
// releases the sandbox's placement. CALLER MUST HOLD the transition lock
// (§4.4-B). Shared by the user Pause verb ("paused") and the reaper's
// auto-pause ("auto-paused") / crash-pause ("crash-paused"): identical state
// transition + placement release, the Kind carries WHY (§4.1: three pause-like
// Kinds, all state=paused). Pause always lands a legal `paused` (§4.4: no middle
// dirty state), so we CAS the state first, then release placement-local
// resources; the Overlay artifact is already on the backend and captured NOTHING
// (§4.2 零捕获). Stop is best-effort and idempotent, so it is also correct for a
// crash-pause whose container is already dead (it reclaims the lingering netns).
func (m *Mediator) pauseLocked(ctx context.Context, id model.SandboxID, kind string) error {
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Running, model.Paused, newEvent(id, kind, model.Paused)); err != nil {
		return err
	}
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
	return m.resumeLocked(ctx, rec)
}

// resumeLocked cold-starts a NEW container on the sandbox's Overlay (new IP,
// §3.3) and flips paused→running at ⑦. CALLER MUST HOLD the transition lock and
// have verified the source state is `paused` (§4.4-B). Shared by the user Resume
// verb and the reaper's transport-down crash-loop escalation (D4b: after
// crash-pause, the same lock resumes to break the wedge). On failure it unwinds
// the half-built placement and leaves the record `paused` (§4.4-A / D5.2 ④).
func (m *Mediator) resumeLocked(ctx context.Context, rec model.Sandbox) error {
	id := rec.ID
	// ① re-open + re-mount the SAME Overlay artifact (resume prefix), re-resolving
	// the lineage lower stack from the store (§5.B multi-lowerdir).
	lower, err := m.resolveLower(ctx, rec.Template)
	if err != nil {
		return fmt.Errorf("resume: resolve lineage: %w", err)
	}
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), lower)
	if err != nil {
		return fmt.Errorf("resume: open overlay: %w", err)
	}
	if err := m.coldStart(ctx, id, ov); err != nil {
		// resume has a fallback state: reclaim half-built placement, stay paused
		// (persistent Overlay artifact untouched; §4.4-A).
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: cold start: %w", err)
	}

	// fault injection: simulate a crash after the placement is fully built but
	// before the committing CAS — store still reads `paused` (§4.4-A resume var).
	if m.deps.FaultAfterColdStart != nil {
		m.deps.FaultAfterColdStart(id)
	}

	// ⑦ readiness passed → flip paused→running (CAS) + emit "resumed" (§4.4-C).
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Paused, model.Running, newEvent(id, "resumed", model.Running)); err != nil {
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: commit running: %w", err)
	}
	return nil
}

// Destroy = terminal `destroyed` (§4.1). The transition (mark destroyed) is
// decoupled from byte reclamation: the Overlay is deleted synchronously here;
// an async refcount sweep (§4.3/§4.4) is the eventual form. Valid from running
// or paused; idempotent (already-gone record → nil). The lock held here is the
// same one resume holds
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
	return m.destroyLocked(ctx, rec)
}

// destroyLocked marks a sandbox `destroyed` and reclaims its bytes + placement.
// CALLER MUST HOLD the transition lock (§4.4-B). Shared by the user Destroy verb
// (valid from running or paused) and the reaper's evict (a paused sandbox past
// retention). The transition (mark destroyed) is decoupled from byte
// reclamation: the Overlay is deleted synchronously here; an async refcount
// sweep (§4.3/§4.4) is the eventual form. Emitting Kind "destroyed" matches
// §4.1 — evict is "the reaper's automatic destroy", the SAME terminal, not a
// distinct Kind. Because the lock is held for the whole chain, destroy/evict
// CANNOT delete an Overlay mid-cold-start (§4.4-B: destroy-vs-resume race).
//
// NOTE — no source-state CAS here (unlike pause/resume). destroy is deliberately
// valid from BOTH `running` and `paused` (§4.1 / §4.4-A decouples it) and is
// idempotent, so there is no single "expected" source state to compare-and-set
// against. The invariant is LOCK-enforced, not CAS-enforced: the held transition
// lock serializes destroy against every other verb, and a `creating` record is
// never observable here because Create holds the lock for its whole chain. The
// terminal DeleteSandbox is idempotent (ErrNotFound → already gone).
func (m *Mediator) destroyLocked(ctx context.Context, rec model.Sandbox) error {
	id := rec.ID
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
	// reclaim the Overlay artifact (byte deletion; §4.1 destroy). Byte reclaim
	// uses only the OverlayID (DestroyOverlay derives StateDir/<id>); the lower
	// stack is irrelevant here (no mount), so a single-element stack suffices and
	// avoids a lineage walk that could fail mid-teardown.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)})
	if err == nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
	// delete the record → destroyed + emit "destroyed" in the same commit
	// (§4.4-C). The outbox row outlives the deleted record.
	if err := m.deps.Store.DeleteSandbox(ctx, id, newEvent(id, "destroyed", model.Destroyed)); err != nil {
		return fmt.Errorf("destroy: delete record: %w", err)
	}
	return nil
}

// Promote freezes a sandbox's current Overlay into a new Template layer (§3.1 /
// §5.B). It accepts a `running` or `paused` source; a `running` source is
// first quiesced to a consistent at-rest point via pauseLocked (equivalent to a
// pause, §4.2) and LEFT paused (the caller resumes on demand). The whole
// operation holds the source's transition lock (§4.4-B), so it serializes
// against concurrent destroy/resume — destroy cannot delete the Overlay bytes
// mid-freeze.
//
// bytes-first (§4.3 INV-3 / §5.B): the new layer bytes are written FIRST
// (PromoteToTemplate), then the `templates` row is inserted. The layer id is
// registered in buildingLayers for the freeze window so a concurrent periodic
// sweep does not reclaim the in-flight layer (§4.4-A). A crash after the bytes are
// written but before the row insert leaves an orphan layer the sweep reclaims —
// never a row pointing at nonexistent bytes.
//
// Returns the new TemplateID. The new Template's parent is the source sandbox's
// current Template, so lineage is the source's stack + this frozen layer on top
// (§5.B multi-lowerdir).
func (m *Mediator) Promote(ctx context.Context, id model.SandboxID) (model.TemplateID, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return "", fmt.Errorf("promote: %w", err)
	}
	switch rec.State {
	case model.Running:
		// quiesce to a consistent at-rest point (equivalent to pause, §4.2) and
		// stay paused. Uses the "paused" Kind — from the event stream's view this
		// is an ordinary pause; Promote is not a §4.1 lifecycle transition.
		if err := m.pauseLocked(ctx, id, "paused"); err != nil {
			return "", fmt.Errorf("promote: quiesce: %w", err)
		}
	case model.Paused:
		// already at-rest; the placement is already released.
	default:
		return "", fmt.Errorf("promote: %w (state=%s, want running or paused)", store.ErrConflict, rec.State)
	}

	newID := model.TemplateID("tmpl_" + randToken())

	// register the in-flight layer BEFORE writing bytes so a concurrent sweep
	// protects it (§4.4-A). Unregister in all exit paths.
	m.markBuilding(newID)
	defer m.unmarkBuilding(newID)

	// open the source Overlay by ID (byte read only, no mount) and freeze its
	// at-rest upper into the new layer dir (bytes-first).
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)})
	if err != nil {
		return "", fmt.Errorf("promote: open overlay: %w", err)
	}
	if err := m.deps.Rootfs.PromoteToTemplate(ctx, ov, string(newID)); err != nil {
		return "", fmt.Errorf("promote: freeze layer: %w", err)
	}

	// bytes are down → NOW insert the templates row (parent = source's Template,
	// builtin=0). If this fails, the layer bytes are an orphan the sweep reclaims
	// (bytes-first: no broken ready row).
	if err := m.deps.Store.CreateTemplate(ctx, model.Template{
		ID:        newID,
		Parent:    rec.Template,
		Builtin:   false,
		CreatedAt: time.Now(),
	}); err != nil {
		return "", fmt.Errorf("promote: insert template row: %w", err)
	}
	return newID, nil
}

// markBuilding / unmarkBuilding manage the §4.4-A in-flight-Promote keep-set.
func (m *Mediator) markBuilding(id model.TemplateID) {
	m.mu.Lock()
	m.buildingLayers[id] = true
	m.mu.Unlock()
}

func (m *Mediator) unmarkBuilding(id model.TemplateID) {
	m.mu.Lock()
	delete(m.buildingLayers, id)
	m.mu.Unlock()
}

// coldStart runs ①(mount)④(launch)⑤⑥⑦(health) for a given Overlay handle,
// shared by create and resume (guest-runtime D5.1). On success the live
// placement + merged are recorded in the mediator. On any error it returns
// without recording (the caller unwinds).
func (m *Mediator) coldStart(ctx context.Context, id model.SandboxID, ov *rootfs.Overlay) error {
	// ① mount lower(Template)/upper/work → merged.
	merged, err := m.deps.Rootfs.Mount(ctx, ov)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	// ④ launch runsc (sandboxd entrypoint), ⑤ guest boots, ⑥ IP discovered.
	pl, err := m.deps.Runtime.Launch(ctx, runtime.LaunchSpec{
		Sandbox: string(id),
		Rootfs:  runtime.RootfsSource{MergedPath: merged.MergedPath},
	})
	if err != nil {
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return fmt.Errorf("launch: %w", err)
	}
	// Record the live placement the MOMENT it physically exists — before the
	// readiness gate, not after ⑦. A concurrent periodic reconcile that started
	// after our create/resume was snapshotted would otherwise see this freshly
	// launched netns/container as an orphan (not in `recs`, not yet in the
	// live-map) and reclaim it during the awaitHealthy window. Seeding it here
	// puts it in liveIDs() so the periodic keep-set protects it (§4.4-A). The
	// transition lock is held for the whole chain, so this is not visible to a
	// user verb. On a health-gate failure we take() it back out and tear down.
	m.put(id, &liveState{placement: pl, merged: merged})
	// ⑦ readiness gate: poll Health RPC to first success (not bare TCP accept;
	// guest-runtime D5.2 要点①).
	if err := m.awaitHealthy(ctx, pl); err != nil {
		m.take(id) // un-record before teardown so a racing reconcile sees no live handle
		_ = m.deps.Runtime.Destroy(ctx, pl)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return fmt.Errorf("health gate: %w", err)
	}
	return nil
}

// awaitHealthy polls the transport Health RPC until first success or timeout
// (⑦; the single readiness barrier of the cold-start chain, guest-runtime D5).
func (m *Mediator) awaitHealthy(ctx context.Context, pl *runtime.Placement) error {
	deadline := time.Now().Add(m.deps.HealthTimeout)
	cl := m.deps.Dial(transport.Endpoint(pl.TransportEndpoint))
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := cl.Health(hctx)
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

// SeedBuiltinTemplates is the boot seed: an idempotent reconcile that ensures
// a `templates` row exists (builtin=1, no parent) for every static Template
// directory the deployment ships (Rootfs.ListBuiltinTemplates). It MUST run
// before the engine serves any create — the sandboxes.template FK (§4.3) would
// otherwise reject a create referencing a builtin whose row is absent.
//
// bytes-first is inherent (§5.B / §4.3 INV-3): the layer directory exists on the
// backend before this inserts its row, so there is never a row pointing at
// nonexistent bytes. Idempotent: a row that already exists (ErrConflict) is a
// no-op — re-running across restarts leaves the builtin row count unchanged.
// Failure policy matches §4.4-A startup reconcile —
// skip-and-log a single un-seedable Template rather than aborting boot; only a
// failure to enumerate at all is returned fatal.
func (m *Mediator) SeedBuiltinTemplates(ctx context.Context) error {
	ids, err := m.deps.Rootfs.ListBuiltinTemplates(ctx)
	if err != nil {
		return fmt.Errorf("seed templates: list builtins: %w", err)
	}
	now := time.Now()
	for _, id := range ids {
		err := m.deps.Store.CreateTemplate(ctx, model.Template{
			ID:        model.TemplateID(id),
			Builtin:   true,
			CreatedAt: now,
		})
		if err != nil && err != store.ErrConflict {
			// skip-and-log: one bad builtin must not abort boot; the seed is
			// idempotent so it retries next start (§4.4-A skip-and-log).
			m.deps.Logf("seed templates: skip builtin %s: %v", id, err)
		}
	}
	return nil
}

// SweepOrphanLayers reclaims Promote-built layer bytes with no live `templates`
// row (§5.B / §4.4-A). It is the §5.B twin of ReclaimOrphans: the mediator reads the
// AUTHORITATIVE store for the live layer IDs, snapshots the in-flight-Promote
// keep-set, and hands both to the FS provider — which enumerates its own layer
// dirs and deletes the difference (§4 subsystems don't call the store). Run at
// boot (a crashed Promote's staging layer is a true orphan → reclaimed, the
// buildingLayers set being empty) and periodically by the reaper (an in-flight
// Promote is protected by buildingLayers, §4.4-A). Skip-and-log per §4.4-A: a sweep
// failure is not fatal; it retries next pass.
//
// Only Promote-built (builtin=false) layers are swept: builtins live under the
// deployment-shipped TemplatesDir, a different root the provider never scans, so
// they are structurally out of the candidate set — a builtin row's absence
// could never orphan a builtin dir.
func (m *Mediator) SweepOrphanLayers(ctx context.Context) error {
	tmpls, err := m.deps.Store.ListTemplates(ctx)
	if err != nil {
		return fmt.Errorf("sweep layers: list templates: %w", err)
	}
	live := make([]string, 0, len(tmpls))
	for _, t := range tmpls {
		// A builtin's dir is under TemplatesDir (not LayersDir), so it is not a
		// sweep candidate; only Promote-built layers have a LayersDir dir. Passing
		// only non-builtin IDs keeps `live` tight without affecting correctness
		// (a builtin id would simply never match a LayersDir entry).
		if !t.Builtin {
			live = append(live, string(t.ID))
		}
	}
	building := m.buildingLayerIDs()
	n, err := m.deps.Rootfs.SweepOrphanLayers(ctx, live, building)
	if err != nil {
		return fmt.Errorf("sweep layers: %w", err)
	}
	if n > 0 {
		m.deps.Logf("sweep layers: reclaimed %d orphan layer(s)", n)
	}
	return nil
}

// resolveLower walks the authoritative `templates.parent` lineage from the given
// Template down to its base, returning the read-only lower STACK leaf-first
// (§5.B multi-lowerdir: [leaf, ...ancestors, base]). The mediator owns this walk
// because lineage lives in the store (§4.3) and the FS provider must not read it
// (§4 subsystems don't call the store) — the resolved ID stack crosses the seam,
// host paths stay inside (§3.1.1). A cycle is impossible (lineage is a DAG built
// child→existing-parent, §4.3), but we bound the walk defensively.
func (m *Mediator) resolveLower(ctx context.Context, template model.TemplateID) ([]string, error) {
	lower := make([]string, 0, 4)
	seen := make(map[model.TemplateID]bool)
	for id := template; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("resolve lineage: cycle at %s", id)
		}
		seen[id] = true
		t, err := m.deps.Store.GetTemplate(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve lineage: %s: %w", id, err)
		}
		lower = append(lower, string(id))
		id = t.Parent
	}
	if len(lower) == 0 {
		return nil, fmt.Errorf("resolve lineage: empty for %q", template)
	}
	return lower, nil
}

// buildingLayerIDs snapshots the in-flight-Promote keep-set (§4.4-A). Ordering is
// irrelevant — it is a set to the provider.
func (m *Mediator) buildingLayerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.buildingLayers))
	for id := range m.buildingLayers {
		out = append(out, string(id))
	}
	return out
}

// Reconcile is the §4.4-A startup crash-reconciliation: one local scan diffing
// host placement × store records, run ONCE at orchestrator boot before serving
// (§4.4-A). The process just (re)started, so the live-map is empty and
// all transition locks are fresh — every in-flight resume that was running is
// dead, so half-built placements need NO lock to identify (lock-based
// identification is only for the periodic reconcile against a LIVE
// orchestrator, deferred to open-decisions §2). It is a dual operation, NOT
// reclaim-all:
//
//   - `creating` record → the create crashed mid-chain, never reached running,
//     has no at-rest resting state → delete the record (→ destroyed); its
//     half-built placement is an orphan, reclaimed below.
//   - `paused` record → keep; any half-built placement (crashed mid-resume) is
//     an orphan, reclaimed below. The upper is intact and resumable (§4.2).
//   - `running` record → PROBE liveness via Adopt: alive → rebuild the live-map
//     handle at the SAME IP (not a resume, no new epoch — the isolation unit
//     never restarted, §4.4-A 另一半); dead → CAS-demote to `paused` (§4.2
//     involuntary pause), emitting an event (ties §4.4-A to §4.4-C).
//
// Any placement backing none of the kept sandboxes is an orphan → ReclaimOrphans
// tears it down (reverse-dep order, persistent upper untouched, §4.4-A).
//
// Failure policy is SKIP-AND-LOG, not fail-fast: a single un-reconcilable
// sandbox must not abort recovery of every other one (that would turn one bad
// overlay handle into a boot outage). Only a failure that makes the whole scan
// impossible (listing the records) is fatal. The overriding safety rule when a
// `running` sandbox cannot be cleanly resolved is CONSERVATIVE: protect its
// placement from reclamation (add it to keep) rather than risk tearing down a
// container that may still be live — leaking a placement is recoverable on the
// next boot; killing a live survivor is not. Reconcile is idempotent, so a
// skipped item is simply retried next start.
func (m *Mediator) Reconcile(ctx context.Context) error {
	return m.reconcile(ctx, false)
}

// ReconcilePeriodic runs the §4.4-A reverse-diff against a LIVE orchestrator
// (reaper-health D4). It is the SAME diff as startup Reconcile, plus the two
// additions that only matter when the orchestrator is alive with a populated
// live-map and in-flight transitions:
//
//  1. TryLock-SKIP a `running` sandbox whose transition lock is HELD — a held
//     lock means an in-flight resume / user verb is driving it (§4.4-B). At
//     startup all locks are fresh so this never triggers; live, treating a
//     held (being-built) placement as an orphan would tear down the resource
//     out from under an active resume. Skipped items retry next tick.
//  2. PROTECT live-map placements THIS process built: a `running` sandbox we
//     already hold a live handle for is not an orphan and needs no Adopt
//     re-recognition (that is the crash-recovery path). Add it straight to keep.
//     Only a `running` with NO live handle (never built here → shouldn't happen,
//     or first tick after a crash = startup semantics) runs adopt/demote.
//
// It shares Reconcile's body; the periodic flag selects these two behaviors.
func (m *Mediator) ReconcilePeriodic(ctx context.Context) error {
	return m.reconcile(ctx, true)
}

func (m *Mediator) reconcile(ctx context.Context, periodic bool) error {
	recs, err := m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: list records: %w", err)
	}

	// keep = SandboxIDs whose placement must NOT be reclaimed: successfully
	// re-adopted `running` survivors PLUS any `running` sandbox we could not
	// resolve (protect-on-uncertainty) PLUS — in the periodic path — any sandbox
	// a verb is actively building (held lock) or that we already hold a live
	// handle for. A `paused`/`creating` sandbox with no in-flight verb holds no
	// legitimate live placement, so it never enters keep.
	keep := make([]string, 0)

	for _, rec := range recs {
		if periodic {
			// A held transition lock means a verb (in-flight create / resume /
			// user pause) is driving this sandbox — its placement is being BUILT,
			// not orphaned. Skip it UNIFORMLY IN ALL STATES (§4.4-A "对持锁沙箱
			// 一律跳过"; reaper-health D4 addition 1), not just `running`: tearing
			// down a being-built create (destroys overlay + empty upper) or resume
			// (tears down the netns/container it just launched) out from under the
			// verb is the exact corruption the skip exists to prevent. Protect its
			// placement (add to keep) and retry next tick. At startup this never
			// triggers — locks are all fresh (see Reconcile).
			l, ok := m.tryLock(rec.ID)
			if !ok {
				keep = append(keep, string(rec.ID))
				continue
			}
			// Re-read under the lock: a verb that finished between the snapshot and
			// acquiring the lock may have changed the state. Reconcile the CURRENT
			// record; a gone record (a crashed-create just deleted) → nothing to do.
			cur, gerr := m.deps.Store.GetSandbox(ctx, rec.ID)
			if gerr != nil {
				l.Unlock()
				continue
			}
			m.reconcileRecordLocked(ctx, cur, &keep)
			l.Unlock()
			continue
		}
		// startup: locks are all fresh, live-map empty → adoptOrDemote takes its
		// own lock (adoptOrDemote vs adoptOrDemoteLocked, §4.4-B).
		m.reconcileRecord(ctx, rec, &keep)
	}

	// Periodic only: protect every placement THIS process currently holds a live
	// handle for (§4.4-A / reaper-health D4 addition 2). A build that completed
	// AFTER the ListSandboxes snapshot is in the live-map but not in `recs`, and a
	// re-adopted survivor is already here — seeding keep from the live-map covers
	// both so ReclaimOrphans never tears down a live placement it just missed in
	// the record scan. The dedup keeps the keep-set a true set (a re-adopted
	// survivor is in both the record scan and the live-map). At startup the
	// live-map is empty, so this is a no-op.
	if periodic {
		keep = dedup(append(keep, m.liveIDs()...))
	}

	// reclaim every engine placement not backing a kept sandbox: orphans from
	// crashed create/resume + placements of cleanly-demoted-to-paused sandboxes
	// (§4.4-A 持久态分毫不动 — only placement, never a kept upper). The mediator has
	// already force-detached (MNT_DETACH) the orphan Overlay mounts in the loop
	// above; ReclaimOrphans then does runsc kill → veth → netns. At STARTUP
	// reconcile this overlay-umount-before-runsc-kill order is deliberate and safe
	// (§4.4-A 回收次序): the mediator has no live placement handle, only a table
	// scan, and the orphan is being torn down regardless — a lazy detach just
	// unhooks the mount, the kernel reclaims it when the container's last ref
	// drops, and the upper is never touched. (The live Destroy path DOES hold the
	// handle and uses strict reverse-dep order: runsc destroy → overlay umount.)
	// Reclaim failure is logged, not fatal — the placement stays leaked and is
	// retried next boot.
	if err := m.deps.Runtime.ReclaimOrphans(ctx, keep); err != nil {
		m.deps.Logf("reconcile: reclaim orphans (retried next boot): %v", err)
	}
	return nil
}

// reconcileRecord handles one record at STARTUP (no lock held, live-map empty).
// running → adoptOrDemote takes its own transition lock (§4.4-B).
func (m *Mediator) reconcileRecord(ctx context.Context, rec model.Sandbox, keep *[]string) {
	switch rec.State {
	case model.Creating:
		// crashed mid-create: never reached a resting state → delete record. Its
		// half-built placement (if any) falls to ReclaimOrphans (not in keep); its
		// empty Overlay upper never held useful state → reclaim it. Publishes
		// NOTHING (nil Event): a crashed `creating` never reached a running/visible
		// state, so — exactly like the live create-unwind path — there was no
		// "created" event and there must be no unpaired "destroyed" (a billing
		// subscriber must not see a destroyed for a sandbox it never saw created).
		m.reclaimOrphanOverlay(ctx, rec, true)
		if err := m.deps.Store.DeleteSandbox(ctx, rec.ID, nil); err != nil && err != store.ErrNotFound {
			m.deps.Logf("reconcile: skip creating %s: delete record: %v", rec.ID, err)
		}
	case model.Paused:
		// keep the record; a half-built placement (crashed mid-resume) is an orphan
		// → unmount its lingering Overlay mount, but KEEP the upper (resumable,
		// §4.2). The netns/container fall to ReclaimOrphans.
		m.reclaimOrphanOverlay(ctx, rec, false)
	case model.Running:
		adopted, err := m.adoptOrDemote(ctx, rec)
		m.reconcileRunningOutcome(ctx, rec, adopted, err, keep)
	}
}

// reconcileRecordLocked handles one record in the PERIODIC path with the
// transition lock ALREADY HELD by the caller (tryLock succeeded → no verb is
// driving this sandbox right now). Same dual as startup, except a `running`
// sandbox we already hold a live handle for is not an orphan and needs no Adopt
// re-recognition (that is the crash-recovery path) — keep it straight away
// (reaper-health D4 addition 2). Only a `running` with NO live handle runs
// adopt/demote (never built here → shouldn't happen, or first tick after a crash
// = startup semantics).
func (m *Mediator) reconcileRecordLocked(ctx context.Context, rec model.Sandbox, keep *[]string) {
	switch rec.State {
	case model.Creating:
		// Lock held + still `creating` means the create is NOT in flight (it would
		// hold the lock and we'd have skipped) — so this is a crashed-create orphan,
		// same as startup. Delete + reclaim the empty upper.
		m.reclaimOrphanOverlay(ctx, rec, true)
		if err := m.deps.Store.DeleteSandbox(ctx, rec.ID, nil); err != nil && err != store.ErrNotFound {
			m.deps.Logf("reconcile: skip creating %s: delete record: %v", rec.ID, err)
		}
	case model.Paused:
		// Lock held + `paused` means no resume is in flight — a cleanly paused
		// sandbox has no merged mount, so this is a best-effort no-op; a lingering
		// mount from a crashed mid-resume is unmounted, upper KEPT (§4.2).
		m.reclaimOrphanOverlay(ctx, rec, false)
	case model.Running:
		if m.hasLive(rec.ID) {
			*keep = append(*keep, string(rec.ID)) // live handle we built → not an orphan
			return
		}
		adopted, aerr := m.adoptOrDemoteLocked(ctx, rec)
		m.reconcileRunningOutcome(ctx, rec, adopted, aerr, keep)
	}
}

// liveIDs snapshots the SandboxIDs the mediator currently holds a live placement
// handle for (§4.4-A periodic keep-set seed). Ordering is irrelevant — keep is a
// set to the provider.
func (m *Mediator) liveIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.live))
	for id, ls := range m.live {
		if ls != nil && ls.placement != nil {
			out = append(out, string(id))
		}
	}
	return out
}

// dedup returns ids with duplicates removed, order preserved. The keep-set is a
// SET to ReclaimOrphans; the record scan and the live-map seed can name the same
// re-adopted survivor, and the assertion-friendly single-entry form keeps tests
// (and logs) honest.
func dedup(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// reconcileRunningOutcome folds an adopt-or-demote result into keep / orphan
// handling — shared by startup and periodic reconcile (§4.4-A dual). err =
// unresolved → PROTECT (keep, retry next pass); adopted → keep; cleanly dead →
// demoted inside adoptOrDemote, unmount its orphan Overlay mount (upper kept).
func (m *Mediator) reconcileRunningOutcome(ctx context.Context, rec model.Sandbox, adopted bool, err error, keep *[]string) {
	switch {
	case err != nil:
		// could not resolve this survivor (probe error, un-rebuildable handle). Do
		// NOT reclaim it — it may still be live; a torn-down live container leaks
		// and breaks transport (§4.4-A protect-on-uncertainty). Retry next pass.
		m.deps.Logf("reconcile: skip running %s (protected, not reclaimed): %v", rec.ID, err)
		*keep = append(*keep, string(rec.ID))
	case adopted:
		// alive + ready + handle rebuilt → keep its placement.
		*keep = append(*keep, string(rec.ID))
	default:
		// cleanly determined dead → demoted to paused inside adoptOrDemote; unmount
		// its orphan Overlay mount, keep the upper.
		m.reclaimOrphanOverlay(ctx, rec, false)
	}
}

// reclaimOrphanOverlay unmounts a sandbox's lingering merged Overlay mount left
// by a crash (§4.4-A reverse-dep order: overlay umount is the mediator-side step
// between runsc teardown and netns delete). The overlayfs mount is kernel state
// that survives the orchestrator process; if the record's container is gone the
// mount is an orphan holding nothing. destroyUpper additionally reclaims the
// upper bytes (create's empty Overlay → destroyed); for paused/demoted the upper
// is the persistent SoT and is KEPT (§4.4-A 持久态分毫不动). Best-effort: a
// not-mounted overlay (nothing to clean) is not an error.
func (m *Mediator) reclaimOrphanOverlay(ctx context.Context, rec model.Sandbox, destroyUpper bool) {
	// ID-only reclamation (AdoptMerged/DestroyOverlay derive paths from the
	// OverlayID); no mount, so a single-element lower stack suffices.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)})
	if err != nil {
		return // no artifact on disk → nothing to unmount
	}
	if merged, err := m.deps.Rootfs.AdoptMerged(ctx, ov); err == nil {
		_ = m.deps.Rootfs.Unmount(ctx, merged)
	}
	if destroyUpper {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
}

// adoptOrDemote handles a `running` record during reconciliation. It returns
// (adopted, err):
//   - adopted=true, err=nil  → survivor is live AND serving; handle rebuilt,
//     recorded in the live-map (same IP, no epoch bump, §4.4-A 另一半).
//   - adopted=false, err=nil → cleanly determined not-adoptable (isolation unit
//     dead, OR alive-but-not-serving); CAS-demoted to paused (§4.2 involuntary
//     pause) with an event. Caller reclaims its orphan placement.
//   - err!=nil               → could not resolve (probe error / un-rebuildable
//     handle). Caller PROTECTS the placement (does not reclaim) and retries next
//     boot — never tears down a possibly-live container on uncertainty.
//
// Readiness is a two-layer judgement matching cold-start's ⑦ gate, not just the
// runtime's L1 liveness (§4.3 two-layer probe). Adopt gives L1 (the isolation
// unit is up) + the endpoint; we then fire ONE Health RPC (L2) before trusting
// the survivor. This closes the crack where a `created`-but-not-listening
// container would be optimistically adopted with an un-serving endpoint: if L2
// does not answer, we demote to paused (recoverable via a fresh cold-start on the
// next resume) rather than record a dead handle. It is a SINGLE boot-time probe,
// deliberately NOT the continuous four-quadrant health classifier (deferred to
// open-decisions §2 §4.4-A row).
func (m *Mediator) adoptOrDemote(ctx context.Context, rec model.Sandbox) (bool, error) {
	l := m.lock(rec.ID)
	l.Lock()
	defer l.Unlock()
	return m.adoptOrDemoteLocked(ctx, rec)
}

// adoptOrDemoteLocked is adoptOrDemote's body with the transition lock ALREADY
// HELD by the caller. Startup reconcile takes the lock blocking (adoptOrDemote);
// periodic reconcile takes it via tryLock (skip-if-held) and calls this — Go
// mutexes are not reentrant, so the two entry points must not both lock.
func (m *Mediator) adoptOrDemoteLocked(ctx context.Context, rec model.Sandbox) (bool, error) {
	pl, alive, err := m.deps.Runtime.Adopt(ctx, string(rec.ID))
	if err != nil {
		return false, fmt.Errorf("adopt probe: %w", err) // uncertain → protect
	}
	// L2 readiness: an L1-live unit that does not answer Health right now is not
	// cleanly adoptable — treat as not-serving and demote (aligns adopt with
	// cold-start ⑦; §4.3 two-layer probe).
	if alive && !m.probeReady(ctx, pl) {
		alive = false
	}
	if !alive {
		// dead / not-serving → involuntary pause (§4.2). Its placement, if any, is
		// reclaimed as an orphan (this sandbox is not added to keep). The event
		// Kind is "crash-paused", NOT "paused": this is the §4.2 involuntary pause
		// (found dead/not-serving at reconcile), distinct from a user pause and
		// from the reaper's idle "auto-paused". State is the same `paused`; the
		// Kind lets a subscriber tell why it paused (§4.1 names transitions
		// distinctly). See store.Event.Kind.
		if err := m.deps.Store.CompareAndSetState(ctx, rec.ID, model.Running, model.Paused, newEvent(rec.ID, "crash-paused", model.Paused)); err != nil && err != store.ErrConflict {
			return false, fmt.Errorf("demote to paused: %w", err) // uncertain → protect
		}
		return false, nil
	}

	// alive + serving → re-attach the still-mounted overlay + record the live
	// handle so destroy/transport work again (§4.4-A: without this the survivor is
	// un-addressable — destroy would leak the live container). AdoptMerged
	// recovers the EXISTING kernel mount by OverlayID (no re-mount), so the lower
	// stack is not needed — a single-element stack satisfies validation.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)})
	if err != nil {
		return false, fmt.Errorf("reopen overlay: %w", err) // uncertain → protect
	}
	merged, err := m.deps.Rootfs.AdoptMerged(ctx, ov)
	if err != nil {
		return false, fmt.Errorf("adopt merged: %w", err) // uncertain → protect
	}
	m.put(rec.ID, &liveState{placement: pl, merged: merged})
	return true, nil
}

// probeReady fires a single Health RPC against an adopted placement's endpoint
// (L2 readiness). Bounded by a short timeout so a booting/hung survivor demotes
// promptly rather than stalling boot. This is the same readiness signal
// cold-start's ⑦ gate polls, fired once here (guest-runtime D5.2 要点①).
func (m *Mediator) probeReady(ctx context.Context, pl *runtime.Placement) bool {
	if pl == nil {
		return false
	}
	cl := m.deps.Dial(transport.Endpoint(pl.TransportEndpoint))
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := cl.Health(hctx)
	return err == nil
}

// Client returns a stateless transport client for the sandbox's CURRENT
// endpoint (§5.E: built per endpoint, discarded on resume). This is how the
// orchestrator proxies process/fs by sandbox-id WITHOUT exposing placement
// (§6.2): callers address by SandboxID; the endpoint resolution stays here.
// Errors if the sandbox has no live placement (not running).
func (m *Mediator) Client(id model.SandboxID) (transport.Client, error) {
	ep, err := m.Endpoint(id)
	if err != nil {
		return nil, err
	}
	return m.deps.Dial(transport.Endpoint(ep)), nil
}

// Endpoint returns the sandbox's current transport endpoint. INTERNAL — not part
// of the §6.1 external API (which never exposes placement, §6.2). Used by the
// orchestrator to proxy transport and by tests to assert IP-as-epoch (§3.3/§4.3).
func (m *Mediator) Endpoint(id model.SandboxID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.live[id]
	if ls == nil || ls.placement == nil {
		return "", fmt.Errorf("sandbox %s has no live placement (not running)", id)
	}
	return ls.placement.TransportEndpoint, nil
}

// hasLive reports whether the mediator holds a live placement handle for the
// sandbox (§4.4-A: the periodic reconcile protects live-map placements that
// THIS process built — they are not orphans and need no Adopt re-recognition,
// reaper-health D4).
func (m *Mediator) hasLive(id model.SandboxID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.live[id]
	return ls != nil && ls.placement != nil
}

// State returns the sandbox's current lifecycle state (§6.1 GET).
func (m *Mediator) State(ctx context.Context, id model.SandboxID) (model.Lifecycle, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return "", err
	}
	return rec.State, nil
}

// List returns all sandbox records (§6.1 GET /v1/sandboxes).
func (m *Mediator) List(ctx context.Context) ([]model.Sandbox, error) {
	return m.deps.Store.ListSandboxes(ctx)
}

// ListTemplates returns all Template rows (§6.1 GET /v1/templates).
func (m *Mediator) ListTemplates(ctx context.Context) ([]model.Template, error) {
	return m.deps.Store.ListTemplates(ctx)
}

// DeleteTemplate removes a Template row iff nothing references it (§6.1 DELETE
// /v1/templates/{id}). The store's reference protection (§4.3) surfaces as
// store.ErrInUse — a referenced Template (by a sandbox, or a child layer via
// parent) cannot be deleted — which the API layer maps to 409 template in use.
// The layer's now-orphaned bytes are reclaimed by the §5.B orphan sweep, NOT in
// this row delete (§4.4 状态转换与字节回收解耦). This is a pure store operation:
// no placement, no transition lock — Template is not a state-machine object
// (§4.3 existence-is-state), so there is no per-sandbox lock to hold.
func (m *Mediator) DeleteTemplate(ctx context.Context, id model.TemplateID) error {
	return m.deps.Store.DeleteTemplate(ctx, id)
}

// put/take manage the in-memory liveState map.
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

// newEvent builds an outbox Event for a §4.1 transition, carrying a
// transition-unique ID (§4.4-C: subscribers dedup on it; a redelivered event
// keeps the SAME ID). The mediator constructs it per transition semantics; the
// store commits it atomically with the state change (§4.4-C).
func newEvent(id model.SandboxID, kind string, state model.Lifecycle) *store.Event {
	return &store.Event{
		ID:         "evt_" + randToken(),
		Sandbox:    id,
		Kind:       kind,
		State:      state,
		OccurredAt: time.Now(),
	}
}

// randToken returns a short opaque hex token for sandbox/overlay IDs. Opaque =
// not derived from placement (§3.3) and safe as a single path segment (§5.B
// validation rejects separators).
func randToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
