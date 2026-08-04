// Package lifecycle is the §4 mediator — the coordinator ABOVE the five
// subsystems, not a sixth peer. It owns the state machine and, per state,
// CALLS each subsystem's mechanism (§4: verbs belong to §4, mechanisms to §5).
// Subsystems never call each other — that is the start of a dependency cycle.
// Any "Rootfs pokes Runtime on resume" is rejected in review.
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
	"errors"
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

// ErrResourceTerminated is the §4.3 resource-terminated verdict surfaced as a
// verb error: this sandbox was terminated for exceeding its RAM safety limit, so
// resume is REFUSED (unlike dead-recoverable, which resumes). The API layer maps
// it to 409 — a state conflict the caller resolves by raising the cap or
// destroying the sandbox, not a server fault.
var ErrResourceTerminated = errors.New("lifecycle: sandbox resource-terminated")

// ErrInsufficientCapacity is the §3.4 admission verdict: honoring this sandbox's
// caps would oversubscribe the node. Distinct from a state conflict — nothing is
// wrong with the sandbox, the node is simply full — so the API maps it to 507.
var ErrInsufficientCapacity = errors.New("lifecycle: insufficient node capacity")

// ErrCapRequired is the §3.4 verdict for a request that omits a cap on an axis
// the node meters. DELIBERATELY distinct from ErrInsufficientCapacity: nothing is
// full, so 507 would be a lie — the request is invalid for THIS engine's
// configuration, which the API maps to 400. `0 = unlimited` stays legal on any
// axis the operator has not declared a budget for.
var ErrCapRequired = errors.New("lifecycle: node meters this axis, so a cap is required")

// deathCauseOf translates the §5.A provider enum into the persisted model enum.
// Two types on purpose (§5.A): the provider seam must not depend on the
// control-plane record shape. The translation lives HERE, in the mediator, which
// is the only place that legitimately knows both.
func deathCauseOf(c runtime.DeathCause) model.DeathCause {
	switch c {
	case runtime.CauseGone:
		return model.CauseGone
	case runtime.CauseResourceExceeded:
		return model.CauseResourceExceeded
	default:
		return model.CauseUnknown
	}
}

// Deps are the mechanisms the mediator orchestrates. It holds the seams, not
// concrete implementations — swapping local_runsc / SQLite is registering an
// implementation (§1.3), not editing this file.
type Deps struct {
	Store   store.Store
	Runtime runtime.Provider
	Rootfs  rootfs.Provider
	// Dial builds a stateless transport client for a discovered endpoint
	// (§5.E: constructed per endpoint, discarded on resume).
	//
	// Returns the FULL Client, deliberately, even though §4's own logic only ever
	// probes health (probeHealth / classify both narrow to transport.HealthProber at
	// the point of use). The width is here because Mediator.Client re-exports this
	// to the data plane, and THAT is worth the cost: endpoint resolution stays
	// inside §4, so the orchestrator addresses sandboxes by SandboxID and never
	// learns a placement (§6.2). Narrowing this field would move that resolution
	// out and leak placement — a worse trade than one wide field.
	//
	// The visible cost is that an L1 fake must satisfy all three halves. Accepted:
	// it is a test-only cost, and it buys the §6.2 invariant.
	Dial func(transport.Endpoint) transport.Client

	// HealthTimeout bounds the ⑦ readiness gate (guest-runtime D5.1).
	HealthTimeout time.Duration

	// DrainGrace bounds the §4.2 pause drain barrier: how long a voluntary freeze
	// waits for engine-issued mutating fs RPCs to finish before freezing anyway
	// (D4.4). Timing out is NOT a failure — it lands where a crash lands, which
	// §4.2 accepts — so this only trades "how long a pause may wait" against "how
	// likely a write is truncated". Zero disables the drain (straight to the
	// preemptive freeze); New applies defaultDrainGrace when unset.
	DrainGrace time.Duration

	// NodeCapacity is the §3.4 no-oversubscribe budget the admission gate admits
	// against (§1.1 single-binary config). Zero on a dimension leaves that axis
	// unmetered — see NodeCapacity.
	NodeCapacity NodeCapacity

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
	deps Deps
	// mu guards the mediator's in-memory state — the locks, live, buildingLayers
	// and buildingOverlays maps plus pendingUncappedBytes — each held only across
	// an O(1) operation, never across a transition lock, a store call or a provider
	// call. A transitionLock's Unlock takes mu AFTER releasing the transition
	// mutex, so the two never nest; admitMu is only ever taken OUTSIDE mu.
	mu    sync.Mutex
	locks map[model.SandboxID]*transitionLock

	// barrier drains engine-issued mutating fs RPCs before a voluntary freeze, so
	// a pause cannot land mid-write and truncate a file (§4.2 / D4.4; see
	// barrier.go). Separate from the transition lock: that serializes VERBS
	// against each other, this one serializes a verb against in-flight DATA-PLANE
	// work, which never takes the transition lock at all.
	barrier *barrier

	// admitMu serializes the §3.4 no-oversubscribe gate. The per-sandbox
	// transition lock (§4.4-B) CANNOT do this job: the gate's invariant is
	// node-GLOBAL (a sum over every record), while those locks are per-sandbox —
	// and `Create` mints a fresh SandboxID, so two concurrent creates take two
	// different locks and never serialize against each other at all. Each would
	// read the store before either inserts, both would pass, and the node ends up
	// over its declared budget with every individual cap honored.
	//
	// So the sum-then-commit sequence needs one arbiter. This is the same shape as
	// §4.3's reference-reclaim invariant, which resolves the analogous
	// read-then-decide-vs-concurrent-insert race by requiring a single serializing
	// authority — there the store's atomic commit, here this mutex, because the
	// decision spans a provider measurement the store cannot see.
	//
	// Held only across {read records → measure → compare → commit}, never across a
	// cold start, so it does not couple unrelated sandboxes' launch latency.
	admitMu sync.Mutex

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
	// (the placement live-map's twin, §4.4-A). A crashed Promote's staging layer
	// is absent here at boot, so the sweep correctly reclaims it as a true orphan.
	// Guarded by mu.
	buildingLayers map[model.TemplateID]bool

	// buildingOverlays is buildingLayers' twin for Overlay UNITS: OverlayIDs whose
	// storage the mediator has begun materializing and not yet settled. The unit
	// sweep must skip these. Note the hazard differs from Promote's: create inserts
	// its record BEFORE allocating the unit, so write order alone would seem to
	// protect it — but the sweep's `live` set is a SNAPSHOT read before the
	// provider enumerates, so a create committing in between presents as bytes with
	// no live row, indistinguishable from an orphan. Guarded by mu.
	buildingOverlays map[model.OverlayID]bool

	// pendingUncappedBytes is in-flight growth of the §3.4 accounting formula's
	// MEASURED term: bytes a gated control-plane action has been admitted to add
	// but that the provider's measurement cannot see yet (a Promote mid-copy). The
	// admission gate adds it to the left side so a concurrent run cannot spend the
	// same capacity twice. Guarded by mu; see reserveUncapped for why a reservation
	// rather than simply holding admitMu across the action.
	pendingUncappedBytes int64
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

	// defaultDrainGrace bounds the §4.2 pause drain (D4.4). Seconds, not minutes:
	// the operations it waits for are single fs RPCs, so a healthy one finishes in
	// milliseconds and anything still outstanding after this is stuck rather than
	// slow. Waiting longer would delay every pause to accommodate a caller that is
	// not coming back.
	defaultDrainGrace = 5 * time.Second
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
	if deps.DrainGrace == 0 {
		deps.DrainGrace = defaultDrainGrace
	}
	// A NEGATIVE grace means "no drain at all" and survives defaulting — only a
	// test wants it (to restore the truncate window and prove the barrier closes a
	// real one). Normalized to zero, which drainForPause treats as disabled.
	if deps.DrainGrace < 0 {
		deps.DrainGrace = 0
	}
	return &Mediator{
		deps:             deps,
		locks:            make(map[model.SandboxID]*transitionLock),
		barrier:          newBarrier(),
		live:             make(map[model.SandboxID]*liveState),
		buildingLayers:   make(map[model.TemplateID]bool),
		buildingOverlays: make(map[model.OverlayID]bool),
	}
}

