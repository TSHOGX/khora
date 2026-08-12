package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/TSHOGX/warren/internal/lifecycle"
	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/store"
	"github.com/TSHOGX/warren/internal/transport"
)

// activityFakeClient records the stamp and answers with a canned reading, or fails
// the way the real inner client fails when the guest is unreachable.
type activityFakeClient struct {
	*barrierFakeClient
	idle   time.Duration
	err    error
	stamps int
}

func (c *activityFakeClient) RecordActivity(context.Context) (transport.HealthStatus, error) {
	if c.err != nil {
		return transport.HealthStatus{}, c.err
	}
	c.stamps++
	return transport.HealthStatus{Idle: c.idle}, nil
}

// activityFixture serves one running sandbox whose transport dials cl. Separate
// from barrierFixture because that one hard-wires the client it takes, and the
// recording client IS the input under test here.
func activityFixture(t *testing.T, cl transport.Client) (base string, id model.SandboxID, closeFn func()) {
	t.Helper()
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:         mem,
		Runtime:       barrierFakeRuntime{},
		Rootfs:        barrierFakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return cl },
		HealthTimeout: 2 * time.Second,
		Logf:          func(string, ...any) {},
	})
	sid, err := med.Create(context.Background(), lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	srv := httptest.NewServer((&Engine{Mediator: med, Store: mem}).HTTPHandler())
	return srv.URL, sid, srv.Close
}

// TestActivityStampsTheGuestClockAndReportsItBack: the happy path, and it asserts
// the two things that make this endpoint what §6.1 says it is — the stamp reached
// the GUEST (reaper-health D1: the clock is guest-side, never a host timestamp),
// and the body carries the reading taken after it rather than a bare 204.
func TestActivityStampsTheGuestClockAndReportsItBack(t *testing.T) {
	cl := &activityFakeClient{barrierFakeClient: &barrierFakeClient{}, idle: 7 * time.Millisecond}
	base, sid, closeFn := activityFixture(t, cl)
	defer closeFn()

	resp := do(t, http.MethodPost, base+"/v1/sandboxes/"+string(sid)+"/activity")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var got activityResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.IdleMillis != 7 {
		t.Errorf("idle_millis = %d, want 7 (the reading taken after the stamp)", got.IdleMillis)
	}
	if cl.stamps != 1 {
		t.Errorf("guest stamps = %d, want 1 — the stamp must happen in the guest, not here", cl.stamps)
	}
}

// TestActivityIsUnavailableWhenL2Is: an unreachable guest is a 503, and that is
// contract rather than an implementation detail — a caller must not be able to hold
// a sandbox looking active while the channel that would prove it is dead.
func TestActivityIsUnavailableWhenL2Is(t *testing.T) {
	cl := &activityFakeClient{
		barrierFakeClient: &barrierFakeClient{},
		err: connect.NewError(connect.CodeUnavailable,
			errors.New("dial tcp 10.88.0.7:9000: connect: connection refused")),
	}
	base, sid, closeFn := activityFixture(t, cl)
	defer closeFn()

	resp := do(t, http.MethodPost, base+"/v1/sandboxes/"+string(sid)+"/activity")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", resp.StatusCode, body)
	}
	// §6.2: the veth address must not reach the caller, same rule as the file bypass.
	if strings.Contains(string(body), "10.88.0.7") {
		t.Errorf("response leaks the placement: %s", body)
	}
}

// TestActivityOnAPausedSandboxIsAConflict: 409, NOT a resume. This is the property
// that keeps the endpoint from becoming resume-by-side-effect — a paused sandbox has
// no clock to stamp, and the engine will not start one because a caller asserted
// activity about it.
func TestActivityOnAPausedSandboxIsAConflict(t *testing.T) {
	cl := &activityFakeClient{barrierFakeClient: &barrierFakeClient{}}
	base, sid, closeFn := activityFixture(t, cl)
	defer closeFn()

	resp := do(t, http.MethodPost, base+"/v1/sandboxes/"+string(sid)+"/pause")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause: want 200, got %d", resp.StatusCode)
	}

	before := cl.stamps
	resp = do(t, http.MethodPost, base+"/v1/sandboxes/"+string(sid)+"/activity")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 on a paused sandbox, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), string(model.Paused)) {
		t.Errorf("409 body should name the state so the caller knows to resume: %s", body)
	}
	if cl.stamps != before {
		t.Errorf("a paused sandbox was stamped anyway (%d → %d)", before, cl.stamps)
	}
	// And the refusal did not quietly start it.
	sr := do(t, http.MethodGet, base+"/v1/sandboxes/"+string(sid))
	var rec struct{ State string }
	_ = json.NewDecoder(sr.Body).Decode(&rec)
	sr.Body.Close()
	if rec.State != string(model.Paused) {
		t.Errorf("state = %q after a refused activity call, want paused — this must not resume", rec.State)
	}
}

// TestActivityRejectsUnknownIDsAndWrongMethods: 404 for an id that does not exist
// (never a 409 that implies it does), and 405 for a wrong method on a route that
// exists (§6.2's general rule, not the default 404 arm).
func TestActivityRejectsUnknownIDsAndWrongMethods(t *testing.T) {
	cl := &activityFakeClient{barrierFakeClient: &barrierFakeClient{}}
	base, sid, closeFn := activityFixture(t, cl)
	defer closeFn()

	resp := do(t, http.MethodPost, base+"/v1/sandboxes/sbx_nope/activity")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: want 404, got %d", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, base+"/v1/sandboxes/"+string(sid)+"/activity")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the activity route: want 405, got %d", resp.StatusCode)
	}
}
