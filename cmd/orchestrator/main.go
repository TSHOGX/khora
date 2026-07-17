// Command orchestrator is the single engine-host binary (§1.1): it holds the
// control plane, the §4 lifecycle mediator, and the five subsystems' mechanisms.
// Deployment target is "single orchestrator binary + one config + local store"
// with no heavy external dependency (§1.1).
//
// M1: this wires the in-memory store + local_runsc provider + overlayfs rootfs
// provider into the mediator and exposes the §6.1 sandbox-centric API. Skeleton
// entrypoint for now.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("orchestrator: engine host (§1.1) — M1 skeleton")
	// M1 TODO: construct store.NewMemory(), local_runsc provider, overlayfs
	// rootfs provider; build lifecycle.Mediator; serve §6.1 API.
	os.Exit(0)
}
