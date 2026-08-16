// Package lifecycle is the lifecycle mediator — the coordinator ABOVE the five
// subsystems, not a sixth peer. It owns the state machine and, per state,
// CALLS each subsystem's mechanism (the lifecycle: verbs belong to the lifecycle, mechanisms to the subsystem boundaries).
// Subsystems never call each other (enforced by internal/arch, not by review).
//
// The verbs create / pause / resume / destroy are here. Each is a serialized,
// CAS-guarded transition (per-sandbox transition serialization: one transition lock per sandbox, verbs
// serialize on it, and inside the lock a compare-and-set precondition check
// rejects a verb whose source state is wrong). Every transition commits its
// state change and its outbox event in one atomic store op (the transactional outbox).
package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/volume"
)

// ErrResourceTerminated is the health and reclamation resource-terminated verdict surfaced as a
// verb error: this sandbox was terminated for exceeding its RAM safety limit, so
// resume is REFUSED (unlike dead-recoverable, which resumes). The API layer maps
// it to 409 — a state conflict the caller resolves by raising the cap or
// destroying the sandbox, not a server fault.
var ErrResourceTerminated = errors.New("lifecycle: sandbox resource-terminated")

// ErrInsufficientCapacity is the resource-admission verdict: honoring this sandbox's
// caps would oversubscribe the node. Distinct from a state conflict — nothing is
// wrong with the sandbox, the node is simply full — so the API maps it to 507.
var ErrInsufficientCapacity = errors.New("lifecycle: insufficient node capacity")

// ErrCapRequired is the resource admission verdict for a request that omits a cap on an axis
// the node meters. DELIBERATELY distinct from ErrInsufficientCapacity: nothing is
// full, so 507 would be a lie — the request is invalid for THIS engine's
// configuration, which the API maps to 400. `0 = unlimited` stays legal on any
// axis the operator has not declared a budget for.
var ErrCapRequired = errors.New("lifecycle: node meters this axis, so a cap is required")

// ErrUncappedDiskNotCappable is the resource admission refusal of a `0 → N` write on a sandbox's
// disk axis: an uncapped sandbox cannot be GIVEN a cap by a PATCH.
//
// 400 at the WRITE POINT rather than at the next placement, because accepting it bricks the
// sandbox: the cap materializes as a quota on the Overlay's storage unit, which the Rootfs subsystem builds as
// a btrfs subvolume only when a cap was present AT CREATE (`CreateOverlay` for `capByte > 0`),
// so an uncapped sandbox's upper is a plain directory tree with no quota row to write. the Rootfs subsystem
// refuses a placement it cannot enforce a cap for, so a 200 here records a cap every future
// placement refuses.
//
// DIRECTIONAL: `N → 0` and `N → M` stay legal, and only disk is affected — memory and CPU do
// not materialize as storage units.
var ErrUncappedDiskNotCappable = errors.New("lifecycle: an uncapped sandbox's disk axis cannot be capped by a PATCH")

// ErrDiskCapBelowUsage is the resource admission refusal of a disk cap that is not STRICTLY above the
// sandbox's current Overlay usage — the Rootfs subsystem twin of ErrVolumeCapBelowUsage, and the
// same 400.
//
// STRICT inequality, because `cap == usage` means FULL rather than "exactly enough":
// measured, a limit set exactly equal to the unit's `referenced` is accepted and stored, and
// re-applying or raising it afterwards both return ENOSPC. At or below usage the unit is
// wedged — file creation, `mkdir`, truncate-to-zero and the quota write itself all return
// ENOSPC, so the cap can be neither raised nor removed (the quota-lowering constraint).
//
// WORSE HERE THAN ON A VOLUME: the Rootfs subsystem re-applies the record's cap at EVERY placement and fails
// the same way on a wedged unit, so a 200 makes the sandbox permanently unresumable — the
// brick ErrUncappedDiskNotCappable prevents from the other direction. (`Mount`'s fresh-workdir
// step fails with ENOSPC too, so skipping the quota write would not resume it either.)
//
// It fires on ANY positive cap this request writes, not only a lowering: if the unit's quota
// row was lost to an external change — the case the Rootfs subsystem's per-placement re-application exists for
// — the guest can grow past the recorded cap, so even a raise can land at or below usage.
//
// It does NOT fire for a sandbox that filled its own cap the ordinary resource admission way: squota stops
// the guest with ENOSPC while usage is still strictly below the limit, so that cap can be
// re-asserted and raised (health and reclamation's resource-terminated remedy).
var ErrDiskCapBelowUsage = errors.New("lifecycle: sandbox disk cap must be strictly greater than current Overlay usage")

// deathCauseOf translates the runtime provider contract provider enum into the persisted model enum.
// Two types on purpose (the runtime provider contract): the provider seam must not depend on the
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
// implementation (the extension boundary), not editing this file.
type Deps struct {
	Store   store.Store
	Runtime runtime.Provider
	Rootfs  rootfs.Provider
	// Volume is the Volume subsystem backend (volume.Backend). REQUIRED whenever the disk axis
	// is metered, and required in practice: the resource admission disk inequality sums BOTH
	// subsystems' uncapped bytes, so a nil backend would make the gate blind to every
	// volume's bytes — and a pile of uncapped volumes could then fill the node
	// without the gate refusing once. admitVolume therefore fails CLOSED on a nil
	// backend rather than treating it as "no volumes", the same choice the gate makes
	// for an unmeasurable provider (the data-plane exposure boundary).
	Volume volume.Backend
	// Dial builds a stateless transport client for a discovered endpoint
	// (the transport data plane: constructed per endpoint, discarded on resume).
	//
	// Returns the FULL Client, deliberately, even though the lifecycle's own logic only ever
	// probes health (probeHealth / classify both narrow to transport.HealthProber at
	// the point of use). The width is here because Mediator.Client re-exports this to
	// the data plane: endpoint resolution stays inside the lifecycle, so the orchestrator
	// addresses sandboxes by SandboxID and never learns a placement (the API error contract).
	// Narrowing it would move that resolution out and leak placement; the cost is
	// that an L1 fake must satisfy all three halves.
	Dial func(transport.Endpoint) transport.Client

	// HealthTimeout bounds the cold-start readiness probe.
	HealthTimeout time.Duration

	// DrainGrace bounds the FS-first pause and resume pause drain barrier: how long a voluntary freeze
	// waits for engine-issued mutating fs RPCs to finish before freezing anyway
	// Timing out the drain is not a pause failure: it lands where a crash lands, which
	// FS-first pause and resume accepts — so this only trades "how long a pause may wait" against "how
	// likely a write is truncated". Zero disables the drain (straight to the
	// preemptive freeze); New applies defaultDrainGrace when unset.
	DrainGrace time.Duration

	// NodeCapacity is the resource admission no-oversubscribe budget the admission gate admits
	// against (the engine vocabulary boundary single-binary config). Zero on a dimension leaves that axis
	// unmetered — see NodeCapacity.
	NodeCapacity NodeCapacity

	// Deployment carries what the DEPLOYING PARTY hands the engine, as distinct from
	// what a declaration carries (the provider security contract / the API error contract): today, the uid of the host-local
	// process behind `egress.via[].endpoint`, which pins the SECOND hop.
	//
	// It is deployment input rather than a declaration field because of what the
	// predicate is: the engine still does not know or ask what sits behind that
	// endpoint (the network declaration) — only which LOCAL uid's egress must be held under the same
	// floor. With none supplied, no second-hop table is installed and `via[]`
	// declarations are refused at the write point, because recording one would leave
	// a confused deputy live while every first-hop assertion still passed.
	Deployment network.Deployment

	// DefaultIdleTimeout / DefaultRetention are the engine-wide TTL defaults
	// (health and reclamation / the engine vocabulary boundary single-binary config) applied when a CreateSpec omits them.
	// Zero here falls back to the package defaults (defaultIdleTimeout /
	// defaultRetention) so a caller that leaves Deps unset still gets a sane,
	// non-zero TTL rather than a sandbox that auto-pauses instantly.
	DefaultIdleTimeout time.Duration
	DefaultRetention   time.Duration

	// EventRetention is the transactional outbox's replayable-window size: a DELIVERED outbox row older
	// than this is trimmed. 0 = NEVER TRIM, and unlike the two TTLs above that zero is
	// NOT replaced with a package default — it is the shipped behavior.
	//
	// Not defaulted, for the same reason NodeCapacity is not: the correct value is "the
	// reconnect duration promised to external subscribers", which the engine cannot
	// know. The cost of 0 is that the outbox grows without bound at rest (the transactional outbox's
	// known limitation); the cost of a wrong non-zero default is deletion of
	// billing-relevant history on a number nobody chose.
	EventRetention time.Duration

	// Logf reports per-item reconciliation outcomes that are skipped rather than
	// fatal (atomic transition rollback startup reconcile is skip-and-log: one un-adoptable sandbox
	// must not abort recovery of the rest). Defaults to log.Printf; tests inject a
	// capturing sink.
	Logf func(format string, args ...any)

	// FaultAfterColdStart is a TEST-ONLY injection hook: if non-nil it runs
	// right after coldStart records the live placement but BEFORE the committing
	// CAS to `running`. A test panics/exits from it to simulate the orchestrator
	// crashing mid-transition — leaving a fully-built placement while the store
	// still reads the SOURCE state (`paused` for resume / `creating` for create).
	// This manufactures the atomic transition rollback crash scenarios (orphan half-build + surviving
	// container with lost handle) that Reconcile must clean up. nil in production;
	// it carries NO production semantics.
	FaultAfterColdStart func(id model.SandboxID)

	// FaultAfterVolumeBytes is a TEST-ONLY injection hook: if non-nil it runs inside
	// snapshot/clone, after the backend has produced the bytes AND written the cap, but
	// BEFORE the `volumes` row is inserted. A test panics/exits from it to manufacture
	// the Volume subsystem crash window — bytes on disk, no row — and to prove the cap was written
	// by the backend operation rather than by a follow-up the crash would have skipped
	// (volume cap enforcement). nil in production; it carries NO production semantics.
	FaultAfterVolumeBytes func(id model.VolumeID)
}

