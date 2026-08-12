package orchestrator

// The operational surface: /healthz, /readyz, /metrics (M10 item 2).
//
// THE DISTINCTION THAT MAKES THESE TWO ENDPOINTS WORTH HAVING SEPARATELY, and the
// reason a single /health would be worse than either: liveness answers "is this
// process still the right process to be running", readiness answers "should traffic
// come here now". They must diverge on the one failure this engine actually has in
// this class — a WEDGED OUTBOX DRAIN. The publisher-halt path is already detectable
// (Publisher.Stats samples atomics on an independent goroutine, so the numbers stay
// truthful precisely when the drain blocks — §4.4-C operability), and until now it
// emitted only a log line: exactly a signal with no surface to appear on.
//
// A halted publisher must make this instance NOT READY while leaving it ALIVE:
//   - not ready, because the event stream has stopped, and for a billing subscriber
//     that means charges stop accruing. That is not a degraded mode that heals
//     itself; clearing it is an operator action (Publisher.Resume).
//   - still alive, because restarting the process does NOT fix it. The halt is a
//     poison envelope in a DURABLE outbox, so a fresh process re-reads the same row
//     and halts again. A liveness probe that failed here would produce a restart
//     loop that destroys the running sandboxes' placements on every cycle while
//     never addressing the cause.
//
// That asymmetry is the whole design. Everything else follows from it.
//
// WHY THE METRICS SURFACE HAS NO DEPENDENCY. Prometheus text format is a handful of
// lines to render, and §1.1's "no heavyweight external dependencies" is a standing
// constraint rather than a preference to weigh here. What the format buys is that
// every scraper already reads it.
//
// WHAT MUST NOT HAPPEN HERE — and it is the easy mistake, because these numbers are
// already available: metrics must NOT become a second, differently-shaped account of
// what §6.1 already reports. `used_byte` on the sandbox read and the per-rule hit
// counters on GET …/network are read surfaces with their own shapes, and re-exporting
// them here would create two answers to one question that drift apart. So this
// surface carries PROCESS-level facts only: publisher health, outbox depth, sandbox
// counts by state, and build/uptime. Per-object numbers stay on §6.1.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"warren/internal/model"
)

// health is the operational state the three endpoints read. It is a field on Engine
// rather than a package-level value so two Engines in one test process do not share
// counters.
type health struct {
	// startedAt anchors uptime. Set by New.
	startedAt time.Time
	// draining is set the moment a shutdown begins, and it is the ONLY input to
	// readiness that is not derived: once draining, this instance must stop
	// attracting traffic even though every other signal still looks healthy. A load
	// balancer that keeps sending here during the drain window turns a graceful
	// shutdown into refused requests.
	draining atomic.Bool
	// ready is cleared until the boot sequence completes. Serving is the last step of
	// boot, but the boot sequence's own steps (seed, reconcile, the sweeps) run
	// BEFORE the listener opens, so anything that scrapes during boot gets
	// connection-refused rather than a wrong answer. This flag covers the remaining
	// window: the listener is up but MarkReady has not been called.
	ready atomic.Bool
}

// MarkReady declares the boot sequence complete (§4.4-A reconcile has run, the
// second-hop table is installed, the sweeps have had their first pass). Call it
// immediately before serving.
//
// Separate from New because readiness is about the BOOT SEQUENCE, not the assembly:
// New wires seams and touches no substrate, while what readiness gates on is
// reconciliation having decided which placements survive. An Engine that answered
// ready straight out of New would tell a load balancer to send traffic to a process
// that has not yet re-adopted its running sandboxes.
func (e *Engine) MarkReady() { e.health.ready.Store(true) }

// BeginDraining marks this instance as no longer accepting traffic. Call it FIRST in
// the shutdown path, before anything is torn down, so a load balancer's next poll
// takes this instance out of rotation while in-flight work finishes.
func (e *Engine) BeginDraining() { e.health.draining.Store(true) }

// Draining reports whether a shutdown has begun.
func (e *Engine) Draining() bool { return e.health.draining.Load() }

