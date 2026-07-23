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
		Sudo:                *sudo,
	})
	if err != nil {
		log.Fatalf("orchestrator: assemble: %v", err)
	}

	// §8 boot seed BEFORE anything else: ensure a builtin `templates` row per
	// static Template directory, so a create's sandboxes.template FK (§4.3)
	// resolves. Idempotent across restarts.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		log.Fatalf("orchestrator: seed builtin templates: %v", err)
	}

	// §4.4-A startup reconciliation BEFORE serving: reclaim orphan placements
	// left by a prior crash + re-adopt survivors so they are addressable again.
	if err := eng.Reconcile(context.Background()); err != nil {
		log.Fatalf("orchestrator: startup reconciliation: %v", err)
	}

	// §9 orphan-layer sweep at boot: reclaim any Promote-built layer bytes with
	// no live templates row (e.g. a Promote that crashed after writing the layer
	// but before inserting the row — bytes-first, §5.B). Non-fatal; the reaper
	// repeats it. buildingLayers is empty at boot, so a crashed-Promote staging
	// layer is correctly a true orphan.
	if err := eng.SweepOrphanLayers(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-layer sweep (retried by reaper): %v", err)
	}

	// async outbox publisher (§4.4-C): drain the durable outbox to the event sink.
	go eng.Publisher.Run(context.Background(), time.Second)

	// §4.3 reaper: the autonomous control loop (auto-pause / crash-pause / evict
	// + periodic reconcile). Started AFTER startup reconcile so it never races
	// the boot-time orphan reclaim / re-adoption.
	go eng.Reaper.Run(context.Background())

	log.Printf("orchestrator: serving §6.1 API on %s (templates=%s state=%s store=%s)", *addr, *templates, *state, *storePath)
	if err := http.ListenAndServe(*addr, eng.HTTPHandler()); err != nil {
		log.Fatalf("orchestrator: serve: %v", err)
	}
}
