package lifecycle

import (
	"context"
	"fmt"

	"github.com/TSHOGX/khora/internal/model"
)

// Admission — the the documented invariant / rootfs-overlay the documented invariant no-oversubscribe gate.
//
// WHY THIS EXISTS: per-sandbox caps (the documented invariant) say nothing about the SUM. Promise 100 sandboxes
// 10 GiB each on a 200 GiB node and every individual cap is honored right up to the moment the
// node runs dry, at which point writes fail for sandboxes nowhere near their own cap — the documented invariant's
// containment promise broken from the other direction. So a cap the node cannot honor is
// refused at PROMISE time, here.
//
// The formula is stated by the documented invariant / rootfs-overlay the documented invariant, and its shape is forced by
// the choice of squota:
//
//	Σ(declared caps) + Σ(bytes under no cap) ≤ configured node capacity
//
// The second term is SEPARATE and non-negotiable: squota charges a sandbox only for bytes IT
// wrote, and copy-up from a Template on the same fs is a free reflink (a reason we chose it —
// the documented invariant), so Template and layer bytes occupy real disk no sandbox cap covers. Omit it and the
// accounting silently overcommits by the whole Template library.
//
// The two terms are DISJOINT on the predicate "is it covered by a declared cap", not "is it a
// layer": a CAPPED upper contributes zero to the measured term, charging its usage on top of its
// own cap billing the same bytes twice. Mixing a promise with a measurement is sound because the
// measured bytes are ALREADY ON DISK — the cap where one exists, the measurement where none does
// (the documented invariant).
//
// RAM is admitted on the same principle against a SEPARATE budget: memory and disk are not
// fungible, so two ceilings, never one blended "resources" number.
//
// A METERED AXIS REQUIRES A CAP (the documented invariant): an uncapped sandbox on a metered axis contributes 0 to
// the left side while free to consume all of the right, which does not undercount — it makes
// the inequality describe a different device than the one being written to. Enforced here
// rather than assumed. `0 = unlimited` stays legal on an UNMETERED axis.
//
// BOTH TERMS SUM ACROSS the documented invariant AND the documented invariant, not just sandboxes (the documented invariant), each subsystem reporting
// through the same-shaped seam method. No third term: a volume cap and a sandbox cap are LIKE
// TERMS on the same left-hand side, even though a volume cap is recorded on the volume and a
// volume has no placement of its own. Omitting them is not "counting a little low" — a node can
// be filled by a pile of uncapped volumes while the gate refuses nothing.
//
// THREE ENTRY POINTS, one inequality: `admit` for a new/raised per-SANDBOX promise (first term),
// `admitUncappedGrowth` for bytes joining the SECOND term uncapped (Promote), and `admitVolume`
// for a new volume cap (first term). Separate because which term an action lands in is the call
// site's to state. `admitVolume` specifically must not reuse `admit`, whose metered-axis check
// covers all three SANDBOX axes and would demand a memory cap from an object that cannot have
// one.
//
// In the the documented invariant structure: admission reads the AUTHORITATIVE store and asks the provider to measure
// its own bytes — both mediator actions, the provider never consulting the store.

// NodeCapacity is the operator-configured budget the gate admits against (the documented invariant
// single-binary config). Zero on a dimension = UNMETERED: that axis is not
// gated at all, which is the correct default for a deployment that has not
// declared its budget — the engine must not invent a number and start refusing
// creates on a guess.
//
// WHAT THE BUDGET MEANS (the documented invariant): the bytes the engine may PROMISE to guests — not
// the host's physical headroom. Substrate costs the engine cannot name without
// breaking a seam live OUTSIDE it: runsc state and bundles on disk, the
// sentry/gofer footprint in RAM (that one is a provider implementation constant
// by the documented invariant's design, deliberately not a contract member). Operators set each
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
	// Honest residual (the documented invariant): the uncounted per-sandbox overhead scales with the
	// sandbox COUNT, so its share of the budget grows as caps shrink — a few
	// percent at a 1 GiB cap, up to 100% at a 64 MiB one. No "reserve X% of
	// physical RAM" rule is therefore safe across all cap sizes.
	MemoryByte int64
}

