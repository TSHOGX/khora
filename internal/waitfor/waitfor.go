// Package waitfor holds the one polling loop the test suites share.
//
// It exists because that loop was written six times (internal/lifecycle twice,
// cmd/sandboxd, internal/runtime, test/ twice) and every copy carried the SAME
// latent flake:
//
//	for time.Now().Before(deadline) { if cond() { return }; time.Sleep(tick) }
//	t.Fatal("timed out")
//
// The final sleep can end after the deadline, so a condition that becomes true
// DURING it is never observed — a rare FALSE failure, which looks like the engine
// broke. Until re-checks once after the loop, so the answer is "cond was false at the
// deadline", not "cond was false a tick before it".
//
// Not a leaf in the documentation's sense — a leaf exists so two the documented invariant subsystems can share a
// FACT, and this is test scaffolding carrying no production edge. It follows the
// internal/store/storetest and internal/volume/volumetest precedent instead: a
// test-support package under internal/, imported only from _test.go files.
//
// tick stays an explicit argument rather than a default. The two classes of caller
// are decades apart — in-process conditions poll at 1-5 ms, while a health probe
// that reaches a guest over HTTP polls at 200 ms — and a shared default would either
// make the fast callers sluggish or hammer the substrate ones hundreds of times per
// second.
package waitfor

import (
	"testing"
	"time"
)

// Until polls cond every tick until it returns true or timeout elapses, and reports
// whether it became true. Use it when a timeout is a legitimate outcome the caller
// interprets; use Must when a timeout is a test failure.
//
// cond is checked BEFORE the first sleep, so an already-satisfied condition costs
// nothing, and once more AFTER the deadline for the reason in the package comment.
func Until(timeout, tick time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			// The re-check above already ran at or past the deadline, so this is a
			// genuine timeout rather than the tick-boundary artifact.
			return false
		}
		time.Sleep(tick)
	}
}

// Must is Until with the timeout turned into a failure. what names the condition and
// is interpolated as "timed out waiting for <what>", so it reads best as a noun
// phrase ("the reaper to observe the exit").
//
// It calls t.Fatalf: a caller that wants to add its own diagnosis (the observed value,
// a rule-set dump) should use Until and fail on its own terms — a generic message that
// buries the specific one is how a test stops explaining itself.
func Must(t *testing.T, what string, timeout, tick time.Duration, cond func() bool) {
	t.Helper()
	if !Until(timeout, tick, cond) {
		t.Fatalf("timed out after %s waiting for %s", timeout, what)
	}
}