// Mediator holds per-sandbox transition locks (per-sandbox transition serialization).
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
	// a pause cannot land mid-write and truncate a file; see
	// barrier.go). Separate from the transition lock: that serializes VERBS
	// against each other, this one serializes a verb against in-flight DATA-PLANE
	// work, which never takes the transition lock at all.
	barrier *barrier

	// admitMu serializes the resource admission no-oversubscribe gate, which the per-sandbox transition lock
	// (per-sandbox transition serialization) cannot do: the invariant is node-GLOBAL (a sum over every record) and `Create`
	// mints a fresh SandboxID, so two concurrent creates take different locks, both read the
	// store before either inserts, both pass, and the node lands over its declared budget with
	// every individual cap honored.
	//
	// Same shape as health and reclamation's reference-reclaim invariant — one serializing authority for a
	// read-then-decide-vs-concurrent-insert race — but a mutex rather than the store's atomic
	// commit, because the decision spans a provider measurement the store cannot see.
	//
	// Held only across {read records → measure → compare → commit}, never across a cold start,
	// so it does not couple unrelated sandboxes' launch latency.
	admitMu sync.Mutex

	// live holds the per-sandbox liveState (placement handle + mounted merged
	// rootfs). In-memory and non-authoritative: placement-local state (the ID-reference model,
	// rebound every resume), NOT persistent SoT. Crash reconciliation (atomic transition rollback)
	// rebuilds the equivalent by scanning host resources against the authoritative
	// store.
	live map[model.SandboxID]*liveState

	// buildingLayers is the in-flight-Promote keep-set: TemplateIDs whose
	// layer bytes are being written to the backend but whose `templates` row has
	// not been inserted yet (bytes-first, the Rootfs subsystem). The orphan-layer sweep must skip
	// these — "bytes on disk, no row" is a Promote in progress, not an orphan
	// (the placement live-map's twin, atomic transition rollback). A crashed Promote's staging layer
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

	// inFlightWrites counts direct content writes in flight per volume (the Volume subsystem write
	// regime). Published in the SAME critical section that decides "no live rw
	// holder" — see admitDirectWrite for why the two are inseparable. Guarded by mu,
	// the same lock the live-map uses, so no new lock class enters the order.
	inFlightWrites map[model.VolumeID]int

	// buildingVolumes is the in-flight-create keep-set for volume storage (the Volume subsystem). Its
	// hazard is stronger than buildingOverlays': volume creation is BYTES-FIRST (the ID
	// must name the storage before any row can exist), so a perfectly healthy in-flight
	// create presents as "bytes, no row" even to a completely fresh snapshot.
	// Guarded by mu.
	buildingVolumes map[model.VolumeID]bool

	// pendingUncappedBytes is in-flight growth of the resource admission accounting formula's
	// MEASURED term: bytes a gated control-plane action has been admitted to add
	// but that the provider's measurement cannot see yet (a Promote mid-copy). The
	// admission gate adds it to the left side so a concurrent run cannot spend the
	// same capacity twice. Guarded by mu; see reserveUncapped for why a reservation
	// rather than simply holding admitMu across the action.
	pendingUncappedBytes int64
}

// liveState is the mediator's in-memory handle to a running sandbox's
// placement-local resources (the ID-reference model): the runtime placement + the mounted merged
// overlay. Discarded on pause, rebuilt on resume.
type liveState struct {
	placement *runtime.Placement
	merged    *rootfs.Merged
	// mounts are the Volume subsystem materializations this placement is using, held so pause and
	// destroy can Release each one. The MOUNT ROWS are not here — those are persistent
	// control-plane state that survives pause (a resume re-mounts from the CURRENT
	// table, resource admission); these are the placement-local handles, discarded with the placement.
	mounts []*volume.Materialized
}

// TTL defaults (health and reclamation / the health and reaper boundary): conservative, revisable numeric knobs
// noted at their config site. They bound no invariant's shape — only how long
// an idle `running` / a `paused` sandbox lingers before the reaper acts. A
// create that omits them, and a Deps that leaves the Default* fields zero, both
// resolve to these.
const (
	defaultIdleTimeout = 30 * time.Minute // running idle → auto-pause
	defaultRetention   = 24 * time.Hour   // paused → evict

	// defaultDrainGrace bounds the pause drain. Seconds, not minutes:
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
		buildingVolumes:  make(map[model.VolumeID]bool),
		inFlightWrites:   make(map[model.VolumeID]int),
	}
}

// transitionLock is one sandbox's transition mutex (per-sandbox transition serialization) plus the refcount
// that decides when its map entry may be dropped.
//
// The naive reclamation — delete the entry when a verb finishes, or on destroy — is a
// CORRECTNESS BUG: a verb already blocked in mu.Lock() holds a pointer to this mutex, so
// deleting the entry lets the NEXT verb mint a fresh one and the two then serialize on
// DIFFERENT mutexes, both "holding the sandbox's transition lock" concurrently (per-sandbox transition serialization).
//
// refs counts outstanding lock() handouts, not holders. Incremented under m.mu in the SAME
// critical section that finds-or-creates the entry, so no caller is handed a pointer to an
// entry being concurrently deleted; decremented only after the mutex is released, so a blocked
// waiter always keeps refs > 0. refs == 0 therefore means nobody holds it, nobody is blocked on
// it, and nobody is between lock() and Lock() — at which point deleting it is unobservable.
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

// Unlock releases the transition lock AND the ref the matching lock() took, in
// that ORDER: the mutex first so a blocked waiter can proceed, and only then the
// ref (see transitionLock's refs).
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
		// under a holder and silently break per-sandbox transition serialization. Fail loudly instead.
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
// (per-sandbox transition serialization: all lifecycle verbs for the SAME sandbox serialize on it).
//
// The returned handle carries a ref that MUST be released exactly once, which
// Unlock does — so the standard shape
//
//	l := m.lock(id); l.Lock(); defer l.Unlock()
//
// is complete: no separate release call, and the deferred Unlock covers every
// return path. The one shape that leaks is calling lock() and never unlocking —
// see tryLock, the only caller that can reach it.
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
// (per-sandbox transition serialization). Returns (mutex, true) with the lock HELD on success — the caller
// must Unlock it. Returns (nil, false) if a verb already holds it: for the
// reaper and periodic reconcile, a held lock means "a verb is driving this
// sandbox (in-flight resume / user pause / …)", so we SKIP it this round rather
// than misclassify it (reaper serialization: an in-flight resume has L2 down and
// would classify dead-recoverable; skipping is the single-flight guard, adding
// no the lifecycle state machine state). This is the ONLY non-blocking acquire; every user verb takes
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

// CreateSpec is the create verb input (the control-plane API POST /v1/sandboxes).
type CreateSpec struct {
	Template model.TemplateID

	// Network is the network declaration reachability declaration (the control-plane API `network?`). nil = the whole
	// object was OMITTED, which is legal and normalizes to an explicit
	// `{deny, [], [], []}` written into the record HERE, at the write point (the network declaration).
	//
	// A POINTER because the two cases must stay distinguishable all the way down:
	// "omitted" is legal, while "given but missing `egress.default`" is the API error contract's first
	// 400 class — collapsing them lets whichever is handled first eat the other.
	//
	// Bound at CREATE through this control-plane path, the flow SHAPE the API error contract requires
	// (see Volumes below).
	Network *model.NetworkDecl

	// IdleTimeout / Retention are the OPTIONAL TTL Binding inputs (resource admission / health and reclamation;
	// the control-plane API POST /v1/sandboxes idle_timeout?/retention?). nil = omitted → the
	// engine-wide default (Deps.Default*) is materialized into the stored record
	// at create time, so the reaper always reads a concrete duration, never a
	// sentinel. A non-nil pointer is honored verbatim (including an explicit
	// zero, which the reaper treats as "no such deadline" — see the reaper's
	// TTL handling).
	IdleTimeout *time.Duration
	Retention   *time.Duration

	// Volumes is the volume-binding input (the control-plane API POST /v1/sandboxes `volumes?`): the
	// COMPLETE set of mounts this sandbox is created with.
	//
	// Bound at CREATE and injected through this control-plane path, which the API error contract requires
	// as a flow SHAPE rather than as a timing constraint: a later warm-pool (late
	// binding) reuses the same injection point, so the shape must not be short-circuited
	// now even though nothing needs it yet.
	//
	// Each entry's Sandbox field is IGNORED and overwritten — the ID does not exist
	// until Create mints it, so accepting one from the caller could only be wrong.
	Volumes []model.Mount

	// Limits is the OPTIONAL resource-limit Binding input (resource admission; the control-plane API POST
	// /v1/sandboxes cpu_millis?/memory_byte?/disk_byte?). Zero on an axis =
	// unlimited there. Unlike the TTLs there is no engine-wide default to
	// substitute: "unlimited" is a meaningful, honest resting value for a cap,
	// whereas a made-up default cap would silently constrain workloads the
	// operator never limited.
	Limits model.Limits
}

