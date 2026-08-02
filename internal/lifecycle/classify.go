package lifecycle

import (
	"context"
	"time"

	"engine/internal/model"
	"engine/internal/runtime"
	"engine/internal/transport"
)

// Health is the §4.3 four-quadrant health classification of a `running`
// sandbox, plus an explicit "unknown → skip" for the cases the classifier must
// not guess at (reaper-health D3). The reaper dispatches per quadrant.
type Health int

const (
	// HealthUnknown = could not cleanly classify this round → the reaper does
	// NOTHING and retries next tick. Two sources: the sandbox is not in the
	// live-map (a store-`running` with no live handle is reconcile's takeover
	// concern, D4 — not a health quadrant), or the L1 probe was UNCERTAIN
	// (L1-alive but endpoint unrecoverable, §4.4-A protect-on-uncertainty). Never
	// a disposition — the safe default.
	HealthUnknown Health = iota
	// HealthHealthy = L1 alive + L2 up + recent I/O (idle < idle_timeout). Keep
	// running, do nothing (§4.3).
	HealthHealthy
	// HealthIdle = L1 alive + L2 up + ALL connections idle (idle ≥ idle_timeout).
	// The reaper auto-pauses it (§4.3), re-reading activity under the lock first
	// (D3 activity-race guard).
	HealthIdle
	// HealthTransportDown = L1 alive, L2 down — the transport child crashed and
	// PID1 is restarting it in place on the SAME IP (§5.E.1). Transient: wait,
	// never crash-pause. Only escalates if it stays down past the restart budget
	// (D4b) — that escalation is the reaper's, not the classifier's.
	HealthTransportDown
	// HealthDeadRecoverable = L1 dead (the isolation unit is gone), persistent
	// Overlay intact, AND the death was not a resource cap. Recovery is the resume
	// cold-start path, NOT auto-pause's quiesce (§4.3) — the reaper crash-pauses it
	// (§4.2 involuntary pause), and a resume may follow.
	HealthDeadRecoverable
	// HealthResourceTerminated = L1 dead AND the death cause was a resource cap
	// (the §3.4 RAM safety limit, whole-group termination).
	//
	// PARALLEL to HealthDeadRecoverable, not a subtype of it (§4.3): both have L1
	// dead and intact persistent state, and they are INDISTINGUISHABLE by
	// observation — only the death cause separates them — yet their dispositions
	// are OPPOSITE. dead-recoverable must resume; this must NOT, because resuming
	// re-runs the same workload into the same cap, which is a crash loop. It is
	// still crash-paused (the state is `paused` either way); what differs is that
	// the recorded cause then blocks resume.
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
// serial, so every probe is on the critical path (reaper-health D3: cheap
// concurrent-ish probing, short dispositions).
const classifyProbeTimeout = 2 * time.Second

// classify computes the §4.3 health quadrant of a sandbox from L1 (substrate
// liveness) × L2 (transport Health) × activity (idle duration). It is READ-ONLY
// — no transition lock, no state change (reaper-health D3: the classifier only
// reads; dispositions take the transition lock). The ORDER is the D2
// unreadable-idle guard made structural:
//
//	L2 up  ⟹ L1 alive (sandboxd answered from inside the live container), so a
//	         successful probe skips the L1 round-trip entirely and splits
//	         healthy vs idle by the idle_millis it carried back.
//	L2 down → the ONLY case that needs an explicit L1 probe, to tell
//	          transport-down (L1 alive) from dead-recoverable (L1 dead).
//
// Because idle is read ONLY on the L2-up branch, an unreachable sandbox can
// NEVER be misclassified as idle from a stale last-known activity (D2): no
// trustworthy idle reading exists without a successful probe.
// It returns the death cause ALONGSIDE the quadrant, and that pairing is
// structural rather than convenient (§4.3 / reaper-health D4c). The classifier is
// read-only: it discards the placement handle Adopt rebuilt. But the death signal
// lives IN that placement and dies with it, so a cause read here and not returned
// is a cause thrown away — and the disposition cannot re-read it, because by then
// teardown may have destroyed the evidence. Hence the cause travels out with the
// classification, to be committed by the disposition in the same CAS.
//
// The returned cause is meaningful ONLY for the two dead quadrants; it is
// CauseUnknown elsewhere.
func (m *Mediator) classify(ctx context.Context, rec model.Sandbox) (Health, runtime.DeathCause) {
	// Only a `running` sandbox has a health quadrant (§4.3). paused → TTL/evict,
	// creating/destroyed → not the classifier's concern.
	if rec.State != model.Running {
		return HealthUnknown, runtime.CauseUnknown
	}

	// Resolve the endpoint from the live-map. A `running` sandbox the live
	// orchestrator manages holds a live handle here; one that does NOT (store
	// says running, live-map empty) is the periodic reconcile's takeover concern
	// (D4), not a health quadrant — skip it (Unknown).
	ep, err := m.Endpoint(rec.ID)
	if err != nil {
		return HealthUnknown, runtime.CauseUnknown
	}

	// L2 probe (§4.3 L2 + activity signal, one RPC — D2).
	cl := m.deps.Dial(transport.Endpoint(ep))
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
	// outcome — alive / dead / uncertain — matching §4.4-A). We only need its
	// liveness verdict and, if dead, its death cause; the rebuilt placement is
	// discarded (the classifier is read-only, dispositions rebuild handles under
	// the transition lock).
	_, alive, cause, aerr := m.deps.Runtime.Adopt(ctx, string(rec.ID))
	switch {
	case aerr != nil:
		// UNCERTAIN (L1-alive but endpoint unrecoverable, §4.4-A). Never tear down
		// or misclassify on uncertainty — skip and retry next tick.
		return HealthUnknown, runtime.CauseUnknown
	case alive:
		return HealthTransportDown, runtime.CauseUnknown
	case cause == runtime.CauseResourceExceeded:
		// Dead BECAUSE of a resource cap → the parallel quadrant with the opposite
		// disposition (§4.3): crash-pause but never resume.
		return HealthResourceTerminated, cause
	default:
		// Dead of something else, or undeterminable. CauseUnknown lands here too, and
		// that is the deliberate §4.3 fallback: treat it as dead-recoverable and
		// resume, with bounded retry self-correcting a misread — if it really was the
		// cap, the same workload re-triggers termination, this time under the control
		// plane's eye, and THAT observation records the real cause.
		return HealthDeadRecoverable, cause
	}
}
