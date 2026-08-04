package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
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
	reaper := newReapTable()

	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		for range sigchld {
			reaper.drain()
		}
	}()

	// Container-level signals → fan out to EVERY other process in the guest,
	// then exit (PID 1 exiting tears down the container). PID 1 must install
	// these explicitly. kill(-1, sig) is the container-level broadcast: the
	// kernel signals every process in our pid namespace except PID 1 itself,
	// so template-provided long-running siblings and transport's whole subtree
	// are covered, not just the one child we spawned. This stays on the PID 1
	// side of D4.3: it targets no particular pid/pgid, so it is not a directed
	// signal — those remain transport's (it holds the pid/pgid handles).
	//
	// NO GRACE PERIOD, and that predates the fan-out: the Exit below follows the
	// broadcast immediately, and PID 1 exiting tears down the namespace, so the
	// TERM cannot actually be acted on. Unreachable from the engine today — destroy
	// sends KILL then `delete --force` and never TERMs PID 1 — and whether the
	// engine wants a graceful-shutdown semantic at all is registered in
	// open-decisions §4, not decided here.
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

// reapTable is PID 1's single-waiter reaper plus the handoff to supervisors.
// The reaper drains every reapable child (the global-orphan backstop duty);
// for pids someone registered, it delivers the wait status instead of dropping
// it. Registration is serialized against a drain so a child can never be reaped
// before its channel is in the table.
type reapTable struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
}

func newReapTable() *reapTable {
	return &reapTable{waiters: make(map[int]chan syscall.WaitStatus)}
}

// start forks cmd and returns its pid plus a channel that yields its wait
// status exactly once. The lock spans Start so a SIGCHLD for this child cannot
// be drained before the waiter is registered. The caller must NOT call
// cmd.Wait() — the os.Process handle is released here (hence the returned pid),
// since the reaper owns the wait.
func (r *reapTable) start(cmd *exec.Cmd) (int, <-chan syscall.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	// Drop the wait obligation (and the pidfd it holds) so a long-lived PID 1
	// does not leak one handle per restart; the reaper is the only waiter.
	_ = cmd.Process.Release()
	// Buffered so the reaper never blocks on a receiver that has not arrived —
	// and never loses the status if the child dies before the loop selects.
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = ch
	return pid, ch, nil
}

// drain reaps every currently-reapable child (§ D4.1 waitpid loop) and delivers
// the status of any registered pid. An unregistered pid (a true global orphan
// inherited by PID 1) is simply reaped and dropped; a registered one is handed
// off over a cap-1 channel that is only ever sent to once, so the handoff itself
// cannot block.
//
// Note the lock is taken AFTER Wait4, not around it — holding a mutex across a
// syscall would buy nothing here. A pid can never be reaped before its waiter is
// registered, because the registration side (start) holds r.mu across BOTH
// cmd.Start and the map insert: if this loop reaps a child whose start has not
// finished registering, it blocks at the Lock below until it has, and then finds
// the channel. That ordering is where the no-lost-status property comes from.
func (r *reapTable) drain() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return // 0 = no reapable child right now; ECHILD = none at all
		}
		r.mu.Lock()
		if ch, ok := r.waiters[pid]; ok {
			delete(r.waiters, pid)
			ch <- ws // cap 1, one send per registration: cannot block
		}
		r.mu.Unlock()
	}
}

// exitCode maps a wait status to the conventional exit code (128+signo on
// signal) — the D4.1 checklist item, here for PID 1's own supervision log.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
