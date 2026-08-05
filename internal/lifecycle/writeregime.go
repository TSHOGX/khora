package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"engine/internal/model"
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

// ErrVolumeLiveWriter is §6.2's fourth Volume 409: a direct content change was
// refused because a live read-write mount holds the volume.
//
// 409 rather than 400 because it is a RETRYABLE state conflict — pausing the holder
// (or destroying it) makes the same request succeed, which is the documented remedy.
var ErrVolumeLiveWriter = errors.New("lifecycle: volume has a live read-write mount")

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
			// A live READ-ONLY reader is not a writer, so it cannot tear anything. Whether
			// to warn about its gofer cache going stale relative to a direct write is a
			// PRECISION question deliberately left open (volume.md D8) — not something to
			// settle by quietly refusing here, which would again trade a capability for
			// convenience.
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
