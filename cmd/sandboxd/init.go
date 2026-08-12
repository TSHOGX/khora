package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// initMode is the minimal self-authored PID 1 (§5.E.1 / guest-runtime D4.1).
// tini source is the correctness checklist; we implement the subset we need:
//   - reaper: waitpid(-1, WNOHANG) loop draining zombies on SIGCHLD;
//   - container-level signal fan-out: SIGTERM/SIGINT reach EVERY process in the
//     guest, not just transport (directed per-process signals belong to
//     transport mode, D4.3);
//   - PID 1 has NO default signal handlers — we install the ones we handle;
//   - exit code 128+signo propagation for the supervised child;
//   - supervise + in-place restart of the transport child (D5.3): a transport
//     crash degrades to process-internal-recoverable, NOT substrate death (§4.2).
//
// The transport child is re-exec'd from /proc/self/exe (one binary, two modes,
// §5.E.1). PR_SET_CHILD_SUBREAPER is set by the child itself (transport.go), so
// the child reaps its own process subtree; PID 1 is the global-orphan backstop.
func initMode() {
	logf("init(pid %d): starting", os.Getpid())

	// SINGLE WAITER. PID 1's reaper must own Wait4(-1)
	// exclusively — it is the global-orphan backstop and cannot exclude a pid
	// from a wildcard wait. So the supervise loop below does NOT call cmd.Wait():
	// a second waiter on the same child races the reaper, and the loser gets
	// ECHILD with the exit status gone. Go 1.23 pidfd does NOT fix this: its
	// pidfdWait is waitid(P_PIDFD, WEXITED) *without* WNOWAIT, i.e. it reaps too
	// — pidfd removes the pid-reuse race, not the two-reapers race. Instead the
	// reaper hands the status to whoever registered that pid.
	// The table is shared with transport mode (reap.go) — the single-waiter
	// problem is identical on both sides; only which pids are "ours" differs.
	reaper := newReapTable()
	reaper.drainOnSIGCHLD()

	// Container-level signals → fan out to EVERY other process in the guest,
	// then exit (PID 1 exiting tears down the container). PID 1 must install
	// these explicitly. kill(-1, sig) is the container-level broadcast: the
	// kernel signals every process in our pid namespace except PID 1 itself,
	// so template-provided long-running siblings and transport's whole subtree
	// are covered, not just the one child we spawned. This stays on the PID 1
	// side of D4.3: it targets no particular pid/pgid, so it is not a directed
	// signal — those remain transport's (it holds the pid/pgid handles).
	//
	// NO GRACE PERIOD, DECIDED rather than pending (guest-runtime D4.3): Exit follows the
	// broadcast immediately and PID 1 exiting tears down the namespace, so this TERM cannot be
	// acted on — equivalent to KILL. A sandbox-level grace period is REJECTED because what has
	// state to flush is the WORKLOAD process, directed signals already exist (SendSignal), and
	// making this "graceful" would have the engine decide which guest processes deserve waiting
	// for and how long — product-layer policy §1.3's litmus keeps outside.
	//
	// The engine still never sends this: destroy is `runsc kill KILL` +
	// `delete --force` (§5.A). What a caller CAN reach is SendSignal, which refuses
	// pid <= 0 precisely so this PID 1 path stays PID 1's (D4.3).
	termCh := make(chan os.Signal, 4)
	signal.Notify(termCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-termCh
		// ESRCH just means we had no other processes left; nothing to do.
		if err := syscall.Kill(-1, sig.(syscall.Signal)); err != nil && err != syscall.ESRCH {
			logf("init: fan-out %v: %v", sig, err)
		}
		logf("init: fanned %v out to all guest processes, exiting", sig)
		os.Exit(0)
	}()

	// Supervise: (re)start the transport child in place (D5.3).
	for {
		cmd := exec.Command("/proc/self/exe", "transport")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		pid, exited, err := reaper.start(cmd)
		if err != nil {
			logf("init: start transport failed: %v; retrying in 500ms", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		logf("init: transport child pid %d", pid)
		ws := <-exited
		logf("init: transport child exited (code %d); restarting in place (D5.3) in 300ms", exitCode(ws))
		time.Sleep(300 * time.Millisecond)
	}
}

// The reapTable itself lives in reap.go — transport mode needs the same
// single-waiter shape for its own subtree (§5.E.1), so it is not PID 1's alone.
