package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warren/internal/model"
	"warren/internal/store"
)

// healthEngine assembles a root-free Engine over the in-memory store arm, marked
// ready as the boot sequence would.
func healthEngine(t *testing.T) *Engine {
	t.Helper()
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	eng.MarkReady()
	return eng
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// THE LOAD-BEARING CLAIM of having two endpoints instead of one: a halted outbox
// publisher makes this instance NOT READY while leaving it ALIVE.
//
// Both halves must hold, and each protects against a different wrong design:
//
//   - if /readyz stayed ok, the only signal for a stopped event stream would remain a
//     log line — a caller would keep creating sandboxes, getting correct API answers,
//     while every subscriber downstream silently stopped seeing them (for a billing
//     subscriber, charges stop accruing).
//   - if /healthz went unhealthy, a supervisor would restart the process — and the
//     halt is a poison envelope in the DURABLE outbox, so the fresh process re-reads
//     the same row and halts again. That is a restart loop which tears down every
//     running sandbox's contact with the engine on each cycle and never touches the
//     cause.
func TestAHaltedPublisherIsNotReadyButStaysAlive(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	// Healthy first: the control on everything below.
	if code, body := get(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz on a healthy engine = %d (%s), want 200", code, body)
	}
	if code, _ := get(t, srv, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz on a healthy engine = %d, want 200", code)
	}

	// Halt the publisher the way the poison-envelope path does.
	haltPublisher(t, eng)

	code, body := get(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with a HALTED publisher = %d (%s), want 503 — the §4.4-C event stream has "+
			"stopped, and the API answering normally is exactly why this needs a surface", code, body)
	}
	if !strings.Contains(body, "halted") {
		t.Errorf("/readyz body does not say what is wrong: %s", body)
	}

	code, body = get(t, srv, "/healthz")
	if code != http.StatusOK {
		t.Errorf("/healthz with a halted publisher = %d (%s), want 200 — a restart does NOT clear a "+
			"poison envelope in the DURABLE outbox, so failing liveness here produces a restart loop "+
			"that destroys every running sandbox's placement contact on each cycle", code, body)
	}
}

// haltPublisher drives the engine's publisher into its halted state through the real
// poison-envelope path — a sink that always fails, retried until the attempt budget is
// exhausted — rather than by setting a flag. What /readyz reads must be the state the
// production mechanism actually produces.
//
// It substitutes a publisher over the SAME store, because the engine's own sink is a
// log writer that never fails, so nothing would ever halt.
func haltPublisher(t *testing.T, eng *Engine) {
	t.Helper()
	ctx := context.Background()

	// A halt needs an undelivered row to halt ON: a real transition through the
	// store's own commit path, which is what puts an event in the outbox (§4.4-C).
	if err := eng.Store.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if err := eng.Store.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating},
		&store.Event{ID: "ev-poison", Sandbox: "a", Kind: "created", State: model.Creating, OccurredAt: time.Now()}); err != nil {
		t.Fatalf("seed sandbox + event: %v", err)
	}

	const maxAttempts = 3
	eng.Publisher = store.NewPublisherWithConfig(eng.Store, sinkFunc(func(store.Event) error {
		return errors.New("the sink is down")
	}), store.PublisherConfig{MaxAttempts: maxAttempts, BaseBackoff: 0})

	for i := 0; i < maxAttempts+1; i++ {
		if _, err := eng.Publisher.PublishOnce(ctx); errors.Is(err, store.ErrHalted) {
			break
		}
	}
	if !eng.Publisher.Stats().Halted {
		t.Fatal("could not drive the publisher into its halted state, so this test asserts nothing")
	}
}

type sinkFunc func(store.Event) error

func (f sinkFunc) Deliver(ctx context.Context, ev store.Event) error { return f(ev) }

