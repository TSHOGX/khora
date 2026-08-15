//go:build substrate

// L3 acceptance for the documented invariant's POST /v1/sandboxes/{id}/activity against the REAL
// substrate. Requires root.
//
// The property under test is the one the endpoint exists for and the only one a
// unit test cannot show: a sandbox doing work the ENGINE CANNOT SEE — no output, no
// engine-mediated I/O, nothing on stdin — survives the idle sweep as long as its
// caller keeps stamping, and is auto-paused as soon as the stamps stop. Both halves
// are needed: without the second, a test would pass equally well against an
// endpoint that had disabled the idle disposition altogether.
package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/waitfor"
)

// TestActivityEndpointHoldsOffTheIdleSweepRealRunsc drives the REST surface rather
// than the transport client, because the endpoint's contract (the documented invariant) is what a
// platform caller has — including that the stamp lands in the GUEST's clock
// (reaper-health the documented invariant), which is exactly what the sweep then reads back.
func TestActivityEndpointHoldsOffTheIdleSweepRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)

	sid, err := m.Create(ctx, lifecycle.CreateSpec{
		Template: "bare", IdleTimeout: dur(2 * time.Second), Retention: dur(0),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	stamp := func() int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/sandboxes/"+string(sid)+"/activity", "application/json", nil)
		if err != nil {
			t.Fatalf("POST activity: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST activity: HTTP %d: %s", resp.StatusCode, body)
		}
		return resp.StatusCode
	}

	// No process is ever started: this sandbox produces no output and does no
	// engine-mediated I/O, so the ONLY thing standing between it and auto-pause is
	// the stamp. Sweep across a window three times idle_timeout.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		stamp()
		r.Sweep(ctx)
		if stt, _ := m.State(ctx, sid); stt != model.Running {
			t.Fatalf("a stamping sandbox was auto-paused: state=%s (the stamp did not reach the guest clock the sweep reads)", stt)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("✓ stamped every 500ms across 3× idle_timeout → still running")

	// Now stop stamping. The disposition must fire — otherwise the endpoint has
	// silently disabled idle auto-pause rather than deferred it.
	waitfor.Must(t, "auto-pause after the stamps stop", 20*time.Second, 500*time.Millisecond, func() bool {
		r.Sweep(ctx)
		stt, _ := m.State(ctx, sid)
		return stt == model.Paused
	})
	if k := lastKind(t, eng, sid); k != "auto-paused" {
		t.Fatalf("idle disposition Kind=%q; want auto-paused", k)
	}
	t.Logf("✓ stamps stopped → auto-paused (the hold-off is deferral, not disablement)")

	// And on the paused sandbox the endpoint refuses with 409 rather than resuming
	// it (the documented invariant: this is not resume-by-side-effect).
	resp, err := http.Post(srv.URL+"/v1/sandboxes/"+string(sid)+"/activity", "application/json", nil)
	if err != nil {
		t.Fatalf("POST activity on paused: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST activity on a paused sandbox: HTTP %d (want 409): %s", resp.StatusCode, body)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Paused {
		t.Fatalf("state=%s after a refused stamp; the refusal must not resume the sandbox", stt)
	}
	t.Logf("✓ paused → 409, and still paused afterwards")
}