// healthzHandler answers LIVENESS: is this process the right process to keep
// running?
//
// It deliberately checks almost nothing, and that restraint is the design. A liveness
// probe's only remedy is a restart, so anything a restart cannot fix must not appear
// here. The two candidates worth naming and refusing:
//
//   - a HALTED PUBLISHER is not a liveness failure. The poison envelope is in the
//     durable outbox, so a restarted process re-reads it and halts again; failing
//     liveness would produce a restart loop that tears down every running sandbox's
//     placement on each cycle and never touches the cause.
//   - a STORE ERROR is not a liveness failure either. A corrupt or unwritable store
//     file survives a restart too, and a restart loop over it destroys the running
//     sandboxes' contact with the engine (§4.4-A takeover happens per boot) while the
//     disk stays as full as it was.
//
// So this answers 200 whenever the process is running and has not begun draining.
// During a drain it reports 503: the process is going away by intent, and a
// supervisor that would restart it should do so AFTER it exits, not while it drains.
func (e *Engine) healthzHandler(w http.ResponseWriter, r *http.Request) {
	if e.health.draining.Load() {
		writeHealth(w, http.StatusServiceUnavailable, healthBody{
			Status: "draining",
			Detail: "a shutdown is in progress; this process is going away by intent",
			Uptime: e.uptime().String(),
		})
		return
	}
	writeHealth(w, http.StatusOK, healthBody{Status: "ok", Uptime: e.uptime().String()})
}

// readyzHandler answers READINESS: should traffic come to this instance now?
//
// Three ways to be unready, and each is a state where sending traffic here is worse
// than sending it elsewhere (or, on a single node, worse than a clear refusal):
//
//  1. DRAINING — a shutdown has begun. New work must not arrive.
//  2. BOOTING — the listener is open but the boot sequence has not finished.
//  3. THE OUTBOX PUBLISHER IS HALTED — the event stream has stopped, so lifecycle
//     transitions are no longer reaching subscribers. The API still works, and that
//     is exactly why this must show up somewhere: a caller creating sandboxes
//     against a halted stream gets correct API answers while everything downstream
//     silently stops seeing them.
//
// Case 3 is the one that earns this endpoint, and its remedy is an operator calling
// Publisher.Resume after fixing the sink — not a restart, which is why it is here and
// not in /healthz.
func (e *Engine) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if e.health.draining.Load() {
		writeHealth(w, http.StatusServiceUnavailable, healthBody{
			Status: "draining",
			Detail: "a shutdown is in progress; send no new work here",
			Uptime: e.uptime().String(),
		})
		return
	}
	if !e.health.ready.Load() {
		writeHealth(w, http.StatusServiceUnavailable, healthBody{
			Status: "booting",
			Detail: "the boot sequence has not completed: startup reconciliation (§4.4-A) decides which " +
				"placements survive, and serving before it finishes would answer for sandboxes this " +
				"process has not yet re-adopted",
			Uptime: e.uptime().String(),
		})
		return
	}
	if s := e.Publisher.Stats(); s.Halted {
		writeHealth(w, http.StatusServiceUnavailable, healthBody{
			Status: "outbox publisher halted",
			Detail: fmt.Sprintf("the §4.4-C event stream has STOPPED at event %s (seq %d) after %d failed "+
				"attempts: %s. %d row(s) are undelivered and nothing behind that event will be published "+
				"until an operator intervenes (Publisher.Resume) — the API itself still works, which is "+
				"why this is a readiness failure rather than a liveness one, and a restart will NOT clear "+
				"it: the blocked row is in the durable outbox",
				s.HeadEventID, s.HeadSeq, s.Attempts, s.LastError, s.Backlog),
			Uptime: e.uptime().String(),
		})
		return
	}
	writeHealth(w, http.StatusOK, healthBody{Status: "ok", Uptime: e.uptime().String()})
}

type healthBody struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Uptime string `json:"uptime"`
}

func writeHealth(w http.ResponseWriter, code int, b healthBody) {
	w.Header().Set("Content-Type", "application/json")
	// No caching: a probe reading a cached "ok" through the drain window is the one
	// failure this endpoint cannot tolerate.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(b)
}

func (e *Engine) uptime() time.Duration {
	return time.Since(e.health.startedAt).Round(time.Second)
}