// Create runs the cold-start chain for a fresh sandbox (the cold-start sequence,
// create prefix: build empty Overlay, then ). The transition lock is held
// for the whole chain (per-sandbox transition serialization); on failure the half-built placement + empty
// Overlay are reclaimed and the record deleted → destroyed (the subsystem boundaries create exit).
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
		// Materialize the TTL Binding once, at create (resource admission: Binding is stable
		// across resume; the stored value is the concrete duration the reaper
		// reads). Omitted inputs resolve to the engine-wide default here.
		IdleTimeout: m.deps.DefaultIdleTimeout,
		Retention:   m.deps.DefaultRetention,
		// Resource limits are materialized into the record the same way and for the
		// same reason (resource admission Binding): every placement re-reads them from here, so
		// the stored value is the single authority for all three axes. Omitted =
		// zero = unlimited on that axis.
		Limits: spec.Limits,
		// The network declaration, materialized into the record the same way and for the
		// same reason (resource admission Binding): every placement regenerates its rule set from
		// here. An OMITTED `network` is normalized to an explicit {deny, [], [], []}
		// AT THIS WRITE POINT (the network declaration) — so the record always holds a chosen value, and
		// "a null cannot be told from a choice" is structurally impossible rather than
		// merely unlikely. `deny` because it is the fail-closed side, and because it is
		// exactly the minimal declaration.
		Network:   network.Zero(),
		CreatedAt: now, UpdatedAt: now,
	}
	if spec.Network != nil {
		rec.Network = spec.Network.Normalize()
	}
	// the API error contract's declaration refusals, applied AT THE WRITE POINT rather than only at the
	// API layer: a validation that lives in one caller is a validation the next caller
	// does not have. Not hypothetical — a substrate test driving the mediator directly
	// successfully created a sandbox whose `via[].endpoint` was inside the L0 floor
	// (the API error contract ClassViaEndpointInFloor), and then measured the resulting permanent fail-closure.
	//
	// BEFORE the admission gate below, which the API error contract requires: an invalid declaration is
	// refused before any byte is touched or any budget recorded, so 400 and 507 can
	// never both be true for one request.
	if err := m.ValidateDeclaration(rec.Network, spec.Network != nil); err != nil {
		return "", err
	}
	if spec.IdleTimeout != nil {
		rec.IdleTimeout = *spec.IdleTimeout
	}
	if spec.Retention != nil {
		rec.Retention = *spec.Retention
	}

	// Admission gate (resource admission / uncapped rootfs accounting node-level no-oversubscribe): the
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
	// an externally-visible the lifecycle state machine transition — it publishes nothing (nil Event).
	// The "created" event is emitted only when create COMPLETES → running below.
	// Inserting it here also makes this promise visible to the next gate run.
	if err := m.deps.Store.CreateSandbox(ctx, rec, nil); err != nil {
		m.admitMu.Unlock()
		return "", fmt.Errorf("create: insert record: %w", err)
	}
	m.admitMu.Unlock()

	// Mount rows go in AFTER the record (mounts.sandbox FK needs it) and BEFORE the
	// cold start (which reads the table to materialize). Inside the transition lock,
	// like every other step of the verb.
	//
	// The store — not this function — arbitrates the exclusive lease, in one statement.
	// A pre-check here could not work: Create just minted a fresh SandboxID, so two
	// concurrent creates hold two DISJOINT transition locks and nothing serializes them
	// (measured; store.Store.CreateMounts states it in full).
	if len(spec.Volumes) > 0 {
		binding := make([]model.Mount, 0, len(spec.Volumes))
		for _, mt := range spec.Volumes {
			binding = append(binding, model.Mount{
				Sandbox: sid, // overwritten deliberately: the ID did not exist until now
				Volume:  mt.Volume,
				Path:    store.NormalizeMountPath(mt.Path),
				Access:  mt.Access,
			})
		}
		if err := m.deps.Store.CreateMounts(ctx, binding); err != nil {
			// Nothing is built yet, so this needs no unwind beyond dropping the record —
			// and the mount rows are all-or-nothing, so there are none to remove.
			_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
			return "", fmt.Errorf("create: bind volumes: %w", err)
		}
	}

	//  build empty Overlay over the resolved lineage lower stack (the Rootfs subsystem
	// multi-lowerdir) + run the cold-start chain.
	lower, err := m.resolveLower(ctx, spec.Template)
	if err != nil {
		// Mount rows are already in, so they must go before the record or RESTRICT
		// blocks it — the same ordering destroy uses.
		_ = m.deps.Store.DeleteMounts(ctx, sid)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: resolve lineage: %w", err)
	}
	// The capacity cap is applied HERE, as the Overlay is allocated: it is an FS
	// quota on the upper+work storage unit, so it belongs to the Rootfs subsystem's
	// materialization, not to the runtime provider (resource admission / the Rootfs subsystem). The mediator only
	// passes the numeric intent across the seam.
	// Protect the unit from the orphan-unit sweep for the whole create window, held
	// until create settles either way (atomic transition rollback; see buildingOverlays for the race).
	m.markBuildingOverlay(oid)
	defer m.unmarkBuildingOverlay(oid)
	ov, err := m.deps.Rootfs.CreateOverlay(ctx, string(oid), lower, rec.Limits.DiskByte)
	if err != nil {
		// This branch is BEFORE the cold start, so `unwind` has not run and the mount
		// rows are still in place — same required ordering as above.
		_ = m.deps.Store.DeleteMounts(ctx, sid)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		// the Rootfs subsystem's strict-inequality guard (rootfs.ErrCapBelowUsage) is reached on THIS path
		// only when the requested cap is at or below the storage unit's own metadata floor
		// (measured: an empty unit already charges 16 KiB) — i.e. the caller asked for a cap
		// that is unenforceable from the unit's first instant. Create CARRIES `disk_byte`, so
		// the verdict is a property of this request and belongs on the write-point sentinel
		// (400), not on the placement-side one (409, whose remedy is to change state
		// elsewhere). Same split as the control-plane API already makes for the second-hop uid: a 400 at the
		// write point that carries the field, a 409 at the placement that does not.
		if errors.Is(err, rootfs.ErrCapBelowUsage) {
			return "", fmt.Errorf("create: %w: a %d-byte disk_byte is at or below what the Overlay storage unit costs empty, so the cap could not be enforced from the sandbox's first instant: %w",
				ErrDiskCapBelowUsage, rec.Limits.DiskByte, err)
		}
		return "", fmt.Errorf("create: overlay: %w", err)
	}
	pl, err := m.coldStart(ctx, rec, ov)
	if err != nil {
		// create has no fallback state: reclaim half-built placement + empty
		// Overlay, then delete the record. The
		// sandbox was never running, so this unwind-delete publishes nothing.
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
		return "", fmt.Errorf("create: cold start: %w", err)
	}

	// fault injection: simulate a crash after the placement is fully built but
	// before the committing CAS — store still reads `creating` (atomic transition rollback create var).
	if m.deps.FaultAfterColdStart != nil {
		m.deps.FaultAfterColdStart(sid)
	}

	// The readiness probe passed: flip creating to running and emit "created" in the
	// same commit (the transactional outbox).
	if err := m.deps.Store.CompareAndSetState(ctx, sid, model.Creating, model.Running, newPlacementEvent(sid, model.EventCreated, model.Running, pl.Address), nil); err != nil {
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
		return "", fmt.Errorf("create: commit running: %w", err)
	}
	return sid, nil
}

// Pause = quiesce, then cleanly stop the container (FS-first pause and resume). Stopping releases
// RAM/CPU and the placement — a paused sandbox occupies storage only (FS-first pause and resume).
// CAS-guarded: requires `running`. See pauseLocked for the ordering.
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

// pauseLocked commits a running→paused transition with the given event Kind and releases the
// sandbox's placement. CALLER MUST HOLD the transition lock (per-sandbox transition serialization). Shared by the user Pause
// verb ("paused") and the reaper's auto-pause / crash-pause: identical transition + placement
// release, the Kind carrying WHY (the lifecycle state machine). Pause always lands a legal `paused` (the transition contract: no middle
// dirty state), so the CAS comes first and placement-local resources are released after — the
// Overlay is already on the backend and captured NOTHING (FS-first pause and resume). Stop is best-effort and
// idempotent, so it also fits a crash-pause whose container is already dead, reclaiming the
// lingering netns.
//
// cause carries the health and reclamation death cause for the INVOLUNTARY variants, committed in the SAME store
// call as the demotion CAS (health and reclamation / the transactional outbox). nil for the voluntary ones, which are not deaths
// and must leave any previously recorded cause alone.
func (m *Mediator) pauseLocked(ctx context.Context, id model.SandboxID, kind model.EventKind, cause *model.DeathCause) error {
	// Drain engine-issued mutating fs RPCs before freezing, for the VOLUNTARY variants only
	// (FS-first pause and resume / the lifecycle contract). Both crash-pause paths skip it because both have L2 down, so no mutating
	// fs RPC can still complete: draining a dead unit only delays recording a death already
	// observed, and draining the lifecycle contract transport-down escalation (L1 alive, transport wedged)
	// stalls the sweep on RPCs that cannot finish — the wedge the escalation exists to break.
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
		// Release each volume materialization, and note what is NOT done here: THE MOUNT
		// ROWS STAY, so the exclusive lease is NOT released by a pause (the Volume subsystem — the lease
		// is the row's existence), symmetric to the Overlay's zero-capture. The resume
		// re-materializes from the table.
		m.releaseMounts(ctx, ls.mounts)
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	return nil
}

// Resume = cold-start a NEW container on the same Overlay artifact, new netns
// IP (FS-first pause and resume / the ID-reference model). Same cold-start chain as create minus the empty-Overlay
// prefix (the cold-start sequence). CAS precondition: requires `paused`. The lock is
// held for the whole chain; the flip to `running` happens only after readiness.
// On failure, unwind the half-built Placement and stay `paused`.
func (m *Mediator) Resume(ctx context.Context, id model.SandboxID) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	// CAS precondition check (read side): must be paused. The actual state flip
	// is deferred until readiness; the lock (not the CAS) holds off concurrent verbs for
	// the whole cold-start window (per-sandbox transition serialization).
	if rec.State != model.Paused {
		return fmt.Errorf("resume: %w (state=%s, want paused)", store.ErrConflict, rec.State)
	}
	return m.resumeLocked(ctx, rec)
}

