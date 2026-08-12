package lifecycle

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TSHOGX/warren/internal/model"
)

// --- pause drain barrier (§4.2 quiescent point / guest-runtime D4.4) ----------
//
// The barrier closes ONE specific window, and it is the engine's own: a mutating
// fs RPC is in flight when pause freezes the sandbox. The guest's fs.Write is
// truncate-then-write (os.WriteFile), so a freeze landing between the two steps
// leaves the file EMPTY — data destroyed, not merely stale, in a call the engine
// itself issued. Promote quiesces through the same path, so a truncated file can
// be frozen into a Template permanently.
//
// It is host-side, and that is what makes it DETERMINISTIC: the engine issued
// those RPCs, so it can simply stop issuing more and wait for the ones it knows
// about. No guest cooperation, no ack channel, nothing to ask of the workload.
// The in-guest cooperative freeze (SIGTSTP + observing /proc) is a separate,
// REJECTED idea — see D4.4 for why.
//
// What it is NOT: a correctness premise. §4.4 puts the quiesce on the
// "optimization" side, so every path must still reach a legal `paused`. Hence the
// timeout PROCEEDS with the preemptive freeze rather than failing the pause — a
// stuck RPC must not make a sandbox un-pausable. (A reference implementation
// aborts its pause on timeout instead; that trade is wrong here, because our
// timeout path lands exactly where a crash lands, which §4.2 already accepts.)

// barrier tracks in-flight engine-issued mutating operations per sandbox and lets
// a pause wait for them to drain.
//
// Both maps are keyed per sandbox and both RECLAIM, so neither grows with sandbox
// churn — a load-bearing invariant, not an incidental tidiness, because these maps
// are keyed by an identifier the caller mints without bound:
//
//   - an inFlight entry disappears when the LAST in-flight operation for that
//     sandbox releases (enter's returned closure deletes the whole set once it
//     empties, not just its own op);
//   - a draining entry disappears at undrain, which every pause registers with
//     defer BEFORE its CAS so a failed pause reclaims it too (§4.2 / D4.4).
//
// Emptiness is therefore the steady state: between operations a quiet sandbox has
// no entry in either map, and the transition lock's refcount (§4.4-B) has no
// analogue to port here — the count that matters IS the in-flight set, and it is
// already exact.
//
// What keeps that true is a discipline at the call sites rather than anything the
// type can enforce: enter hands back a release closure, and losing it strands an
// entry forever, which no build error and no other assertion would notice. Hence
// barrier_test.go's structural guard over the Enter call sites — see it before
// adding one.
type barrier struct {
	mu sync.Mutex
	// cond signals when a sandbox's in-flight count reaches zero.
	cond *sync.Cond
	// draining marks sandboxes whose pause is waiting: new operations are refused
	// so the count can actually reach zero (otherwise a busy caller starves the
	// drain indefinitely).
	draining map[model.SandboxID]bool
	// inFlight is the live operation set per sandbox, keyed by a monotonic id so a
	// straggler can be NAMED in the timeout log — "3 operations outstanding" sends
	// the reader nowhere.
	inFlight map[model.SandboxID]map[uint64]inFlightOp
	nextID   uint64
}

// inFlightOp is one outstanding mutating operation, kept for diagnostics.
type inFlightOp struct {
	op      string // e.g. "fs.Write /root/x"
	started time.Time
}

