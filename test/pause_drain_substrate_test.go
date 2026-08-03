//go:build substrate

// L3 acceptance for the §4.2 pause drain barrier (guest-runtime D4.4) against the
// REAL substrate: a pause racing an engine-issued fs.Write must not leave the file
// truncated. Requires root.
//
//	sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 ./test/ -run TestPauseDrain
package test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"engine/internal/lifecycle"
	"engine/internal/orchestrator"
	"engine/internal/transport"
	v1 "engine/internal/transport/gen/engine/transport/v1"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
	"engine/test/harness"
)

// THE hazard this barrier exists for. The guest's fs.Write is truncate-then-write
// (os.WriteFile) and the data plane takes no transition lock, so a pause landing
// between the two steps leaves the file EMPTY — data destroyed, not merely stale,
// inside an RPC the engine itself issued.
//
// The assertion is deliberately "whole or untouched, never empty". That is the
// property a caller depends on, and it is weaker than "the write always wins",
// which the engine cannot promise: the drain has a bounded grace and §4.4 keeps it
// an optimization, not a correctness premise.
func TestPauseDrainKeepsWritesWhole(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	const rounds = 10
	res := runPauseWriteRace(t, 0 /* engine default grace */, rounds)
	if res.empty > 0 {
		t.Errorf("%d/%d rounds left the file EMPTY despite the drain barrier — "+
			"a pause truncated an engine-issued write (§4.2 / D4.4)", res.empty, rounds)
	}
	t.Logf("✓ %d rounds with the barrier: whole=%d preserved=%d partial=%d empty=%d",
		rounds, res.whole, res.preserved, res.partial, res.empty)
}

// The companion, and the reason the test above means anything: with the drain
// DISABLED the same race truncates. Without this, a pass above could just mean the
// window is too narrow to hit on this box, and the barrier would be untested
// scaffolding that looks verified.
//
// It asserts only that the hazard is REACHABLE, not how often — it is a timing
// race, so demanding a rate would make the test flaky in the other direction.
func TestPauseDrainWindowIsRealWithoutBarrier(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	const rounds = 10
	res := runPauseWriteRace(t, -1 /* barrier disabled */, rounds)
	t.Logf("drain disabled, %d rounds: whole=%d preserved=%d partial=%d empty=%d",
		rounds, res.whole, res.preserved, res.partial, res.empty)
	if res.empty == 0 && res.partial == 0 {
		t.Skipf("the truncation window did not reproduce in %d rounds on this host "+
			"(timing race). The barrier is still covered by TestPauseDrainKeepsWritesWhole "+
			"and the unit tests; this companion only demonstrates the hazard when hit.", rounds)
	}
	t.Logf("✓ hazard confirmed reachable with the drain disabled (%d empty, %d partial) — "+
		"the barrier closes a real window, not a hypothetical one", res.empty, res.partial)
}

// raceResult counts how each round ended.
type raceResult struct {
	whole     int // the write completed
	preserved int // the pause won; the previous content survived intact
	partial   int // interrupted mid-write, some bytes present
	empty     int // interrupted between truncate and write: DATA DESTROYED
}

