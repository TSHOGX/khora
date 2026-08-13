package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// reapTable is a process's SINGLE wildcard waiter plus the handoff to whoever started each
// child. Both sandboxd modes need it for the same structural reason: `Wait4(-1)` cannot
// exclude a pid, so a process that owns one can have no other waiter — yet both modes also
// need per-child exit codes. The reaper is the only waiter; for a registered pid it delivers
// the wait status instead of dropping it.
//
//	init mode (PID 1) : reaps true global orphans; the status it hands off is the
//	                    transport child's, for the supervise/restart loop (D5.3).
//	transport mode    : reaps ITS OWN SUBTREE's orphans — it declares
//	                    PR_SET_CHILD_SUBREAPER, so they reparent to it and are
//	                    reaped by it (§5.E.1 / guest-runtime D4.2) — while the
//	                    statuses it hands off are the process.Start children's,
//	                    which proc.run turns into `end` frames.
//
// An unregistered pid (a true orphan the process was handed) is reaped and
// dropped. Registration is serialized against a drain so a child can never be
// reaped before its channel is in the table.
type reapTable struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
}

func newReapTable() *reapTable {
	return &reapTable{waiters: make(map[int]chan syscall.WaitStatus)}
}

// startSubtreeReaper makes the calling process the reaper of its own process subtree: it
// declares PR_SET_CHILD_SUBREAPER **and** installs the wildcard drain that collects what that
// declaration attracts.
//
// ONE call, because a subreaper declaration without a wildcard waiter is strictly WORSE than
// no declaration: the subtree's orphans stop reparenting to PID 1 — whose drain would reap and
// drop them — and instead pile up as permanent zombies under a process that never waits. With
// no exported way to do just that half, the state is unexpressible.
//
// A prctl failure is logged, not fatal: the subtree's orphans then fall back to PID 1, which
// is the already-specified lossy case (guest-runtime C2/C3), not a broken sandbox.
func startSubtreeReaper() *reapTable {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		logf("transport: PR_SET_CHILD_SUBREAPER failed (%v); subtree orphans fall back to PID 1", err)
	}
	r := newReapTable()
	r.drainOnSIGCHLD()
	return r
}

// drainOnSIGCHLD installs the reaper loop: every SIGCHLD triggers a full drain.
//
// The channel is buffered because SIGCHLD is not queued — several children dying
// at once can collapse into one signal, which is why drain() loops until nothing
// is reapable. The buffer only has to keep at least one drain PENDING per burst:
// signal.Notify drops a signal only when the buffer is full, i.e. only when a
// drain is already queued, and that queued drain runs after the death that was
// dropped and reaps it too.
func (r *reapTable) drainOnSIGCHLD() {
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		for range sigchld {
			r.drain()
		}
	}()
}

// start forks cmd and returns its pid plus a channel that yields its wait status exactly once.
// The lock spans Start so a SIGCHLD for this child cannot be drained before the waiter is
// registered. The caller must NOT call cmd.Wait() — the os.Process handle is released here
// (hence the returned pid), since the reaper owns the wait.
//
// Releasing the handle also means nothing else closes the parent ends of cmd's pipes (cmd.Wait's
// other job), so a caller using StdoutPipe owns closing them — see source in process.go.
func (r *reapTable) start(cmd *exec.Cmd) (int, <-chan syscall.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	// Drop the wait obligation (and the pidfd it holds) so a long-lived process
	// does not leak one handle per child; the reaper is the only waiter.
	_ = cmd.Process.Release()
	// Buffered so the reaper never blocks on a receiver that has not arrived —
	// and never loses the status if the child dies before the caller selects.
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = ch
	return pid, ch, nil
}

// drain reaps every currently-reapable child (D4.1 waitpid loop) and delivers the status of any
// registered pid. An unregistered pid is reaped and dropped; a registered one is handed off over
// a cap-1 channel sent to only once, so the handoff cannot block.
//
// The lock is taken AFTER Wait4, not around it. A pid can never be reaped before its waiter is
// registered, because start holds r.mu across BOTH cmd.Start and the map insert: if this loop
// reaps a child whose start has not finished registering, it blocks at the Lock below until it
// has, and then finds the channel. That ordering is where the no-lost-status property comes from.
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
// signal) — the D4.1 checklist item, used both for PID 1's supervision log and
// for the `end` frame's exit_code in transport mode.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
