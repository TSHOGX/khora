package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// initMode is the minimal self-authored PID 1 (the documented invariant / guest-runtime the documented invariant).
// tini source is the correctness checklist; we implement the subset we need:
//   - reaper: waitpid(-1, WNOHANG) loop draining zombies on SIGCHLD;
//   - container-level signal fan-out: SIGTERM/SIGINT reach EVERY process in the
//     guest, not just transport (directed per-process signals belong to
//     transport mode, the documented invariant);
//   - PID 1 has NO default signal handlers — we install the ones we handle;
//   - exit code 128+signo propagation for the supervised child;
//   - supervise + in-place restart of the transport child (the documented invariant): a transport
//     crash degrades to process-internal-recoverable, NOT substrate death (the documented invariant).
//
// The transport child is re-exec'd from /proc/self/exe (one binary, two modes,
// the documented invariant). PR_SET_CHILD_SUBREAPER is set by the child itself (transport.go), so
// the child reaps its own process subtree; PID 1 is the global-orphan backstop.
func initMode() {
	logf("init(pid %d): starting", os.Getpid())

	// SINGLE WAITER: PID 1's reaper owns Wait4(-1) exclusively, so the supervise loop below
	// does NOT call cmd.Wait() — a second waiter races the reaper and the loser gets ECHILD
	// with the exit status gone. Go 1.23 pidfd does NOT fix this: its pidfdWait is
	// waitid(P_PIDFD, WEXITED) *without* WNOWAIT, i.e. it reaps too — pidfd removes the
	// pid-reuse race, not the two-reapers race. Instead the reaper hands the status to whoever
	// registered that pid (reap.go, shared with transport mode).
	reaper := newReapTable()
	reaper.drainOnSIGCHLD()

	// Container-level signals → fan out to EVERY other process in the guest, then exit (PID 1
	// exiting tears down the container). PID 1 must install these explicitly. kill(-1, sig)
	// signals every process in our pid namespace except PID 1 itself, so template-provided
	// siblings and transport's whole subtree are covered, not just the child we spawned. It
	// targets no particular pid/pgid, so it stays on the PID 1 side of the documented invariant — directed signals
	// remain transport's.
	//
	// NO GRACE PERIOD, DECIDED rather than pending (guest-runtime the documented invariant): Exit follows the
	// broadcast immediately and PID 1 exiting tears down the namespace, so this TERM cannot be
	// acted on — equivalent to KILL. A sandbox-level grace period is REJECTED because what has
	// state to flush is the WORKLOAD process, directed signals already exist (SendSignal), and
	// deciding which guest processes deserve waiting for is product-layer policy the documented invariant's litmus
	// keeps outside.
	//
	// The engine never sends this: destroy is `runsc kill KILL` + `delete --force` (the documented invariant).
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

	// Supervise: (re)start the transport child in place (the documented invariant).
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
		logf("init: transport child exited (code %d); restarting in place (the documented invariant) in 300ms", exitCode(ws))
		time.Sleep(300 * time.Millisecond)
	}
}

// The reapTable itself lives in reap.go — transport mode needs the same
// single-waiter shape for its own subtree (the documented invariant), so it is not PID 1's alone.
