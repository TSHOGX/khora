package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/TSHOGX/khora/internal/model"
)

// The write-regime invariant (§5.C, cross-cutting): a Volume has TWO write paths —
// through a mount, and through the direct content API — and the engine holds ONE
// invariant to stop them tearing each other.
//
//	While a LIVE READ-WRITE MOUNT holds the volume, a direct mutating operation
//	(import) is REFUSED. With no live rw mount, the direct write is allowed.
//
// The hazard is real rather than theoretical: the same backend bytes would be written
// concurrently by a guest through the gofer and by the engine from the host, and
// gofer/sentry page caches are not coherent with a host-side write.
//
// THE GATE OPENS ON A *LIVE* WRITER, NOT ON "APPEARS IN THE MOUNT TABLE", and that
// distinction is a PROMISE rather than an implementation convenience (§5.C): a
// `paused` sandbox is NOT a live writer — its container is stopped and no gofer is
// alive — so a direct write is safe then, and the sandbox naturally sees the new
// content when it resumes. Tightening this to "any row in the table" would be more
// conservative and would not tear anything, but it would DELETE a documented
// capability, which volume.md D5 calls the third kind of wrong (trading a settled
// ability for implementation convenience).
//
// THE PREDICATE READS THE LIVE-MAP, NOT `sandboxes.state`, and that column is
// measurably unfaithful in BOTH directions:
//
//   - a cold start records the live placement BEFORE the health gate and before the
//     CAS to `running`, so the record says `creating`/`paused` while the gofer is
//     already alive — a window as long as HealthTimeout;
//   - pause CASes to `paused` FIRST and stops the container afterwards, so the record
//     says `paused` while the container is still running.
//
// No lock and no SQL statement can fix that, because THE FACT BEING TESTED DOES NOT
// LIVE IN THE STORE. The live-map, by contrast, IS a faithful gofer predicate: a
// placement is put into it at the moment it physically exists (coldStart, before the
// readiness gate) precisely so a concurrent reconcile does not mistake it for an
// orphan.

// THE INVARIANT IS SYMMETRIC, AND BOTH SIDES ARE GATED (§5.C): the hazard above does
// not care who arrived first, so while a direct mutating operation is in flight, a cold
// start that would give that volume an rw mount is refused too (admitColdStartRW below).
// Gating one side only is equivalent to gating neither — an import admitted at T0 plus a
// create/resume completing at T1 produces exactly the two concurrent writers, with no
// `paused` state anywhere in the story.
//
// THE TWO SIDES DECIDE OVER ONE TABLE, keyed by volume and guarded by the ONE existing
// m.mu: the live-rw holder set and the in-flight direct-write markers are two halves of
// a single mutual-exclusion table, not two checks somebody has to keep consistent by
// hand. Deliberately NOT a per-volume lock class: volume.md D3 DERIVES a deadlock from
// the lock order for keying locks by volume (a volume-dimension verb reaching back for
// its holders closes sbx→vol→sbx into a cycle) — derived, with no reproduction vehicle in
// the repo, unlike D3's first two findings — which is why this stays one flat map under
// the mediator's existing mutex.
//
// THE COLD-START SIDE REFUSES, IT DOES NOT WAIT. The direct-write marker is held for
// the WHOLE stream by contract, so its duration is unbounded in the payload size; a
// cold start holds the sandbox's transition lock for its entire chain (§4.4-B), so
// waiting would park destroy / pause / evict behind somebody's 40 GiB upload. The refusal
// lands on §4.4-A's existing atomic return to the source state — resume stays `paused`,
// create deletes its record — so it adds NO new failure semantics.

// ErrVolumeLiveWriter is §6.2's fourth Volume 409: a direct content change was
// refused because a live read-write mount holds the volume.
//
// 409 rather than 400 because it is a RETRYABLE state conflict — pausing the holder
// (or destroying it) makes the same request succeed, which is the documented remedy.
var ErrVolumeLiveWriter = errors.New("lifecycle: volume has a live read-write mount")

// ErrVolumeDirectWriteInFlight is ErrVolumeLiveWriter's MIRROR — the same §5.C
// invariant refusing from the other side: a cold start was refused because a direct
// content write is in flight on a volume it would mount read-write.
//
// A DISTINCT sentinel rather than a reuse: the REMEDIES differ — this names a SANDBOX
// verb (create / resume) resolved by waiting for the import to finish, ErrVolumeLiveWriter
// a volume verb resolved by pausing a sandbox — so folding them would hand the caller the
// wrong remedy in one of the two directions.
var ErrVolumeDirectWriteInFlight = errors.New("lifecycle: a direct write is in flight on a volume this sandbox would mount read-write")

