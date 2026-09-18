// Command orchestrator is the single engine-host binary. The assembly lives in
// internal/orchestrator (importable + testable); this is the thin entrypoint that reads
// flags, builds the Engine, and serves the control-plane API. It runs as root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/orchestrator"
)

// version is stamped by the Makefile's -X at build time; "dev" means built by a
// bare `go build`. It answers what a binary in the field IS, which is the one
// thing a tag cannot do on its own. Per-binary rather than a shared package: the
// gateway "imports nothing from internal/" (the deployment boundary).
var version = "dev"

func main() {
	if runOrchestratorDocGenerator(os.Args[1:]) {
		return
	}
	var flags orchestratorFlags
	fs := newOrchestratorFlagSet(os.Args[0], flag.ExitOnError, &flags)
	_ = fs.Parse(os.Args[1:])

	// Before anything is validated or opened: --version must work on a host where
	// no flag would pass muster, which is where an operator asks it.
	if flags.showVersion {
		fmt.Printf("orchestrator %s\n", version)
		return
	}

	if flags.quotaReclaimTimeout <= 0 {
		log.Fatal("orchestrator: quota-reclaim-timeout must be positive")
	}
	// -1 is the unset sentinel because uid 0 is a real uid, and "the operator said root"
	// must behave oppositely from "the operator said nothing". Root is refused in
	// orchestrator.New rather than here, so the substrate harness cannot bypass the check.
	var secondHopUID *int
	if flags.viaUID >= 0 {
		secondHopUID = &flags.viaUID
	}

	eng, err := orchestrator.New(orchestrator.Config{
		QuotaReclaimTimeout: flags.quotaReclaimTimeout,
		ListenAddr:          flags.addr,
		InsecureListen:      flags.insecureListen,
		TemplatesDir:        flags.templatesDir,
		StateDir:            flags.stateDir,
		StorePath:           flags.storePath,
		VolumesDir:          flags.volumesDir,
		VolumeProvider:      orchestrator.VolumeProvider(flags.volumeProvider),
		HealthTimeout:       flags.healthTimeout,
		DefaultIdleTimeout:  flags.idleTimeout,
		DefaultRetention:    flags.retention,
		EventRetention:      flags.eventRetention,
		SweepInterval:       flags.reaperInterval,
		TransportDownBudget: flags.transportDownBudget,
		ProcessRetention:    model.ProcessRetentionPolicy{ExitedFor: flags.processRetention},
		NodeDiskByte:        flags.nodeDiskByte,
		NodeMemoryByte:      flags.nodeMemoryByte,
		SecondHopUID:        secondHopUID,
		HostPublishIP:       flags.publishIP,
		Sudo:                flags.sudo,
	})
	if err != nil {
		log.Fatalf("orchestrator: assemble: %v", err)
	}

	// The boot sequence below is ORDERED, not a list. Everything through the second-hop
	// table runs before anything can create a storage unit or adopt a record; Reconcile
	// then decides which records survive, and the two byte-sweeps run after it because
	// they need that answer. The sweeps are non-fatal — the reaper repeats each one —
	// and at boot their in-flight keep-set is empty, so an unrecorded unit is a true
	// orphan. Serving comes last (MarkReady).

	// the health and reclamation contract + atomic transition rollback: a builtin `templates` row per Template directory, so a create's
	// sandboxes.template FK resolves. Idempotent across restarts.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		log.Fatalf("orchestrator: seed builtin templates: %v", err)
	}

	// Run before any create: a leaked quota row does not reserve its unit id, so a
	// restart can hand that id, along with its cap, to a brand-new volume.
	if err := eng.SweepOrphanVolumes(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-volume sweep (retried by reaper): %v", err)
	}

	// A crash between a snapshot's two substrate calls leaves a live uncapped volume
	// that no later read can distinguish from a deliberately uncapped one.
	if err := eng.RepairVolumeCaps(context.Background()); err != nil {
		log.Printf("orchestrator: boot volume-cap repair (retried by reaper): %v", err)
	}

	// Reclaim quota rows so a later volume cannot inherit a leaked id and cap.
	if err := eng.ReclaimQuotaRows(context.Background()); err != nil {
		log.Printf("orchestrator: boot quota-row reclamation (retried by reaper): %v", err)
	}

	// The engine-level table pins the second hop. A live `via[]` declaration without it
	// is an unpinned confused deputy, so failure is fatal. With no --via-uid this removes
	// a stale table from a previous run, and `via[]` is refused at the write point.
	if err := eng.InstallSecondHopTable(context.Background()); err != nil {
		log.Fatalf("orchestrator: install second-hop table: %v", err)
	}

	// atomic transition rollback: reclaim orphan placements left by a prior crash, re-adopt survivors.
	if err := eng.Reconcile(context.Background()); err != nil {
		log.Fatalf("orchestrator: startup reconciliation: %v", err)
	}

	// the Rootfs subsystem: layer bytes with no live templates row — a Promote that crashed after writing
	// the layer and before inserting the row (bytes-first).
	if err := eng.SweepOrphanLayers(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-layer sweep (retried by reaper): %v", err)
	}

	// atomic transition rollback: Overlay units backing no sandbox row — residue of a destroy whose byte
	// reclamation failed in a previous run (the transition contract licenses the deferral; this is the retry).
	if err := eng.SweepOrphanOverlays(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-unit sweep (retried by reaper): %v", err)
	}

	// Publisher (the transactional outbox), its health monitor, and the health and reclamation contract reaper. THE ASSEMBLY OWNS THEIR
	// LIFETIME: stopping them is a middle step of Engine.Shutdown, so holding them here
	// would state the teardown order in one place and execute it in another.
	eng.StartLoops(orchestrator.LoopConfig{})

	// Logged as well as flagged (the deployment boundary): the flag records the decision in `ps`, this
	// records it where an operator looks when asking why the control plane is reachable.
	if flags.insecureListen {
		log.Printf("WARNING: --insecure-listen: serving the control-plane API on %s, which is NOT loopback. "+
			"The API has NO authentication and NO TLS, so anything that can route to this host can "+
			"create, read and destroy sandboxes and read every file in them", flags.addr)
	}

	srv := &http.Server{Addr: flags.addr, Handler: eng.HTTPHandler()}

	// A SANDBOX IS NOT A REQUEST: the drain leaves running sandboxes running for the next
	// boot to re-adopt (atomic transition rollback takeover). Engine.Shutdown argues it.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("orchestrator: %v — draining. Running sandboxes are LEFT RUNNING and re-adopted at "+
			"the next boot (atomic transition rollback); in-flight requests have %s to finish", sig, flags.shutdownTimeout)

		// A second signal stops immediately: a hard stop is survivable by construction
		// (atomic transition rollback), so refusing would only push the operator to SIGKILL.
		go func() {
			s2 := <-sigCh
			log.Printf("orchestrator: second %v — exiting now without waiting for the drain", s2)
			os.Exit(1)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), flags.shutdownTimeout)
		defer cancel()
		// One call, one deadline: every step and every wait belongs to Engine.Shutdown, so
		// -shutdown-timeout bounds all of it. Its comment has the sequence.
		err := eng.Shutdown(ctx, srv)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("orchestrator: shutdown: %v", err)
			os.Exit(1)
		}
		log.Printf("orchestrator: stopped cleanly")
		os.Exit(0)
	}()

	// Ready only now: /readyz answering ok before reconciliation would send a load
	// balancer's traffic to a process that has not yet re-adopted its running sandboxes.
	eng.MarkReady()

	log.Printf("orchestrator: serving the control-plane API on %s (templates=%s state=%s store=%s)", flags.addr, flags.templatesDir, flags.stateDir, flags.storePath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("orchestrator: serve: %v", err)
	}
	// ListenAndServe returned ErrServerClosed: the signal goroutine owns the exit.
	// Block so the process does not race past it.
	select {}
}
