package lifecycle

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
)

// The §4.4-B transition-lock map must be BOUNDED and its exclusion must hold
// ACROSS reclamation. Those two pull in opposite directions, which is why the
// naive reclamation (delete the entry when a verb finishes / on destroy) is a
// correctness bug rather than a tidy-up: a verb already blocked in Lock() still
// points at the old mutex, so deleting the entry lets the next verb mint a fresh
// one and the two run concurrently while both believe they hold "the" transition
// lock for that sandbox. See transitionLock's doc for the refcount that closes it.
//
// The two tests below pin the halves separately: collection (the map empties
// after churn) and exclusion under churn (never two holders for one id, while the
// entry is created and collected repeatedly underneath).

// lockMapLen reports the live entry count of the transition-lock map.
func lockMapLen(m *Mediator) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}

// lockHeldFor reports whether the map currently has an entry for id — i.e.
// whether any ref for that sandbox is outstanding.
func lockHeldFor(m *Mediator, id model.SandboxID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.locks[id]
	return ok
}

// Collection: after high-churn create/pause/resume/update/destroy over many
// distinct SandboxIDs — with the reaper and periodic reconcile concurrently
// taking the same locks via tryLock — the map must be EMPTY.
//
// This is the only assertion that catches a MISSED release: an unreleased ref is
// a permanent entry, i.e. the original leak with more code, and it is invisible
// to the build and to every other test in this package. Distinct ids make it
// sharp — map size is exactly the number of ids whose entry outlived its verbs,
// so one forgotten release shows up as residue proportional to the churn rather
// than as a race that may not trigger.
func TestTransitionLockMapCollectsAfterChurn(t *testing.T) {
	st := store.NewMemory()
	seedBare(t, st)
	m, _, _ := stressMediator(st, true)
	r := NewReaper(m, time.Hour, time.Hour)

	// Background sweeps exercise BOTH tryLock outcomes concurrently with the
	// churn: acquired (ref taken and released) and skipped-because-held (ref taken
	// and released on the failure path, which is where the reaper would otherwise
	// leak one ref per tick, forever).
	stop := make(chan struct{})
	sweepsDone := make(chan struct{})
	go func() {
		defer close(sweepsDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.Sweep(context.Background())
			_ = m.ReconcilePeriodic(context.Background())
		}
	}()

	const (
		workers = 16
		rounds  = 12
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			ok := func(err error) bool {
				return err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)
			}
			for i := 0; i < rounds; i++ {
				id, err := m.Create(ctx, CreateSpec{Template: "bare"})
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				// The full verb spread on one id, so every BLOCKING acquire site
				// (pause / resume / update-binding / destroy) runs against an entry
				// that must survive until the last of them releases. The reaper may
				// have moved the record underneath, so a CAS rejection is legal.
				if err := m.Pause(ctx, id); !ok(err) {
					t.Errorf("pause %s: %v", id, err)
					return
				}
				if err := m.Resume(ctx, id); !ok(err) {
					t.Errorf("resume %s: %v", id, err)
					return
				}
				idle := 5 * time.Minute
				if err := m.UpdateBinding(ctx, id, store.Binding{IdleTimeout: &idle}); !ok(err) {
					t.Errorf("update %s: %v", id, err)
					return
				}
				if err := m.Destroy(ctx, id); !ok(err) {
					t.Errorf("destroy %s: %v", id, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-sweepsDone

	// Every verb has returned and the sweeper is joined, so no ref can legitimately
	// be outstanding: the map must be empty, not merely small.
	if n := lockMapLen(m); n != 0 {
		t.Fatalf("transition-lock map holds %d entries after %d churned sandboxes; want 0 "+
			"(an entry outliving its verbs is the open-decisions §4 item ④ leak — some "+
			"acquire site is missing its release)", n, workers*rounds)
	}
}

// Promote is the one blocking acquire site the churn test cannot drive (it needs
// a promotable source and mints a Template, not a sandbox transition), so its
// release is pinned here: a verb that takes the lock and forgets to give the ref
// back is a permanent entry, and no other assertion in the package would notice.
func TestTransitionLockReleasedByPromote(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	m, _, _ := stressMediator(st, true)
	seedPausedStore(t, st, "sbx_prom", "ov_prom", "bare")

	if _, err := m.Promote(ctx, "sbx_prom"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n := lockMapLen(m); n != 0 {
		t.Fatalf("transition-lock map holds %d entries after Promote; want 0 "+
			"(Promote took a ref and did not release it)", n)
	}
}

// Exclusion under churn: hammer ONE id while its map entry is repeatedly created
// and collected, and assert no two goroutines are ever inside the critical
// section together.
//
// This is the test the naive reclamation fails. Two deliberately redundant
// detectors:
//   - overlaps, via an atomic CAS 0→1 on entry, catches an overlap directly;
//   - plain, a NON-atomic counter mutated under the transition lock alone, so
//     -race reports any genuinely concurrent critical section even if the CAS
//     window happened to slip past.
//
// It must also SEE reclamation happen, else a run in which the entry was never
// collected would prove only that a never-collected map is safe — the pre-fix
// behavior, i.e. vacuous. The witness is ENTRY IDENTITY: each hammerer records
// the *transitionLock it was handed, and a second distinct pointer for the same
// id can only exist if the first was collected and a fresh one created. (Under
// the naive reclamation the same signal appears, but accompanied by overlaps —
// there the two pointers are alive AT ONCE, which is the bug.)
//
// FALSIFIED: with release() replaced by an unconditional delete (the naive fix),
// this fails at ~666 overlaps plus a -race report on plain. The collection test
// above fails on the same scaffold for a more consequential reason — the reaper's
// tryLock deletes the entry while Create still holds it, so a later sweep drives
// the record out from under the in-flight create ("commit running: sandbox not
// found"). §4.4-B breakage as damage, not just as a counter.
func TestTransitionLockExclusionHoldsAcrossCollection(t *testing.T) {
	st := store.NewMemory()
	seedBare(t, st)
	m, _, _ := stressMediator(st, true)

	// Waves, not one flat loop: inside a wave all hammerers contend flat out (so
	// waiters are parked in Lock() while a holder is inside), and BETWEEN waves
	// they all go idle, which is the deterministic drain point where refs for this
	// id can actually reach zero and the entry be collected. A flat loop never
	// drains under 24-way contention, so it could never witness reclamation.
	const (
		id            = model.SandboxID("sbx_churn")
		hammerers     = 24
		waves         = 8
		roundsPerWave = 25
		rounds        = waves * roundsPerWave
	)

	var (
		inside   atomic.Int32
		overlaps atomic.Int32
		plain    int // guarded by the transition lock ALONE — -race watches it

		seenMu sync.Mutex
		seen   = map[*transitionLock]bool{} // distinct entries handed out for `id`

		collected atomic.Int32 // waves that observed the entry actually gone
		waveEnd   sync.WaitGroup
	)

	// Real verbs on the SAME id, cycling the record present→absent→present, plus
	// reaper/reconcile tryLock traffic. So the hammered entry is contended by
	// genuine lifecycle transitions while it is being collected and re-created.
	seedPausedStore(t, st, string(id), "ov_churn", "bare")
	r := NewReaper(m, time.Hour, time.Hour)

	startGate := make(chan struct{})
	verbsStop := make(chan struct{})
	verbsDone := make(chan struct{})
	verbErr := make(chan error, 1)

	go func() {
		defer close(verbsDone)
		<-startGate
		ctx := context.Background()
		ok := func(err error) bool {
			return err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)
		}
		for {
			select {
			case <-verbsStop:
				return
			default:
			}
			for _, step := range []func() error{
				func() error { return m.Resume(ctx, id) },
				func() error { return m.Pause(ctx, id) },
				func() error { return m.Destroy(ctx, id) },
			} {
				if err := step(); !ok(err) {
					select {
					case verbErr <- err:
					default:
					}
					return
				}
			}
			// Re-seed so the next round has a record to drive. A NotFound in the
			// steps above is therefore a legal interleaving, not a failure.
			_ = st.CreateSandbox(ctx, model.Sandbox{
				ID: id, Template: "bare", Overlay: "ov_churn", State: model.Creating,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}, nil)
			_ = st.CompareAndSetState(ctx, id, model.Creating, model.Paused, nil, nil)
			r.Sweep(ctx)
			_ = m.ReconcilePeriodic(ctx)
		}
	}()

	var wg sync.WaitGroup
	waveStart := make([]chan struct{}, waves)
	for w := range waveStart {
		waveStart[w] = make(chan struct{})
	}
	for h := 0; h < hammerers; h++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			for w := 0; w < waves; w++ {
				<-waveStart[w]
				for i := 0; i < roundsPerWave; i++ {
					l := m.lock(id)
					seenMu.Lock()
					seen[l] = true
					seenMu.Unlock()
					l.Lock()
					if !inside.CompareAndSwap(0, 1) {
						overlaps.Add(1)
					}
					plain++
					// Widen the window so waiters are certainly parked in Lock() while a
					// holder is inside — the exact state the naive delete corrupts (the
					// waiter's mutex is evicted from the map, so the next arrival creates
					// a SECOND mutex for this sandbox and walks straight in).
					runtime.Gosched()
					inside.Store(0)
					l.Unlock()
				}
				waveEnd.Done()
			}
		}()
	}

	close(startGate)
	for w := 0; w < waves; w++ {
		waveEnd.Add(hammerers)
		close(waveStart[w])
		waveEnd.Wait()
		// Every hammerer has released. The verb driver may still hold a ref, so
		// poll briefly rather than asserting outright: the driver cycles fast, so a
		// drained moment arrives. Observing it is what makes the exclusion result
		// non-vacuous — the entry really is collected mid-run, not just at the end.
		deadline := time.Now().Add(2 * time.Second)
		for {
			if !lockHeldFor(m, id) {
				collected.Add(1)
				break
			}
			if time.Now().After(deadline) {
				break
			}
			runtime.Gosched()
		}
	}
	wg.Wait()
	close(verbsStop)
	<-verbsDone

	select {
	case err := <-verbErr:
		t.Fatalf("verb driver failed for an unexpected reason: %v", err)
	default:
	}

	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d overlapping critical sections for one SandboxID — two holders of "+
			"DIFFERENT mutexes for the same sandbox (§4.4-B violated: an entry was "+
			"collected while a waiter still pointed at it)", n)
	}
	if plain != hammerers*rounds {
		t.Fatalf("critical section ran %d times; want %d (a lost update means "+
			"unsynchronized concurrent entry)", plain, hammerers*rounds)
	}
	// Non-vacuity, two independent witnesses. Identity is the strong one: >1
	// distinct *transitionLock for a single id can only arise if an earlier entry
	// was collected and a later verb created a fresh one.
	seenMu.Lock()
	distinct := len(seen)
	seenMu.Unlock()
	if distinct < 2 {
		t.Fatalf("only %d distinct transition-lock entries for %s across %d waves — the "+
			"entry was never reclaimed, so the exclusion result above is vacuous (it would "+
			"also hold for a map that never reclaims anything)", distinct, id, waves)
	}
	if collected.Load() == 0 {
		t.Fatalf("no wave observed the entry for %s absent from the map", id)
	}
	if n := lockMapLen(m); n != 0 {
		t.Fatalf("transition-lock map holds %d entries after churn on one id; want 0", n)
	}
}