// Draining flips BOTH endpoints, and that is the point of a separate signal: during a
// drain the process is going away by intent, so a load balancer must stop sending
// traffic (readiness) AND a supervisor must not treat the drain as a hang to restart
// (liveness reports the intent rather than health).
func TestDrainingIsReportedOnBothEndpoints(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	eng.BeginDraining()
	if !eng.Draining() {
		t.Fatal("BeginDraining did not set Draining")
	}

	for _, path := range []string{"/readyz", "/healthz"} {
		code, body := get(t, srv, path)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s during a drain = %d (%s), want 503 — a probe reading ok through the drain "+
				"window keeps traffic arriving at a process that is shutting down", path, code, body)
		}
		var hb healthBody
		if err := json.Unmarshal([]byte(body), &hb); err != nil {
			t.Errorf("%s body is not JSON: %s", path, body)
			continue
		}
		if hb.Status != "draining" {
			t.Errorf("%s reports status %q during a drain, want \"draining\"", path, hb.Status)
		}
	}
}

// An engine that has not finished booting is NOT ready. §4.4-A reconciliation decides
// which placements survive, so answering ready before it completes tells a load
// balancer to send traffic to a process that has not re-adopted its running sandboxes
// — those requests would get "no live placement" for sandboxes that are in fact alive.
func TestAnEngineThatHasNotBootedIsNotReady(t *testing.T) {
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT calling MarkReady.
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	code, body := get(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before MarkReady = %d (%s), want 503", code, body)
	}
	if !strings.Contains(body, "booting") {
		t.Errorf("/readyz does not report booting: %s", body)
	}
	// Liveness is fine while booting: the process is doing exactly what it should,
	// and restarting it would only start the boot sequence over.
	if code, _ := get(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz while booting = %d, want 200 — a booting process is not a broken one, and "+
			"restarting it just restarts the boot", code)
	}

	// And the control: after MarkReady it flips.
	eng.MarkReady()
	if code, body := get(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after MarkReady = %d (%s), want 200 — without this the assertion above would "+
			"pass even if /readyz were permanently 503", code, body)
	}
}

