package lifecycle

import (
	"context"
	"fmt"

	"engine/internal/model"
)

// Admission — the §3.4 / rootfs-overlay D6.5 no-oversubscribe gate.
//
// WHY THIS EXISTS AT ALL. The per-sandbox caps (§3.4) only downgrade "one sandbox
// fills the whole disk" into "one sandbox fills its own quota". They say nothing
// about the SUM: promise 100 sandboxes 10 GiB each on a 200 GiB node and every
// individual cap is honored right up to the moment the node runs dry, at which
// point writes start failing for sandboxes that are nowhere near their own cap.
// That breaks §2.3's containment promise from the other direction — a sandbox's
// write failing because of OTHER sandboxes' usage. So a cap the node cannot honor
// must be refused at PROMISE time, which is here.
//
// The formula is stated by §3.4 / rootfs-overlay D6.5, and its shape is forced by
// the choice of squota:
//
//	Σ(declared caps) + Σ(bytes under no cap) ≤ configured node capacity
//
// The second term is SEPARATE and non-negotiable: squota charges a sandbox only
// for bytes IT wrote, and copy-up from a Template on the same fs is a reflink,
// which is free under squota (a reason we chose it — D6.2). So Template and layer
// bytes occupy real disk that NO sandbox cap covers. Omit that term and the
// accounting silently overcommits by the entire size of the Template library.
//
// The two terms are DISJOINT, and the predicate is "is it covered by a declared
// cap", not "is it a layer": a CAPPED Overlay upper is counted by the cap term and
// so contributes zero to the measured one (charging its current usage on top of
// its own cap bills the same bytes twice, and usage is a forecast where the cap is
// already a bound). Physically shared bytes count at most once — squota does not
// charge a reflinked copy-up to the sandbox, and the layer side counts that extent
// once. The property that forces the second term to exist is the same one that
// keeps the two from overlapping.
//
// Mixing a promise with a measurement is sound because the measured bytes are
// ALREADY ON DISK: the gate is not forecasting them, it is subtracting consumed
// capacity before comparing promises against the remainder. One rule, uniformly
// applied — take the strongest available statement about each object: the cap where
// one exists, the measurement where none does (§3.4).
//
// RAM is admitted on the same principle but against a separate budget: memory and
// disk are not fungible, so they are two independent ceilings, never one blended
// "resources" number.
//
// A METERED AXIS REQUIRES A CAP (§3.4). A sandbox with no cap on a metered axis
// contributes 0 to the left side while free to consume all of the right — it does
// not undercount, it makes the inequality describe a device that is not the one
// being written to. So the formula is sound only under the premise that every
// admitted sandbox is capped on every metered axis, and that premise is enforced
// here rather than assumed. `0 = unlimited` stays legal on an UNMETERED axis,
// where it is an honest resting value.
//
// Where this sits in the §4 structure: admission reads the AUTHORITATIVE store
// (committed promises) and asks the Rootfs provider to measure its own bytes.
// Both are mediator actions — the mediator reads the store and calls subsystem
// mechanisms; the provider is never asked to consult the store (§4 subsystems
// don't call each other).

// NodeCapacity is the operator-configured budget the gate admits against (§1.1
// single-binary config). Zero on a dimension = UNMETERED: that axis is not
// gated at all, which is the correct default for a deployment that has not
// declared its budget — the engine must not invent a number and start refusing
// creates on a guess.
//
// WHAT THE BUDGET MEANS (§3.4): the bytes the engine may PROMISE to guests — not
// the host's physical headroom. Substrate costs the engine cannot name without
// breaking a seam live OUTSIDE it: runsc state and bundles on disk, the
// sentry/gofer footprint in RAM (that one is a provider implementation constant
// by §5.A's design, deliberately not a contract member). Operators set each
// budget below the physical figure; that gap is what pays for the substrate.
type NodeCapacity struct {
	// DiskByte is the total bytes available for sandbox Overlay upper+work plus
	// Template/layer bytes. Set BELOW the raw filesystem size — the store, logs,
	// and per-sandbox runsc state are real bytes this term does not model.
	DiskByte int64
	// MemoryByte is the total host RAM the engine may promise to sandboxes. Set
	// below physical RAM: the host itself, the orchestrator, and per-sandbox
	// sentry/gofer overhead all live outside this budget.
	//
	// Honest residual (§3.4): the uncounted per-sandbox overhead scales with the
	// sandbox COUNT, so its share of the budget grows as caps shrink — a few
	// percent at a 1 GiB cap, up to 100% at a 64 MiB one. No "reserve X% of
	// physical RAM" rule is therefore safe across all cap sizes.
	MemoryByte int64
}