// transitionLock is one sandbox's transition mutex (§4.4-B) plus the refcount
// that decides when its map entry may be dropped.
//
// The naive reclamation — delete the entry when a verb finishes, or on destroy —
// is a CORRECTNESS BUG, not a tidy-up: a verb already blocked in mu.Lock() holds
// a pointer to this mutex, so deleting the entry lets the NEXT verb mint a fresh
// mutex and the two then serialize on DIFFERENT mutexes. Both would be "holding
// the sandbox's transition lock" while running concurrently, which is exactly
// what §4.4-B forbids (and what a destroy racing an in-flight resume needs).
//
// refs is what closes that: it counts outstanding lock() handouts, not holders.
// It is incremented under m.mu in the SAME critical section that finds-or-creates
// the entry, so a caller cannot be handed a pointer to an entry that is
// concurrently being deleted; and it is decremented only after the mutex is
// released, so a blocked waiter always keeps refs > 0. An entry therefore reaches
// refs == 0 only when nobody holds it, nobody is blocked on it, and nobody is
// between lock() and Lock() — at which point deleting it is unobservable: the
// next verb creates a fresh mutex with no contender to disagree with.
type transitionLock struct {
	mu sync.Mutex

	// m/id let Unlock drop this handle's ref without the caller having to.
	m  *Mediator
	id model.SandboxID

	// refs counts outstanding lock() handouts for this id. Guarded by m.mu, NOT
	// by mu — it must be readable while mu is held by someone else.
	refs int
}

// Lock acquires the transition lock, blocking until it is free.
func (l *transitionLock) Lock() { l.mu.Lock() }

// Unlock releases the transition lock AND the ref the matching lock() took. The
// order matters: the mutex is released first so a blocked waiter can proceed,
// and only then is the ref dropped — while a waiter exists its own ref keeps the
// entry alive, so the drop can never collect an entry someone is still using.
func (l *transitionLock) Unlock() {
	l.mu.Unlock()
	l.release()
}

// release drops one ref, collecting the map entry at zero.
func (l *transitionLock) release() {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	l.refs--
	switch {
	case l.refs > 0:
		return
	case l.refs < 0:
		// Unreachable by construction (one release per lock()). If it ever fires,
		// some path released twice — which would let a live entry be collected
		// under a holder and silently break §4.4-B. Fail loudly instead.
		panic(fmt.Sprintf("lifecycle: transition lock for %s released more times than acquired", l.id))
	}
	// Only delete OUR entry: a re-created entry for the same id would be a
	// different *transitionLock, and dropping that one here would collect an
	// entry with live refs.
	if cur, ok := l.m.locks[l.id]; ok && cur == l {
		delete(l.m.locks, l.id)
	}
}