// admitUncappedGrowth is the SECOND entry point to the same inequality, for a
// control-plane action that adds bytes to the MEASURED term instead of making a
// new promise (the documented invariant: "any control-plane action that increases uncapped bytes
// should clear the same inequality"; rootfs-overlay the documented invariant's residual). Promote is the
// first such action — a frozen layer is charged to no sandbox cap, so it lands
// entirely in the second term.
//
// It is a separate function rather than `admit(model.Limits{DiskByte: n})` because the
// arithmetic coincides while the meaning does not. `want` is a PROMISE — an upper bound
// the gate then expects to find in the store as a committed cap and to count in the first
// term forever after. These bytes are the opposite: they are about to be REAL, no cap will
// ever cover them, and the next gate run will find them by MEASURING (UncappedBytes), not
// by summing rows. Feeding them through `want` would also trip the
// metered-axis-requires-a-cap check, which says nothing about a layer.
//
// growth is a FORECAST, unlike the measured term it joins: it is asked before the
// action runs, so it must be an upper bound (rootfs.Provider.PromoteCostBytes
// states that contract). Only the disk budget is consulted — uncapped bytes are
// disk by construction, and the two budgets are not fungible (the documented invariant).
//
// It is the CALLER's job to hold admitMu across {gate → make the growth visible},
// exactly as create holds it across {gate → insert the record}. For Promote the
// bytes do not become measurable until the copy finishes, which is far too long to
// hold a node-global lock, so the mediator publishes a RESERVATION instead — see
// pendingUncappedBytes.
func (m *Mediator) admitUncappedGrowth(ctx context.Context, growth int64) error {
	capDisk := m.deps.NodeCapacity.DiskByte
	if capDisk <= 0 {
		return nil // disk unmetered → this inequality does not describe this node
	}
	if growth < 0 {
		return fmt.Errorf("admission: negative uncapped growth %d", growth)
	}
	committedDisk, err := m.committedDisk(ctx, "", "")
	if err != nil {
		return err
	}
	uncapped, err := m.uncappedBytes(ctx)
	if err != nil {
		// Fail closed, for the reason on uncappedBytes.
		return fmt.Errorf("admission: %w: %v", ErrInsufficientCapacity, err)
	}
	pending := m.pendingUncapped()
	if total := committedDisk + uncapped + pending + growth; total > capDisk {
		return fmt.Errorf("admission: %w: uncapped growth — committed caps %d + uncapped bytes %d + in-flight %d + growth %d = %d > node capacity %d",
			ErrInsufficientCapacity, committedDisk, uncapped, pending, growth, total, capDisk)
	}
	return nil
}

// sumCommitted is Σ over COMMITTED promises, shared by both entry points so the
// first term cannot drift between them. excludeID drops one sandbox's own row (see
// admit); model.Destroyed records are already gone in every sense that matters.
func sumCommitted(recs []model.Sandbox, excludeID model.SandboxID) (disk, mem int64) {
	for _, r := range recs {
		if r.ID == excludeID || r.State == model.Destroyed {
			continue
		}
		disk += r.Limits.DiskByte
		mem += r.Limits.MemoryByte
	}
	return disk, mem
}

// sumVolumeCaps is the the documented invariant half of the FIRST term: Σ(volume caps). Volume caps are
// like terms with sandbox caps — both are declared upper bounds — so they join the
// same sum rather than getting a term of their own (the documented invariant).
//
// excludeID drops one volume's own committed cap, used by a cap RAISE for the same
// reason resume excludes its own sandbox: the existing cap is already a committed
// row, so counting it beside the requested value would charge the volume twice and
// refuse a raise that only needs the delta.
func sumVolumeCaps(vols []model.Volume, excludeID model.VolumeID) int64 {
	var total int64
	for _, v := range vols {
		if v.ID == excludeID {
			continue
		}
		total += v.CapByte
	}
	return total
}

// committedDisk is the FIRST term in full: sandbox caps + volume caps, read from the
// authoritative store. One function so no caller can accidentally sum only half of
// it — omitting the volume half is a silent overcommit, not a rounding error (the documented invariant).
func (m *Mediator) committedDisk(ctx context.Context, excludeSandbox model.SandboxID, excludeVolume model.VolumeID) (int64, error) {
	recs, err := m.deps.Store.ListSandboxes(ctx)
	if err != nil {
		return 0, fmt.Errorf("admission: list sandboxes: %w", err)
	}
	sandboxDisk, _ := sumCommitted(recs, excludeSandbox)
	vols, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("admission: list volumes: %w", err)
	}
	return sandboxDisk + sumVolumeCaps(vols, excludeVolume), nil
}

