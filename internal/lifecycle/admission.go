package lifecycle

import (
	"context"
	"fmt"

	"warren/internal/model"
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
// BOTH TERMS SUM ACROSS §5.B AND §5.C, NOT JUST SANDBOXES (§3.4). The first term is
// Σ(sandbox disk caps) + Σ(VOLUME caps); the second is
// Σ(§5.B uncapped bytes) + Σ(§5.C uncapped bytes), each reported by its own
// subsystem through the same-shaped seam method. There is NO third term: a volume
// cap and a sandbox cap are LIKE TERMS on the same left-hand side — both are upper
// bounds the engine has declared — even though a volume cap is recorded on the
// volume rather than in some sandbox's Binding (§5.C: a volume has no placement of
// its own). Omitting them is not "counting a little low": a node can be filled by a
// pile of uncapped volumes while the gate refuses nothing at all.
//
// THREE ENTRY POINTS, one inequality. `admit` gates a new/raised per-SANDBOX
// promise (create, resume, a cap raise) and adds to the FIRST term.
// `admitUncappedGrowth` gates a control-plane action that adds bytes to the SECOND
// term with no cap over them (Promote freezing a layer). `admitVolume` gates a new
// VOLUME cap, which also lands in the first term.
//
// They are deliberately not one function: which term an action lands in is the whole
// content of the accounting argument above, so the call site has to say it. And
// `admitVolume` specifically must NOT reuse `admit` — that function's
// metered-axis-requires-a-cap check is a statement about a SANDBOX (all three axes,
// including RAM), while a volume has exactly one axis and no Binding at all. Passing
// a volume through `admit` would demand a memory cap from an object that cannot have
// one.
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

// admitUncappedGrowth is the SECOND entry point to the same inequality, for a
// control-plane action that adds bytes to the MEASURED term instead of making a
// new promise (§3.4: "any control-plane action that increases uncapped bytes
// should clear the same inequality"; rootfs-overlay D6.5 残留). Promote is the
// first such action — a frozen layer is charged to no sandbox cap, so it lands
// entirely in the second term.
//
// It is a separate function rather than `admit(model.Limits{DiskByte: n})` for a
// reason that is semantic, not cosmetic. The arithmetic coincides; the meaning does
// not. `want` is a PROMISE — an upper bound on future growth, which the gate then
// expects to find in the store as a committed cap and to count in the first term
// forever after. These bytes are the opposite: they are about to be REAL, no cap
// will ever cover them, and the next gate run will find them by MEASURING
// (UncappedBytes), not by summing rows. Feeding them through `want` would also trip
// the metered-axis-requires-a-cap check, which is a statement about sandboxes and
// says nothing about a layer. Keeping the two entry points distinct is what stops
// the two disjoint terms from being conflated (see the header).
//
// growth is a FORECAST, unlike the measured term it joins: it is asked before the
// action runs, so it must be an upper bound (rootfs.Provider.PromoteCostBytes
// states that contract). Only the disk budget is consulted — uncapped bytes are
// disk by construction, and the two budgets are not fungible (§3.4).
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
		// Same fail-closed reasoning as admit: an unmeasurable backend means the
		// inequality cannot be evaluated, and the cost of admitting blind lands on
		// unrelated sandboxes (§2.3), not on this caller.
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

// sumVolumeCaps is the §5.C half of the FIRST term: Σ(volume caps). Volume caps are
// like terms with sandbox caps — both are declared upper bounds — so they join the
// same sum rather than getting a term of their own (§3.4).
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
// it — omitting the volume half is a silent overcommit, not a rounding error (§3.4).
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
// cap (§3.4 — the term sums across §5.B and §5.C, each answering about its own
// storage through the same-shaped seam method).
//
// FAILS CLOSED on either side, including a missing volume backend. An unmeasurable
// half means the inequality cannot be evaluated, and admitting on an unknown total is
// how a node gets overcommitted; the cost of failing closed is a rejected create,
// while the cost of failing open lands as ENOSPC on unrelated sandboxes (§2.3).
func (m *Mediator) uncappedBytes(ctx context.Context) (int64, error) {
	total, err := m.deps.Rootfs.UncappedBytes(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot measure uncapped rootfs bytes: %w", err)
	}
	if m.deps.Volume == nil {
		// Not "no volumes" — unknown. With the disk axis metered, a nil backend means
		// every volume's bytes are invisible to this term, which is precisely the
		// silent-overcommit shape the term exists to prevent.
		return 0, fmt.Errorf("no volume backend registered, so §5.C's share of the uncapped term cannot be measured (§3.4)")
	}
	volBytes, err := m.deps.Volume.UncappedBytes(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot measure uncapped volume bytes: %w", err)
	}
	return total + volBytes, nil
}

// admitVolume gates a NEW volume cap against the disk budget (§3.4 / §5.C). It is
// the volume-shaped sibling of `admit`, deliberately not a call into it — see the
// header for why routing a volume through `admit` would demand a memory cap from an
// object that has no Binding.
//
// A METERED DISK AXIS REQUIRES A VOLUME CAP, for the reason §3.4 gives verbatim for
// sandboxes: an uncapped volume contributes 0 to the left side while free to consume
// all of the right, which does not undercount so much as make the inequality describe
// a different device. So `size` is REQUIRED (400) while disk is metered, and
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
		return fmt.Errorf("admission: %w: volume size — the node meters disk, so an uncapped volume would contribute nothing to the budget while free to consume all of it (§3.4 / §5.C)", ErrCapRequired)
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

// admitVolumeRaise gates a cap RAISE on an existing volume (§6.1 PATCH: "raising
// re-clears admission", §3.4: any write to a cap changes Σ at THIS commit rather
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
	// (§4.2: pause captures nothing) and will expect its cap honored on resume, so
	// its reservation
	// is real. RAM is the one axis where a paused sandbox releases the resource
	// (no container, no memcg) — but we still count it, because admitting new RAM
	// against memory that a paused sandbox is entitled to reclaim on resume is
	// exactly the oversubscription this gate exists to prevent.
	_, committedMem := sumCommitted(recs, excludeID)

	if cap.DiskByte > 0 {
		// The disk half of the first term sums BOTH subsystems: sandbox caps AND volume
		// caps (§3.4). They are like terms, so they share one sum — and leaving the
		// volume half out would let a pile of volumes overcommit the node while every
		// sandbox cap stayed individually honored.
		committedDisk, err := m.committedDisk(ctx, excludeID, "")
		if err != nil {
			return err
		}
		// The term the caps cannot express (D6.2): bytes under no declared cap —
		// Template/layer dirs, any uncapped Overlay unit, and any uncapped VOLUME. Each
		// subsystem measures its own storage; the mediator asks, the subsystem answers
		// (§5.B/§5.C, the same shape as SweepOrphanLayers).
		uncapped, err := m.uncappedBytes(ctx)
		if err != nil {
			// Refuse rather than admit blind: an unmeasurable backend means the
			// inequality cannot be evaluated, and admitting on an unknown total is how
			// a node gets overcommitted. Failing closed costs a rejected create;
			// failing open costs write failures across unrelated sandboxes (§2.3).
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
// term so a CONCURRENT gate run sees it (§3.4 no-oversubscribe under concurrency).
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
// unrelated sandbox as ENOSPC (§2.3), which is the harm the gate exists to prevent.
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