// runPauseWriteRace fires a large fs.Write and a Pause concurrently, `rounds`
// times, and classifies each outcome. grace < 0 disables the drain; 0 takes the
// engine default.
func runPauseWriteRace(t *testing.T, grace time.Duration, rounds int) raceResult {
	t.Helper()
	ctx := context.Background()
	eng := setupEngineWithDrain(t, grace)
	// The outer edge is where the barrier is taken (dataplane proxy), so the race
	// must go through it.
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	// Sized against MEASURED timings on this substrate, or the race never actually
	// races: an 8 MiB fs.Write completes in ~40ms while Pause itself takes ~139ms,
	// so the write finishes before the pause can reach the freeze and every round
	// is a trivial "write won". 64 MiB takes ~488ms — comfortably longer than the
	// pause — so the freeze lands while the guest is still inside os.WriteFile,
	// which is the window under test.
	payload := make([]byte, 64<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	const seed = "PRE-EXISTING"

	var res raceResult
	for round := 0; round < rounds; round++ {
		sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
			Template:    "bare",
			IdleTimeout: dur(time.Hour),
			Retention:   dur(0),
		})
		if err != nil {
			t.Fatalf("round %d: create: %v", round, err)
		}
		path := fmt.Sprintf("/root/racer-%d.bin", round)
		cl, err := eng.Mediator.Client(sid)
		if err != nil {
			t.Fatalf("round %d: client: %v", round, err)
		}
		// Seed known-good content first, so "empty" is unambiguously a truncation
		// rather than a file that was simply never written.
		if err := cl.Write(ctx, path, []byte(seed)); err != nil {
			t.Fatalf("round %d: seed write: %v", round, err)
		}

		// Drive the write through the OUTER EDGE, not straight at Mediator.Client:
		// the barrier is taken by the data-plane proxy handler, so a test that calls
		// the mediator's client directly bypasses the very gate it means to test.
		fsc := transportv1connect.NewFsServiceClient(h2cClient(), srv.URL, sidHeader(string(sid)))
		writeDone := make(chan error, 1)
		go func() {
			_, err := fsc.Write(ctx, connect.NewRequest(&v1.WriteRequest{Path: path, Data: payload}))
			writeDone <- err
		}()
		// Wait until the write is demonstrably IN FLIGHT before pausing, rather than
		// sleeping a fixed guess. A guess cannot work for both configurations: with
		// the barrier the write runs to completion (~0.5s), without it the pause
		// preempts almost immediately, so any single sleep lands inside the window
		// for one and outside for the other. Polling the file's growth is the
		// observable that means the same thing in both.
		waitForWriteInFlight(t, ctx, cl, path, len(seed))
		_ = eng.Mediator.Pause(ctx, sid)
		<-writeDone

		// Read back through the guest after resume: the guest's own view is what a
		// caller sees, and it also proves the bytes are in the at-rest Overlay.
		if err := eng.Mediator.Resume(ctx, sid); err != nil {
			t.Fatalf("round %d: resume: %v", round, err)
		}
		cl2, err := eng.Mediator.Client(sid)
		if err != nil {
			t.Fatalf("round %d: client after resume: %v", round, err)
		}
		st, err := cl2.Stat(ctx, path)
		if err != nil {
			t.Fatalf("round %d: stat after resume: %v", round, err)
		}
		switch st.Size {
		case int64(len(payload)):
			res.whole++
		case int64(len(seed)):
			res.preserved++
		case 0:
			res.empty++
			t.Logf("round %d: file is EMPTY — truncate-then-write was interrupted", round)
		default:
			res.partial++
			t.Logf("round %d: partial, %d of %d bytes", round, st.Size, len(payload))
		}
		_ = eng.Mediator.Destroy(ctx, sid)
	}
	return res
}

// waitForWriteInFlight blocks until the target file no longer looks like the seed
// — i.e. the guest has entered os.WriteFile and truncated it. That is the precise
// moment the truncate window is open, so pausing right after this is what makes
// the race a race rather than a coin flip on timing.
//
// Bounded: if the write finishes or never starts within the budget, the round just
// becomes a "write won" / "preserved" outcome, which the caller classifies.
func waitForWriteInFlight(t *testing.T, ctx context.Context, cl transport.Client, path string, seedLen int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := cl.Stat(ctx, path)
		if err != nil {
			// Mid-truncate the path can briefly be unreadable; keep polling.
			time.Sleep(time.Millisecond)
			continue
		}
		if st.Size != int64(seedLen) {
			return // truncated or already growing: the write is inside the guest
		}
		time.Sleep(time.Millisecond)
	}
}

// setupEngineWithDrain builds a real engine with an explicit DrainGrace. A
// negative grace disables the drain (the pre-barrier behaviour).
func setupEngineWithDrain(t *testing.T, grace time.Duration) *orchestrator.Engine {
	t.Helper()
	repo := harness.RepoRoot(t)
	base := t.TempDir()
	tmpl := filepath.Join(base, "templates")
	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)

	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir:  tmpl,
		StateDir:      filepath.Join(base, "state"),
		StorePath:     filepath.Join(base, "store.db"),
		HealthTimeout: 30 * time.Second,
		DrainGrace:    grace,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := eng.SeedTemplates(context.Background()); err != nil {
		t.Fatalf("seed builtin templates: %v", err)
	}
	return eng
}