// uncappedBytes is the SECOND term in full: both subsystems' bytes under no declared
// cap (the documented invariant — the term sums across the documented invariant and the documented invariant, each answering about its own
// storage through the same-shaped seam method).
//
// FAILS CLOSED on either side, including a missing volume backend. An unmeasurable
// half means the inequality cannot be evaluated, and admitting on an unknown total is
// how a node gets overcommitted; the cost of failing closed is a rejected create,
// while the cost of failing open lands as ENOSPC on unrelated sandboxes (the documented invariant).
func (m *Mediator) uncappedBytes(ctx context.Context) (int64, error) {
	total, err := m.deps.Rootfs.UncappedBytes(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot measure uncapped rootfs bytes: %w", err)
	}
	if m.deps.Volume == nil {
		// Not "no volumes" — unknown. With the disk axis metered, a nil backend means
		// every volume's bytes are invisible to this term, which is precisely the
		// silent-overcommit shape the term exists to prevent.
		return 0, fmt.Errorf("no volume backend registered, so the documented invariant's share of the uncapped term cannot be measured (the documented invariant)")
	}
	volBytes, err := m.deps.Volume.UncappedBytes(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot measure uncapped volume bytes: %w", err)
	}
	return total + volBytes, nil
}

// admitVolume gates a NEW volume cap against the disk budget (the documented invariant / the documented invariant). It is
// the volume-shaped sibling of `admit`, deliberately not a call into it — see the
// header for why routing a volume through `admit` would demand a memory cap from an
// object that has no Binding.
//
// A METERED DISK AXIS REQUIRES A VOLUME CAP, for the reason the documented invariant gives for sandboxes
// (see the header): so `size` is REQUIRED (400) while disk is metered, and
// `0 = uncapped` stays legal only on an unmetered axis.
//
// The caller holds admitMu across {gate → make the promise visible}, exactly as
// create does.
func (m *Mediator) admitVolume(ctx context.Context, capBytes int64) error {
	capDisk := m.deps.NodeCapacity.DiskByte
	if capDisk <= 0 {
		return nil // disk unmetered → this inequality does not describe this node
	}
	if capBytes <= 0 {
		return fmt.Errorf("admission: %w: volume size — this node meters disk, and an uncapped volume contributes nothing to the budget while free to consume all of it (the documented invariant / the documented invariant)", ErrCapRequired)
	}
	committed, err := m.committedDisk(ctx, "", "")
	if err != nil {
		return err
	}
	uncapped, err := m.uncappedBytes(ctx)
	if err != nil {
		return fmt.Errorf("admission: %w: %v", ErrInsufficientCapacity, err)
	}
	pending := m.pendingUncapped()
	if total := committed + uncapped + pending + capBytes; total > capDisk {
		return fmt.Errorf("admission: %w: volume — committed caps %d + uncapped bytes %d + in-flight %d + requested %d = %d > node capacity %d",
			ErrInsufficientCapacity, committed, uncapped, pending, capBytes, total, capDisk)
	}
	return nil
}

// admitVolumeRaise gates a cap RAISE on an existing volume (the documented invariant PATCH: "raising
// re-clears admission", the documented invariant: any write to a cap changes Σ at THIS commit rather
// than at some later placement).
//
// The volume's own committed cap is EXCLUDED, so the check is against the increase
// rather than double-charging a promise the node has already accepted — the same
// exclusion resume applies to a sandbox's own caps.
func (m *Mediator) admitVolumeRaise(ctx context.Context, id model.VolumeID, capBytes int64) error {
	capDisk := m.deps.NodeCapacity.DiskByte
	if capDisk <= 0 {
		return nil
	}
	committed, err := m.committedDisk(ctx, "", id)
	if err != nil {
		return err
	}
	uncapped, err := m.uncappedBytes(ctx)
	if err != nil {
		return fmt.Errorf("admission: %w: %v", ErrInsufficientCapacity, err)
	}
	pending := m.pendingUncapped()
	if total := committed + uncapped + pending + capBytes; total > capDisk {
		return fmt.Errorf("admission: %w: volume cap raise — committed caps %d + uncapped bytes %d + in-flight %d + requested %d = %d > node capacity %d",
			ErrInsufficientCapacity, committed, uncapped, pending, capBytes, total, capDisk)
	}
	return nil
}