// admitDirectWrite decides "no live rw holder" AND publishes this write's in-flight
// marker INSIDE THE SAME CRITICAL SECTION.
//
// The two cannot be separated (§5.C). If the marker were published after the lock was
// released, a sweep running in between would see backend bytes belonging to a volume
// it cannot prove is in use and could reclaim them under an in-flight write. Same
// shape as the in-flight build keep-set, and deliberately the SAME LOCK — no new lock
// class, so nothing new can deadlock against the existing order.
//
// Returns a release function the caller MUST call (defer) to drop the marker.
func (m *Mediator) admitDirectWrite(ctx context.Context, id model.VolumeID) (func(), error) {
	// The mount rows are read BEFORE taking m.mu: the store must never be called under
	// that mutex (it is held only across O(1) work, never across a store or provider
	// call — the discipline the whole locking argument rests on).
	rows, err := m.deps.Store.ListMountsByVolume(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("direct write: read mount table: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mt := range rows {
		if mt.Access.ReadOnly() {
			// A live READ-ONLY reader is not a writer, so it cannot tear anything, and
			// SETTLED (§5.C): no warning, no version stamp, no refusal. The reader's
			// exposure is bounded to stale-but-whole / a momentary ENOENT / a prefix —
			// never a torn file, because an import unlinks rather than overwriting in
			// place — and freshness beyond that is §5.C's EXISTING non-promise, the same
			// one that makes cross-sandbox rw coherence undefined. Refusing here would
			// trade a capability (booting a sandbox that only READS a volume being
			// seeded) for convenience.
			continue
		}
		ls := m.live[mt.Sandbox]
		if ls != nil && ls.placement != nil {
			return nil, fmt.Errorf("direct write to %s: %w: sandbox %s holds it read-write at %s and its container is live — pause that sandbox and retry (a `paused` sandbox is NOT a live writer, §5.C)",
				id, ErrVolumeLiveWriter, mt.Sandbox, mt.Path)
		}
	}
	// Admitted. Publish the marker in this same critical section.
	m.inFlightWrites[id]++
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.inFlightWrites[id]--
		if m.inFlightWrites[id] <= 0 {
			delete(m.inFlightWrites, id)
		}
	}, nil
}

// admitColdStartRW is the write regime's OTHER side: it refuses a cold start that
// would give a volume an rw mount while a direct write is in flight on that volume
// (§5.C). Called by coldStart with the mount rows it is about to materialize.
//
// It takes the ROWS rather than reading them itself, because coldStart has already
// read them — and re-reading would be worse than redundant: the decision would then
// be made about a different snapshot than the one being materialized.
//
// READ-ONLY MOUNTS PASS — the exact mirror of admitDirectWrite letting a live ro
// holder through, for the reason stated there.
//
// ONE ACQUISITION OF m.mu over the whole row set, not one per row: a write that
// starts on the second volume after the first was cleared must not slip through the
// gap. This is the same table admitDirectWrite decides on, under the same mutex,
// which is what makes the two halves one mutual exclusion rather than two checks.
// No store call happens under the lock (the rows are already in hand) — the
// discipline the whole locking argument rests on.
func (m *Mediator) admitColdStartRW(id model.SandboxID, rows []model.Mount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mt := range rows {
		if mt.Access.ReadOnly() {
			continue
		}
		if m.inFlightWrites[mt.Volume] > 0 {
			return fmt.Errorf("%w: sandbox %s would mount volume %s read-write at %s while a direct content write is streaming into it — wait for that import to finish and retry (§5.C: the invariant is symmetric, and the cold start refuses rather than waits because it holds the sandbox's transition lock throughout)",
				ErrVolumeDirectWriteInFlight, id, mt.Volume, mt.Path)
		}
	}
	return nil
}

// inFlightWriteIDs snapshots the volumes with a direct write in flight, for the
// orphan sweep's keep-set.
//
// It is a COUNT per volume, not a boolean, so two concurrent imports to different
// paths of one volume cannot have the first one's release drop the second's
// protection. That is the same reason the transition lock counts handouts rather than
// holders.
func (m *Mediator) inFlightWriteIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.inFlightWrites))
	for id, n := range m.inFlightWrites {
		if n > 0 {
			out = append(out, string(id))
		}
	}
	return out
}
