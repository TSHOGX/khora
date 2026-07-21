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

	// Logf reports per-item reconciliation outcomes that are skipped rather than
	// fatal (§4.4-A startup reconcile is skip-and-log: one un-adoptable sandbox
	// must not abort recovery of the rest). Defaults to log.Printf; tests inject a
	// capturing sink.
	Logf func(format string, args ...any)

	// FaultAfterColdStart is a TEST-ONLY (R5) injection hook: if non-nil it runs
	// right after coldStart records the live placement but BEFORE the committing
	// CAS to `running`. A test panics/exits from it to simulate the orchestrator
	// crashing mid-transition — leaving a fully-built placement while the store
	// still reads the SOURCE state (`paused` for resume / `creating` for create).
	// This manufactures the §4.4-A crash scenarios (orphan half-build + surviving
	// container with lost handle) that Reconcile must clean up. nil in production;
	// it carries NO production semantics (plan §7 R5).
	FaultAfterColdStart func(id model.SandboxID)
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
	if deps.Logf == nil {
		deps.Logf = log.Printf
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
	// The transient creating-insert is an internal step of the create verb, not
	// an externally-visible §4.1 transition — it publishes nothing (nil Event).
	// The "created" event is emitted only when create COMPLETES → running below.
	if err := m.deps.Store.CreateSandbox(ctx, rec, nil); err != nil {
		return "", fmt.Errorf("create: insert record: %w", err)
	}

	// ① build empty Overlay + run the cold-start chain.
	ov, err := m.deps.Rootfs.CreateOverlay(ctx, string(oid), string(spec.Template))
	if err != nil {
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil) // nothing built, never visible
		return "", fmt.Errorf("create: overlay: %w", err)
	}
	if err := m.coldStart(ctx, sid, ov, spec.GuestUser); err != nil {
		// create has no fallback state: reclaim half-built placement + empty
		// Overlay, delete the record → destroyed (§4.4-A / D5.2 要点④). The
		// sandbox was never running, so this unwind-delete publishes nothing.
		m.unwind(ctx, sid, ov, true)
		_ = m.deps.Store.DeleteSandbox(ctx, sid, nil)
		return "", fmt.Errorf("create: cold start: %w", err)
	}

	// R5 fault injection: simulate a crash after the placement is fully built but
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

	if err := m.deps.Store.CompareAndSetState(ctx, id, model.Running, model.Paused, newEvent(id, "paused", model.Paused)); err != nil {
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

	// R5 fault injection: simulate a crash after the placement is fully built but
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
	// delete the record → destroyed + emit "destroyed" in the same commit
	// (§4.4-C). The outbox row outlives the deleted record.
	if err := m.deps.Store.DeleteSandbox(ctx, id, newEvent(id, "destroyed", model.Destroyed)); err != nil {
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

// Reconcile is the §4.4-A startup crash-reconciliation: one local scan diffing
// host placement × store records, run ONCE at orchestrator boot before serving
// (§4.4-A / plan §4). The process just (re)started, so the live-map is empty and
// all transition locks are fresh — every in-flight resume that was running is
// dead, so half-built placements need NO lock to identify (lock-based
// identification is only for the M3 periodic reconcile against a LIVE
// orchestrator). It is a dual operation, NOT reclaim-all:
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
	recs, err := m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: list records: %w", err)
	}

	// keep = SandboxIDs whose placement must NOT be reclaimed: successfully
	// re-adopted `running` survivors PLUS any `running` sandbox we could not
	// resolve (protect-on-uncertainty). A `paused`/`creating` sandbox holds no
	// legitimate live placement, so it never enters keep.
	keep := make([]string, 0)

	for _, rec := range recs {
		switch rec.State {
		case model.Creating:
			// crashed mid-create: never reached a resting state → delete record.
			// Its half-built placement (if any) falls to ReclaimOrphans (not in
			// keep); its empty Overlay upper never held useful state → reclaim it.
			// Publishes NOTHING (nil Event): a crashed `creating` never reached a
			// running/visible state, so — exactly like the live create-unwind path
			// (see Create), which deletes with nil — there was no "created" event
			// and there must be no unpaired "destroyed" (a billing subscriber must
			// not see a destroyed for a sandbox it never saw created). §4.4-A frames
			// this as an internal half-step, not an externally-visible §4.1 transition.
			m.reclaimOrphanOverlay(ctx, rec, true)
			if err := m.deps.Store.DeleteSandbox(ctx, rec.ID, nil); err != nil && err != store.ErrNotFound {
				m.deps.Logf("reconcile: skip creating %s: delete record: %v", rec.ID, err)
			}
		case model.Paused:
			// keep the record; a half-built placement (crashed mid-resume) is an
			// orphan → unmount its lingering Overlay mount, but KEEP the upper
			// (resumable, §4.2). The netns/container fall to ReclaimOrphans.
			m.reclaimOrphanOverlay(ctx, rec, false)
		case model.Running:
			adopted, err := m.adoptOrDemote(ctx, rec)
			switch {
			case err != nil:
				// could not resolve this survivor (probe error, un-rebuildable
				// handle). Do NOT reclaim it — it may still be live; a torn-down
				// live container leaks and breaks transport (§4.4-A). Protect the
				// placement and retry on the next boot (idempotent).
				m.deps.Logf("reconcile: skip running %s (protected, not reclaimed): %v", rec.ID, err)
				keep = append(keep, string(rec.ID))
			case adopted:
				// alive + ready + handle rebuilt → keep its placement.
				keep = append(keep, string(rec.ID))
			default:
				// cleanly determined dead → demoted to paused inside adoptOrDemote;
				// unmount its orphan Overlay mount, keep the upper.
				m.reclaimOrphanOverlay(ctx, rec, false)
			}
		}
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

// reclaimOrphanOverlay unmounts a sandbox's lingering merged Overlay mount left
// by a crash (§4.4-A reverse-dep order: overlay umount is the mediator-side step
// between runsc teardown and netns delete). The overlayfs mount is kernel state
// that survives the orchestrator process; if the record's container is gone the
// mount is an orphan holding nothing. destroyUpper additionally reclaims the
// upper bytes (create's empty Overlay → destroyed); for paused/demoted the upper
// is the persistent SoT and is KEPT (§4.4-A 持久态分毫不动). Best-effort: a
// not-mounted overlay (nothing to clean) is not an error.
func (m *Mediator) reclaimOrphanOverlay(ctx context.Context, rec model.Sandbox, destroyUpper bool) {
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), string(rec.Template))
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
// deliberately NOT M3's continuous four-quadrant health classifier
// (open-decisions §2 §4.4-A row).
func (m *Mediator) adoptOrDemote(ctx context.Context, rec model.Sandbox) (bool, error) {
	l := m.lock(rec.ID)
	l.Lock()
	defer l.Unlock()

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
	// un-addressable — destroy would leak the live container).
	ov, err := m.deps.Rootfs.OpenOverlay(ctx, string(rec.Overlay), string(rec.Template))
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
	return cl.Health(hctx) == nil
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
