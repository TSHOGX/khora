// Command orchestrator is the single engine-host binary (§1.1): it holds the
// control plane, the §4 lifecycle mediator, and the five subsystems' mechanisms.
// Deployment target is "single orchestrator binary + one config + local store"
// with no heavy external dependency (§1.1).
//
// The assembly lives in internal/orchestrator (importable + testable); this is
// the thin entrypoint that reads flags, builds the Engine, and serves the §6.1
// API. Runs as root (ADR-0002: --network=sandbox + overlay mount need root).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"engine/internal/orchestrator"
	"engine/internal/store"
)

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:8080", "listen address for the §6.1 API")
		templates = flag.String("templates", "./templates", "Template directory root (TemplateID → dir)")
		state     = flag.String("state", "./scratch/state", "state root (overlays + runsc state/bundles)")
		storePath = flag.String("store", "./scratch/store.db", "SQLite store file (ADR-0004)")
		health    = flag.Duration("health-timeout", 30*time.Second, "cold-start readiness gate timeout (⑦)")
		idleTO    = flag.Duration("idle-timeout", 30*time.Minute, "default idle TTL: running idle past this → auto-pause (§4.3)")
		retention = flag.Duration("retention", 24*time.Hour, "default retention TTL: paused past this → evict (§4.3)")
		sweep     = flag.Duration("reaper-interval", 30*time.Second, "reaper sweep cadence (§4.3)")
		tdBudget  = flag.Duration("transport-down-budget", 2*time.Minute, "transport-down restart budget before escalate→resume (D4b)")
		nodeDisk  = flag.Int64("node-disk-byte", 0, "§3.4 no-oversubscribe disk budget: bytes for sandbox Overlay upper+work plus Template/layer bytes; 0 = unmetered (axis not gated). Set BELOW the raw device size — the budget counts only what the engine may PROMISE guests, so per-sandbox substrate overhead (runsc state on disk) lives outside it. While metered, a create must specify a disk cap or it is refused")
		nodeMem   = flag.Int64("node-memory-byte", 0, "§3.4 no-oversubscribe memory budget: total host RAM the engine may promise to sandboxes; 0 = unmetered (axis not gated). Set BELOW physical RAM — the budget counts only what the engine may PROMISE guests, so per-sandbox substrate overhead (sentry/gofer footprint) lives outside it. While metered, a create must specify a memory cap or it is refused")
		sudo      = flag.Bool("sudo", false, "prefix runsc/ip with sudo (dev; prod runs as root)")
	)
	flag.Parse()

	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir:        *templates,
		StateDir:            *state,
		StorePath:           *storePath,
		HealthTimeout:       *health,
		DefaultIdleTimeout:  *idleTO,
		DefaultRetention:    *retention,
		SweepInterval:       *sweep,
		TransportDownBudget: *tdBudget,
		NodeDiskByte:        *nodeDisk,
		NodeMemoryByte:      *nodeMem,
		Sudo:                *sudo,
	})
	if err != nil {
		log.Fatalf("orchestrator: assemble: %v", err)
	}

	// boot seed (§4.3 + §4.4-A) BEFORE anything else: ensure a builtin
	// `templates` row per static Template directory, so a create's
	// sandboxes.template FK (§4.3)
	// resolves. Idempotent across restarts.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		log.Fatalf("orchestrator: seed builtin templates: %v", err)
	}

	// §4.4-A startup reconciliation BEFORE serving: reclaim orphan placements
	// left by a prior crash + re-adopt survivors so they are addressable again.
	if err := eng.Reconcile(context.Background()); err != nil {
		log.Fatalf("orchestrator: startup reconciliation: %v", err)
	}

	// §5.B / §4.4-A orphan-layer sweep at boot: reclaim any Promote-built layer bytes with
	// no live templates row (e.g. a Promote that crashed after writing the layer
	// but before inserting the row — bytes-first, §5.B). Non-fatal; the reaper
	// repeats it. buildingLayers is empty at boot, so a crashed-Promote staging
	// layer is correctly a true orphan.
	if err := eng.SweepOrphanLayers(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-layer sweep (retried by reaper): %v", err)
	}

	// §4.4-A orphan-UNIT sweep at boot: reclaim Overlay storage units backing no
	// sandbox row — the residue of any destroy whose byte reclamation failed in a
	// previous run (§4.4 licenses the deferral; this is the retry). Boot is where it
	// pays off most: the in-flight keep-set is empty, so every unrecorded unit is a
	// true orphan. Runs after Reconcile, which decides which records survive.
	// Non-fatal; the reaper repeats it each tick.
	if err := eng.SweepOrphanOverlays(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-unit sweep (retried by reaper): %v", err)
	}

	// async outbox publisher (§4.4-C): drain the durable outbox to the event sink.
	const publishInterval = time.Second
	go eng.Publisher.Run(context.Background(), publishInterval)

	// Publisher health, sampled on an INDEPENDENT goroutine (§4.4-C operability).
	// This repo has no metrics dependency and this does not introduce one — the
	// requirement is that a wedged drain be DISTINGUISHABLE from an idle one, and
	// the log is what this codebase already uses for that class of fact. What
	// makes it work is the goroutine, not the exporter: sampling on the drain
	// itself would freeze the numbers exactly when the drain blocks, so silence
	// would mean both "healthy" and "wedged".
	//
	// A halted publisher is the line that must be noticed: the event stream has
	// STOPPED, which for a billing subscriber means charges stop accruing — it is
	// not a degraded mode that heals itself, and clearing it is an operator action
	// (Publisher.Resume).
	go eng.Publisher.Monitor(context.Background(), 10*time.Second, 30*publishInterval, func(s store.Stats) {
		if s.Halted {
			log.Printf("ALERT: outbox publisher HALTED on event %s (seq %d) after %d attempts: %s — "+
				"the event stream has stopped; %d rows are undelivered and nothing behind this event will be published until an operator intervenes",
				s.HeadEventID, s.HeadSeq, s.Attempts, s.LastError, s.Backlog)
			return
		}
		log.Printf("WARN: outbox publisher unhealthy: head=%s seq=%d attempts=%d backlog=%d published=%d since_heartbeat=%s in_flight_for=%s last_err=%q",
			s.HeadEventID, s.HeadSeq, s.Attempts, s.Backlog, s.Published,
			s.SinceHeartbeat.Round(time.Millisecond), s.InFlightFor.Round(time.Millisecond), s.LastError)
	})

	// §4.3 reaper: the autonomous control loop (auto-pause / crash-pause / evict
	// + periodic reconcile). Started AFTER startup reconcile so it never races
	// the boot-time orphan reclaim / re-adoption.
	go eng.Reaper.Run(context.Background())

	log.Printf("orchestrator: serving §6.1 API on %s (templates=%s state=%s store=%s)", *addr, *templates, *state, *storePath)
	if err := http.ListenAndServe(*addr, eng.HTTPHandler()); err != nil {
		log.Fatalf("orchestrator: serve: %v", err)
	}
}
