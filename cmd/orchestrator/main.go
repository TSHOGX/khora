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
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"warren/internal/orchestrator"
	"warren/internal/store"
)

func main() {
	var (
		addr       = flag.String("addr", "127.0.0.1:8080", "listen address for the §6.1 API. MUST be loopback unless --insecure-listen is passed, and that is REFUSED AT STARTUP rather than documented (ADR-0007): the API has no authentication and no TLS — by design, since §1.4 excludes the credential family from the engine permanently — so a routable listener publishes a control plane able to create root-capable sandboxes. A default is not a boundary; pinning this to loopback is what makes a TLS-terminating, authenticating gateway in front of it the only outward-facing surface BY CONSTRUCTION rather than by an operator remembering. Every loopback spelling is accepted (127/8, ::1, the v4-mapped form); an empty host is not, since it binds every interface, and a NAME is accepted only when every address it resolves to is loopback")
		insecure   = flag.Bool("insecure-listen", false, "permit a NON-loopback --addr, publishing the unauthenticated §6.1 API (ADR-0007). It buys exactly one thing: the loopback check is not consulted — no authentication appears either way, so anything that can route to this host can create, read and destroy sandboxes and read every file in them. Pass it only where the network itself is the boundary (a private namespace, a host firewall). It exists so that publishing the API is a DECISION on the record — in `ps` output and in the startup log — rather than an omission; refusing outright would only push an operator to patch the source, which takes the decision out of the record entirely")
		templates  = flag.String("templates", "/var/lib/warren/templates", "BUILT Template directory root: each subdirectory is a builtin Template whose directory name IS its TemplateID, and the boot seed registers every one it finds. The repo holds Template DEFINITIONS (templates/bare/), and `templates/bare/build.sh` writes the built artifact here — so this must NOT point at the definitions directory, or the seed registers source files as a runnable Template. Defaults to an ABSOLUTE path outside any Go module, unlike --state/--store, and that asymmetry is deliberate: a Template contains mode-0700 root-owned directories (Debian's /etc/ssl/private), so one built inside a module tree makes `go build ./...` fail for a non-root user. build.sh refuses an in-module destination for the same reason")
		state      = flag.String("state", "./scratch/state", "state root (overlays + runsc state/bundles)")
		storePath  = flag.String("store", "./scratch/store.db", "SQLite store file (ADR-0004)")
		volumes    = flag.String("volumes-dir", "", "§5.C volume storage root, one unit per volume; empty = <state>/volumes. A SIBLING of the overlay root, never nested inside it — each subsystem's orphan sweep enumerates its own root, so nesting would put one subsystem's units inside the other's enumeration")
		health     = flag.Duration("health-timeout", 30*time.Second, "cold-start readiness gate timeout (⑦)")
		idleTO     = flag.Duration("idle-timeout", 30*time.Minute, "default idle TTL: running idle past this → auto-pause (§4.3)")
		retention  = flag.Duration("retention", 24*time.Hour, "default retention TTL: paused past this → evict (§4.3)")
		sweep      = flag.Duration("reaper-interval", 30*time.Second, "reaper sweep cadence (§4.3)")
		evtRetain  = flag.Duration("event-retention", 0, "§4.4-C replayable event-stream window: DELIVERED outbox rows older than this are trimmed, which advances `earliest` in GET /v1/events and makes its 410 reachable. 0 = never trim (the default), keeping all history replayable at the cost of unbounded at-rest growth. Argue the value from the reconnect window you promise your SUBSCRIBERS, never from disk: a subscriber whose cursor falls below `earliest` gets 410 and must restart from there, losing every event in between. Age is measured from DELIVERY, not occurrence, so a publisher halted for a week does not make its backlog instantly trimmable. Undelivered rows are never trimmed at any setting")
		tdBudget   = flag.Duration("transport-down-budget", 2*time.Minute, "transport-down restart budget before escalate→resume (D4b)")
		nodeDisk   = flag.Int64("node-disk-byte", 0, "§3.4 no-oversubscribe disk budget: bytes for sandbox Overlay upper+work plus Template/layer bytes; 0 = unmetered (axis not gated). Set BELOW the raw device size — the budget counts only what the engine may PROMISE guests, so per-sandbox substrate overhead (runsc state on disk) lives outside it. While metered, a create must specify a disk cap or it is refused")
		nodeMem    = flag.Int64("node-memory-byte", 0, "§3.4 no-oversubscribe memory budget: total host RAM the engine may promise to sandboxes; 0 = unmetered (axis not gated). Set BELOW physical RAM — the budget counts only what the engine may PROMISE guests, so per-sandbox substrate overhead (sentry/gofer footprint) lives outside it. While metered, a create must specify a memory cap or it is refused")
		sudo       = flag.Bool("sudo", false, "prefix runsc/ip with sudo (dev; prod runs as root)")
		viaUID     = flag.Int("via-uid", -1, "§2.4 invariant 2: the uid of the host-local process behind `egress.via[].endpoint`, whose OWN egress the engine pins under the same L0 floor. A DEPLOYMENT input, never a declaration field — the engine does not know or ask what is behind that endpoint, only which local uid to pin. Unset (-1) ⇒ no second-hop table is installed AND `egress.via[]` declarations are REFUSED (400): accepting one would record a declaration whose invariant the engine cannot enforce, leaving a confused deputy live while every first-hop assertion still passes. Must NOT be this process's own uid — that value puts the engine's own guest dials under the second-hop floor and is refused at startup (it would break transport for every sandbox, intermittently)")
		pubIP      = flag.String("publish-ip", "", "host address §5.D `ingress.publish[]` endpoints bind to; empty = 127.0.0.1. The conservative default is deliberate: publish is an EXPLICIT inbound exception (§2.1 ③), so reachability from outside this host must come from a decision rather than from omitting a flag")
		shutdownTO = flag.Duration("shutdown-timeout", 30*time.Second, "how long a SIGTERM/SIGINT drain waits for in-flight requests before closing them. RUNNING SANDBOXES ARE NOT TOUCHED at any value: they keep running and the next boot re-adopts them (§4.4-A takeover — same placement, same IP, no new epoch), because a pause would discard guest memory (§4.2 stores none) and make a routine restart destroy every guest's in-memory work. A DEADLINE IS REQUIRED rather than optional: the data plane's normal shape is a long-lived stream (an attached shell at a prompt), and a drain waiting on one would never return — measured. When it expires those streams are closed, which is reconnectable (§5.E Connect); a shutdown that never completes is not. Set it above your longest unary request, not above your longest stream")
	)
	flag.Parse()

	// -1 is the "unset" sentinel because uid 0 is a REAL uid: a plain int flag could
	// not tell "the operator said root" from "the operator said nothing", and those
	// must behave oppositely (install the table and permit `via[]` vs neither).
	//
	// "uid 0 is a real uid" says nothing about whether it is a LEGAL one here: it is
	// this process's own uid (ADR-0002 — the engine runs as root), and that value would
	// put the engine's own dials to guest addresses under the second-hop floor.
	// orchestrator.New refuses it — in the assembly rather than here, so the substrate
	// harness (which builds an Engine directly) cannot bypass the check.
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

	// boot seed (§4.3 + §4.4-A) BEFORE anything else: ensure a builtin
	// `templates` row per static Template directory, so a create's
	// sandboxes.template FK (§4.3)
	// resolves. Idempotent across restarts.
	if err := eng.SeedTemplates(context.Background()); err != nil {
		log.Fatalf("orchestrator: seed builtin templates: %v", err)
	}

	// §5.C / §4.4-A orphan-VOLUME sweep, and its position here is LOAD-BEARING rather
	// than a matter of taste: it runs BEFORE anything can create a storage unit.
	//
	// A leaked quota row does not reserve its own unit id, so a restart can hand that
	// id — ALONG WITH ITS CAP — to a brand-new volume, which then meets ENOSPC on a
	// limit nobody set for it, or is silently over-provisioned. The engine cannot
	// detect either from the inside: the cap it reads back looks exactly like the
	// volume's own (§5.C / volume.md D7.1). Deferring this to the reaper's first tick
	// would leave a window in which precisely that inheritance happens, so it is
	// ordered ahead of Reconcile (which can materialize storage while re-adopting) and
	// ahead of serving.
	//
	// Non-fatal: the reaper repeats it. At boot the in-flight keep-set is empty, so
	// every unit without a record is a true orphan.
	if err := eng.SweepOrphanVolumes(context.Background()); err != nil {
		log.Printf("orchestrator: boot orphan-volume sweep (retried by reaper): %v", err)
	}

	// §5.C / volume.md D7.3 CAP REPAIR, in the same pre-creation position and for a
	// related reason: writing a snapshot's cap is necessarily the second substrate call,
	// so a crash in between leaves a registered, live, UNCAPPED volume — and that is
	// invisible afterwards on its own, because the limit read cannot tell "never capped"
	// from "deliberately uncapped". The record is re-asserted onto the storage; the
	// reverse direction would ratify the crash residue.
	if err := eng.RepairVolumeCaps(context.Background()); err != nil {
		log.Printf("orchestrator: boot volume-cap repair (retried by reaper): %v", err)
	}

	// §5.C / volume.md D7 LEAKED QUOTA ROWS. A leaked row does not reserve its own unit
	// id, so a remount can hand that id — along with its cap — to a brand-new volume.
	// Runs here, before anything creates a unit, for exactly that reason.
	if err := eng.ReclaimQuotaRows(context.Background()); err != nil {
		log.Printf("orchestrator: boot quota-row reclamation (retried by reaper): %v", err)
	}

	// §2.4 invariant 2's second half: the ENGINE-LEVEL table pinning the second hop,
	// installed once here. BEFORE serving, and before Reconcile adopts anything, for
	// the same reason the per-sandbox rules go on before the isolated unit runs — a
	// `via[]` declaration live while that table is absent is an unpinned confused
	// deputy, and the symptom is a floor counter that does not move rather than an
	// error (network.md D5 ㉔).
	//
	// FATAL rather than logged, unlike the sweeps above: those are retried by the
	// reaper, while this one has no retry and its absence is a silent security gap.
	// With no --via-uid it installs nothing (and removes a stale table from a previous
	// run), which is not a gap — `via[]` is refused at the write point instead.
	if err := eng.InstallSecondHopTable(context.Background()); err != nil {
		log.Fatalf("orchestrator: install the second-hop table (§2.4 invariant 2): %v", err)
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

	// ONE CONTEXT OWNING EVERY BACKGROUND LOOP, cancelled by the signal handler
	// below. The loops (publisher, monitor, reaper) each exit on ctx.Done, so
	// cancelling this is how they stop — and they must stop BEFORE the store closes,
	// or a tick landing afterwards logs a spurious "store closed" error that reads
	// like a defect.
	//
	// Previously context.Background() per loop, which made them unstoppable: the
	// process only ever ended by SIGKILL or log.Fatalf.
	runCtx, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	var loops sync.WaitGroup

	// async outbox publisher (§4.4-C): drain the durable outbox to the event sink.
	const publishInterval = time.Second
	loops.Add(1)
	go func() { defer loops.Done(); eng.Publisher.Run(runCtx, publishInterval) }()

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
	loops.Add(1)
	go func() {
		defer loops.Done()
		eng.Publisher.Monitor(runCtx, 10*time.Second, 30*publishInterval, func(s store.Stats) {
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
	}()

	// §4.3 reaper: the autonomous control loop (auto-pause / crash-pause / evict
	// + periodic reconcile). Started AFTER startup reconcile so it never races
	// the boot-time orphan reclaim / re-adoption.
	loops.Add(1)
	go func() { defer loops.Done(); eng.Reaper.Run(runCtx) }()

	// The published case is logged as a WARNING at the moment of serving, not merely
	// permitted by the flag (ADR-0007): the flag records the decision in `ps` output,
	// and this records it in the log an operator reads afterwards when asking why an
	// unauthenticated control plane is reachable.
	if *insecure {
		log.Printf("WARNING: --insecure-listen: serving the §6.1 API on %s, which is NOT loopback. "+
			"The API has NO authentication and NO TLS, so anything that can route to this host can "+
			"create, read and destroy sandboxes and read every file in them (ADR-0007)", *addr)
	}

	srv := &http.Server{Addr: *addr, Handler: eng.HTTPHandler()}

	// SIGTERM / SIGINT → graceful shutdown. `cmd/sandboxd` is the in-repo precedent
	// for the shape (signal.Notify in the init path); what differs is what the signal
	// MEANS here, and that difference is the design:
	//
	// A SANDBOX IS NOT A REQUEST, so this does NOT pause running sandboxes. They keep
	// running, and the next boot re-adopts them (§4.4-A takeover: the same placement,
	// the same IP, no new epoch). Pausing on the way out would be the larger blast
	// radius, not the safer one — a pause discards guest memory (§4.2 stores none,
	// permanently), so restarting the orchestrator to change a flag would destroy
	// every guest's in-memory work. Engine.Shutdown's comment carries the full
	// argument.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("orchestrator: %v — draining. Running sandboxes are LEFT RUNNING and re-adopted at "+
			"the next boot (§4.4-A); in-flight requests have %s to finish", sig, *shutdownTO)

		// A SECOND signal stops immediately. An operator who sends TERM twice means
		// it, and a hard stop is survivable by construction (§4.4-A) — so refusing to
		// honor it would just push them to SIGKILL, which is the same exit with the
		// store closed less cleanly.
		go func() {
			s2 := <-sigCh
			log.Printf("orchestrator: second %v — exiting now without waiting for the drain", s2)
			os.Exit(1)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), *shutdownTO)
		defer cancel()
		// The order matters: drain HTTP first (in-flight requests keep working, and a
		// mid-flight resume completes its chain intact — Shutdown does not cancel a
		// handler's context), then stop the loops, then close the store.
		stopLoops()
		err := eng.Drain(ctx, srv)
		// A background loop may be blocked in a substrate operation even after its
		// context is cancelled. Keep that wait inside the same advertised shutdown
		// budget as the HTTP drain.
		if waitErr := waitForLoops(ctx, &loops); err == nil {
			err = waitErr
		}
		if closeErr := eng.CloseStore(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("orchestrator: shutdown: %v", err)
			os.Exit(1)
		}
		log.Printf("orchestrator: stopped cleanly")
		os.Exit(0)
	}()

	// READY only now: everything above — the seed, the sweeps, the second-hop table,
	// §4.4-A reconciliation — had to finish first, because /readyz answering "ok"
	// before reconciliation would tell a load balancer to send traffic to a process
	// that has not yet re-adopted its running sandboxes.
	eng.MarkReady()

	log.Printf("orchestrator: serving §6.1 API on %s (templates=%s state=%s store=%s)", *addr, *templates, *state, *storePath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("orchestrator: serve: %v", err)
	}
	// ListenAndServe returned ErrServerClosed: the signal goroutine owns the exit.
	// Block so the process does not race past it.
	select {}
}

func waitForLoops(ctx context.Context, loops *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		loops.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
