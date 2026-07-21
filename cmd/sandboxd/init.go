package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// initMode is the minimal self-authored PID 1 (§5.E.1 / guest-runtime D4.1).
// tini source is the correctness checklist; we implement the subset we need:
//   - reaper: waitpid(-1, WNOHANG) loop draining zombies on SIGCHLD;
//   - container-level signal fan-out: forward SIGTERM/SIGINT to the transport
//     child (directed per-process signals belong to transport mode, D4.3);
//   - PID 1 has NO default signal handlers — we install the ones we handle;
//   - supervise + in-place restart of the transport child (D5.3): a transport
//     crash degrades to process-internal-recoverable, NOT substrate death (§4.2).
//
// The transport child is re-exec'd from /proc/self/exe (one binary, two modes,
// §5.E.1). PR_SET_CHILD_SUBREAPER is set by the child itself (transport.go), so
// the child reaps its own process subtree; PID 1 is the global-orphan backstop.
func initMode() {
	logf("init(pid %d): starting", os.Getpid())

	// Reaper. SIGCHLD → drain all reapable children (§ D4.1 waitpid loop).
	// KNOWN LIMITATION: this blind Wait4(-1) also reaps the supervised transport
	// child, racing the supervise loop's cmd.Wait() below — whichever wins, the
	// loser gets ECHILD and the transport exit status is lost. The loop restarts
	// the child either way, so this is exit-code/telemetry-only, not a correctness
	// bug (open-decisions §2 known-items). Go 1.23+ pidfd-based Wait may narrow it.
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		for range sigchld {
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break // 0 = no reapable child right now; ECHILD = none at all
				}
			}
		}
	}()

	// Hold the current transport child pid so the signal handler can forward.
	var childPID atomic.Int64

	// Container-level signals → forward to transport child, then exit (PID 1
	// exiting tears down the container). PID 1 must install these explicitly.
	// This is a single-child forward, correct while the transport child is PID 1's
	// only child — PID-1 exit reaps the rest. When template-provided long-running
	// siblings land, this becomes a real group fan-out (open-decisions §2
	// known-items; the "fan-out" naming anticipates that).
	termCh := make(chan os.Signal, 4)
	signal.Notify(termCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-termCh
		if pid := childPID.Load(); pid > 0 {
			_ = syscall.Kill(int(pid), sig.(syscall.Signal))
		}
		logf("init: forwarded %v to transport, exiting", sig)
		os.Exit(0)
	}()

	// Supervise: (re)start the transport child in place (D5.3).
	for {
		cmd := exec.Command("/proc/self/exe", "transport")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			logf("init: start transport failed: %v; retrying in 500ms", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		childPID.Store(int64(cmd.Process.Pid))
		logf("init: transport child pid %d", cmd.Process.Pid)
		_ = cmd.Wait()
		childPID.Store(0)
		logf("init: transport child exited; restarting in place (D5.3) in 300ms")
		time.Sleep(300 * time.Millisecond)
	}
}