// The metrics surface exports the process-level facts, in a format a scraper parses,
// and takes the outbox depth from the store's OWN gauge rather than a second read
// path.
func TestMetricsExportsTheProcessLevelFacts(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	for _, want := range []string{
		"warren_up 1",
		"warren_ready 1",
		"warren_draining 0",
		"warren_outbox_publisher_halted 0",
		"warren_outbox_backlog_rows",
		"warren_outbox_published_total",
		"warren_outbox_since_heartbeat_seconds",
		"warren_outbox_deliver_in_flight_seconds",
		`warren_sandboxes{state="any"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q:\n%s", want, body)
		}
	}
	// Every metric needs its HELP/TYPE pair, or a scraper treats it as untyped.
	for _, name := range []string{"warren_up", "warren_ready", "warren_outbox_backlog_rows"} {
		if !strings.Contains(body, "# HELP "+name+" ") || !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("/metrics has no HELP/TYPE for %s", name)
		}
	}
}

// The metrics surface must NOT become a second account of what §6.1 already reports.
// `used_byte` on the sandbox read and the per-rule hit counters on GET …/network are
// existing read surfaces with their own shapes; re-exporting them here would give one
// question two answers that drift. This is the guard on that, and it is mechanical
// because the temptation is real — those numbers are already in hand.
func TestMetricsDoesNotDuplicateThe61ReadSurfaces(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	_, body := get(t, srv, "/metrics")
	for _, banned := range []string{"used_byte", "hit_count", "rule_hits", "per_rule"} {
		if strings.Contains(body, banned) {
			t.Errorf("/metrics exports %q, which §6.1 already reports on its own read surfaces — two "+
				"differently-shaped accounts of one number drift apart, and the §6.1 one is the "+
				"contract", banned)
		}
	}
}

// Draining flips warren_ready to 0 while warren_up stays 1, so a dashboard can tell a
// planned drain from a dead process. The two gauges would be redundant if they moved
// together.
func TestTheReadyGaugeFollowsReadinessNotLiveness(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	eng.BeginDraining()
	_, body := get(t, srv, "/metrics")
	if !strings.Contains(body, "warren_ready 0") {
		t.Errorf("warren_ready is not 0 during a drain:\n%s", body)
	}
	if !strings.Contains(body, "warren_up 1") {
		t.Errorf("warren_up is not 1 during a drain — a drain is not a dead process, and a dashboard "+
			"needs to tell them apart:\n%s", body)
	}
	if !strings.Contains(body, "warren_draining 1") {
		t.Errorf("warren_draining is not 1 during a drain:\n%s", body)
	}
}

// The operational endpoints are NOT under /v1: the §6.1 contract is the
// sandbox-centric API, and keeping them off that prefix leaves "who may scrape
// metrics" a routing decision a gateway can make separately from "who may create
// sandboxes".
func TestTheOperationalEndpointsAreNotPartOfTheV1Contract(t *testing.T) {
	eng := healthEngine(t)
	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	for _, path := range []string{"/v1/healthz", "/v1/readyz", "/v1/metrics"} {
		code, _ := get(t, srv, path)
		if code == http.StatusOK {
			t.Errorf("%s answered 200 — the operational surface must not appear under the §6.1 prefix, "+
				"so a deployment can route or refuse it separately from the API", path)
		}
	}
}

// Shutdown marks the instance draining BEFORE it tears anything down, so the window
// in which traffic is steered away overlaps the window in which this instance can
// still serve. Marking it afterwards would refuse requests that arrive during the
// drain instead of letting the balancer move them.
func TestShutdownMarksDrainingBeforeTearingAnythingDown(t *testing.T) {
	eng := healthEngine(t)

	// A server stub that records whether draining was already set when its Shutdown
	// ran. That ordering is the assertion.
	var drainingAtShutdown bool
	stub := shutdownFunc(func(context.Context) error {
		drainingAtShutdown = eng.Draining()
		return nil
	})

	if err := eng.Shutdown(context.Background(), stub); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !drainingAtShutdown {
		t.Error("the HTTP drain began before the instance was marked draining — then a load balancer " +
			"was still sending work here while requests were already being refused")
	}
	if !eng.Draining() {
		t.Error("Shutdown left the engine not marked as draining")
	}
}

// A drain that exceeds its deadline reports that fact rather than hanging or
// swallowing it. This is the EXPECTED path when a stream is attached — measured,
// http.Server.Shutdown waits for an open connection and returns
// "context deadline exceeded" — so the error has to name the condition, or an
// operator reads a scary line for a routine shutdown.
func TestADrainThatExceedsItsDeadlineSaysSo(t *testing.T) {
	eng := healthEngine(t)
	stub := shutdownFunc(func(ctx context.Context) error { return context.DeadlineExceeded })

	err := eng.Shutdown(context.Background(), stub)
	if err == nil {
		t.Fatal("Shutdown swallowed a deadline overrun — an operator needs to know the drain was cut short")
	}
	for _, want := range []string{"deadline", "reconnectable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the overrun error does not mention %q, so it does not explain that severed streams "+
				"are recoverable and a hung shutdown is not: %v", want, err)
		}
	}
}

// A timed-out http.Server.Shutdown leaves active connections open. The forced
// Close must therefore make their handlers return before the store is closed.
func TestTimedOutDrainWaitsForHandlersBeforeClosingStore(t *testing.T) {
	eng := healthEngine(t)
	var storeClosed bool
	eng.Store = &closeTrackingStore{Store: eng.Store, close: func() { storeClosed = true }}

	if !eng.httpHandlers.enter() {
		t.Fatal("handler tracker unexpectedly draining")
	}
	srv := shutdownCloseStub{
		shutdown: func(context.Context) error { return context.DeadlineExceeded },
		close: func() error {
			if storeClosed {
				t.Error("store closed before the active handler was forced to return")
			}
			eng.httpHandlers.leave()
			return nil
		},
	}

	if err := eng.Shutdown(context.Background(), srv); err == nil {
		t.Fatal("timed-out drain was not reported")
	}
	if !storeClosed {
		t.Error("store was not closed after the active handler returned")
	}
}

type shutdownFunc func(context.Context) error

func (f shutdownFunc) Shutdown(ctx context.Context) error { return f(ctx) }
func (f shutdownFunc) Close() error                       { return nil }

type shutdownCloseStub struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s shutdownCloseStub) Shutdown(ctx context.Context) error { return s.shutdown(ctx) }
func (s shutdownCloseStub) Close() error                       { return s.close() }

type closeTrackingStore struct {
	store.Store
	close func()
}

func (s *closeTrackingStore) Close() error {
	s.close()
	if c, ok := s.Store.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}
