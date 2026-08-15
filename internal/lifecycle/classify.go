package lifecycle

import (
	"context"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/transport"
)

// Health is the health and reclamation health classification (five states) of a `running`
// sandbox, plus an explicit "unknown → skip" for the cases the classifier must
// not guess at (reaper serialization). The reaper dispatches per health state.
type Health int

const (
	// HealthUnknown = could not cleanly classify this round → the reaper does
	// NOTHING and retries next tick. Two sources: the sandbox is not in the
	// live-map (a store-`running` with no live handle is reconcile's takeover
	// concern, health classification — not a health state), or the L1 probe was UNCERTAIN
	// (L1-alive but endpoint unrecoverable, atomic transition rollback protect-on-uncertainty). Never
	// a disposition — the safe default.
	HealthUnknown Health = iota
	// HealthHealthy = L1 alive + L2 up + recent I/O (idle < idle_timeout). Keep
	// running, do nothing (health and reclamation).
	HealthHealthy
	// HealthIdle = L1 alive + L2 up + ALL connections idle (idle ≥ idle_timeout).
	// The reaper auto-pauses it (health and reclamation), re-reading activity under the lock first
	// (health classification activity-race guard).
	HealthIdle
	// HealthTransportDown = L1 alive, L2 down — the transport child crashed and
	// PID1 is restarting it in place on the SAME IP (health classification). Transient: wait,
	// never crash-pause. Only escalates if it stays down past the restart budget
	// (health classification) — that escalation is the reaper's, not the classifier's.
	HealthTransportDown
	// HealthDeadRecoverable = L1 dead (the isolation unit is gone), persistent
	// Overlay intact, AND the death was not a resource cap. Recovery is the resume
	// cold-start path, NOT auto-pause's quiesce (health and reclamation) — the reaper crash-pauses it
	// (FS-first pause and resume involuntary pause), and a resume may follow.
	HealthDeadRecoverable
	// HealthResourceTerminated = L1 dead AND the death cause was a resource cap
	// (the resource admission RAM safety limit, whole-group termination).
	//
	// PARALLEL to HealthDeadRecoverable, not a subtype of it (health and reclamation): both have L1
	// dead and intact persistent state, INDISTINGUISHABLE by observation — only the
	// death cause separates them — yet the dispositions are OPPOSITE. This one must
	// NOT resume, because resuming re-runs the same workload into the same cap.
	HealthResourceTerminated
)

func (h Health) String() string {
	switch h {
	case HealthHealthy:
		return "healthy"
	case HealthIdle:
		return "idle"
	case HealthTransportDown:
		return "transport-down"
	case HealthDeadRecoverable:
		return "dead-recoverable"
	case HealthResourceTerminated:
		return "resource-terminated"
	default:
		return "unknown"
	}
}

// classifyProbeTimeout bounds the classifier's L2 Health RPC. Short so a hung
// guest classifies promptly rather than stalling the sweep; the reaper sweep is
// serial, so every probe is on the critical path (reaper serialization: cheap
// concurrent-ish probing, short dispositions).
const classifyProbeTimeout = 2 * time.Second

// classify computes the health and reclamation health state of a sandbox from L1 (substrate
// liveness) × L2 (transport Health) × activity (idle duration). It is READ-ONLY
// — no transition lock, no state change (reaper serialization: the classifier only
// reads; dispositions take the transition lock). The ORDER is the health classification
// unreadable-idle guard made structural:
//
//	L2 up  ⟹ L1 alive (sandboxd answered from inside the live container), so a
//	         successful probe skips the L1 round-trip entirely and splits
//	         healthy vs idle by the idle_millis it carried back.
//	L2 down → the ONLY case that needs an explicit L1 probe, to tell
//	          transport-down (L1 alive) from dead-recoverable (L1 dead).
//
// Because idle is read ONLY on the L2-up branch, an unreachable sandbox can
// NEVER be misclassified as idle from a stale last-known activity (health classification): no
// trustworthy idle reading exists without a successful probe.
// It returns the death cause ALONGSIDE the health state, and that pairing is
// structural rather than convenient (health and reclamation / death-cause propagation): the death signal lives
// IN the placement handle this read-only classifier discards, so a cause read here and
// not returned is a cause thrown away — and the disposition cannot re-read it, because
// by then teardown may have destroyed the evidence. So it travels out with the
// classification, to be committed by the disposition in the same CAS.
//
// The returned cause is meaningful ONLY for the two dead health states; it is
// CauseUnknown elsewhere.
func (m *Mediator) classify(ctx context.Context, rec model.Sandbox) (Health, runtime.DeathCause) {
	// Only a `running` sandbox has a health state (health and reclamation). paused → TTL/evict,
	// creating/destroyed → not the classifier's concern.
	if rec.State != model.Running {
		return HealthUnknown, runtime.CauseUnknown
	}

	// Resolve the endpoint from the live-map. A `running` sandbox the live
	// orchestrator manages holds a live handle here; one that does NOT (store
	// says running, live-map empty) is the periodic reconcile's takeover concern
	// (health classification), not a health state — skip it (Unknown).
	ep, err := m.Endpoint(rec.ID)
	if err != nil {
		return HealthUnknown, runtime.CauseUnknown
	}

	// L2 probe (health and reclamation L2 + activity signal, one RPC — health classification).
	var cl transport.HealthProber = m.deps.Dial(transport.Endpoint(ep))
	hctx, cancel := context.WithTimeout(ctx, classifyProbeTimeout)
	st, herr := cl.Health(hctx)
	cancel()
	if herr == nil {
		// L2 up ⟹ L1 alive. Split healthy vs idle by activity. A zero/negative
		// IdleTimeout means "no idle deadline" → never idle (the sandbox opted out
		// of auto-pause); it stays healthy regardless of how long it has been quiet.
		if rec.IdleTimeout > 0 && st.Idle >= rec.IdleTimeout {
			return HealthIdle, runtime.CauseUnknown
		}
		return HealthHealthy, runtime.CauseUnknown
	}

	// L2 down: disambiguate via an L1 liveness probe (Adopt gives the three-way
	// outcome — alive / dead / uncertain — matching atomic transition rollback). We only need its
	// liveness verdict and, if dead, its death cause; the rebuilt placement is
	// discarded (the classifier is read-only, dispositions rebuild handles under
	// the transition lock).
	_, alive, cause, aerr := m.deps.Runtime.Adopt(ctx, string(rec.ID))
	switch {
	case aerr != nil:
		// UNCERTAIN (L1-alive but endpoint unrecoverable, atomic transition rollback). Never tear down
		// or misclassify on uncertainty — skip and retry next tick.
		return HealthUnknown, runtime.CauseUnknown
	case alive:
		return HealthTransportDown, runtime.CauseUnknown
	case cause == runtime.CauseResourceExceeded:
		// Dead BECAUSE of a resource cap → the parallel health state with the opposite
		// disposition (health and reclamation): crash-pause but never resume.
		return HealthResourceTerminated, cause
	default:
		// Dead of something else, or undeterminable. CauseUnknown lands here too, and
		// that is the deliberate health and reclamation fallback: treat it as dead-recoverable, which a
		// caller may then resume. No resume-attempt bound exists or is needed — the
		// self-correction is STRUCTURAL rather than counted, and the engine never
		// resumes a dead-recoverable sandbox itself (health and reclamation / death-cause propagation, which
		// also states the one residual where convergence is not guaranteed).
		return HealthDeadRecoverable, cause
	}
}
