package lifecycle

import (
	"context"
	"fmt"

	"engine/internal/model"
	"engine/internal/store"
)

// This file holds the ONE write point behind `PATCH /v1/sandboxes/{id}` (§6.1 /
// §3.4.5). It is one mediator verb rather than three because §4.4-B says so
// literally: PATCH "holds this sandbox's lock FOR ITS WHOLE DURATION, and is
// therefore mutually exclusive with destroy/resume/evict — a cap edit cannot
// interleave with a resume that is in progress".
//
// Three separately-locking mediator calls (the previous shape: UpdateBinding →
// SetNetwork → RebindVolumes) satisfy that sentence for each call and violate it for
// the REQUEST, because between call N's Unlock and call N+1's Lock there is a window
// a whole resume/evict fits inside. The reachable damage was not hypothetical:
// `{retention, network}` on a `paused` sandbox committed the retention, a resume then
// took the lock, and the declaration write answered 409 `state=running` — the caller
// was told the request was refused while half of it was in the store. With evict in
// the window it is a 404 plus a write to a record that no longer exists.
//
// THE DECIDING ARGUMENT IS IN THE STORE CONTRACT. store.Store.ReplaceMounts pays a
// real price (one statement expressing a cross-row predicate, in each backend) to
// avoid TWO independent commits, and justifies it with §6.2's "a sandbox holds one
// complete binding at every instant". A control plane that tolerates three commits
// per request above a seam that refuses two below it is contradicting itself, not
// making a trade.

// RebindSpec is one PATCH request's whole payload: the §3.4 Binding fields plus the
// §5.C mount binding. It exists so the three field classes reach the mediator as ONE
// value — a caller cannot express "half a PATCH" and therefore cannot be served half
// of one.
type RebindSpec struct {
	// Binding carries caps, TTLs and the §5.D declaration. Each field is a pointer, so
	// "not given" is distinguishable from "set to zero" (store.Binding says why that
	// distinction is load-bearing on every axis, the declaration included).
	Binding store.Binding

	// Mounts is the COMPLETE mount binding, or nil for "`volumes` was omitted".
	//
	// A POINTER TO A SLICE, and the indirection is required rather than fussy: an EMPTY
	// binding legitimately means "detach everything" (§6.2 — PATCH swaps whole binding
	// for whole binding, there is no detach-one operation), so `len(x) == 0` cannot
	// distinguish "detach everything" from "leave the mounts alone". A plain nil slice
	// would silently turn every unrelated PATCH into a detach-all.
	Mounts *[]model.Mount
}

// isEmpty reports whether the request asks for no change at all.
func (s RebindSpec) isEmpty() bool { return s.Binding.IsEmpty() && s.Mounts == nil }