// resumeLocked cold-starts a NEW container on the sandbox's Overlay (new IP,
// the ID-reference model) and flips paused to running after readiness. The caller must hold the transition lock and
// have verified the source state is `paused` (per-sandbox transition serialization). Shared by the user Resume
// verb and the reaper's transport-down crash-loop escalation. After
// crash-pause, the same lock resumes to break the wedge). On failure it unwinds
// the half-built Placement and leaves the record `paused`.
func (m *Mediator) resumeLocked(ctx context.Context, rec model.Sandbox) error {
	id := rec.ID
	// health and reclamation resource-terminated: this sandbox was terminated for exceeding its RAM
	// safety limit, and that disposition is the OPPOSITE of dead-recoverable's —
	// resuming re-runs the same workload into the same cap, i.e. a crash loop. It
	// stays `paused` until the cap is raised (the control-plane API PATCH, which clears the cause in
	// the same commit) or it is destroyed. This is exactly why the death cause has
	// to be persisted (health and reclamation): the state alone cannot distinguish the two cases.
	//
	// The check lives HERE rather than in the Resume verb so every path that performs a
	// resume passes it, including the reaper's transport-down escalation, which
	// cannot itself carry this cause (it fires only when L1 is ALIVE, and a `running`
	// record cannot hold CauseResourceExceeded).
	if rec.DeathCause == model.CauseResourceExceeded {
		return fmt.Errorf("resume: %w: sandbox was resource-terminated (RAM cap %d bytes) — resuming would re-trigger termination. Raise memory_byte via PATCH /v1/sandboxes/%s (re-runs the admission gate; may be refused for node capacity), which clears the death cause and permits resume; or Promote it to keep its bytes under a new sandbox; or destroy it (health and reclamation)",
			ErrResourceTerminated, rec.Limits.MemoryByte, id)
	}
	// Admission gate before anything is materialized (resource admission no-oversubscribe). This
	// sandbox's own promised cap is EXCLUDED from the committed total — it is
	// already counted there, and double-counting it would make a resume of an
	// unchanged sandbox fail against its own reservation. On failure the record
	// stays `paused`, the atomic fallback source state (atomic transition rollback).
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
	//  re-open + re-mount the SAME Overlay artifact (resume prefix), re-resolving
	// the lineage lower stack from the store (the Rootfs subsystem multi-lowerdir).
	lower, err := m.resolveLower(ctx, rec.Template)
	if err != nil {
		return fmt.Errorf("resume: resolve lineage: %w", err)
	}
	// Re-apply the capacity cap from the CURRENT record (resource admission Binding): a resume
	// re-materializes the quota rather than trusting whatever the subvolume was
	// left carrying, so an edited cap takes effect and a lost quota is restored.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), lower, rec.Limits.DiskByte)
	if err != nil {
		return fmt.Errorf("resume: open overlay: %w", err)
	}
	pl, err := m.coldStart(ctx, rec, ov)
	if err != nil {
		// resume has a fallback state: reclaim half-built placement, stay paused
		// (persistent Overlay artifact untouched; atomic transition rollback).
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: cold start: %w", err)
	}

	// fault injection: simulate a crash after the placement is fully built but
	// before the committing CAS — store still reads `paused` (atomic transition rollback resume var).
	if m.deps.FaultAfterColdStart != nil {
		m.deps.FaultAfterColdStart(id)
	}

	//  readiness passed → flip paused→running (CAS) + emit "resumed" (the transactional outbox).
	// CLEAR the death cause in this same commit: the sandbox is up, so its previous
	// death no longer explains its state. This is also what bounds the health and reclamation
	// self-correcting retry — an Unknown death that resumes fine does not stay
	// pinned to a stale cause, and if the workload re-triggers the cap the NEXT
	// observation writes the real cause in the control plane's plain view.
	cleared := model.CauseNone
	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Paused, model.Running, newPlacementEvent(id, model.EventResumed, model.Running, pl.Address), &cleared); err != nil {
		m.unwind(ctx, id, ov, false)
		return fmt.Errorf("resume: commit running: %w", err)
	}
	return nil
}

// Destroy = terminal `destroyed` (the lifecycle state machine). Valid from running or paused;
// idempotent (already-gone record → nil). The lock held here is the same one
// resume holds for its whole chain, so destroy CANNOT delete an Overlay
// mid-cold-start (per-sandbox transition serialization: closes the destroy-vs-resume data race).
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
	// CauseRequested: this is the operator's verb. The reaper's evict reaches
	// destroyLocked with the other cause (health and reclamation), and those are the only two routes —
	// atomic transition rollback's protect-on-uncertainty means no health verdict destroys.
	return m.destroyLocked(ctx, rec, model.CauseRequested)
}