// admit checks whether granting `want` keeps the node within its declared
// capacity, counting every already-committed promise (§3.4 no-oversubscribe).
//
// excludeID names a sandbox whose OWN promise must not be counted, used by
// resume: the resuming sandbox's caps are already committed rows, so counting
// them alongside `want` would charge it twice and fail a resume that changes
// nothing. Empty for create, whose promise is not yet committed.
//
// A refusal happens BEFORE any bytes or placement are materialized, so the caller
// is left in a clean source state — create has inserted nothing, resume stays
// `paused` (§4.4-A atomic return to the source state).
func (m *Mediator) admit(ctx context.Context, want model.Limits, excludeID model.SandboxID) error {
	cap := m.deps.NodeCapacity
	if cap.DiskByte <= 0 && cap.MemoryByte <= 0 {
		return nil // no declared budget → nothing to gate against (unmetered)
	}

	// The metered-axis cap premise (§3.4), checked before any arithmetic: without
	// it the inequality below is not about this node's storage at all. Refusing is
	// deliberate over substituting a default — a default would override an
	// EXPLICIT request (0 means unlimited here, stated in the wire contract, the
	// model, and the schema), and would then produce ENOSPC at a boundary the
	// caller never named, which is exactly what §2.3's "ENOSPC is an EXPECTED
	// data-plane result" stops being true about. A refusal is visible; a
	// substituted cap is not.
	if cap.DiskByte > 0 && want.DiskByte <= 0 {
		return fmt.Errorf("admission: %w: disk", ErrCapRequired)
	}
	if cap.MemoryByte > 0 && want.MemoryByte <= 0 {
		return fmt.Errorf("admission: %w: memory", ErrCapRequired)
	}

	recs, err := m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("admission: list sandboxes: %w", err)
	}
	// Σ over COMMITTED promises. Every non-destroyed record counts, including
	// `paused` ones: a paused sandbox still holds its Overlay bytes on the backend
	// (§4.2 零捕获) and will expect its cap honored on resume, so its reservation
	// is real. RAM is the one axis where a paused sandbox releases the resource
	// (no container, no memcg) — but we still count it, because admitting new RAM
	// against memory that a paused sandbox is entitled to reclaim on resume is
	// exactly the oversubscription this gate exists to prevent.
	var committedDisk, committedMem int64
	for _, r := range recs {
		if r.ID == excludeID || r.State == model.Destroyed {
			continue
		}
		committedDisk += r.Limits.DiskByte
		committedMem += r.Limits.MemoryByte
	}

	if cap.DiskByte > 0 {
		// The term the caps cannot express (D6.2): bytes under no declared cap —
		// Template/layer dirs, and any uncapped Overlay unit. The provider measures
		// its own storage; the mediator asks, the provider answers (§5.B, the same
		// shape as SweepOrphanLayers).
		uncapped, err := m.deps.Rootfs.UncappedBytes(ctx)
		if err != nil {
			// Refuse rather than admit blind: an unmeasurable backend means the
			// inequality cannot be evaluated, and admitting on an unknown total is how
			// a node gets overcommitted. Failing closed costs a rejected create;
			// failing open costs write failures across unrelated sandboxes (§2.3).
			return fmt.Errorf("admission: %w: cannot measure uncapped bytes: %v", ErrInsufficientCapacity, err)
		}
		if total := committedDisk + uncapped + want.DiskByte; total > cap.DiskByte {
			return fmt.Errorf("admission: %w: disk — committed caps %d + uncapped bytes %d + requested %d = %d > node capacity %d",
				ErrInsufficientCapacity, committedDisk, uncapped, want.DiskByte, total, cap.DiskByte)
		}
	}

	if cap.MemoryByte > 0 {
		if total := committedMem + want.MemoryByte; total > cap.MemoryByte {
			return fmt.Errorf("admission: %w: memory — committed caps %d + requested %d = %d > node capacity %d",
				ErrInsufficientCapacity, committedMem, want.MemoryByte, total, cap.MemoryByte)
		}
	}
	return nil
}