// metricsHandler renders the process-level operational facts in Prometheus text
// format.
//
// The outbox depth comes from the store's OWN gauge (Publisher.Stats().Backlog,
// counted on the last completed drain pass) rather than from a read path invented
// here — one source, so the number a scraper sees is the number the publisher acted
// on.
//
// The sandbox counts are a state histogram, which is the shape that answers the
// operational questions ("is everything paused?", "is anything stuck creating?")
// without duplicating any per-sandbox §6.1 field.
func (e *Engine) metricsHandler(w http.ResponseWriter, r *http.Request) {
	s := e.Publisher.Stats()

	var b strings.Builder
	metric := func(name, help, typ string, value any, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %v\n", name, value)
			return
		}
		fmt.Fprintf(&b, "%s{%s} %v\n", name, strings.Join(labels, ","), value)
	}

	metric("warren_up", "1 while the process is serving.", "gauge", 1)
	metric("warren_uptime_seconds", "Seconds since this process assembled its engine.", "gauge",
		int64(time.Since(e.health.startedAt).Seconds()))
	metric("warren_ready", "1 when this instance should receive traffic; 0 while booting, draining, or with a halted outbox publisher.", "gauge",
		boolGauge(e.health.ready.Load() && !e.health.draining.Load() && !s.Halted))
	metric("warren_draining", "1 once a shutdown has begun.", "gauge", boolGauge(e.health.draining.Load()))

	// --- §4.4-C outbox publisher ---
	metric("warren_outbox_publisher_halted",
		"1 when the event stream has stopped at a poison envelope. Requires an operator action (Publisher.Resume); a restart does not clear it, since the blocked row is durable.",
		"gauge", boolGauge(s.Halted))
	metric("warren_outbox_backlog_rows",
		"Undelivered outbox rows seen on the last completed drain pass. From the store's own gauge, so a scraper reads the number the publisher acted on.",
		"gauge", s.Backlog)
	metric("warren_outbox_published_total",
		"Events delivered and marked published since process start.", "counter", s.Published)
	metric("warren_outbox_head_attempts",
		"Failed delivery attempts by the currently blocked event (0 = nothing blocked).", "gauge", s.Attempts)
	// The two staleness numbers are the wedge detector, and both are exported
	// because they separate failure modes a single number cannot: a sink blocked
	// inside Deliver grows in_flight_seconds, while a drain loop wedged elsewhere
	// grows only since_heartbeat_seconds.
	metric("warren_outbox_since_heartbeat_seconds",
		"Seconds since the drain loop last COMPLETED a pass. Grows without bound if the drain goroutine is wedged; stays near the tick interval when merely idle.",
		"gauge", s.SinceHeartbeat.Seconds())
	metric("warren_outbox_deliver_in_flight_seconds",
		"Seconds the current Deliver call has been running (0 = none in flight). Separates a blocked sink from a wedged drain loop.",
		"gauge", s.InFlightFor.Seconds())

	// --- sandbox counts by lifecycle state ---
	//
	// Best-effort: a store read failure exports the counts as absent rather than as
	// zero, following the §6.1 discipline that an unreadable value is omitted rather
	// than reported as 0 — a zeroed gauge here would read as "the node is empty",
	// which is a different and alarming claim.
	if list, err := e.Mediator.List(r.Context()); err == nil {
		counts := map[model.Lifecycle]int{}
		for _, sb := range list {
			counts[sb.State]++
		}
		metric("warren_sandboxes", "Sandboxes by lifecycle state (§4.1).", "gauge", len(list), `state="any"`)
		states := make([]string, 0, len(counts))
		for st := range counts {
			states = append(states, string(st))
		}
		sort.Strings(states)
		for _, st := range states {
			fmt.Fprintf(&b, "warren_sandboxes{state=%q} %d\n", st, counts[model.Lifecycle(st)])
		}
	} else {
		// THE CAUSE IS DELIBERATELY NOT IN THIS COMMENT, and that is the §6.2 rule
		// rather than terseness: an error's raw text carries absolute host paths
		// (btrfs wraps every call as `<verb> <path>: %w`), and this comment is a
		// response body like any other — `edge_test.go`'s funnel guard catches the
		// attempt, which is how this line came to be written this way. The cause goes
		// to the log, where host paths belong.
		//
		// Omitted rather than exported as 0: a zeroed gauge here would read as "the
		// node is empty", which is a different and more alarming claim than "this
		// number could not be read" (the same discipline §6.1 applies to an
		// unreadable `used_byte`).
		b.WriteString("# warren_sandboxes omitted: the store read failed; see the process log for the cause.\n")
		log.Printf("metrics: sandbox state counts omitted, store read failed: %v", err)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(b.String()))
}