// destroyLocked marks a sandbox `destroyed` and reclaims its bytes + placement. CALLER MUST HOLD
// the transition lock (per-sandbox transition serialization). Shared by the user Destroy verb (valid from running or paused)
// and the reaper's evict (a paused sandbox past retention). Marking the record destroyed is
// decoupled from byte reclamation: the Overlay is deleted synchronously here, and a crash between
// the two leaves it an orphan for SweepOrphanOverlays (health and reclamation/atomic transition rollback).
//
// Kind "destroyed" matches the lifecycle state machine — evict is "the reaper's automatic destroy", the SAME terminal.
//
// NO SOURCE-STATE CAS here, unlike pause/resume: destroy is valid from BOTH `running` and
// `paused` (the lifecycle state machine / atomic transition rollback) and idempotent, so there is no single "expected" source state. The
// invariant is LOCK-enforced instead — the held transition lock serializes destroy against
// every other verb, and the terminal DeleteSandbox is idempotent (ErrNotFound → already gone).
//
// A `creating` record IS reachable here: "Create holds the lock for its whole chain" holds
// only within one process lifetime, so a crash mid-create leaves a `creating` row that
// survives if reconcile's cleanup delete also fails. Such a record never emitted `created`, so
// it must not emit `destroyed` — see the Kind selection below.
func (m *Mediator) destroyLocked(ctx context.Context, rec model.Sandbox, cause model.EventCause) error {
	id := rec.ID
	// tear down any live placement + mount (running case).
	ls := m.take(id)
	if ls != nil {
		if ls.placement != nil {
			_ = m.deps.Runtime.Destroy(ctx, ls.placement)
		}
		m.releaseMounts(ctx, ls.mounts)
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	// Delete the mount rows BEFORE the sandbox record, and the order is REQUIRED, not
	// stylistic (measured): mounts.sandbox RESTRICT fires IMMEDIATELY, so leaving the
	// rows would make DeleteSandbox below fail with ErrSandboxHasMounts and strand an
	// undeletable record. Wrapping both in one transaction would not help, and
	// `defer_foreign_keys` is rejected for a store-level reason (see
	// store.Store.DeleteMounts).
	//
	// THIS IS THE LEASE RELEASE POINT (the Volume subsystem): the lease is the row's existence, so the
	// rows going away IS the release — including the reaper's evict of a paused sandbox
	// past retention, which reaches this same function.
	//
	// A failure here is returned rather than swallowed: unlike byte reclamation (which
	// the transition contract lets the sweep retry), an undeleted mount row BLOCKS the record delete, so
	// proceeding would just fail one line later with a less informative error.
	if err := m.deps.Store.DeleteMounts(ctx, id); err != nil {
		return fmt.Errorf("destroy: delete mount rows: %w", err)
	}
	// reclaim the Overlay artifact (byte deletion; the lifecycle state machine destroy). Byte reclaim
	// uses only the OverlayID (DestroyOverlay derives StateDir/<id>); the lower
	// stack is irrelevant here (no mount), so a single-element stack suffices and
	// avoids a lineage walk that could fail mid-teardown.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
	if err == nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
	// delete the record → destroyed + emit "destroyed" in the same commit
	// (the transactional outbox). The outbox row outlives the deleted record.
	//
	// EXCEPT from `creating`, which publishes nothing: that record never emitted
	// `created` (Create publishes only once the chain commits), so a `destroyed`
	// here would be an unpaired terminal — a billing subscriber would be told a
	// sandbox ended that it never saw begin. Reconcile suppresses it for the same
	// reason when it cleans up a crashed create.
	var ev *store.Event
	if rec.State != model.Creating {
		// The cause rides the event because it CANNOT be asked for afterwards: the row
		// is deleted in this same commit, so a subscriber's follow-up GET is a 404 and
		// "was this a delete or a TTL?" becomes permanently unanswerable (the transactional outbox).
		ev = newCausedEvent(id, "destroyed", model.Destroyed, cause)
	}
	if err := m.deps.Store.DeleteSandbox(ctx, id, ev); err != nil {
		return fmt.Errorf("destroy: delete record: %w", err)
	}
	return nil
}

// Promote freezes a sandbox's current Overlay into a new Template layer (the Template and Overlay model /
// the Rootfs subsystem). It accepts a `running` or `paused` source; a `running` source is
// first quiesced to a consistent at-rest point via pauseLocked (equivalent to a
// pause, FS-first pause and resume) and LEFT paused (the caller resumes on demand). The whole
// operation holds the source's transition lock (per-sandbox transition serialization), so it serializes
// against concurrent destroy/resume — destroy cannot delete the Overlay bytes
// mid-freeze.
//
// bytes-first (health and reclamation / the Rootfs subsystem): the new layer bytes are written FIRST
// (PromoteToTemplate), then the `templates` row is inserted. The layer id is
// registered in buildingLayers for the freeze window so a concurrent periodic
// sweep does not reclaim the in-flight layer (atomic transition rollback). A crash after the bytes are
// written but before the row insert leaves an orphan layer the sweep reclaims —
// never a row pointing at nonexistent bytes.
//
// Returns the new TemplateID. The new Template's parent is the source sandbox's
// current Template, so lineage is the source's stack + this frozen layer on top
// (the Rootfs subsystem multi-lowerdir).
//
// ADMISSION (resource admission / uncapped rootfs accounting): a frozen layer is charged to no sandbox cap, so it
// grows the MEASURED term and must clear the same inequality as a create — refused with
// ErrInsufficientCapacity (507), the harm being an UNRELATED sandbox meeting ENOSPC nowhere
// near its own cap. Gated through admitUncappedGrowth, never `admit` (admission.go).
//
// The gate runs BEFORE the quiesce, paying in forecast accuracy rather than in the state
// machine: gating after it buys an exact count at the price of A REFUSAL THAT MUTATED STATE —
// a running sandbox left `paused`, with a `paused` event published, because the node was full.
// The source is therefore still live when measured, so the number is an upper bound bounded by
// that sandbox's own headroom.
//
// A refused Promote leaves the sandbox in its ENTRY state, no layer bytes, no `templates` row.
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

	// Depth gate, BEFORE the quiesce and before any bytes — for the same reason the
	// capacity gate below comes first (the Template and Overlay model: a refused Promote is not an observable
	// transition). The new layer's parent is the source's Template, so the stack it
	// produces is one deeper than the source's; refusing here is what stops a Promote
	// from succeeding into a Template that can never be mounted (checkLowerDepth).
	srcLower, err := m.resolveLower(ctx, rec.Template)
	if err != nil {
		return "", fmt.Errorf("promote: %w", err)
	}
	if err := m.checkLowerDepth(ctx, len(srcLower)+1); err != nil {
		return "", fmt.Errorf("promote: %w", err)
	}

	// open the source Overlay by ID (byte read only, no mount). Needed by the gate
	// below as well as the freeze, so it is opened before either.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
	if err != nil {
		return "", fmt.Errorf("promote: open overlay: %w", err)
	}

	// This sandbox's volume mount destinations, read from the mount table the mediator
	// already holds. They are passed to BOTH Promote calls below, and the same slice is
	// used for each: the forecast must model exactly what the freeze will do, or it
	// charges shadowed bytes that are never copied and refuses Promotes against
	// capacity that was never going to be consumed (rootfs mount constraints).
	//
	// This is also why the set is a PARAMETER: the Rootfs subsystem can never ask the Volume subsystem for it, so no
	// subsystem edge appears (the lifecycle) — the mediator holds the table and threads it through.
	excludedMounts, err := m.mountDests(ctx, id)
	if err != nil {
		return "", fmt.Errorf("promote: read mount table: %w", err)
	}

	// Admission gate, before the quiesce and before any bytes (see the doc above).
	// Under admitMu for the same reason create is (see the field). The reservation
	// published inside the critical section is what makes this Promote's claim visible
	// to the next gate run before its bytes exist.
	cost, err := m.deps.Rootfs.PromoteCostBytes(ctx, ov, excludedMounts)
	if err != nil {
		// Unmeasurable → refuse, not admit blind: same fail-closed choice the gate
		// makes for UncappedBytes, for the same reason (the data-plane exposure boundary).
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
		// quiesce to a consistent at-rest point (equivalent to pause, FS-first pause and resume) and
		// stay paused. Uses the "paused" Kind — from the event stream's view this
		// is an ordinary pause; Promote is not a lifecycle state machine lifecycle transition.
		if err := m.pauseLocked(ctx, id, "paused", nil); err != nil {
			return "", fmt.Errorf("promote: quiesce: %w", err)
		}
	}

	newID := model.TemplateID("tmpl_" + randToken())

	// register the in-flight layer BEFORE writing bytes so a concurrent sweep
	// protects it (atomic transition rollback). Unregister in all exit paths.
	m.markBuilding(newID)
	defer m.unmarkBuilding(newID)

	// freeze the at-rest upper into the new layer dir (bytes-first).
	if err := m.deps.Rootfs.PromoteToTemplate(ctx, ov, string(newID), excludedMounts); err != nil {
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

// markBuilding / unmarkBuilding manage the atomic transition rollback in-flight-Promote keep-set.
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

// coldStart runs (mount)(launch)(health) for a given Overlay handle,
// shared by create and resume (the cold-start sequence). On success the live
// placement + merged are recorded in the mediator and the placement is returned
// so its portable facts can be committed with the transition. On any error it
// returns nil (the caller unwinds).
func (m *Mediator) coldStart(ctx context.Context, rec model.Sandbox, ov *rootfs.Overlay) (*runtime.Placement, error) {
	id, limits := rec.ID, rec.Limits
	//  the write-regime gate, cold-start side (the Volume subsystem, symmetric with
	// admitDirectWrite): if a direct content write is streaming into a volume this
	// placement would mount READ-WRITE, refuse. Gating only the direct-write side
	// would leave the hazard fully open in the other order — an import admitted at T0
	// plus this cold start completing at T1 is the two concurrent writers the Volume subsystem
	// forbids, and no `paused` state appears anywhere in that story.
	//
	// FIRST, before the overlay is even mounted: a refusal that has built nothing is
	// the atomic transition rollback "atomic return to the source state" for free — resume stays `paused`,
	// create deletes its record — and there is nothing to unwind.
	//
	// The rows are read here and read AGAIN by materializeMounts below. That is not a
	// TOCTOU: the caller holds this sandbox's transition lock for the whole chain
	// (per-sandbox transition serialization) and Rebind — the only writer of this sandbox's rows — takes the same
	// lock, so the two reads cannot disagree.
	rows, err := m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	if err := m.admitColdStartRW(id, rows); err != nil {
		return nil, err
	}
	//  mount lower(Template)/upper/work → merged.
	merged, err := m.deps.Rootfs.Mount(ctx, ov)
	if err != nil {
		return nil, fmt.Errorf("mount overlay: %w", err)
	}
	// b materialize this sandbox's Volumes from the CURRENT mount table (the Volume subsystem).
	//
	// Reading the table here rather than taking it as an argument is what makes the
	// mount relationship a Binding (resource admission): create and resume run the same code, and a
	// resume therefore re-mounts per whatever the table says NOW — not per whatever it
	// said when the sandbox was last running, and not from any frozen byte.
	mounts, specs, err := m.materializeMounts(ctx, id)
	if err != nil {
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return nil, err
	}
	// b derive the network declaration rule set from the CURRENT record (resource admission Binding: exactly like
	// the caps, it is re-derived at every placement rather than read back from a
	// frozen value, so a declaration edited while paused takes effect on this resume).
	//
	// A rule-rendering failure fails the COLD START, and there is no "launch it without
	// the rules" branch — that would be the fail-open window the provider security contract forbids, and the provider security contract is
	// explicit that escape is a FAILURE rather than a degradation. create deletes its
	// record, resume stays `paused` (atomic transition rollback).
	steering, err := m.steeringFor(rec)
	if err != nil {
		m.releaseMounts(ctx, mounts)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return nil, err
	}
	//  launch runsc (sandboxd entrypoint),  guest boots,  IP discovered.
	// limits come from the RECORD, re-materialized at every placement (resource admission Binding,
	// like the declaration above). The capacity axis is NOT here — it is an FS quota
	// applied by the Rootfs subsystem (the Rootfs subsystem), not a provider concern (resource admission).
	//
	// The provider installs the rule set INSIDE this call, between building the
	// netns/veth and starting the isolated unit — the atomicity the provider security contract requires, and
	// the only hook point that exists for it (the network enforcement locus).
	pl, err := m.deps.Runtime.Launch(ctx, runtime.LaunchSpec{
		Sandbox:  string(id),
		Rootfs:   runtime.RootfsSource{MergedPath: merged.MergedPath},
		Mounts:   specs,
		Steering: steering,
		Limits: runtime.ResourceLimits{
			CPUMillis:  limits.CPUMillis,
			MemoryByte: limits.MemoryByte,
		},
	})
	if err != nil {
		m.releaseMounts(ctx, mounts)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return nil, fmt.Errorf("launch: %w", err)
	}
	if pl == nil || !pl.Address.IsValid() {
		if pl != nil {
			_ = m.deps.Runtime.Destroy(ctx, pl)
		}
		m.releaseMounts(ctx, mounts)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return nil, fmt.Errorf("launch: provider returned a placement without a valid guest address")
	}
	// Record the live placement the MOMENT it physically exists — before the
	// readiness gate, not after it. A concurrent periodic reconcile that started
	// after our create/resume was snapshotted would otherwise see this freshly
	// launched netns/container as an orphan (not in `recs`, not yet in the
	// live-map) and reclaim it during the awaitHealthy window. Seeding it here
	// puts it in liveIDs() so the periodic keep-set protects it (atomic transition rollback). The
	// transition lock is held for the whole chain, so this is not visible to a
	// user verb. On a health-gate failure we take() it back out and tear down.
	m.put(id, &liveState{placement: pl, merged: merged, mounts: mounts})
	// Poll the Health RPC to first success. Bare TCP acceptance is not readiness.
	if err := m.awaitHealthy(ctx, pl); err != nil {
		m.take(id) // un-record before teardown so a racing reconcile sees no live handle
		_ = m.deps.Runtime.Destroy(ctx, pl)
		m.releaseMounts(ctx, mounts)
		_ = m.deps.Rootfs.Unmount(ctx, merged)
		return nil, fmt.Errorf("health gate: %w", err)
	}
	return pl, nil
}

// awaitHealthy polls the transport Health RPC until first success or timeout.
// It is the cold-start chain's single readiness barrier.
// probeHealth resolves a placement to just the readiness/liveness half of the
// channel: returning HealthProber rather than Client states in code that the lifecycle never
// reaches into the process or fs surfaces.
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

// unwind reclaims a half-built placement (atomic transition rollback: failure recycles placement,
// persistent Overlay artifact untouched). isCreate additionally destroys the
// just-built empty Overlay (create has no fallback state; resume keeps it).
// UNWIND MUST DELETE THE MOUNT ROWS ON THE CREATE PATH (the Volume subsystem / the mount table). They are written
// BEFORE the cold start, so every post-cold-start unwind branch is followed by a DeleteSandbox
// that hits mounts.sandbox RESTRICT — and discarding that error leaves A ZOMBIE SANDBOX RECORD
// THAT CANNOT BE DELETED by anyone, ever: the record blocks on rows nothing else knows to remove,
// and the sandbox never ran so no later verb revisits it. Hence here, and not in the caller's
// `_ =` DeleteSandbox, which legitimately ignores its error.
//
// isCreate gates it because RESUME MUST NOT delete them: a failed resume stays
// `paused` (atomic transition rollback atomic return to the source state), and its binding is persistent
// control-plane state that the next resume re-materializes from. Deleting it would
// silently detach volumes from a sandbox that is merely paused — and release its
// exclusive leases, letting another sandbox take a volume this one still expects.
func (m *Mediator) unwind(ctx context.Context, id model.SandboxID, ov *rootfs.Overlay, isCreate bool) {
	if ls := m.take(id); ls != nil {
		if ls.placement != nil {
			_ = m.deps.Runtime.Destroy(ctx, ls.placement)
		}
		m.releaseMounts(ctx, ls.mounts)
		if ls.merged != nil {
			_ = m.deps.Rootfs.Unmount(ctx, ls.merged)
		}
	}
	if isCreate {
		// Reported, not discarded: if this fails the caller's DeleteSandbox is about to
		// fail too, and the log is the only place that will say why the record survived.
		if err := m.deps.Store.DeleteMounts(ctx, id); err != nil {
			m.deps.Logf("unwind %s: delete mount rows FAILED (%v) — the record delete will now be blocked by mounts.sandbox RESTRICT, leaving an undeletable zombie record", id, err)
		}
	}
	if isCreate && ov != nil {
		_ = m.deps.Rootfs.DestroyOverlay(ctx, ov)
	}
}

// SeedBuiltinTemplates is the boot seed: an idempotent reconcile that ensures
// a `templates` row exists (builtin=1, no parent) for every static Template
// directory the deployment ships (Rootfs.ListBuiltinTemplates). It MUST run
// before the engine serves any create — the sandboxes.template FK (health and reclamation) would
// otherwise reject a create referencing a builtin whose row is absent.
//
// bytes-first is inherent (the Rootfs subsystem / health and reclamation): the layer directory exists on the
// backend before this inserts its row, so there is never a row pointing at
// nonexistent bytes. Idempotent: a row that already exists (ErrConflict) is a
// no-op — re-running across restarts leaves the builtin row count unchanged.
// Failure policy matches atomic transition rollback startup reconcile —
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
			// skip-and-log (atomic transition rollback); the seed is idempotent so it retries next start.
			m.deps.Logf("seed templates: skip builtin %s: %v", id, err)
		}
	}
	return nil
}