// lock returns the per-sandbox transition lock, creating it on first use
// (§4.4-B: all lifecycle verbs for the SAME sandbox serialize on it).
//
// The returned handle carries a ref that MUST be released exactly once, which
// Unlock does — so the standard shape
//
//	l := m.lock(id); l.Lock(); defer l.Unlock()
//
// is complete: no separate release call, and the deferred Unlock covers every
// return path. The one shape that leaks is calling lock() and never unlocking;
// tryLock is the only caller that can reach it, and it releases explicitly on
// its failure path.
func (m *Mediator) lock(id model.SandboxID) *transitionLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[id]
	if !ok {
		l = &transitionLock{m: m, id: id}
		m.locks[id] = l
	}
	l.refs++
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
//
// On the SKIP path it releases the ref lock() took: the reaper and periodic
// reconcile skip on every tick for as long as a verb is driving a sandbox, so a
// ref retained here would accumulate per skipped round and the entry would never
// be collected — the leak this refcount exists to close, with more code.
func (m *Mediator) tryLock(id model.SandboxID) (*transitionLock, bool) {
	l := m.lock(id)
	if l.mu.TryLock() {
		return l, true
	}
	l.release()
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

	// Limits is the OPTIONAL resource-limit Binding input (§3.4; §6.1 POST
	// /v1/sandboxes cpu_millis?/memory_byte?/disk_byte?). Zero on an axis =
	// unlimited there. Unlike the TTLs there is no engine-wide default to
	// substitute: "unlimited" is a meaningful, honest resting value for a cap,
	// whereas a made-up default cap would silently constrain workloads the
	// operator never limited.
	Limits model.Limits
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
		// Resource limits are materialized into the record the same way and for the
		// same reason (§3.4 Binding): every placement re-reads them from here, so
		// the stored value is the single authority for all three axes. Omitted =
		// zero = unlimited on that axis.
		Limits:    spec.Limits,
		CreatedAt: now, UpdatedAt: now,
	}
	if spec.IdleTimeout != nil {
		rec.IdleTimeout = *spec.IdleTimeout
	}
	if spec.Retention != nil {
		rec.Retention = *spec.Retention
	}

	// Admission gate (§3.4 / rootfs-overlay D6.5 node-level no-oversubscribe): the
	// node must be able to HONOR every cap it has promised, so a new promise is
	// checked against the capacity actually left. Runs BEFORE any bytes or
	// placement are materialized, so a rejected create leaves nothing half-built.
	//
	// admitMu spans {gate → record insert} as ONE critical section — see the
	// field's doc for why the per-sandbox lock cannot close this race. Released
	// before the cold start, which is the long part and needs no serialization.
	m.admitMu.Lock()
	if err := m.admit(ctx, rec.Limits, ""); err != nil {
		m.admitMu.Unlock()
		return "", err
	}
	// The transient creating-insert is an internal step of the create verb, not
	// an externally-visible §4.1 transition — it publishes nothing (nil Event).
	// The "created" event is emitted only when create COMPLETES → running below.
	// Inserting it here also makes this promise visible to the next gate run.
	if err := m.deps.Store.CreateSandbox(ctx, rec, nil); err != nil {
		m.admitMu.Unlock()
		return "", fmt.Errorf("create: insert record: %w", err)
	}
	m.admitMu.Unlock()

	// ① build empty Overlay over the resolved lineage lower stack (§5.B
	// multi-lowerdir) + run the cold-start chain.
	lower, err := m.resolveLower(ctx, spec.Template)
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: resolve lineage: %w", err)
	}
	// The capacity cap is applied HERE, as the Overlay is allocated: it is an FS
	// quota on the upper+work storage unit, so it belongs to the Rootfs subsystem's
	// materialization, not to the runtime provider (§3.4 / §5.B). The mediator only
	// passes the numeric intent across the seam.
	// Protect the unit from the orphan-unit sweep for the whole create window
	// (§4.4-A): the sweep's `live` snapshot may predate this sandbox's row even
	// though the row was inserted first, so bytes-on-disk-with-no-live-entry is
	// reachable without any ordering mistake. Held until create settles either way.
	m.markBuildingOverlay(oid)
	defer m.unmarkBuildingOverlay(oid)
	ov, err := m.deps.Rootfs.CreateOverlay(ctx, string(oid), lower, rec.Limits.DiskByte)
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: overlay: %w", err)
	}
	if err := m.coldStart(ctx, sid, ov, rec.Limits); err != nil {
		// create has no fallback state: reclaim half-built placement + empty
		// Overlay, delete the record → destroyed (§4.4-A / D5.2 point ④). The
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
	if err := m.deps.Store.CompareAndSetState(ctx, sid, model.Creating, model.Running, newEvent(sid, "created", model.Running), nil); err != nil {
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
		return "", fmt.Errorf("create: commit running: %w", err)
	}
	return sid, nil
}