// admit checks whether granting `want` keeps the node within its declared
// capacity, counting every already-committed promise (the documented invariant no-oversubscribe).
//
// excludeID names a sandbox whose OWN promise must not be counted, used by
// resume: the resuming sandbox's caps are already committed rows, so counting
// them alongside `want` would charge it twice and fail a resume that changes
// nothing. Empty for create, whose promise is not yet committed.
//
// A refusal happens BEFORE any bytes or placement are materialized, so the caller
// is left in a clean source state — create has inserted nothing, resume stays
// `paused` (the documented invariant atomic return to the source state).
func (m *Mediator) admit(ctx context.Context, want model.Limits, excludeID model.SandboxID) error {
	cap := m.deps.NodeCapacity
	if cap.DiskByte <= 0 && cap.MemoryByte <= 0 {
		return nil // no declared budget → nothing to gate against (unmetered)
	}

	// The metered-axis cap premise (the documented invariant), checked before any arithmetic: without
	// it the inequality below is not about this node's storage at all. Refusing is
	// deliberate over substituting a default — a default would override an
	// EXPLICIT request (0 means unlimited here, stated in the wire contract, the
	// model, and the schema), and would then produce ENOSPC at a boundary the
	// caller never named, which is exactly what the documented invariant's "ENOSPC is an EXPECTED
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
	// (the documented invariant: pause captures nothing) and will expect its cap honored on resume. RAM
	// is the one axis where a paused sandbox releases the resource (no container, no
	// memcg) — but we still count it, because admitting new RAM against memory that a
	// paused sandbox is entitled to reclaim on resume is exactly the oversubscription
	// this gate exists to prevent.
	_, committedMem := sumCommitted(recs, excludeID)

	if cap.DiskByte > 0 {
		// The disk half of the first term sums BOTH subsystems: sandbox caps AND volume
		// caps (the documented invariant). They are like terms, so they share one sum — and leaving the
		// volume half out would let a pile of volumes overcommit the node while every
		// sandbox cap stayed individually honored.
		committedDisk, err := m.committedDisk(ctx, excludeID, "")
		if err != nil {
			return err
		}
		// The term the caps cannot express (the documented invariant): bytes under no declared cap —
		// Template/layer dirs, any uncapped Overlay unit, and any uncapped VOLUME. Each
		// subsystem measures its own storage; the mediator asks, the subsystem answers
		// (the documented invariant/the documented invariant, the same shape as SweepOrphanLayers).
		uncapped, err := m.uncappedBytes(ctx)
		if err != nil {
			// Refuse rather than admit blind, for the reason on uncappedBytes.
			return fmt.Errorf("admission: %w: %v", ErrInsufficientCapacity, err)
		}
		// In-flight uncapped growth counts too (pendingUncappedBytes): a Promote that
		// has cleared its own gate and is mid-copy owns those bytes even though
		// UncappedBytes cannot see all of them yet. Omitting it would let a create
		// admitted during the copy find the capacity gone when the layer lands.
		pending := m.pendingUncapped()
		if total := committedDisk + uncapped + pending + want.DiskByte; total > cap.DiskByte {
			return fmt.Errorf("admission: %w: disk — committed caps %d + uncapped bytes %d + in-flight %d + requested %d = %d > node capacity %d",
				ErrInsufficientCapacity, committedDisk, uncapped, pending, want.DiskByte, total, cap.DiskByte)
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

// reserveUncapped / releaseUncapped publish an in-flight addition to the measured
// term so a CONCURRENT gate run sees it (the documented invariant no-oversubscribe under concurrency).
//
// WHY A RESERVATION AND NOT JUST THE MUTEX. Every other gate caller holds admitMu
// across {evaluate → make the change visible}: create inserts its record, a raise
// commits the column, and the next run's store read finds it. Promote cannot do
// that — its bytes only become visible to UncappedBytes as the copy proceeds, and
// the copy is a full byte-for-byte tree copy. Holding a node-global lock for its
// duration would serialize every create on the whole node behind one Promote. So
// the reservation IS Promote's "commit": published under admitMu before the lock is
// released, dropped once the bytes are real (or the attempt failed).
//
// Without it two concurrent Promotes each measure a pre-copy world, each fit, and
// together overshoot — the same race admitMu exists to stop for create, reached by
// a different route.
//
// It transiently DOUBLE-COUNTS: as the copy lands, those bytes appear in
// UncappedBytes while the reservation still stands. That overstates the left side,
// which refuses a create that might have fit — the conservative direction, and the
// one this gate is supposed to err in. Undercounting would hand the failure to an
// unrelated sandbox as ENOSPC (the documented invariant), which is the harm the gate exists to prevent.
func (m *Mediator) reserveUncapped(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.pendingUncappedBytes += n
	m.mu.Unlock()
}

func (m *Mediator) releaseUncapped(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.pendingUncappedBytes -= n
	neg := m.pendingUncappedBytes < 0
	m.mu.Unlock()
	if neg {
		// Unreachable by construction (one release per admitted reservation, via
		// defer). Clamping to 0 would ERASE the evidence of the opposite bug: a
		// reservation released once too FEW permanently shrinks the node's
		// admissible capacity, and a silent clamp is what makes that invisible.
		// Same choice, same reason, as the transition lock's refs < 0.
		panic("lifecycle: uncapped reservation released more times than acquired")
	}
}

func (m *Mediator) pendingUncapped() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingUncappedBytes
}
