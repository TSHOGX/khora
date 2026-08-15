package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// These tests exercise the PID-1 reap/handoff seam as a
// plain host process — the logic is factored out of initMode precisely so it is
// testable without being PID 1. What they prove: the single waiter never loses a
// status, never blocks, survives an unregistered orphan and a double drain, and
// propagates 128+signo. What they CANNOT prove (needs the real guest — see the
// requested substrate test): that PID 1's SIGCHLD handler is the only waiter in
// a live guest, and the kill(-1) fan-out (a test process is not pid 1 of a pid
// namespace, so kill(-1) there would signal the whole machine).

const handoffTimeout = 10 * time.Second

func recvStatus(t *testing.T, ch <-chan syscall.WaitStatus) syscall.WaitStatus {
	t.Helper()
	select {
	case ws := <-ch:
		return ws
	case <-time.After(handoffTimeout):
		t.Fatal("timed out waiting for exit-status handoff")
		return 0
	}
}

// startDraining polls drain like the SIGCHLD goroutine would (SIGCHLD delivery
// itself is not under test) and stops at test cleanup, so a stale drain loop
// cannot reap the next test's children.
func startDraining(t *testing.T, r *reapTable) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.drain()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// pendingWaiters is the registration count, read under the table's own lock.
func pendingWaiters(r *reapTable) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiters)
}

// The core of : the reaper is the sole waiter and the supervisor still gets the
// exit code. A second waiter (cmd.Wait) would race Wait4(-1) and one side would get
// ECHILD with the status gone.
func TestReapTableDeliversExitCodeToSupervisor(t *testing.T) {
	r := newReapTable()
	startDraining(t, r)
	_, ch, err := r.start(exec.Command("/bin/sh", "-c", "exit 7"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := exitCode(recvStatus(t, ch)); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

// 128+signo propagation through the same handoff, and proof
// that start releases the os.Process handle (so no pidfd leaks per restart and
// no second waiter can exist).
func TestReapTableSignaledChildIs128PlusSigno(t *testing.T) {
	r := newReapTable()
	startDraining(t, r)
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	pid, ch, err := r.start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if cmd.Process.Pid != -1 {
		t.Fatalf("start must Release the handle (Pid cleared), got %d", cmd.Process.Pid)
	}
	// Handle released, so signal by raw pid — exactly as PID 1 does.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	ws := recvStatus(t, ch)
	if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("status = %v, want signaled SIGTERM", ws)
	}
	if got := exitCode(ws); got != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code = %d, want %d", got, 128+int(syscall.SIGTERM))
	}
}

// The status must survive a drain that completes before anyone receives — the
// cap-1 channel is what keeps the reaper non-blocking. A blocking handoff would
// wedge the reaper and pile up zombies.
func TestReapTableDrainBeforeReceiveDoesNotLoseStatus(t *testing.T) {
	r := newReapTable()
	_, ch, err := r.start(exec.Command("/bin/sh", "-c", "exit 3"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(handoffTimeout)
	for pendingWaiters(r) > 0 && time.Now().Before(deadline) {
		r.drain() // nobody receiving
		time.Sleep(2 * time.Millisecond)
	}
	if got := exitCode(recvStatus(t, ch)); got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}

// A true global orphan (nobody registered it) is reaped and dropped, and a
// second drain is a harmless no-op: the reaper must not block or panic.
func TestReapTableUnregisteredPidIsReapedAndDropped(t *testing.T) {
	r := newReapTable()
	// Started outside the table: the "global orphan" shape PID 1 backstops.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	reaped := false
	deadline := time.Now().Add(handoffTimeout)
	for time.Now().Before(deadline) {
		r.drain()
		// Once reaped the pid is gone, so signal 0 must fail.
		if err := syscall.Kill(pid, 0); err != nil {
			reaped = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !reaped {
		t.Fatal("orphan was never reaped by drain")
	}
	r.drain() // second drain: no waiter, no zombie
	if n := pendingWaiters(r); n != 0 {
		t.Fatalf("waiters leaked: %d", n)
	}
}

// A registration is removed on delivery, so a crash-restart loop cannot grow
// the table (and there is no per-child goroutine to leak).
func TestReapTableNoWaiterLeakAcrossRestarts(t *testing.T) {
	r := newReapTable()
	startDraining(t, r)
	for i := 0; i < 5; i++ {
		_, ch, err := r.start(exec.Command("/bin/sh", "-c", "exit 1"))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if got := exitCode(recvStatus(t, ch)); got != 1 {
			t.Fatalf("round %d: exit code = %d, want 1", i, got)
		}
	}
	if n := pendingWaiters(r); n != 0 {
		t.Fatalf("waiters leaked after 5 restarts: %d", n)
	}
}