func newBarrier() *barrier {
	b := &barrier{
		draining: make(map[model.SandboxID]bool),
		inFlight: make(map[model.SandboxID]map[uint64]inFlightOp),
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// enter registers a mutating operation against a sandbox. It returns a release
// func and true when the operation may proceed, or (nil, false) once a pause is
// draining — in which case the caller must fail the request (unavailable) rather
// than block, so a pause cannot be starved by arriving work.
//
// Release is idempotent-safe to call exactly once via defer.
func (b *barrier) enter(id model.SandboxID, op string) (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining[id] {
		return nil, false
	}
	b.nextID++
	opID := b.nextID
	if b.inFlight[id] == nil {
		b.inFlight[id] = make(map[uint64]inFlightOp)
	}
	b.inFlight[id][opID] = inFlightOp{op: op, started: time.Now()}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		ops := b.inFlight[id]
		delete(ops, opID)
		if len(ops) == 0 {
			delete(b.inFlight, id)
			// Broadcast, not Signal: several pausers could in principle wait on one
			// sandbox, and Signal would wake only one of them.
			b.cond.Broadcast()
		}
	}, true
}

// drain stops accepting new mutating operations for a sandbox and waits for the
// in-flight ones to finish, up to grace. It returns the operations still
// outstanding when it gave up — empty means a clean drain.
//
// ALWAYS pair with undrain (defer): leaving a sandbox marked draining would refuse
// its mutating RPCs forever.
func (b *barrier) drain(id model.SandboxID, grace time.Duration) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.draining[id] = true
	if len(b.inFlight[id]) == 0 {
		return nil // the common case: nothing in flight, no waiting at all
	}

	// A timer goroutine wakes the Cond so the wait cannot outlive grace. Cond has
	// no timed wait, so this is the standard shape.
	deadline := time.Now().Add(grace)
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-t.C:
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		case <-done:
		}
	}()

	for len(b.inFlight[id]) > 0 && time.Now().Before(deadline) {
		b.cond.Wait()
	}
	return describeOps(b.inFlight[id])
}

// undrain lets mutating operations through again (after the freeze, or if the
// pause failed before freezing).
func (b *barrier) undrain(id model.SandboxID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.draining, id)
}

// describeOps renders outstanding operations for a log line, oldest first, so the
// straggler that actually blocked the drain reads first.
func describeOps(ops map[uint64]inFlightOp) []string {
	if len(ops) == 0 {
		return nil
	}
	type kv struct {
		id uint64
		op inFlightOp
	}
	list := make([]kv, 0, len(ops))
	for id, op := range ops {
		list = append(list, kv{id, op})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].op.started.Before(list[j].op.started) })
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.op.op+" (held "+
			strconv.FormatInt(time.Since(e.op.started).Milliseconds(), 10)+"ms)")
	}
	return out
}

// Enter is the exported seam the data plane uses to register an engine-issued
// mutating operation (see barrier's doc for which operations qualify and why).
// ok=false means a pause is draining this sandbox: the caller must reject the
// request, not queue behind the pause.
func (m *Mediator) Enter(id model.SandboxID, op string) (func(), bool) {
	return m.barrier.enter(id, op)
}

// drainForPause runs the barrier before a voluntary freeze. Timeout is not a
// failure: it logs the stragglers and lets the caller proceed preemptively, which
// lands exactly where a crash lands (§4.2) — never leaving a sandbox un-pausable.
//
// On reaper sweep latency: this runs while the caller holds the sandbox's
// transition lock, and the reaper sweeps sequentially, so a drain that waits its
// full grace delays the rest of that sweep. It is bounded and rare rather than
// structural — an auto-pause fires only for a sandbox the activity clock calls
// IDLE, and idle means no in-flight I/O, so drain returns immediately with nothing
// to wait for. Paying the grace requires a write that starts in the window between
// the classifier's verdict and the freeze. If that ever shows up as sweep latency,
// the fix is to drain outside the lock, not to shorten the grace.
func (m *Mediator) drainForPause(id model.SandboxID) {
	grace := m.deps.DrainGrace
	if grace <= 0 {
		return // drain disabled: straight to the preemptive freeze
	}
	if stragglers := m.barrier.drain(id, grace); len(stragglers) > 0 {
		m.deps.Logf("pause %s: drain timed out after %s with %d in-flight, freezing anyway: %s",
			id, grace, len(stragglers), strings.Join(stragglers, ", "))
	}
}
