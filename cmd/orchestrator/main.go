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
		health    = flag.Duration("health-timeout", 30*time.Second, "cold-start readiness gate timeout (⑦)")
		sudo      = flag.Bool("sudo", false, "prefix runsc/ip with sudo (dev; prod runs as root)")
	)
	flag.Parse()

	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir:  *templates,
		StateDir:      *state,
		HealthTimeout: *health,
		Sudo:          *sudo,
	})
	if err != nil {
		log.Fatalf("orchestrator: assemble: %v", err)
	}

	log.Printf("orchestrator: serving §6.1 API on %s (templates=%s state=%s)", *addr, *templates, *state)
	if err := http.ListenAndServe(*addr, eng.HTTPHandler()); err != nil {
		log.Fatalf("orchestrator: serve: %v", err)
	}
}