// trimBatch bounds one trim pass. Deliberately not a knob: what it protects is the
// store's write path, not anything a deployment reasons about, and a pass that falls
// behind simply continues next tick.
const trimBatch = 1000

// TrimOutbox applies the transactional outbox's retention window: delivered outbox rows older than
// Deps.EventRetention are deleted, oldest first, bounded per pass.
//
// A no-op when EventRetention is 0, which is the default — so a deployment that has not
// chosen a window keeps every event replayable, and this method costs it one comparison
// per reaper tick rather than a store round trip.
//
// The store enforces every safety property (delivered-only, age-from-delivery,
// never-empty, bounded); this decides only WHETHER to ask and with what cutoff. That
// split is why the dangerous clauses are covered by the shared contract suite rather
// than by a test of this method.
func (m *Mediator) TrimOutbox(ctx context.Context) error {
	if m.deps.EventRetention <= 0 {
		return nil
	}
	n, err := m.deps.Store.TrimOutbox(ctx, time.Now().Add(-m.deps.EventRetention), trimBatch)
	if err != nil {
		return fmt.Errorf("trim outbox: %w", err)
	}
	if n > 0 {
		// Logged because it moves the replay window, which is externally visible: a
		// subscriber whose cursor falls below the new lower bound starts getting 410s, and
		// an operator diagnosing that needs to see that trimming is what moved it.
		m.deps.Logf("outbox trim: deleted %d delivered event(s) older than %s", n, m.deps.EventRetention)
	}
	return nil
}

// SweepOrphanLayers reclaims Promote-built layer bytes with no live `templates`
// row (the Rootfs subsystem / atomic transition rollback). It is the Rootfs subsystem twin of ReclaimOrphans: the mediator reads the
// AUTHORITATIVE store for the live layer IDs, snapshots the in-flight-Promote
// keep-set, and hands both to the rootfs provider — which enumerates its own layer
// dirs and deletes the difference (the lifecycle subsystems don't call the store). Run at
// boot (a crashed Promote's staging layer is a true orphan → reclaimed, the
// buildingLayers set being empty) and periodically by the reaper (an in-flight
// Promote is protected by buildingLayers, atomic transition rollback). Skip-and-log per atomic transition rollback: a sweep
// failure is not fatal; it retries next pass.
//
// Only Promote-built (builtin=false) layers are swept: builtins live under the
// deployment-shipped TemplatesDir, a different root the provider never scans, so
// they are structurally out of the candidate set.
func (m *Mediator) SweepOrphanLayers(ctx context.Context) error {
	tmpls, err := m.deps.Store.ListTemplates(ctx)
	if err != nil {
		return fmt.Errorf("sweep layers: list templates: %w", err)
	}
	live := make([]string, 0, len(tmpls))
	for _, t := range tmpls {
		// Passing only non-builtin IDs keeps `live` tight without affecting
		// correctness (a builtin id would never match a LayersDir entry).
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
// (atomic transition rollback) — the unit twin of SweepOrphanLayers, and the retry the transition contract licenses for a
// destroy whose byte reclamation failed (the transition contract: a failed byte delete does not
// block the transition; the sweep retries it asynchronously).
// Without it a failed DestroyOverlay leaks its unit forever: the record is gone,
// so nothing else ever revisits that OverlayID.
//
// `live` is every OverlayID the store still has a sandbox row for, in ANY state —
// a `creating` or `paused` sandbox's unit is emphatically not an orphan. `building`
// is snapshotted as LATE as possible, immediately before the call, because it guards
// a stale-snapshot race (see buildingOverlays).
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
// (the Rootfs subsystem multi-lowerdir: [leaf, ...ancestors, base]). The mediator owns this walk
// because lineage lives in the store (health and reclamation) and the rootfs provider must not read it
// (the lifecycle subsystems don't call the store) — the resolved ID stack crosses the seam,
// host paths stay inside (the ID-addressed Overlay model). A cycle is impossible (lineage is a DAG built
// child→existing-parent, health and reclamation); the walk carries the check anyway, and additionally
// bounds DEPTH against what the provider can mount (see checkLowerDepth).
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
	if err := m.checkLowerDepth(ctx, len(lower)); err != nil {
		return nil, err
	}
	return lower, nil
}

// ErrLineageTooDeep is a lineage whose lower stack is deeper than the rootfs provider can
// mount. the API error contract maps it to a 400: no retry and no state change makes it legal, only a
// shallower parent (the same reasoning as volume.ErrUnsupported).
var ErrLineageTooDeep = errors.New("lifecycle: template lineage is deeper than the rootfs provider can mount")

// checkLowerDepth refuses a lower stack the provider cannot mount, at the gate rather than
// at mount time. It matters most for Promote: the layer bytes are frozen BEFORE anything
// mounts the result, so without this check a Promote reports success and leaves a sandbox
// that can never start again — a silent failure whose only symptom arrives one create later
// (rootfs provider portability, **Two checks, because one cannot cover the other.**).
//
// The provider owns the number because the ceiling derives from host path lengths (the ID-addressed Overlay model).
// A provider reporting 0 declares no limit.
func (m *Mediator) checkLowerDepth(ctx context.Context, depth int) error {
	maxDepth, err := m.deps.Rootfs.MaxLowerDepth(ctx)
	if err != nil {
		return fmt.Errorf("resolve lineage: max depth: %w", err)
	}
	if maxDepth > 0 && depth > maxDepth {
		return fmt.Errorf("%w: %d layers against a limit of %d", ErrLineageTooDeep, depth, maxDepth)
	}
	return nil
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

// buildingLayerIDs snapshots the in-flight-Promote keep-set (atomic transition rollback). Ordering is
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

// Reconcile is the atomic transition rollback startup crash-reconciliation: one local scan diffing host placement
// × store records, run ONCE at orchestrator boot before serving. The process just (re)started,
// so the live-map is empty and all transition locks are fresh — every in-flight resume is dead,
// and half-built placements need NO lock to identify (that is ReconcilePeriodic's problem,
// against a LIVE orchestrator). A dual operation, NOT reclaim-all:
//
//   - `creating` record → the create crashed mid-chain and has no at-rest resting state →
//     delete the record (→ destroyed); its half-built placement is an orphan, reclaimed below.
//   - `paused` record → keep; any half-built placement (crashed mid-resume) is an orphan,
//     reclaimed below. The upper is intact and resumable (FS-first pause and resume).
//   - `running` record → PROBE liveness via Adopt: alive → rebuild the live-map handle at the
//     SAME IP (the takeover half of atomic transition rollback: no new epoch, the isolation unit never restarted);
//     dead → CAS-demote to `paused` (FS-first pause and resume involuntary pause) with an event (the transactional outbox).
//
// Any placement backing none of the kept sandboxes is an orphan → ReclaimOrphans tears it down
// (reverse-dep order, persistent upper untouched, atomic transition rollback).
//
// Failure policy is SKIP-AND-LOG: one un-reconcilable sandbox must not turn a bad overlay
// handle into a boot outage, and only a failure making the whole scan impossible (listing the
// records) is fatal. An unresolvable `running` sandbox is handled CONSERVATIVELY — protect its
// placement from reclamation by adding it to keep, since leaking a placement is recoverable
// next boot and killing a live survivor is not. Reconcile is idempotent.
func (m *Mediator) Reconcile(ctx context.Context) error {
	return m.reconcile(ctx, false)
}

// ReconcilePeriodic runs the atomic transition rollback reverse-diff against a LIVE orchestrator
// (periodic reconciliation). It is the SAME diff as startup Reconcile, plus the two
// additions that only matter when the orchestrator is alive with a populated
// live-map and in-flight transitions:
//
//  1. TryLock-SKIP a sandbox whose transition lock is HELD — a verb is driving it
//     (per-sandbox transition serialization), so its placement is being built, not orphaned. At startup all locks
//     are fresh so this never triggers (see the skip in reconcile).
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
			// A held transition lock means a verb is driving this sandbox — its
			// placement is being BUILT, not orphaned. Skip it UNIFORMLY IN ALL
			// STATES, not just `running`: tearing down a
			// being-built create (destroys overlay + empty upper) or resume (tears
			// down the netns/container it just launched) out from under the verb is
			// the exact corruption the skip exists to prevent. Protect its placement
			// (add to keep) and retry next tick.
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
		// own lock (adoptOrDemote vs adoptOrDemoteLocked, per-sandbox transition serialization).
		m.reconcileRecord(ctx, rec, &keep)
	}

	// Periodic only: protect every placement THIS process currently holds a live
	// handle for. A build that completed
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
	// (atomic transition rollback leaves persistent state untouched — only placement is reclaimed,
	// never a kept upper). The mediator has already force-detached (MNT_DETACH) the
	// orphan Overlay mounts in the loop above; ReclaimOrphans then does runsc kill →
	// veth → netns. At STARTUP reconcile this overlay-umount-before-runsc-kill order is
	// safe (atomic transition rollback reclamation ordering): the mediator holds no live placement handle,
	// only a table scan, and a lazy detach just unhooks the mount — the kernel reclaims
	// it when the container's last ref drops, and the upper is never touched. (The live
	// Destroy path DOES hold the handle and uses strict reverse-dep order: runsc
	// destroy → overlay umount.) Reclaim failure is logged, not fatal — the placement
	// stays leaked and is retried next boot.
	if err := m.deps.Runtime.ReclaimOrphans(ctx, keep); err != nil {
		m.deps.Logf("reconcile: reclaim orphans (retried next pass): %v", err)
	}
	return nil
}

