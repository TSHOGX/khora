package orchestrator

// The operational surface: /healthz, /readyz and /metrics.
//
// TWO ENDPOINTS, NOT ONE /health: liveness answers "is this still the right process to be
// running", readiness answers "should traffic come here now". They diverge on the one failure
// this engine has in this class — a HALTED OUTBOX PUBLISHER, which is NOT READY but still
// ALIVE. That asymmetry earns the second endpoint; the two handler docs below carry each half.
//
// Prometheus text format is rendered by hand — the engine vocabulary boundary's "no heavyweight external dependencies" is a
// standing constraint, and what the format buys is that every scraper already reads it.
//
// WHAT MUST NOT HAPPEN HERE, and it is the easy mistake because the numbers are already at hand:
// metrics must not become a second, differently-shaped account of what the control-plane API reports. `used_byte`
// and the per-rule hit counters are read surfaces with their own shapes, and re-exporting them
// would create two answers to one question. This surface carries PROCESS-level facts only:
// publisher health, outbox depth, sandbox counts by state, build/uptime.

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

	"github.com/TSHOGX/khora/internal/model"
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

// MarkReady declares the boot sequence complete (atomic transition rollback reconcile has run, the
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
// It deliberately checks almost nothing: a liveness probe's only remedy is a restart, so
// anything a restart cannot fix must not appear here. The two candidates refused:
//
//   - a HALTED PUBLISHER. The poison envelope is in the DURABLE outbox, so a restarted
//     process re-reads it and halts again; failing liveness would produce a restart loop
//     that tears down every running sandbox's placement on each cycle and never touches
//     the cause. (readyzHandler case 3 owns the readiness half.)
//   - a STORE ERROR. A corrupt or unwritable store file survives a restart too, and a
//     restart loop over it destroys the running sandboxes' contact with the engine (atomic transition rollback
//     takeover happens per boot) while the disk stays as full as it was.
//
// So: 200 whenever the process is running and has not begun draining. During a drain, 503 —
// the process is going away by intent, and a supervisor that would restart it should do so
// AFTER it exits.
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
// Three ways to be unready, each a state where sending traffic here is worse than sending
// it elsewhere (or, on a single node, worse than a clear refusal):
//
//  1. DRAINING — a shutdown has begun. New work must not arrive.
//
//  2. BOOTING — the listener is open but the boot sequence has not finished.
//
//  3. THE OUTBOX PUBLISHER IS HALTED — PUSH delivery has stopped, so lifecycle transitions
//     are no longer reaching store.Sink subscribers. The API still works, which is why this
//     must show up somewhere: a caller creating sandboxes against a halted publisher gets
//     correct API answers while a Sink subscriber silently stops seeing them. Its remedy is
//     an operator calling Publisher.Resume after fixing the sink, not a restart — the reason
//     it is here and not in /healthz.
//
//     SCOPE, narrower than "everything downstream": this says nothing about the PULL read
//     surface. `GET /v1/events` pages the outbox by the retention window and NOT by delivery
//     state (SQLite.Events carries no `published_at` predicate, the transactional outbox: "whether the engine's
//     own sink has seen some row is not a fact an external subscriber should depend on"), so
//     while the publisher is halted a pull subscriber still reads every event, losing none.
//     Stating it the other way round invites a caller to treat a 503 here as "the engine is
//     unusable" and trip a breaker — a self-inflicted outage with zero events lost.
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
			Detail: "the boot sequence has not completed: startup reconciliation (atomic transition rollback) decides which " +
				"placements survive, and serving before it finishes would answer for sandboxes this " +
				"process has not yet re-adopted",
			Uptime: e.uptime().String(),
		})
		return
	}
	if s := e.Publisher.Stats(); s.Halted {
		writeHealth(w, http.StatusServiceUnavailable, healthBody{
			Status: "outbox publisher halted",
			Detail: fmt.Sprintf("the transactional outbox event stream has STOPPED at event %s (seq %d) after %d failed "+
				"attempts: %s. %d row(s) are undelivered and nothing behind that event will be published "+
				"until an operator intervenes (Publisher.Resume) — the API itself still works, which is "+
				"why this is a readiness failure rather than a liveness one, and a restart will NOT clear "+
				"it: the blocked row is in the durable outbox. This halt is PUSH-side only: GET /v1/events "+
				"pages the outbox by the retention window and not by delivery state, so a pull subscriber "+
				"loses no event while this is failing and must not treat it as the engine being unusable",
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
// without duplicating any per-sandbox the control-plane API field.
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

	metric("khora_up", "1 while the process is serving.", "gauge", 1)
	metric("khora_uptime_seconds", "Seconds since this process assembled its engine.", "gauge",
		int64(time.Since(e.health.startedAt).Seconds()))
	metric("khora_ready", "1 when this instance should receive traffic; 0 while booting, draining, or with a halted outbox publisher.", "gauge",
		boolGauge(e.health.ready.Load() && !e.health.draining.Load() && !s.Halted))
	metric("khora_draining", "1 once a shutdown has begun.", "gauge", boolGauge(e.health.draining.Load()))

	// --- the transactional outbox publisher ---
	metric("khora_outbox_publisher_halted",
		"1 when the event stream has stopped at a poison envelope. Requires an operator action (Publisher.Resume); a restart does not clear it, since the blocked row is durable.",
		"gauge", boolGauge(s.Halted))
	metric("khora_outbox_backlog_rows",
		"Undelivered outbox rows seen on the last completed drain pass. From the store's own gauge, so a scraper reads the number the publisher acted on.",
		"gauge", s.Backlog)
	metric("khora_outbox_published_total",
		"Events delivered and marked published since process start.", "counter", s.Published)
	metric("khora_outbox_head_attempts",
		"Failed delivery attempts by the currently blocked event (0 = nothing blocked).", "gauge", s.Attempts)
	// The two staleness numbers are the wedge detector, and both are exported
	// because they separate failure modes a single number cannot: a sink blocked
	// inside Deliver grows in_flight_seconds, while a drain loop wedged elsewhere
	// grows only since_heartbeat_seconds.
	metric("khora_outbox_since_heartbeat_seconds",
		"Seconds since the drain loop last COMPLETED a pass. Grows without bound if the drain goroutine is wedged; stays near the tick interval when merely idle.",
		"gauge", s.SinceHeartbeat.Seconds())
	metric("khora_outbox_deliver_in_flight_seconds",
		"Seconds the current Deliver call has been running (0 = none in flight). Separates a blocked sink from a wedged drain loop.",
		"gauge", s.InFlightFor.Seconds())

	// Loop progress is independent of whether a reclamation attempt succeeded.
	if e.Reaper != nil {
		stats := e.Reaper.Stats()
		var completed int64
		if !stats.LastCompleted.IsZero() {
			completed = stats.LastCompleted.Unix()
		}
		metric("khora_reaper_ticks_completed_total", "Completed maintenance ticks, including recoverable failures.", "counter", stats.TicksCompleted)
		metric("khora_reaper_last_completed_timestamp_seconds", "Unix time of the last completed tick; zero before the first completion.", "gauge", completed)
		metric("khora_reaper_quota_reclaim_failures_total", "Ticks with failed quota reclamation; timeout takes precedence over other failures.", "counter", stats.QuotaTimeouts, `reason="timeout"`)
		fmt.Fprintf(&b, "khora_reaper_quota_reclaim_failures_total{reason=\"error\"} %d\n", stats.QuotaErrors)
	}
	// Optional counts must not hide the in-memory loop signals behind Store I/O.
	countsCtx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	// --- sandbox counts by lifecycle state ---
	if list, err := e.Mediator.List(countsCtx); err == nil {
		counts := map[model.Lifecycle]int{}
		for _, sb := range list {
			counts[sb.State]++
		}
		metric("khora_sandboxes", "Sandboxes by lifecycle state (the lifecycle state machine).", "gauge", len(list), `state="any"`)
		states := make([]string, 0, len(counts))
		for st := range counts {
			states = append(states, string(st))
		}
		sort.Strings(states)
		for _, st := range states {
			fmt.Fprintf(&b, "khora_sandboxes{state=%q} %d\n", st, counts[model.Lifecycle(st)])
		}
	} else {
		// THE CAUSE IS DELIBERATELY NOT IN THE BODY (the API error contract): an error's raw text carries
		// absolute host paths (btrfs wraps every call as `<verb> <path>: %w`), and a metrics
		// body is a response body like any other — `edge_test.go`'s funnel guard catches the
		// attempt, which is how this line came to be written this way.
		//
		// Omitted rather than exported as 0: a zeroed gauge would read as "the node is
		// empty", a different and more alarming claim than "this number could not be read"
		// (the discipline the control-plane API applies to an unreadable `used_byte`).
		b.WriteString("# khora_sandboxes omitted: the store read failed; see the process log for the cause.\n")
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

// Shutdown drains this engine and stops its background loops.
//
// WHAT A SIGTERM DOES TO RUNNING SANDBOXES: NOTHING. They keep running and the next boot
// re-adopts them (atomic transition rollback: "a takeover is not a resume — the same placement, the same IP, and no
// new epoch"), so the cost of exiting on a running sandbox is a gap in contact, not lost state.
// Pausing them on the way out would be WORSE: a pause discards guest memory (FS-first pause and resume / the engine non-goals), and
// taking a per-sandbox lock each (per-sandbox transition serialization) means a busy node cannot finish inside any sane
// TimeoutStopSec — SIGKILL lands halfway anyway, now with half the sandboxes paused.
//
// So what this drains is the ENGINE'S OWN in-flight work, not the guests:
//
//  1. mark not-ready first, so a load balancer stops sending here while the rest proceeds,
//     and close the handler gate so no new request enters;
//  2. stop the reaper and the publisher, and wait for them to exit;
//  3. stop the HTTP listener and let in-flight requests finish. Measured: Server.Shutdown
//     waits for an in-flight handler WITHOUT cancelling its context (a handler that ran
//     through a Shutdown saw r.Context().Err() == nil), which is what lets a mid-flight
//     `resume` complete its cold-start chain rather than be torn in half;
//  4. wait for every ENTERED handler to return, including after a timed-out
//     Server.Shutdown forces connections closed;
//  5. close the store, behind every user of it.
//
// THIS FUNCTION PERFORMS ALL FIVE: step 2 sits in the middle, so an owner of only 1/3/4/5
// forces its caller to re-implement the order. Every wait is bounded by the single ctx, or one
// unbounded wait makes --shutdown-timeout meaningless.
//
// 2 BEFORE 3 is the one ordering that is a trade rather than a dependency: the reaper is an
// AUTONOMOUS MUTATOR that can begin a fresh transition one tick before the deadline, taking a
// per-sandbox lock (per-sandbox transition serialization) and starting substrate work that outlives the budget. Stopping it
// first means in-flight transitions can only shrink, and the cost is DELAY rather than loss —
// a handler finishing in step 3 writes its events to the durable at-least-once outbox, which
// goes out next boot (the transactional outbox).
//
// ONE BUDGET FOR ALL FIVE, not one each: a step consuming the whole deadline leaves the rest to
// fail immediately on the expired ctx and fall through to step 5, which is why 5 is
// unconditional. Per-step budgets would make the operator's one number silently five times
// larger.
//
// THE DEADLINE IS NOT OPTIONAL, measured: the data plane's normal shape is a LONG-LIVED STREAM
// (an attached shell at a prompt) — a 2 s deadline against one open stream returned "context
// deadline exceeded" after the full 2 s, so a deadline-free Shutdown on such a node would never
// return. On expiry the streams are closed and the drain proceeds: a severed stream is
// reconnectable (`Connect` exists for it), a shutdown that never completes is not.
type shutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

func (e *Engine) Shutdown(ctx context.Context, srv shutdownServer) error {
	// Readiness has to flip while this instance can still serve, so the drain window is when
	// traffic is being steered away rather than after it has already been refused.
	e.BeginDraining()
	e.httpHandlers.beginDrain()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	keep(e.stopLoops(ctx))

	// The deadline lives in ctx, supplied by the caller from --shutdown-timeout.
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			// Report the bounded drain timeout while still force-closing streams; the
			// CLI distinguishes this expected condition from cleanup failures.
			keep(fmt.Errorf("http drain did not finish within the deadline (%w) — in-flight streams were "+
				"closed; they are reconnectable (the transport data plane Connect), whereas a shutdown that never returns is not", err))
			// Shutdown does not close active connections when its context expires.
			// Force them closed so their request contexts are cancelled, then wait for
			// every entered handler to return before Store is closed below.
			keep(srv.Close())
		}
	}
	// Close should make handlers return, but substrate calls are not guaranteed to honor
	// request cancellation, and such a call must not extend shutdown beyond the same deadline
	// that bounds Server.Shutdown.
	keep(e.httpHandlers.wait(ctx))

	// UNCONDITIONAL: a store left open is a held file lock, and the errors above are all
	// conditions a next boot recovers from — refusing to close on one would turn a reported
	// overrun into a resource leak.
	keep(e.closeStore())

	return firstErr
}

func (e *Engine) closeStore() error {
	if c, ok := e.Store.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// registerHealthRoutes mounts the three operational endpoints.
//
// NOT under /v1: the control-plane API contract is the sandbox-centric API, and an operational endpoint is
// not part of it. Keeping them off that prefix lets a gateway route or refuse them separately
// — an operator scraping metrics from a monitoring network is a different caller from one
// creating sandboxes, and this leaves that a deployment decision.
func (e *Engine) registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", e.healthzHandler)
	mux.HandleFunc("/readyz", e.readyzHandler)
	mux.HandleFunc("/metrics", e.metricsHandler)
}