// Rebind is the §6.1 PATCH verb: it rewrites any subset of a sandbox's Binding — caps,
// TTLs, the §5.D declaration, the §5.C mount binding — under ONE acquisition of the
// §4.4-B transition lock, and returns whether the change released an exclusive lease.
//
// It is NOT a §4.1 transition: the state does not change, so it publishes no event
// (§3.4.5, the same reasoning Promote uses).
//
// # The state gate is the UNION over the field classes, and it is evaluated in full before any write
//
// Three consumers, three materialization times (§3.4), so three gates:
//
//   - RESOURCE CAPS are consumed by the providers, which materialize only at placement
//     → editable ONLY from `paused`, effective at the next resume. Accepting a cap edit
//     on a `running` sandbox would leave the record and the live isolation unit
//     disagreeing for an unbounded period — the fake invariant CLAUDE.md calls a
//     negative asset. One extra pause buys zero ambiguity.
//   - THE DECLARATION is consumed by the provider at placement too → same gate, and for
//     a sharper version of the same reason: §6.1's read face rests on the
//     biconditional "`running` ⟺ what is installed IS this declaration", and a
//     `running` edit would break it on a security-relevant field. (`ingress.publish[]`
//     has its own hot-add write point, AddPublish, which IS legal on `running` because
//     it is purely additive and touches no isolation unit.)
//   - TTLs are consumed by the reaper, which re-reads the record every sweep with no
//     placement involved → editable from either state, effective immediately.
//   - THE MOUNT BINDING splits: the guest topology changes at the next resume (runsc
//     takes mounts from the OCI spec at startup and does not hot-plug), while a detach
//     releases the exclusive lease IMMEDIATELY, because the lease IS the row (§5.C).
//     Both states are legal; the two effect times are why `released` is returned
//     separately from the caller's `applies_at`.
//
// EVALUATING THE WHOLE UNION FIRST IS THE POINT, not tidiness: a per-class gate checked
// just before that class's own write is exactly the three-commit shape again — the
// second class's refusal would arrive after the first class had committed.
//
// A MERGED GATE ALSO CLOSES A SECOND-ORDER DIVERGENCE that the split shape carried:
// `UpdateBinding` barred caps on `running` but NOT the declaration, while `SetNetwork`
// barred it. No production caller wrote a declaration through the former, so nothing
// was reachable — but "one field, two write points, two criteria" is how a reachable
// version arrives later. With one gate over one spec the agreement is structural.
//
// # Why the writes are ordered mounts-first, binding-second
//
// Not style. ReplaceMounts is the LAST remaining write that can still produce a
// caller-visible VERDICT: the exclusive lease is arbitrated by the store at commit time
// (409 ErrMountLeaseConflict) and a self-contradictory binding is decided there too
// (400 ErrMountBindingInvalid) — store.Store.CreateMounts states why that judgment
// cannot be hoisted into a pre-check. Putting it FIRST means every 4xx this verb can
// answer, without exception, is a refusal that changed nothing.
//
// The Store.UpdateBinding that follows cannot fail for a concurrency reason: its CAS
// expects the state this function read under the transition lock, and every production
// write to a sandbox's state holds that same lock (§4.4-B). What remains is store I/O,
// which no ordering can rule out — see the residue note below.
//
// # Known residue, stated rather than papered over
//
// The mount table and the sandbox row are still TWO commits, so a store I/O failure
// between them can leave "new mounts, old caps". Making it one commit needs a new store
// seam spanning both tables, which is its own decision and is deliberately not taken
// here. There is no reachable defect today: the admission gate sums sandbox caps and
// volume caps and never reads mount rows, and a resume re-materializes from whatever
// the table says — but that is a judgment about today's consumers, not a proof.
func (m *Mediator) Rebind(ctx context.Context, id model.SandboxID, spec RebindSpec) (bool, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	if spec.isEmpty() {
		return false, nil // nothing asked for → no write (idempotent)
	}
	b := spec.Binding

	// §6.2's declaration refusals, at the WRITE POINT rather than only in the API layer —
	// which is what makes them structural, and is where create and publish already apply
	// them (Create, AddPublish). A validation that lives in one caller is a validation
	// the next caller does not have.
	//
	// FIRST, ahead of even the state gate, because this refusal does not depend on any
	// record: the declaration is a pure function of the request, and it is illegal in
	// every state. Gating first would answer a `running` sandbox with 409 "pause first,
	// then PATCH" for a request that pausing cannot make legal — the same mistake the
	// disk `0 → N` check avoids by preceding the admission gate. It also keeps §6.2's
	// "validation precedes the admission gate" true by construction.
	if b.Network != nil {
		if err := m.ValidateDeclaration(*b.Network, true); err != nil {
			return false, err
		}
	}

	// ONE read, feeding every gate below. Re-reading per class would reintroduce the
	// defect in miniature: two reads under one lock cannot disagree, but they invite the
	// next edit to move a write between them.
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return false, fmt.Errorf("rebind: %w", err)
	}

	capsGiven := b.CPUMillis != nil || b.MemoryByte != nil || b.DiskByte != nil
	switch rec.State {
	case model.Paused:
		// everything editable
	case model.Running:
		// Two separate messages rather than one covering both classes: the REMEDIES
		// differ (a declaration has a hot-add endpoint for the one field that can take
		// effect live; a cap has nothing of the kind), and §6.2 asks a refusal to say what
		// the caller should do instead.
		if capsGiven {
			return false, fmt.Errorf("rebind: %w: resource caps are editable only from `paused` (they re-materialize at the next placement, §3.4) — pause first, then PATCH, then resume; TTL and `volumes` are editable while running",
				store.ErrConflict)
		}
		if b.Network != nil {
			return false, fmt.Errorf("rebind: %w: the network declaration is editable only from `paused` "+
				"(the rule set is regenerated at the next placement, §3.4/§5.D) — pause first, then "+
				"PATCH, then resume. `POST /v1/sandboxes/%s/publish` is the one field that hot-adds on a "+
				"running sandbox", store.ErrConflict, id)
		}
	default:
		// `creating` and the terminal states. Excluded for all four classes: a sandbox
		// mid-create has its binding under construction by its own create verb.
		return false, fmt.Errorf("rebind: %w (state=%s: a Binding is editable only from running or paused)",
			store.ErrConflict, rec.State)
	}

	newLimits, _, _, _ := b.Apply(rec)
	// §3.4: `0 → N` on the DISK axis is a refused write, not a raise. The cap has no
	// materialization to land on — the storage unit was only built as a quota-capable
	// subvolume if a cap existed at create — so accepting would record a promise every
	// future placement must refuse, and the sandbox would never resume again. Refused
	// HERE, where nothing has changed yet and the caller can see it, rather than in the
	// recovery path where it becomes an unresumable sandbox (see
	// ErrUncappedDiskNotCappable). The remedy is in the message because there is exactly
	// one: recreate the sandbox with the cap.
	//
	// Placed BEFORE the admission gate deliberately: this request is invalid regardless
	// of how much room the node has, and running the gate first would let a full node
	// answer 507 — telling the caller to wait for capacity that would never make this
	// legal.
	if rec.Limits.DiskByte == 0 && newLimits.DiskByte > 0 {
		return false, fmt.Errorf("rebind: %w: sandbox %s was created uncapped on disk, so its Overlay storage unit carries no quota row and a %d-byte cap could never be enforced — §5.B refuses an unenforceable cap at every placement, so accepting this would leave the sandbox permanently unresumable. Create a new sandbox with disk_byte set (Promote this one first to carry its bytes over); `N → 0` and `N → M` remain legal",
			ErrUncappedDiskNotCappable, id, newLimits.DiskByte)
	}

	// §3.4: an `N → M` disk edit must leave the cap STRICTLY above what the storage unit
	// already holds. See ErrDiskCapBelowUsage for the measured wedge and for why the
	// sandbox side is worse than the volume side. This is the §5.B half of the criterion
	// SetVolumeCapacity already applies to §5.C, and it needed a new seam method to become
	// checkable at all: UncappedBytes reports ZERO for a capped unit BY CONTRACT (the two
	// §3.4 terms are disjoint), so the mediator had no way to read the per-sandbox number.
	//
	// GATED ON THE AXIS BEING WRITTEN (`b.DiskByte != nil`), not on the resulting value.
	// A PATCH that only touches TTLs, `volumes` or the declaration must not pay a backend
	// round trip — and, more importantly, must not be REFUSED because of a pre-existing
	// divergence it does not touch. Within a request that does write the axis, every
	// positive value is checked, raise and lowering alike (ErrDiskCapBelowUsage says why a
	// raise is not automatically safe).
	//
	// PLACED BEFORE THE ADMISSION GATE for the same reason as the `0 → N` refusal above:
	// this request is invalid no matter how much room the node has, and gating first would
	// let a full node answer 507 — telling the caller to wait for capacity that would never
	// make it legal.
	//
	// THE USAGE READ IS SOUND HERE PRECISELY BECAUSE OF THE `paused` GATE ABOVE. Caps are
	// editable only from `paused`, so the container is stopped, the overlay is unmounted,
	// and nothing can grow the upper between this read and the placement that applies the
	// cap — §5.E's fs service needs a live guest, and a volume import writes to a volume,
	// not to this unit. So unlike the volume side, this number cannot go stale under a
	// concurrent writer. (The materialization side re-checks anyway, because §3.4.5
	// requires it against changes that do not come through a write point at all.)
	if b.DiskByte != nil && newLimits.DiskByte > 0 {
		if m.deps.Rootfs == nil {
			return false, fmt.Errorf("rebind: no rootfs provider registered")
		}
		used, _, err := m.deps.Rootfs.Usage(ctx, string(rec.Overlay))
		if err != nil {
			// FAIL CLOSED. Usage IS the decision here (unlike an inspect-shaped read, which
			// may degrade): admitting a cap without knowing usage risks the irreversible
			// wedge above, and that state has no exit through the API at all. The provider
			// already retries the transient wholesale-read failure, so this is a real
			// "cannot tell", not churn.
			return false, fmt.Errorf("rebind: cannot read current Overlay usage of %s, refusing to set a %d-byte disk cap blind (a cap at or below usage wedges the storage unit irreversibly and leaves the sandbox unresumable, §3.4/§5.B): %w", id, newLimits.DiskByte, err)
		}
		if newLimits.DiskByte <= used {
			return false, fmt.Errorf("rebind: %w: requested %d, current usage %d — a cap at or below usage wedges the Overlay storage unit irreversibly (afterwards even truncate-to-zero and the quota write itself return ENOSPC, so the cap can be neither raised nor removed, and every future placement fails, so the sandbox could never resume), and `cap == usage` means FULL rather than exactly enough. Choose a disk_byte strictly greater than %d, or 0 to drop the cap (§3.4 / §5.B)",
				ErrDiskCapBelowUsage, newLimits.DiskByte, used, used)
		}
	}
	raisesMemory := newLimits.MemoryByte > rec.Limits.MemoryByte
	// Any raise on a metered axis is a new promise → re-gate. Excludes this
	// sandbox's own committed caps, exactly as resume does, so it is measured
	// against the delta rather than double-charged.
	//
	// DISK AND MEMORY ONLY — cpu_millis is deliberately absent from the trigger set.
	// §3.4 keeps CPU out of the node accounting entirely (`cpu_millis: 0` is always
	// legal, and neither budget mentions it), so a CPU-only raise asks for zero
	// additional bytes on either axis and the gate has nothing it could correctly
	// refuse: once the left side has filled for reasons this PATCH does not touch
	// (other sandboxes' caps, volume caps, or a grown MEASURED uncapped term), the
	// only thing gating a CPU edit can produce is a FALSE disk- or RAM-flavoured 507
	// against a request for zero bytes. The metered-axis-requires-a-cap check has
	// nothing to bite on either — a CPU edit changes neither cap it inspects. "All
	// raises through one door" buys no invariant when the invariant behind that door
	// is not about CPU.
	//
	// admitMu spans {gate → commit} as ONE critical section, held by defer through both
	// writes below — the same shape create uses for {gate → insert} and
	// SetVolumeCapacity for {gate → apply → commit}. Releasing it after the gate would
	// not serialize anything: two raises on DIFFERENT sandboxes hold DISJOINT
	// per-sandbox transition locks (§4.4-B), so each would sum a Σ missing the other's
	// uncommitted raise, both would pass, and both would commit — the node over budget
	// with every individual cap honored. §3.4 is explicit that a write to a cap changes
	// Σ AT THIS COMMIT.
	//
	// It now also spans the ReplaceMounts below, which costs a little contention and
	// buys ordering the gate does not need (Σ reads sandbox caps and volume caps, never
	// mount rows). Splitting it to release earlier would mean releasing BEFORE the
	// binding commit, which is the falsified shape — so the wider section is the cheap
	// side of the trade.
	if newLimits.DiskByte > rec.Limits.DiskByte || raisesMemory {
		m.admitMu.Lock()
		defer m.admitMu.Unlock()
		if err := m.admit(ctx, newLimits, id); err != nil {
			return false, err
		}
	}

	// --- Writes start here. Everything above is a refusal that changed nothing. ------

	released := false
	if spec.Mounts != nil {
		var err error
		if released, err = m.replaceMountsLocked(ctx, id, *spec.Mounts); err != nil {
			return false, err
		}
	}

	if !b.IsEmpty() {
		var cause *model.DeathCause
		if raisesMemory && rec.DeathCause == model.CauseResourceExceeded {
			// §4.3 offers "raise the cap" as the resource-terminated remedy, and the resume
			// guard reads death_cause with nothing else clearing it before a successful
			// resume — which the guard itself prevents. So the raise and the clear must be
			// ONE commit. Scoped tightly to a RAISE of the terminating axis: a lowering, or
			// an unrelated cap edit, must not re-arm a crash loop by wiping the record of why
			// the sandbox died.
			cleared := model.CauseNone
			cause = &cleared
		}
		// The CAS expects the state read above under this same lock, so it cannot lose to a
		// concurrent transition (§4.4-B: every production state write holds this lock). A
		// failure here is store I/O — the residue this verb's doc comment names.
		if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, b, cause); err != nil {
			return released, fmt.Errorf("rebind: %w", err)
		}
	}
	return released, nil
}