// reconcileRecord handles one record at STARTUP (no lock held, live-map empty).
// running → adoptOrDemote takes its own transition lock (per-sandbox transition serialization).
func (m *Mediator) reconcileRecord(ctx context.Context, rec model.Sandbox, keep *[]string) {
	switch rec.State {
	case model.Creating:
		// crashed mid-create: never reached a resting state → delete record. Its
		// half-built placement (if any) falls to ReclaimOrphans (not in keep); its
		// empty Overlay upper never held useful state → reclaim it. Publishes
		// NOTHING (nil Event) — the unpaired-terminal rule, see destroyLocked.
		m.reclaimOrphanOverlay(ctx, rec, true)
		// Mount rows FIRST — the CRASH twin of unwind's requirement, and the path where
		// getting it wrong is permanent: a create that died between writing its mount
		// rows and completing leaves rows referencing a `creating` record, so RESTRICT
		// blocks this delete on EVERY subsequent boot, and no verb ever revisits it.
		m.deleteMountsForCleanup(ctx, rec.ID)
		if err := m.deps.Store.DeleteSandbox(ctx, rec.ID, nil); err != nil && err != store.ErrNotFound {
			m.deps.Logf("reconcile: skip creating %s: delete record: %v", rec.ID, err)
		}
	case model.Paused:
		// keep the record; a half-built placement (crashed mid-resume) is an orphan
		// → unmount its lingering Overlay mount, but KEEP the upper (resumable,
		// FS-first pause and resume). The netns/container fall to ReclaimOrphans.
		m.reclaimOrphanOverlay(ctx, rec, false)
	case model.Running:
		adopted, err := m.adoptOrDemote(ctx, rec)
		m.reconcileRunningOutcome(ctx, rec, adopted, err, keep)
	}
}

// reconcileRecordLocked handles one record in the PERIODIC path with the
// transition lock ALREADY HELD by the caller (tryLock succeeded → no verb is
// driving this sandbox right now). It also preserves live handles that were not
// present in the record snapshot.
func (m *Mediator) reconcileRecordLocked(ctx context.Context, rec model.Sandbox, keep *[]string) {
	switch rec.State {
	case model.Creating:
		// Lock held + still `creating` means the create is NOT in flight (it would
		// hold the lock and we'd have skipped) — so this is a crashed-create orphan,
		// same as startup. Delete + reclaim the empty upper.
		m.reclaimOrphanOverlay(ctx, rec, true)
		m.deleteMountsForCleanup(ctx, rec.ID) // same requirement as the startup path
		if err := m.deps.Store.DeleteSandbox(ctx, rec.ID, nil); err != nil && err != store.ErrNotFound {
			m.deps.Logf("reconcile: skip creating %s: delete record: %v", rec.ID, err)
		}
	case model.Paused:
		// Lock held + `paused` means no resume is in flight — a cleanly paused
		// sandbox has no merged mount, so this is a best-effort no-op; a lingering
		// mount from a crashed mid-resume is unmounted, upper KEPT (FS-first pause and resume).
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
// handle for (atomic transition rollback periodic keep-set seed). Ordering is irrelevant — keep is a
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
// handling — shared by startup and periodic reconcile (atomic transition rollback dual). err =
// unresolved → PROTECT (keep, retry next pass); adopted → keep; cleanly dead →
// demoted inside adoptOrDemote, unmount its orphan Overlay mount (upper kept).
func (m *Mediator) reconcileRunningOutcome(ctx context.Context, rec model.Sandbox, adopted bool, err error, keep *[]string) {
	switch {
	case err != nil:
		// could not resolve this survivor (probe error, un-rebuildable handle). Do
		// NOT reclaim it — it may still be live; a torn-down live container leaks
		// and breaks transport (atomic transition rollback protect-on-uncertainty). Retry next pass.
		m.deps.Logf("reconcile: skip running %s (protected, not reclaimed): %v", rec.ID, err)
		*keep = append(*keep, string(rec.ID))
	case adopted:
		*keep = append(*keep, string(rec.ID))
	default:
		m.reclaimOrphanOverlay(ctx, rec, false)
	}
}

// reclaimOrphanOverlay unmounts a sandbox's lingering merged Overlay mount left
// by a crash (atomic transition rollback reverse-dep order: overlay umount is the mediator-side step
// between runsc teardown and netns delete). The overlayfs mount is kernel state
// that survives the orchestrator process; if the record's container is gone the
// mount is an orphan holding nothing. destroyUpper additionally reclaims the
// upper bytes (create's empty Overlay → destroyed); for paused/demoted the upper
// is the persistent SoT and is KEPT (atomic transition rollback). Best-effort: a not-mounted overlay
// is not an error.
func (m *Mediator) reclaimOrphanOverlay(ctx context.Context, rec model.Sandbox, destroyUpper bool) {
	// ID-only reclamation, no mount → a single-element lower stack suffices (as in
	// destroyLocked).
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
//     recorded in the live-map (same IP, no epoch bump, atomic transition rollback's other half — the
//     takeover half).
//   - adopted=false, err=nil → cleanly determined not-adoptable (isolation unit
//     dead, OR alive-but-not-serving); CAS-demoted to paused (FS-first pause and resume involuntary
//     pause) with an event. Caller reclaims its orphan placement.
//   - err!=nil               → could not resolve (probe error / un-rebuildable
//     handle). Caller PROTECTS the placement (does not reclaim) and retries next
//     boot — never tears down a possibly-live container on uncertainty.
//
// Readiness is a two-layer judgement matching cold-start's  gate, not just the runtime's L1
// liveness (health and reclamation two-layer probe): Adopt gives L1 plus the endpoint, then ONE Health RPC (L2)
// before trusting the survivor. Otherwise a `created`-but-not-listening container is
// optimistically adopted with an un-serving endpoint; no L2 answer demotes to paused, recoverable
// via a fresh cold-start on the next resume. A SINGLE boot-time probe, NOT the continuous health
// classifier (that is classify plus the reaper's sweep, health and reclamation).
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
	// cold-start ; health and reclamation two-layer probe). notServing marks that case for the commit
	// below, which passes nil rather than stamping a cause.
	notServing := false
	if alive && !m.probeReady(ctx, pl) {
		alive = false
		notServing = true
	}
	if !alive {
		// dead / not-serving → involuntary pause (FS-first pause and resume). Its placement, if any, is
		// reclaimed as an orphan (this sandbox is not added to keep). The event
		// Kind is "crash-paused", NOT "paused": this is the FS-first pause and resume involuntary pause
		// (found dead/not-serving at reconcile), distinct from a user pause and
		// from the reaper's idle "auto-paused". State is the same `paused`; the
		// Kind lets a subscriber tell why it paused (the lifecycle state machine names transitions
		// distinctly). See store.Event.Kind.
		// The death cause is committed in THIS SAME CAS (health and reclamation; see
		// store.CompareAndSetState for why no later window exists).
		//
		// EXCEPT when the unit was L1-alive and merely not serving: nothing died, so
		// there is no cause to record. Pass nil (leave the column untouched) rather
		// than stamping CauseUnknown — "unknown" means "it is GONE but why is
		// undeterminable" (see model.DeathCause), which would be a false statement
		// about a unit L1 just reported as alive, i.e. exactly the guessing the provider security contract
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

	// alive + serving, BUT this deployment cannot enforce the declaration it carries:
	// its record holds `egress.via[]` and no second-hop uid was supplied, so the
	// engine-level `output` table pinning the endpoint process is not installed
	// (network.ValidatePlacement — the mechanism and the measurement are there).
	//
	// Neither of the two obvious dispositions works here:
	//
	//   - PROTECT (the default for every uncertainty below) would leave the sandbox
	//     `running` with its placement in the keep-set — the unenforced deputy still
	//     running, re-decided identically every pass. Protect-on-uncertainty is for facts
	//     the engine could not DETERMINE; this one is determined, and determined unsafe.
	//   - ADOPT-THEN-REFUSE-THE-RULES has nothing to lean on: the rule set installs fine,
	//     the second-hop pin not being part of it (see steeringFor).
	//
	// So it demotes as a FS-first pause and resume involuntary pause, passing NO death cause for the same
	// reason the not-serving arm above passes none.
	//
	// Demoting a live unit is destructive but recoverable: `paused` is the fail-CLOSED
	// resting state (no placement, so not one rule installed), the upper is untouched, a
	// resume succeeds the moment either remedy is applied, and the operator gets a
	// `crash-paused` event plus this log line naming both. The alternative preserves
	// reachability the engine has just determined it cannot pin (the provider security contract).
	if verr := network.ValidatePlacement(rec.Network, m.deps.Deployment); verr != nil {
		m.deps.Logf("reconcile: demoting live sandbox %s to paused — it is alive and serving, but "+
			"this deployment cannot enforce its recorded declaration: %v", rec.ID, verr)
		if err := m.deps.Store.CompareAndSetState(ctx, rec.ID, model.Running, model.Paused,
			newEvent(rec.ID, "crash-paused", model.Paused), nil); err != nil && err != store.ErrConflict {
			// The CAS failed, so the record still says `running` — return an error so the
			// caller PROTECTS the placement and this decision is retaken next pass. Reclaiming
			// on a failed demotion would tear down a live container whose record still claims
			// it, the one outcome worse than leaving it up for one more pass.
			return false, fmt.Errorf("demote unenforceable via[] sandbox to paused: %w", err)
		}
		// adopted=false, err=nil: the caller reclaims the placement as an orphan, which
		// is what actually removes the deputy's reachability — the netns and the rule set
		// go with it.
		return false, nil
	}

	// alive + serving → re-attach the still-mounted overlay + record the live
	// handle so destroy/transport work again (atomic transition rollback: without this the survivor is
	// un-addressable — destroy would leak the live container). AdoptMerged recovers
	// the EXISTING kernel mount, so a single-element lower stack suffices.
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), []string{string(rec.Template)}, rootfs.CapUnchanged)
	if err != nil {
		return false, fmt.Errorf("reopen overlay: %w", err) // uncertain → protect
	}
	merged, err := m.deps.Rootfs.AdoptMerged(ctx, ov)
	if err != nil {
		return false, fmt.Errorf("adopt merged: %w", err) // uncertain → protect
	}
	// Re-install the network declaration rule set from the CURRENT record. the provider security contract is explicit that a
	// rule set is crash-surviving host state exactly like the netns, so taking over a
	// live placement means rebuilding it too — it is not something that can be added
	// afterwards, because "the network is up and the rules are not" is the fail-open
	// window the whole atomicity obligation exists to close.
	//
	// The provider re-renders the whole table into ONE atomic file, which is both
	// required and sufficient: measured, that leaves live flows intact 5/5 while a
	// delete-then-install kills them 5/5, and the permission hidden in the same
	// measurement is that no diff against the installed rules is needed. The provider
	// must use one atomic replacement and must not delete the live table first.
	//
	// A FAILURE HERE IS UNCERTAINTY, NOT DEATH (atomic transition rollback protect-on-uncertainty): we
	// return an error, so the caller keeps this sandbox in the keep-set and retries
	// next pass. Demoting instead would tear down a container L1 just reported alive,
	// which is irreversible; and recording the adoption while the rules failed to
	// install would leave a `running` sandbox whose record claims a reachable set the
	// kernel is not enforcing — precisely the divergence the control-plane API's read face promises
	// cannot persist.
	steering, err := m.steeringFor(rec)
	if err != nil {
		return false, fmt.Errorf("regenerate rule set for adopt: %w", err)
	}
	if err := m.deps.Runtime.ReinstallRuleSet(ctx, pl, steering); err != nil {
		return false, fmt.Errorf("reinstall rule set for adopt: %w", err)
	}
	// Re-recognize this sandbox's the Volume subsystem volume materializations too, so the recovered
	// liveState is COMPLETE.
	//
	// Without this the adopted survivor carries no volume handles, and the next
	// pause/destroy Releases nothing. Invisible on the local arm (Release is a no-op
	// there by contract), but on a remote arm the handle owns a live FUSE process, so a
	// lost handle strands it — the leak the backend-private handle exists to prevent
	// because the backend seam has no side table keyed by Volume ID.
	mounts, err := m.adoptMounts(ctx, rec.ID)
	if err != nil {
		// uncertain → protect. Recording a half-recovered liveState would be the worse
		// outcome — a pause would then silently skip a Release it owed.
		return false, fmt.Errorf("adopt volume materializations: %w", err)
	}
	m.put(rec.ID, &liveState{placement: pl, merged: merged, mounts: mounts})
	return true, nil
}

// probeReady sends one Health request to an adopted placement's endpoint.
// It is bounded by a short timeout
// so a booting/hung survivor demotes promptly rather than stalling boot.
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
// endpoint (the transport data plane: built per endpoint, discarded on resume). This is how the
// orchestrator proxies process/fs by sandbox-id WITHOUT exposing placement
// (the API error contract): callers address by SandboxID; the endpoint resolution stays here.
// Errors if the sandbox has no live placement (not running).
func (m *Mediator) Client(id model.SandboxID) (transport.Client, error) {
	ep, err := m.Endpoint(id)
	if err != nil {
		return nil, err
	}
	return m.deps.Dial(transport.Endpoint(ep)), nil
}

// Endpoint returns the sandbox's current transport endpoint. INTERNAL — not part
// of the control-plane API external API (which never exposes placement, the API error contract). Used by the
// orchestrator to proxy transport and by tests to assert IP-as-epoch (the ID-reference model/health and reclamation).
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
// sandbox. Periodic reconciliation uses this to protect placements missing from
// its record snapshot.
func (m *Mediator) hasLive(id model.SandboxID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.live[id]
	return ls != nil && ls.placement != nil
}

// Get returns the sandbox record (the control-plane API GET /v1/sandboxes/{id}). State is the
// narrower read for callers that only need the lifecycle state; this one exists
// because the control-plane API read shape also carries death_cause (health and reclamation: the recovery
// decision is the caller's, so the caller needs the fact it decides on).
func (m *Mediator) Get(ctx context.Context, id model.SandboxID) (model.Sandbox, error) {
	return m.deps.Store.GetSandbox(ctx, id)
}

// SandboxInfo is the control-plane API single-read shape: the record plus the one fact that does
// not live in it.
type SandboxInfo struct {
	Sandbox model.Sandbox
	// UsedByte is what the Overlay storage unit consumes as the rootfs provider's own
	// backend reports it (upper + work), or NIL when that could not be read.
	//
	// A POINTER for the same reason VolumeInfo's is: the control-plane API says the key is absent IFF
	// the number is unreadable, and zero is a legitimate reading — a fresh uncapped
	// sandbox's upper+work really is 0, the COMMON case here. A value type would make
	// "nothing written yet" and "the backend did not answer" the same answer, which the
	// caller's use for it (choosing a cap STRICTLY greater than it) needs apart.
	UsedByte *int64
}

// Inspect reads one sandbox's record and asks its rootfs provider for current usage
// (the control-plane API GET /v1/sandboxes/{id}).
//
// Separate from Get rather than folded into it: Get is the plain record read that the
// rest of the engine and several tests use, and it must not acquire a backend round
// trip as a side effect. This is the same split GetVolume has against the store's own
// volume read.
func (m *Mediator) Inspect(ctx context.Context, id model.SandboxID) (SandboxInfo, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return SandboxInfo{}, err
	}
	info := SandboxInfo{Sandbox: rec}
	if rp := m.deps.Rootfs; rp != nil && rec.Overlay != "" {
		used, _, err := rp.Usage(ctx, string(rec.Overlay))
		if err != nil {
			// DEGRADES rather than failing the read, which is the seam's own stated split:
			// the cap write point fails closed because usage IS its decision, while an
			// inspect-shaped reader reports what it can. Omitting the key is how the
			// caller learns the difference (the control-plane API) — reporting 0 would tell it this sandbox
			// is empty and invite a cap that the write point then refuses.
			m.deps.Logf("inspect sandbox %s: usage unavailable: %v", id, err)
		} else {
			info.UsedByte = &used
		}
	}
	return info, nil
}

