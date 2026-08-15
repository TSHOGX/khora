// Command orchestrator is the single engine-host binary (the documented invariant). The assembly lives in
// internal/orchestrator (importable + testable); this is the thin entrypoint that reads
// flags, builds the Engine, and serves the the documented invariant API. Runs as root (the recorded design).
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
	"time"

	"github.com/TSHOGX/khora/internal/orchestrator"
)

// version is stamped by the Makefile's -X at build time; "dev" means built by a
// bare `go build`. It answers what a binary in the field IS, which is the one
// thing a tag cannot do on its own. Per-binary rather than a shared package: the
// gateway "imports nothing from internal/" (the recorded design).
var version = "dev"

func main() {
	var (
		showVer    = flag.Bool("version", false, "print the version and exit")
		addr       = flag.String("addr", "127.0.0.1:8080", "listen address for the the documented invariant API; must be loopback unless -insecure-listen (refused at startup, the recorded design)")
		insecure   = flag.Bool("insecure-listen", false, "permit a non-loopback -addr, publishing the API with no authentication and no TLS (the recorded design)")
		templates  = flag.String("templates", "/var/lib/khora/templates", "BUILT Template root, one subdirectory per builtin whose name is its TemplateID; not the definitions in templates/")
		state      = flag.String("state", "./scratch/state", "state root (overlays + runsc state/bundles)")
		storePath  = flag.String("store", "./scratch/store.db", "SQLite store file (the recorded design)")
		volumes    = flag.String("volumes-dir", "", "the documented invariant volume storage root; empty = <state>/volumes, a sibling of the overlay root, never nested in it")
		health     = flag.Duration("health-timeout", 30*time.Second, "cold-start readiness gate timeout (⑦)")
		idleTO     = flag.Duration("idle-timeout", 30*time.Minute, "default idle TTL: running idle past this → auto-pause (the documented invariant)")
		retention  = flag.Duration("retention", 24*time.Hour, "default retention TTL: paused longer than this is evicted — record and Overlay bytes deleted, irreversible, and the only way out of `paused`. Argue it from how long a paused sandbox must stay resumable, not from disk; 0 = never evict (the documented invariant, reaper-health the documented invariant)")
		sweep      = flag.Duration("reaper-interval", 30*time.Second, "reaper sweep cadence (the documented invariant)")
		evtRetain  = flag.Duration("event-retention", 0, "the documented invariant replay window: delivered outbox rows older than this are trimmed, advancing `earliest` and making GET /v1/events' 410 reachable. Argue it from the reconnect window you promise subscribers; 0 = never trim")
		tdBudget   = flag.Duration("transport-down-budget", 2*time.Minute, "transport-down restart budget before escalate→resume (the documented invariantb)")
		nodeDisk   = flag.Int64("node-disk-byte", 0, "the documented invariant disk budget: bytes the engine may promise guests, so set it below the device size; 0 = unmetered. While metered a create must cap disk")
		nodeMem    = flag.Int64("node-memory-byte", 0, "the documented invariant memory budget: bytes the engine may promise guests, so set it below physical RAM; 0 = unmetered. While metered a create must cap memory")
		sudo       = flag.Bool("sudo", false, "prefix runsc/ip with sudo (dev; prod runs as root)")
		viaUID     = flag.Int("via-uid", -1, "the documented invariant invariant 2: uid of the local process behind `egress.via[].endpoint`, whose own egress is pinned under the L0 floor. Unset ⇒ no second-hop table and `via[]` refused; may not be this process's uid")
		pubIP      = flag.String("publish-ip", "", "host address the documented invariant `ingress.publish[]` binds to; empty = 127.0.0.1, so reaching a published port from off-host takes a decision (the documented invariant ③)")
		shutdownTO = flag.Duration("shutdown-timeout", 30*time.Second, "how long a SIGTERM drain waits for in-flight requests, then closes them. Running sandboxes are never touched; set it above your longest unary request, not your longest stream")
	)
	flag.Parse()

	// Before anything is validated or opened: --version must work on a host where
	// no flag would pass muster, which is where an operator asks it.
	if *showVer {
		fmt.Printf("orchestrator %s\n", version)
		return
	}

	// -1 is the unset sentinel because uid 0 is a real uid, and "the operator said root"
	// must behave oppositely from "the operator said nothing". Root is refused in
	// orchestrator.New rather than here, so the substrate harness cannot bypass the check.
	var secondHopUID *int
	if *viaUID >= 0 {
		secondHopUID = viaUID
	}

	eng, err := orchestrator.New(orchestrator.Config{
		ListenAddr:          *addr,
		InsecureListen:      *insecure,
		TemplatesDir:        *templates,
		StateDir:            *state,
		StorePath:           *storePath,
		VolumesDir:          *volumes,
		HealthTimeout:       *health,
		DefaultIdleTimeout:  *idleTO,
		DefaultRetention:    *retention,
		EventRetention:      *evtRetain,
		SweepInterval:       *sweep,
		TransportDownBudget: *tdBudget,
		NodeDiskByte:        *nodeDisk,
		NodeMemoryByte:      *nodeMem,
		SecondHopUID:        secondHopUID,
		HostPublishIP:       *pubIP,
		Sudo:                *sudo,
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

	// the documented invariant + the documented invariant: a builtin `templates` row per Template directory, so a create's
	// sandboxes.template FK resolves. Idempotent across restarts.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		log.Fatalf("orchestrator: seed builtin templates: %v", err)
	}

	// BEFORE ANY CREATE (the documented invariant / the relevant internals page the documented invariant): a leaked quota row does not reserve its
	// unit id, so a restart can hand that id — along with its cap — to a brand-new volume.
	if err := eng.SweepOrphanVolumes(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-volume sweep (retried by reaper): %v", err)
	}

	// the documented invariant / the relevant internals page the documented invariant: a crash between a snapshot's two substrate calls leaves a live
	// UNCAPPED volume no later read can distinguish from a deliberately uncapped one.
	if err := eng.RepairVolumeCaps(context.Background()); err != nil {
		log.Printf("orchestrator: boot volume-cap repair (retried by reaper): %v", err)
	}

	// the documented invariant / the relevant internals page the documented invariant: the same id-plus-cap inheritance, from the quota-row side.
	if err := eng.ReclaimQuotaRows(context.Background()); err != nil {
		log.Printf("orchestrator: boot quota-row reclamation (retried by reaper): %v", err)
	}

	// the documented invariant invariant 2's second half: the engine-level table pinning the second hop. A
	// live `via[]` declaration without it is an unpinned confused deputy whose symptom is
	// a floor counter that does not move rather than an error (the relevant internals page the documented invariant ㉔) — so this
	// one is fatal, having no retry. With no --via-uid it installs nothing and removes a
	// stale table from a previous run; `via[]` is refused at the write point instead.
	if err := eng.InstallSecondHopTable(context.Background()); err != nil {
		log.Fatalf("orchestrator: install the second-hop table (the documented invariant invariant 2): %v", err)
	}

	// the documented invariant: reclaim orphan placements left by a prior crash, re-adopt survivors.
	if err := eng.Reconcile(context.Background()); err != nil {
		log.Fatalf("orchestrator: startup reconciliation: %v", err)
	}

	// the documented invariant: layer bytes with no live templates row — a Promote that crashed after writing
	// the layer and before inserting the row (bytes-first).
	if err := eng.SweepOrphanLayers(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-layer sweep (retried by reaper): %v", err)
	}

	// the documented invariant: Overlay units backing no sandbox row — residue of a destroy whose byte
	// reclamation failed in a previous run (the documented invariant licenses the deferral; this is the retry).
	if err := eng.SweepOrphanOverlays(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-unit sweep (retried by reaper): %v", err)
	}

	// Publisher (the documented invariant), its health monitor, and the the documented invariant reaper. THE ASSEMBLY OWNS THEIR
	// LIFETIME: stopping them is a middle step of Engine.Shutdown, so holding them here
	// would state the teardown order in one place and execute it in another.
	eng.StartLoops(orchestrator.LoopConfig{})

	// Logged as well as flagged (the recorded design): the flag records the decision in `ps`, this
	// records it where an operator looks when asking why the control plane is reachable.
	if *insecure {
		log.Printf("WARNING: --insecure-listen: serving the the documented invariant API on %s, which is NOT loopback. "+
			"The API has NO authentication and NO TLS, so anything that can route to this host can "+
			"create, read and destroy sandboxes and read every file in them (the recorded design)", *addr)
	}

	srv := &http.Server{Addr: *addr, Handler: eng.HTTPHandler()}

	// A SANDBOX IS NOT A REQUEST: the drain leaves running sandboxes running for the next
	// boot to re-adopt (the documented invariant takeover). Engine.Shutdown argues it.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("orchestrator: %v — draining. Running sandboxes are LEFT RUNNING and re-adopted at "+
			"the next boot (the documented invariant); in-flight requests have %s to finish", sig, *shutdownTO)

		// A second signal stops immediately: a hard stop is survivable by construction
		// (the documented invariant), so refusing would only push the operator to SIGKILL.
		go func() {
			s2 := <-sigCh
			log.Printf("orchestrator: second %v — exiting now without waiting for the drain", s2)
			os.Exit(1)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), *shutdownTO)
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

	log.Printf("orchestrator: serving the documented invariant API on %s (templates=%s state=%s store=%s)", *addr, *templates, *state, *storePath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("orchestrator: serve: %v", err)
	}
	// ListenAndServe returned ErrServerClosed: the signal goroutine owns the exit.
	// Block so the process does not race past it.
	select {}
}