// replaceMountsLocked swaps a sandbox's WHOLE mount binding, and reports whether the
// change released an exclusive lease. The CALLER holds the transition lock and has
// already run the state gate — hence `Locked`.
//
// WHOLE BINDING, NOT A DELTA. §6.2 defines PATCH as swapping one complete binding for
// another, and a sandbox holds one complete binding at every instant — so there is no
// "attach one more" or "detach that one" operation, and an EMPTY slice legitimately
// means detach everything. That is also what makes every duplicate a 400: a repeat is a
// single submission contradicting itself, decidable without reading another row.
func (m *Mediator) replaceMountsLocked(ctx context.Context, id model.SandboxID, binding []model.Mount) (bool, error) {
	before, err := m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return false, fmt.Errorf("rebind: read current binding: %w", err)
	}

	// Stamp this sandbox's ID and normalize each path (§5.C — the store's uniqueness
	// assertions are literal string comparisons, so an un-normalized path makes them
	// silently not apply).
	next := make([]model.Mount, 0, len(binding))
	for _, mt := range binding {
		next = append(next, model.Mount{
			Sandbox: id,
			Volume:  mt.Volume,
			Path:    store.NormalizeMountPath(mt.Path),
			Access:  mt.Access,
		})
	}

	// ONE COMMIT, promised by the store contract (§5.C / store.Store.ReplaceMounts). The
	// old rows must go before the new ones — a re-submitted identical binding would
	// otherwise collide with the very rows it replaces, on the lease predicate and on
	// UNIQUE(sandbox, path) — so the two statements are put in a SINGLE commit rather
	// than reordered.
	//
	// THERE IS NO RESTORE BRANCH HERE, and its absence is the point: a refused
	// replacement leaves the sandbox on its old binding by construction, so there is no
	// "new binding rejected AND old one gone" state to compensate for. A compensation
	// path for a state the model denies is a liability — it claims to repair something
	// unreachable, and the repair itself can fail.
	if err := m.deps.Store.ReplaceMounts(ctx, id, next); err != nil {
		return false, fmt.Errorf("rebind: %w", err)
	}

	// Did a detach release an exclusive lease? True when some volume held exclusively
	// before is not held exclusively now — the caller waiting to attach it elsewhere can
	// proceed immediately, even though this sandbox's topology changes only on resume.
	stillExclusive := make(map[model.VolumeID]bool, len(next))
	for _, mt := range next {
		if mt.Access == model.Exclusive {
			stillExclusive[mt.Volume] = true
		}
	}
	for _, mt := range before {
		if mt.Access == model.Exclusive && !stillExclusive[mt.Volume] {
			return true, nil
		}
	}
	return false, nil
}