func boolGauge(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Shutdown drains this engine and stops its background loops (M10 item 2).
//
// WHAT A SIGTERM DOES TO RUNNING SANDBOXES: NOTHING. They keep running, and the
// engine's next boot re-adopts them. This is the decision, and it is argued rather
// than assumed, because "drain, then exit" sounds safer:
//
//   - PAUSING EVERY SANDBOX ON THE WAY OUT WOULD BE WORSE, not safer. A pause is a
//     real lifecycle transition: it stops the world and discards guest memory (§4.2
//     stores no memory state, permanently — §1.4), so resume is a seconds-scale cold
//     start. An operator restarting the orchestrator to change a flag would destroy
//     every guest's in-memory work, which is a far larger blast radius than the
//     restart they asked for. And it would take a per-sandbox transition lock for
//     each one (§4.4-B), so a node with many sandboxes could not finish inside any
//     sane TimeoutStopSec — leaving a SIGKILL to land halfway through, which is the
//     hard stop it was trying to avoid, only now with half the sandboxes paused.
//   - A HARD STOP IS ALREADY SURVIVABLE, by construction. §4.4-A exists precisely for
//     this: startup reconciliation reclaims orphan placements and RETAKES surviving
//     `running` ones — "the provider re-recognizes its own resources by SandboxID,
//     rebuilds the handle, and rediscovers the endpoint", and "a takeover is not a
//     resume — the same placement, the same IP, and no new epoch". So the cost of
//     exiting on a running sandbox is a gap in contact, not a loss of state.
//
// So what this drains is the ENGINE'S OWN in-flight work, not the guests:
//
//  1. mark not-ready first, so a load balancer stops sending here while the rest
//     proceeds;
//  2. stop the HTTP listener and let in-flight requests finish. Measured:
//     http.Server.Shutdown waits for an in-flight handler WITHOUT cancelling its
//     context (a handler that ran through a Shutdown saw r.Context().Err() == nil),
//     which is what lets a mid-flight `resume` complete its cold-start chain
//     intact rather than being torn in half and left for reconciliation;
//  3. stop the reaper and the publisher;
//  4. close the store.
//
// THE DEADLINE IS NOT OPTIONAL, and this is the measured reason: Shutdown waits for
// open connections, and the data plane's normal shape is a LONG-LIVED STREAM (an
// attached shell sitting at a prompt). Measured, a Shutdown with a 2 s deadline
// against one open stream returned "context deadline exceeded" after the full 2 s —
// so a deadline-free Shutdown on a node with an attached shell would never return.
// When the deadline expires, the streams are closed and the drain proceeds: a severed
// stream is a reconnectable condition (the data plane has `Connect` for exactly
// that), while a shutdown that never completes is not.
type shutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

func (e *Engine) Shutdown(ctx context.Context, srv shutdownServer) error {
	firstErr := e.Drain(ctx, srv)
	if err := e.closeStore(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Drain marks the engine unavailable and performs the bounded HTTP drain. It
// intentionally does not close the store, allowing callers with background
// loops to wait for them before final store teardown.
func (e *Engine) Drain(ctx context.Context, srv shutdownServer) error {
	// Step 1 BEFORE anything else. Readiness has to flip while this instance can
	// still serve, so the drain window is when traffic is being steered away rather
	// than after it has already been refused.
	e.BeginDraining()
	e.httpHandlers.beginDrain()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Step 2. The deadline lives in ctx, supplied by the caller from
	// --shutdown-timeout.
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			// Report the bounded drain timeout while still force-closing streams; the
			// CLI distinguishes this expected condition from cleanup failures.
			keep(fmt.Errorf("http drain did not finish within the deadline (%w) — in-flight streams were "+
				"closed; they are reconnectable (§5.E Connect), whereas a shutdown that never returns is not", err))
			// Shutdown does not close active connections when its context expires.
			// Force them closed so their request contexts are cancelled, then wait for
			// every entered handler to return before Store is closed below.
			keep(srv.Close())
		}
	}
	// Close should make handlers return, but substrate calls are not guaranteed to
	// honor request cancellation. Do not let such a call extend shutdown beyond the
	// same deadline that bounds Server.Shutdown.
	keep(e.httpHandlers.wait(ctx))

	return firstErr
}

func (e *Engine) closeStore() error {
	if c, ok := e.Store.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// CloseStore closes the backing store after callers have stopped users of it.
func (e *Engine) CloseStore() error { return e.closeStore() }

// registerHealthRoutes mounts the three operational endpoints.
//
// They are NOT under /v1: the §6.1 contract is the sandbox-centric API, and an
// operational endpoint is not part of it. Keeping them off that prefix means a
// gateway can route or refuse them separately from the API — an operator scraping
// metrics from a monitoring network is a different caller from one creating
// sandboxes, and this leaves that a deployment decision rather than a fixed one.
func (e *Engine) registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", e.healthzHandler)
	mux.HandleFunc("/readyz", e.readyzHandler)
	mux.HandleFunc("/metrics", e.metricsHandler)
}