// Pause = quiesce, then cleanly stop the container (§4.2). The Overlay is
// already on the backend, so pause captures NOTHING; stopping releases RAM/CPU
// and the placement — a paused sandbox occupies storage only (§4.2). CAS-guarded:
// requires `running`. Pause
// always lands in a legal `paused` (§4.4: no middle dirty state), so we CAS the
// state first, then release placement-local resources.
func (m *Mediator) Pause(ctx context.Context, id model.SandboxID) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	// A user pause is not a death: no cause to record (nil leaves the column
	// untouched).
	if err := m.pauseLocked(ctx, id, "paused", nil); err != nil {
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
// (§4.2: pause captures nothing). Stop is best-effort and idempotent, so it is
// also correct for a
// crash-pause whose container is already dead (it reclaims the lingering netns).
// cause carries the §4.3 death cause for the INVOLUNTARY variants (crash-pause):
// it is committed in the SAME store call as the demotion CAS (§4.3 / §4.4-C; see
// store.CompareAndSetState). nil for the voluntary variants (user pause /
// auto-pause), which are not deaths and must leave any previously recorded cause
// alone.
func (m *Mediator) pauseLocked(ctx context.Context, id model.SandboxID, kind string, cause *model.DeathCause) error {
	// Drain engine-issued mutating fs RPCs before freezing, for the VOLUNTARY
	// variants only (§4.2 / D4.4). A crash-pause skips it: the container is already
	// dead, so there is nothing in flight to finish and nothing left to truncate —
	// waiting would only delay recording a death we have already observed.
	//
	// Before the CAS, not after: once the record says `paused`, a still-running
	// write would be landing bytes into a sandbox the control plane already
	// considers frozen. undrain unconditionally, or a failed CAS below would leave
	// the sandbox refusing writes forever.
	if kind != "crash-paused" {
		m.drainForPause(id)
		defer m.barrier.undrain(id)
	}
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Running, model.Paused, newEvent(id, kind, model.Paused), cause); err != nil {
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
	// §4.3 resource-terminated: this sandbox was terminated for exceeding its RAM
	// safety limit, and that disposition is the OPPOSITE of dead-recoverable's —
	// resuming re-runs the same workload into the same cap, i.e. a crash loop. It
	// stays `paused` until the cap is raised (§6.1 PATCH, which clears the cause in
	// the same commit) or it is destroyed. This is exactly why the death cause has
	// to be persisted (§4.3): the state alone cannot distinguish the two cases.
	//
	// The check lives HERE rather than in the Resume verb so the invariant is
	// STRUCTURAL — every path that actually performs a resume passes through this
	// function, including the reaper's D4b transport-down escalation. That
	// escalation cannot carry this cause (it fires only when L1 is ALIVE, and a
	// `running` record cannot hold CauseResourceExceeded), so the guard is
	// unreachable from there — but a guard that depends on call-site convention is
	// one refactor away from being wrong.
	if rec.DeathCause == model.CauseResourceExceeded {
		return fmt.Errorf("resume: %w: sandbox was resource-terminated (RAM cap %d bytes) — resuming would re-trigger termination. Raise memory_byte via PATCH /v1/sandboxes/%s (re-runs the admission gate; may be refused for node capacity), which clears the death cause and permits resume; or Promote it to keep its bytes under a new sandbox; or destroy it (§4.3)",
			ErrResourceTerminated, rec.Limits.MemoryByte, id)
	}
	// Admission gate before anything is materialized (§3.4 no-oversubscribe). This
	// sandbox's own promised cap is EXCLUDED from the committed total — it is
	// already counted there, and double-counting it would make a resume of an
	// unchanged sandbox fail against its own reservation. On failure the record
	// stays `paused`, the atomic fallback source state (§4.4-A).
	//
	// Under admitMu like create, though for a weaker reason: a resume adds NO new
	// promise (its cap is already a committed row, which is why it excludes itself),
	// so it cannot overshoot the budget by racing. It takes the lock to avoid
	// evaluating against a half-inserted create and to keep one arbiter for the
	// gate. Released immediately — the cold start below must not hold it.
	m.admitMu.Lock()
	err := m.admit(ctx, rec.Limits, rec.ID)
	m.admitMu.Unlock()
	if err != nil {
		return err
	}
	// ① re-open + re-mount the SAME Overlay artifact (resume prefix), re-resolving
	// the lineage lower stack from the store (§5.B multi-lowerdir).
	lower, err := m.resolveLower(ctx, rec.Template)
	if err != nil {
		return fmt.Errorf("resume: resolve lineage: %w", err)
	}
	// Re-apply the capacity cap from the CURRENT record (§3.4 Binding): a resume
	// re-materializes the quota rather than trusting whatever the subvolume was
	// left carrying, so an edited cap takes effect and a lost quota is restored.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), lower, rec.Limits.DiskByte)
	if err != nil {
		return fmt.Errorf("resume: open overlay: %w", err)
	}
	if err := m.coldStart(ctx, id, ov, rec.Limits); err != nil {
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
	// CLEAR the death cause in this same commit: the sandbox is up, so its previous
	// death no longer explains its state. This is also what bounds the §4.3
	// self-correcting retry — an Unknown death that resumes fine does not stay
	// pinned to a stale cause, and if the workload re-triggers the cap the NEXT
	// observation writes the real cause in the control plane's plain view.
	cleared := model.CauseNone
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Paused, model.Running, newEvent(id, "resumed", model.Running), &cleared); err != nil {
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: commit running: %w", err)
	}
	return nil
}

// UpdateBinding rewrites a sandbox's Binding fields (§3.4 / §6.1 PATCH) — the
// only write point for them besides create. It is NOT a §4.1 transition: the
// state does not change, so it publishes no event (the same reasoning Promote
// uses).
//
// Two materialization classes, and the split follows WHO CONSUMES the field:
//
//   - resource caps are consumed by the providers, which materialize only at
//     placement — so they are editable ONLY from `paused` and take effect at the
//     next resume. Accepting a cap edit on a `running` sandbox would leave the
//     record and the live isolation unit disagreeing for an unbounded period,
//     which is the kind of fake invariant CLAUDE.md calls a negative asset. One
//     extra pause buys zero ambiguity.
//   - TTLs are consumed by the reaper, which re-reads the record every sweep with
//     no placement involved — so they are editable from either state and take
//     effect immediately.
//
// Raising a cap is a NEW PROMISE and must clear admission (§3.4). The gate sums
// from the store including `paused` records, so a raise changes Σ at THIS commit,
// not at the eventual resume — skip the gate here and the store would immediately
// hold a Σ over budget, refusing some unrelated create for an overcommit the
// engine itself admitted. Lowering needs no gate (Σ shrinks).
//
// Raising memory_byte also CLEARS death_cause in the same commit, which is what
// makes §4.3's resource-terminated remedy actually work: the resume guard reads
// that column, and nothing else clears it before a successful resume — which the
// guard itself prevents. Scoped tightly to a RAISE of the terminating axis: a
// lowering, or an unrelated cap edit, must not re-arm a crash loop by wiping the
// record of why the sandbox died.
func (m *Mediator) UpdateBinding(ctx context.Context, id model.SandboxID, b store.Binding) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	if b.IsEmpty() {
		return nil // nothing asked for → no write (idempotent)
	}

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	switch rec.State {
	case model.Paused:
		// everything editable
	case model.Running:
		if b.CPUMillis != nil || b.MemoryByte != nil || b.DiskByte != nil {
			return fmt.Errorf("update: %w: resource caps are editable only from `paused` (they re-materialize at the next placement, §3.4) — pause first, then PATCH, then resume; TTL fields are editable while running",
				store.ErrConflict)
		}
	default:
		return fmt.Errorf("update: %w (state=%s: a Binding is editable only from running or paused)", store.ErrConflict, rec.State)
	}

	newLimits, _, _ := b.Apply(rec)
	raisesMemory := newLimits.MemoryByte > rec.Limits.MemoryByte
	// Any raise on a metered axis is a new promise → re-gate. Excludes this
	// sandbox's own committed caps, exactly as resume does, so it is measured
	// against the delta rather than double-charged.
	if newLimits.DiskByte > rec.Limits.DiskByte || raisesMemory || newLimits.CPUMillis > rec.Limits.CPUMillis {
		m.admitMu.Lock()
		err := m.admit(ctx, newLimits, id)
		m.admitMu.Unlock()
		if err != nil {
			return err
		}
	}

	var cause *model.DeathCause
	if raisesMemory && rec.DeathCause == model.CauseResourceExceeded {
		cleared := model.CauseNone
		cause = &cleared
	}
	if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, b, cause); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// Destroy = terminal `destroyed` (§4.1). Valid from running or paused;
