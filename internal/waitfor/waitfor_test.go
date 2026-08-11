package waitfor

import (
	"testing"
	"time"
)

// The bug this package exists to remove: a condition that becomes true during the
// FINAL sleep must still be observed.
//
// Constructed so the old shape provably fails it rather than merely being unlikely to
// pass: cond flips true strictly between the last tick and the deadline, which is the
// window the pre-loop check cannot see and the missing post-loop check dropped.
func TestUntilObservesAConditionThatFlipsDuringTheFinalSleep(t *testing.T) {
	const timeout, tick = 60 * time.Millisecond, 25 * time.Millisecond
	// Ticks land at 0, 25, 50; the deadline is 60. A flip at 55 ms is visible to
	// NEITHER the tick at 50 nor a loop that stops checking at 60 — only the
	// post-deadline re-check sees it.
	flip := time.Now().Add(55 * time.Millisecond)
	if !Until(timeout, tick, func() bool { return time.Now().After(flip) }) {
		t.Fatal("Until reported a timeout for a condition that became true before the " +
			"deadline expired — the post-deadline re-check is missing, which is the exact " +
			"false failure this package was written to delete")
	}
}

func TestUntilReportsAGenuineTimeout(t *testing.T) {
	start := time.Now()
	if Until(30*time.Millisecond, 5*time.Millisecond, func() bool { return false }) {
		t.Fatal("Until reported success for a condition that never became true")
	}
	// It must actually wait: returning false immediately would pass the line above
	// while making every caller's timeout meaningless.
	if el := time.Since(start); el < 30*time.Millisecond {
		t.Fatalf("Until returned after %s for a %s timeout — it is not waiting", el, 30*time.Millisecond)
	}
}

// An already-true condition must cost nothing, because the substrate callers poll at
// 200 ms and paying one tick per call would add up across ~20 call sites.
func TestUntilChecksBeforeSleeping(t *testing.T) {
	start := time.Now()
	if !Until(time.Hour, 30*time.Second, func() bool { return true }) {
		t.Fatal("Until failed on an already-true condition")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Until slept %s before checking an already-true condition", el)
	}
}

func TestUntilStopsCallingCondAfterItSucceeds(t *testing.T) {
	calls := 0
	Until(time.Second, time.Millisecond, func() bool { calls++; return true })
	if calls != 1 {
		t.Fatalf("cond called %d times for an immediately-true condition; want 1", calls)
	}
}
