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
// The formula is stated by rootfs-overlay D6.5 / M6 §4, and its shape is forced
// by the choice of squota:
//
//	Σ(per-sandbox disk cap) + Σ(measured layer bytes) ≤ configured node capacity
//
// The layer term is SEPARATE and non-negotiable: squota charges a sandbox only for
// bytes IT wrote, and copy-up from a Template on the same fs is a reflink, which
// is free under squota (that is a reason we chose squota — see D6.2). So Template
// and layer bytes occupy real disk that NO sandbox cap covers. Omit that term and
// the accounting silently overcommits by the entire size of the Template library.
//
// RAM is admitted on the same principle but against a separate budget: memory and
// disk are not fungible, so they are two independent ceilings, never one blended
// "resources" number.
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
type NodeCapacity struct {
	// DiskByte is the total bytes available for sandbox Overlay upper+work plus
	// Template/layer bytes. Operators set it BELOW the raw filesystem size to
	// leave room for the store, logs, and runsc state, which this term does not
	// model.
	DiskByte int64
	// MemoryByte is the total host RAM the engine may promise to sandboxes. Also
	// set below physical RAM: the host itself, the orchestrator, and per-sandbox
	// sentry/gofer overhead all live outside this budget.
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

	if cap.DiskByte > 0 && want.DiskByte > 0 {
		// The term squota cannot see (D6.2): bytes held by Template/layer dirs. The
		// provider measures its own storage — the mediator asks, the provider answers
		// (§5.B, the same shape as SweepOrphanLayers).
		layerBytes, err := m.deps.Rootfs.MeasuredBytes(ctx)
		if err != nil {
			// Refuse rather than admit blind: an unmeasurable backend means the
			// inequality cannot be evaluated, and admitting on an unknown total is how
			// a node gets overcommitted. Failing closed costs a rejected create;
			// failing open costs write failures across unrelated sandboxes (§2.3).
			return fmt.Errorf("admission: %w: cannot measure layer bytes: %v", ErrInsufficientCapacity, err)
		}
		if total := committedDisk + layerBytes + want.DiskByte; total > cap.DiskByte {
			return fmt.Errorf("admission: %w: disk — committed caps %d + layer bytes %d + requested %d = %d > node capacity %d",
				ErrInsufficientCapacity, committedDisk, layerBytes, want.DiskByte, total, cap.DiskByte)
		}
	}

	if cap.MemoryByte > 0 && want.MemoryByte > 0 {
		if total := committedMem + want.MemoryByte; total > cap.MemoryByte {
			return fmt.Errorf("admission: %w: memory — committed caps %d + requested %d = %d > node capacity %d",
				ErrInsufficientCapacity, committedMem, want.MemoryByte, total, cap.MemoryByte)
		}
	}
	return nil
}