// idempotent (already-gone record → nil). The lock held here is the same one
// resume holds for its whole chain, so destroy CANNOT delete an Overlay
// mid-cold-start (§4.4-B: closes the destroy-vs-resume data race).
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
// retention). Marking the record destroyed is decoupled from byte reclamation:
// the Overlay is deleted synchronously here, and a crash between the two leaves
// the Overlay as an orphan the orphan-UNIT sweep reclaims (SweepOrphanOverlays,
// §4.3/§4.4-A).
// Emitting Kind "destroyed" matches §4.1 — evict is "the reaper's automatic
// destroy", the SAME terminal, not a distinct Kind. Because the lock is held for
// the whole chain, destroy/evict CANNOT delete an Overlay mid-cold-start
// (§4.4-B: destroy-vs-resume race).
//
// NOTE — no source-state CAS here (unlike pause/resume). destroy is deliberately
// valid from BOTH `running` and `paused` (§4.1 / §4.4-A decouples it) and is
// idempotent, so there is no single "expected" source state to compare-and-set
// against. The invariant is LOCK-enforced, not CAS-enforced: the held transition
// lock serializes destroy against every other verb. The terminal DeleteSandbox is
// idempotent (ErrNotFound → already gone).
//
// A `creating` record IS reachable here, contrary to what "Create holds the lock
// for its whole chain" would suggest: that holds only within one process lifetime.
// A crash mid-create leaves a `creating` row, and if reconcile's cleanup delete
// then fails it survives to be destroyed by an operator later. Such a record never
// emitted `created`, so it must not emit `destroyed` — see the Kind selection
// below.
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
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
	if err == nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
	// delete the record → destroyed + emit "destroyed" in the same commit
	// (§4.4-C). The outbox row outlives the deleted record.
	//
	// EXCEPT from `creating`, which publishes nothing: that record never emitted
	// `created` (Create publishes only once the chain commits), so a `destroyed`
	// here would be an unpaired terminal — a billing subscriber would be told a
	// sandbox ended that it never saw begin. Reconcile already suppresses the event
	// for exactly this case when it cleans up a crashed create; this is the same
	// rule on the operator-driven path, so the two cannot disagree.
	var ev *store.Event
	if rec.State != model.Creating {
		ev = newEvent(id, "destroyed", model.Destroyed)
	}
	if err := m.deps.Store.DeleteSandbox(ctx, id, ev); err != nil {
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
// bytes-first (§4.3 / §5.B): the new layer bytes are written FIRST
// (PromoteToTemplate), then the `templates` row is inserted. The layer id is
// registered in buildingLayers for the freeze window so a concurrent periodic
// sweep does not reclaim the in-flight layer (§4.4-A). A crash after the bytes are
// written but before the row insert leaves an orphan layer the sweep reclaims —
// never a row pointing at nonexistent bytes.
//
// Returns the new TemplateID. The new Template's parent is the source sandbox's
// current Template, so lineage is the source's stack + this frozen layer on top
// (§5.B multi-lowerdir).
//
// ADMISSION (§3.4 / rootfs-overlay D6.5): a frozen layer is charged to no sandbox
// cap, so it grows the MEASURED term of the node accounting formula and must clear
// the same inequality as a create. The refusal is ErrInsufficientCapacity (507),
// and the harm it prevents is not "this Promote fails" — it is an UNRELATED sandbox
// meeting ENOSPC nowhere near its own cap, i.e. §2.3 containment broken from the
// other side. Gated through admitUncappedGrowth, never through `admit`: these bytes
// join the measured term, not the promise term (see admission.go).
//
// The gate runs BEFORE the quiesce, deliberately, and the cost is stated in the
// forecast's accuracy rather than in the state machine:
//   - Placing it after the quiesce would buy an exact byte count (the upper is
//     frozen) at the price of a REFUSAL THAT MUTATED STATE — a running sandbox left
//     `paused`, with a `paused` event already published, because the node was full.
//     §4.4-A wants a failed transition to return to its source state; "refused, and
//     by the way your sandbox is stopped" is not that.
//   - So the source is still live when it is measured, and the number is an upper
//     bound that a concurrent guest write can outgrow (bounded by the sandbox's own
//     remaining headroom, and PromoteCostBytes overstates on the other axes). This
//     is the same "true at the instant of admission" residual the whole measured
//     term carries (D6.5 残留) — not a new weakness.
//
// A refused Promote therefore leaves the sandbox in its ENTRY state (running stays
// running, paused stays paused), no layer bytes on disk, and no `templates` row.
func (m *Mediator) Promote(ctx context.Context, id model.SandboxID) (model.TemplateID, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return "", fmt.Errorf("promote: %w", err)
	}
	// State check first, so an illegal source is a plain 409 that never measures
	// anything.
	if rec.State != model.Running && rec.State != model.Paused {
		return "", fmt.Errorf("promote: %w (state=%s, want running or paused)", store.ErrConflict, rec.State)
	}

	// open the source Overlay by ID (byte read only, no mount). Needed by the gate
	// below as well as the freeze, so it is opened before either.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
	if err != nil {
		return "", fmt.Errorf("promote: open overlay: %w", err)
	}

	// Admission gate, before the quiesce and before any bytes (see the doc above).
	// Under admitMu for the same reason create is: the inequality is node-GLOBAL
	// while the transition lock is per-sandbox, so two Promotes of two different
	// sandboxes hold two different locks and would not serialize at all. The
	// reservation published inside the critical section is what makes this Promote's
	// claim visible to the next gate run before its bytes exist.
	cost, err := m.deps.Rootfs.PromoteCostBytes(ctx, ov)
	if err != nil {
		// Unmeasurable → refuse, not admit blind: same fail-closed choice the gate
		// makes for UncappedBytes, for the same reason (§2.3).
		return "", fmt.Errorf("promote: %w: cannot forecast layer bytes: %v", ErrInsufficientCapacity, err)
	}
	m.admitMu.Lock()
	err = m.admitUncappedGrowth(ctx, cost)
	if err == nil {
		m.reserveUncapped(cost)
	}
	m.admitMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("promote: %w", err)
	}
	// Released once the bytes are REAL (and therefore visible to UncappedBytes) or
	// the attempt is over. Deferred rather than dropped after the copy so every exit
	// path releases it exactly once — a leaked reservation would permanently shrink
	// the node's admissible capacity.
	defer m.releaseUncapped(cost)

	if rec.State == model.Running {
		// quiesce to a consistent at-rest point (equivalent to pause, §4.2) and
		// stay paused. Uses the "paused" Kind — from the event stream's view this
		// is an ordinary pause; Promote is not a §4.1 lifecycle transition.
		if err := m.pauseLocked(ctx, id, "paused", nil); err != nil {
			return "", fmt.Errorf("promote: quiesce: %w", err)
		}
	}

	newID := model.TemplateID("tmpl_" + randToken())

	// register the in-flight layer BEFORE writing bytes so a concurrent sweep
	// protects it (§4.4-A). Unregister in all exit paths.
	m.markBuilding(newID)
	defer m.unmarkBuilding(newID)

	// freeze the at-rest upper into the new layer dir (bytes-first).
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
func (m *Mediator) coldStart(ctx context.Context, id model.SandboxID, ov *rootfs.Overlay, limits model.Limits) error {
	// ① mount lower(Template)/upper/work → merged.
	merged, err := m.deps.Rootfs.Mount(ctx, ov)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	// ④ launch runsc (sandboxd entrypoint), ⑤ guest boots, ⑥ IP discovered.
	// limits come from the RECORD, re-materialized at every placement — that is
	// what makes them a Binding rather than placement state (§3.4): a resume
	// re-derives the caps from the current record instead of reading back a frozen
	// value, so a cap edited while paused takes effect on the next resume. The
	// capacity axis is NOT here — it is an FS quota applied by the Rootfs
	// subsystem (§5.B), not a provider concern (§3.4).
	pl, err := m.deps.Runtime.Launch(ctx, runtime.LaunchSpec{
		Sandbox: string(id),
		Rootfs:  runtime.RootfsSource{MergedPath: merged.MergedPath},
		Limits: runtime.ResourceLimits{
			CPUMillis:  limits.CPUMillis,
			MemoryByte: limits.MemoryByte,
		},
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
	// guest-runtime D5.2 point ①).
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
// probeHealth resolves a placement to just the readiness/liveness half of the
// channel. Returning HealthProber rather than Client is the point: it states in
// code that §4 never reaches into the process or fs surfaces, so a reader (or a
// future edit) cannot mistake the mediator for a data-plane consumer.
func (m *Mediator) probeHealth(pl *runtime.Placement) transport.HealthProber {
	return m.deps.Dial(transport.Endpoint(pl.TransportEndpoint))
}

func (m *Mediator) awaitHealthy(ctx context.Context, pl *runtime.Placement) error {
	deadline := time.Now().Add(m.deps.HealthTimeout)
	cl := m.probeHealth(pl)
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
// bytes-first is inherent (§5.B / §4.3): the layer directory exists on the
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
// keep-set, and hands both to the rootfs provider — which enumerates its own layer
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

// SweepOrphanOverlays reclaims Overlay storage units backing no sandbox record
// (§4.4-A) — the unit twin of SweepOrphanLayers, and the retry §4.4 licenses for a
// destroy whose byte reclamation failed (§4.4: a failed byte delete does not
// block the transition; the sweep retries it asynchronously).
// Without it a failed DestroyOverlay leaked its unit forever: the record is gone,
// so nothing else ever revisits that OverlayID.
//
// `live` is every OverlayID the store still has a sandbox row for, in ANY state —
// a `creating` or `paused` sandbox's unit is emphatically not an orphan. `building`
// is snapshotted as LATE as possible, immediately before the call, because it
// guards a stale-snapshot race rather than a write-order one: `live` is read first,
// so a create committing after that read presents as bytes with no live entry.
func (m *Mediator) SweepOrphanOverlays(ctx context.Context) error {
	recs, err := m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("sweep overlays: list sandboxes: %w", err)
	}
	live := make([]string, 0, len(recs))
	for _, r := range recs {
		if r.Overlay != "" {
			live = append(live, string(r.Overlay))
		}
	}
	building := m.buildingOverlayIDs()
	n, err := m.deps.Rootfs.SweepOrphanOverlays(ctx, live, building)
	if err != nil {
		return fmt.Errorf("sweep overlays: %w", err)
	}
	if n > 0 {
		m.deps.Logf("sweep overlays: reclaimed %d orphan Overlay unit(s)", n)
	}
	return nil
}

// resolveLower walks the authoritative `templates.parent` lineage from the given
// Template down to its base, returning the read-only lower STACK leaf-first
// (§5.B multi-lowerdir: [leaf, ...ancestors, base]). The mediator owns this walk
// because lineage lives in the store (§4.3) and the rootfs provider must not read it
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

// markBuildingOverlay / unmarkBuildingOverlay bracket the window in which an
// Overlay unit exists on the backend but the sweep's view of the store may not yet
// include its sandbox row (see buildingOverlays).
func (m *Mediator) markBuildingOverlay(id model.OverlayID) {
	m.mu.Lock()
	m.buildingOverlays[id] = true
	m.mu.Unlock()
}

func (m *Mediator) unmarkBuildingOverlay(id model.OverlayID) {
	m.mu.Lock()
	delete(m.buildingOverlays, id)
	m.mu.Unlock()
}

// buildingOverlayIDs snapshots the in-flight-create keep-set.
func (m *Mediator) buildingOverlayIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.buildingOverlays))
	for id := range m.buildingOverlays {
		out = append(out, string(id))
	}
	return out
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
// orchestrator — see ReconcilePeriodic). It is a dual operation, NOT
// reclaim-all:
//
//   - `creating` record → the create crashed mid-chain, never reached running,
//     has no at-rest resting state → delete the record (→ destroyed); its
//     half-built placement is an orphan, reclaimed below.
//   - `paused` record → keep; any half-built placement (crashed mid-resume) is
//     an orphan, reclaimed below. The upper is intact and resumable (§4.2).
//   - `running` record → PROBE liveness via Adopt: alive → rebuild the live-map
//     handle at the SAME IP (not a resume, no new epoch — the isolation unit
//     never restarted, §4.4-A 另一半 — the takeover half); dead → CAS-demote to
//     `paused` (§4.2
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
			// not orphaned. Skip it UNIFORMLY IN ALL STATES (§4.4-A: a lock-held
			// sandbox is skipped, whatever its state; reaper-health D4 addition 1),
			// not just `running`: tearing
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
	// (§4.4-A leaves persistent state untouched — only placement is reclaimed,
	// never a kept upper). The mediator has
	// already force-detached (MNT_DETACH) the orphan Overlay mounts in the loop
	// above; ReclaimOrphans then does runsc kill → veth → netns. At STARTUP
	// reconcile this overlay-umount-before-runsc-kill order is deliberate and safe
	// (§4.4-A 回收次序 — reclamation ordering): the mediator has no live placement
	// handle, only a table
	// scan, and the orphan is being torn down regardless — a lazy detach just
	// unhooks the mount, the kernel reclaims it when the container's last ref
	// drops, and the upper is never touched. (The live Destroy path DOES hold the
	// handle and uses strict reverse-dep order: runsc destroy → overlay umount.)
	// Reclaim failure is logged, not fatal — the placement stays leaked and is
	// retried next boot.
	if err := m.deps.Runtime.ReclaimOrphans(ctx, keep); err != nil {
		m.deps.Logf("reconcile: reclaim orphans (retried next pass): %v", err)
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
// is the persistent SoT and is KEPT (§4.4-A leaves persistent state
// untouched). Best-effort: a
// not-mounted overlay (nothing to clean) is not an error.
func (m *Mediator) reclaimOrphanOverlay(ctx context.Context, rec model.Sandbox, destroyUpper bool) {
	// ID-only reclamation (AdoptMerged/DestroyOverlay derive paths from the
	// OverlayID); no mount, so a single-element lower stack suffices.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
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
//     recorded in the live-map (same IP, no epoch bump, §4.4-A 另一半 — the
//     takeover half).
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
// deliberately NOT the continuous health classifier (that is classify plus the
// reaper's sweep, §4.3).
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
	pl, alive, cause, err := m.deps.Runtime.Adopt(ctx, string(rec.ID))
	if err != nil {
		return false, fmt.Errorf("adopt probe: %w", err) // uncertain → protect
	}
	// L2 readiness: an L1-live unit that does not answer Health right now is not
	// cleanly adoptable — treat as not-serving and demote (aligns adopt with
	// cold-start ⑦; §4.3 two-layer probe).
	// notServing records that we are demoting an L1-ALIVE unit purely because it
	// does not answer Health. Nothing died, so there is no cause to write — see the
	// commit below, which passes nil rather than stamping one.
	notServing := false
	if alive && !m.probeReady(ctx, pl) {
		alive = false
		notServing = true
	}
	if !alive {
		// dead / not-serving → involuntary pause (§4.2). Its placement, if any, is
		// reclaimed as an orphan (this sandbox is not added to keep). The event
		// Kind is "crash-paused", NOT "paused": this is the §4.2 involuntary pause
		// (found dead/not-serving at reconcile), distinct from a user pause and
		// from the reaper's idle "auto-paused". State is the same `paused`; the
		// Kind lets a subscriber tell why it paused (§4.1 names transitions
		// distinctly). See store.Event.Kind.
		// The death cause is committed in THIS SAME CAS (§4.3; see
		// store.CompareAndSetState for why no later window exists).
		//
		// EXCEPT when the unit was L1-alive and merely not serving: nothing died, so
		// there is no cause to record. Pass nil (leave the column untouched) rather
		// than stamping CauseUnknown — "unknown" means "it is GONE but why is
		// undeterminable" (see model.DeathCause), which would be a false statement
		// about a unit L1 just reported as alive, i.e. exactly the guessing §2.4
		// forbids. The reaper's transport-down escalation passes nil for the same
		// reason.
		var causePtr *model.DeathCause
		if !notServing {
			dc := deathCauseOf(cause)
			causePtr = &dc
		}
		if err := m.deps.Store.CompareAndSetState(ctx, rec.ID, model.Running, model.Paused, newEvent(rec.ID, "crash-paused", model.Paused), causePtr); err != nil && err != store.ErrConflict {
			return false, fmt.Errorf("demote to paused: %w", err) // uncertain → protect
		}
		return false, nil
	}

	// alive + serving → re-attach the still-mounted overlay + record the live
	// handle so destroy/transport work again (§4.4-A: without this the survivor is
	// un-addressable — destroy would leak the live container). AdoptMerged
	// recovers the EXISTING kernel mount by OverlayID (no re-mount), so the lower
	// stack is not needed — a single-element stack satisfies validation.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
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
// cold-start's ⑦ gate polls, fired once here (guest-runtime D5.2 point ①).
func (m *Mediator) probeReady(ctx context.Context, pl *runtime.Placement) bool {
	if pl == nil {
		return false
	}
	cl := m.probeHealth(pl)
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

// Get returns the sandbox record (§6.1 GET /v1/sandboxes/{id}). State is the
// narrower read for callers that only need the lifecycle state; this one exists
// because the §6.1 read shape also carries death_cause (§4.3: the recovery
// decision is the caller's, so the caller needs the fact it decides on).
func (m *Mediator) Get(ctx context.Context, id model.SandboxID) (model.Sandbox, error) {
	return m.deps.Store.GetSandbox(ctx, id)
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

// Events serves the §6.1 GET /v1/events replay: at most limit events after the
// given sequence cursor, plus the retained window's bounds (§4.4-C).
//
// PULL ONLY, and that is the load-bearing choice rather than a first step toward
// push: the engine holds NO per-subscriber state (§1.3 forbids it) — no
// subscriber map, no eviction policy, no cleanup path — because a cursor-paged
// read needs none. Adding a server-side poll over this same query later would be
// a presentation adapter reusing this same sequence, which is why it costs
// nothing structurally to omit now. Push as an ADAPTER holds no state; push as
// real fan-out does. That is the line.
//
// It is a pure store read: no placement, no transition lock. The stream is
// engine-level rather than per-sandbox, so there is nothing to serialize against.
func (m *Mediator) Events(ctx context.Context, after int64, limit int) ([]store.Event, int64, int64, error) {
	return m.deps.Store.Events(ctx, after, limit)
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
// this row delete (§4.4: the state transition and byte reclamation are
// decoupled). This is a pure store operation:
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
