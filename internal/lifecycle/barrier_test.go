package lifecycle

import (
	"strings"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
)

// Nothing in flight is the common case, and it must not cost the pause anything —
// a drain that slept even briefly on every idle sandbox would make every pause
// slower for no benefit.
func TestBarrierDrainReturnsImmediatelyWhenIdle(t *testing.T) {
	b := newBarrier()
	start := time.Now()
	if stragglers := b.drain("sbx_idle", time.Hour); len(stragglers) != 0 {
		t.Errorf("idle drain reported stragglers: %v", stragglers)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("idle drain took %s; must return immediately with nothing in flight", el)
	}
}

// The barrier's core job: a pause waits for an in-flight write to finish.
func TestBarrierDrainWaitsForInFlight(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_busy"

	release, ok := b.enter(id, "fs.Write /root/x")
	if !ok {
		t.Fatal("enter refused on an idle barrier")
	}

	released := make(chan struct{})
	go func() {
		// Hold the operation briefly, then finish it. The drain must observe the
		// release rather than time out.
		time.Sleep(150 * time.Millisecond)
		close(released)
		release()
	}()

	start := time.Now()
	stragglers := b.drain(id, 10*time.Second)
	el := time.Since(start)

	select {
	case <-released:
	default:
		t.Fatal("drain returned BEFORE the in-flight operation released — the write was not waited for")
	}
	if len(stragglers) != 0 {
		t.Errorf("clean drain reported stragglers: %v", stragglers)
	}
	if el < 100*time.Millisecond {
		t.Errorf("drain returned after %s; it should have waited ~150ms for the operation", el)
	}
}

// A stuck operation must not make a sandbox un-pausable: §4.4 puts the drain on the
// "optimization" side, so the timeout proceeds and only reports what it left behind.
func TestBarrierDrainTimesOutAndNamesStragglers(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_stuck"

	release, _ := b.enter(id, "fs.Write /root/stuck")
	defer release() // never released before the drain: that is the point

	start := time.Now()
	stragglers := b.drain(id, 200*time.Millisecond)
	el := time.Since(start)

	if len(stragglers) != 1 {
		t.Fatalf("timeout reported %d stragglers; want 1: %v", len(stragglers), stragglers)
	}
	// Naming the straggler is the whole diagnostic value — "1 operation outstanding"
	// would send the reader nowhere.
	if !strings.Contains(stragglers[0], "/root/stuck") {
		t.Errorf("straggler %q does not name the operation that blocked the drain", stragglers[0])
	}
	if el < 150*time.Millisecond {
		t.Errorf("drain gave up after %s; want ~200ms (it must actually wait)", el)
	}
	if el > 3*time.Second {
		t.Errorf("drain took %s; the grace must bound it", el)
	}
}

// Once draining, new writes are REFUSED rather than queued. Queueing would starve
// the drain: the thing it waits for would be arriving work.
func TestBarrierRefusesNewOpsWhileDraining(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_draining"

	b.drain(id, time.Millisecond) // marks draining; nothing in flight
	if _, ok := b.enter(id, "fs.Write /root/late"); ok {
		t.Error("enter succeeded while draining — a pause could be starved by arriving writes")
	}

	// undrain re-opens the gate, or a sandbox would refuse writes forever after one
	// pause.
	b.undrain(id)
	if _, ok := b.enter(id, "fs.Write /root/after"); !ok {
		t.Error("enter still refused after undrain — the sandbox is wedged")
	}
}

// The barrier is per-sandbox: pausing one must not block writes to another. If it
// did, one paused sandbox would stall the whole node.
func TestBarrierIsPerSandbox(t *testing.T) {
	b := newBarrier()
	b.drain("sbx_a", time.Millisecond)
	defer b.undrain("sbx_a")

	if _, ok := b.enter("sbx_b", "fs.Write /root/x"); !ok {
		t.Error("draining sbx_a refused a write to sbx_b — the barrier must be per-sandbox")
	}
}

// Concurrency safety under -race: many operations entering and releasing while
// drains run. Asserts no deadlock and no lost wakeup (every drain returns).
func TestBarrierConcurrentEnterRelease(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_race"

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := b.enter(id, "fs.Write /root/x"); ok {
				time.Sleep(time.Millisecond)
				release()
			}
		}()
	}
	// Drains racing the operations. Each must return within its grace.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct{})
			go func() {
				b.drain(id, 2*time.Second)
				b.undrain(id)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("drain never returned — lost wakeup or deadlock")
			}
		}()
	}
	wg.Wait()

	// Every operation released, so the sandbox's entry is gone entirely (the map
	// deletes empty sets, which is what keeps it from growing per-write).
	b.mu.Lock()
	remaining := len(b.inFlight[id])
	b.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d operations still registered after all released", remaining)
	}
}