// State returns the sandbox's current lifecycle state (the control-plane API GET).
func (m *Mediator) State(ctx context.Context, id model.SandboxID) (model.Lifecycle, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return "", err
	}
	return rec.State, nil
}

// List returns all sandbox records (the control-plane API GET /v1/sandboxes).
func (m *Mediator) List(ctx context.Context) ([]model.Sandbox, error) {
	return m.deps.Store.ListSandboxes(ctx)
}

// Events serves the control-plane API GET /v1/events replay: at most limit events after the
// given sequence cursor, plus the retained window's bounds (the transactional outbox).
//
// PULL ONLY, because the engine must hold NO per-subscriber state (the extension boundary), which a
// cursor-paged read needs none of. Why push as an adapter over this same sequence
// would still cost nothing structurally: the transactional outbox's "Outward = pull only" passage.
//
// It is a pure store read: no placement, no transition lock. The stream is
// engine-level rather than per-sandbox, so there is nothing to serialize against.
func (m *Mediator) Events(ctx context.Context, after int64, limit int) ([]store.Event, int64, int64, error) {
	return m.deps.Store.Events(ctx, after, limit)
}

// ListTemplates returns all Template rows (the control-plane API GET /v1/templates).
func (m *Mediator) ListTemplates(ctx context.Context) ([]model.Template, error) {
	return m.deps.Store.ListTemplates(ctx)
}

// DeleteTemplate removes a Template row iff nothing references it (the control-plane API DELETE
// /v1/templates/{id}). The store's reference protection (health and reclamation) surfaces as
// store.ErrInUse — a referenced Template (by a sandbox, or a child layer via
// parent) cannot be deleted — which the API layer maps to 409 template in use.
// The layer's now-orphaned bytes are reclaimed by the Rootfs subsystem orphan sweep, NOT in
// this row delete (the transition contract: the state transition and byte reclamation are
// decoupled). No placement and no transition lock — Template is not a
// state-machine object (health and reclamation existence-is-state).
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

// newEvent builds an outbox Event for a lifecycle state machine transition, carrying a
// transition-unique ID (the transactional outbox: subscribers dedup on it; a redelivered event
// keeps the SAME ID). The mediator constructs it per transition semantics; the
// store commits it atomically with the state change (the transactional outbox).
func newEvent(id model.SandboxID, kind model.EventKind, state model.Lifecycle) *store.Event {
	return &store.Event{
		ID:         "evt_" + randToken(),
		Sandbox:    id,
		Kind:       kind,
		State:      state,
		OccurredAt: time.Now(),
	}
}

// newPlacementEvent records the portable address established by a transition.
// Only transitions that create a new placement use this constructor; pause-like
// and destroy events intentionally carry no placement address.
func newPlacementEvent(id model.SandboxID, kind model.EventKind, state model.Lifecycle, address netip.Addr) *store.Event {
	ev := newEvent(id, kind, state)
	ev.PlacementAddress = address.String()
	return ev
}

// newCausedEvent is newEvent for a Kind reachable by more than one route, where the
// route is what a subscriber needs and cannot get anywhere else (the transactional outbox). Separate
// constructor rather than a variadic on newEvent so the ~dozen single-route call sites
// keep saying nothing about a cause they do not have.
func newCausedEvent(id model.SandboxID, kind model.EventKind, state model.Lifecycle, cause model.EventCause) *store.Event {
	ev := newEvent(id, kind, state)
	ev.Cause = cause
	return ev
}

// randToken returns a short opaque hex token for sandbox/overlay IDs. Opaque =
// not derived from placement (the ID-reference model) and safe as a single path segment (the Rootfs subsystem
// validation rejects separators).
func randToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
