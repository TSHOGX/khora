package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// subtreeReaperHelperEnv re-enters this test binary as the helper below.
const subtreeReaperHelperEnv = "SANDBOXD_TEST_SUBTREE_REAPER_HELPER"

// TestMain lets one test run in a FRESH process (see
// TestStartSubtreeReaperReapsWithoutExplicitDrain). It must be a separate
// process: startSubtreeReaper installs a permanent SIGCHLD handler and an
// unstoppable wildcard drain loop, which inside the shared test binary would reap
// other tests' children out from under them. A child process also matches what is
// really under test — a process that becomes a subreaper at startup, as
// transportMode does.
func TestMain(m *testing.M) {
	if os.Getenv(subtreeReaperHelperEnv) == "1" {
		os.Exit(subtreeReaperHelper())
	}
	os.Exit(m.Run())
}

// subtreeReaperHelper is the whole §5.E.1 transport-mode contract in one process:
// call startSubtreeReaper (exactly what transportMode does), orphan a grandchild
// into our subtree, and require that it is reaped with NO explicit drain() call
// anywhere — only the SIGCHLD-driven reaper that startSubtreeReaper installed.
//
// Exit 0 = reaped. Exit 1 = the leak: still present in /proc after the timeout,
// i.e. a permanent zombie.
func subtreeReaperHelper() int {
	startSubtreeReaper() // prctl + the drain loop; nothing else waits in here

	cmd := exec.Command("/bin/sh", "-c", "sleep 0.2 & echo $!")
	out, err := cmd.Output() // Output waits for the CHILD only, never the grandchild
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: run orphaner: %v\n", err)
		return 2
	}
	gpid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: parse grandchild pid from %q: %v\n", out, err)
		return 2
	}
	if _, ppid, ok := procState(gpid); ok && ppid != os.Getpid() && ppid != 0 {
		// Not fatal on its own (it may already have exited and been reaped), but
		// worth reporting: if it never reparented here, the prctl did not take and
		// this run proves nothing about our reaping.
		fmt.Fprintf(os.Stderr, "helper: grandchild %d ppid=%d, not us (%d)\n", gpid, ppid, os.Getpid())
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, _, ok := procState(gpid)
		if !ok {
			return 0 // gone from /proc = reaped
		}
		if state == 'Z' {
			fmt.Fprintf(os.Stderr, "helper: grandchild %d is a zombie\n", gpid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "helper: grandchild %d never reaped\n", gpid)
	return 1
}

// These tests cover the TRANSPORT side of the reap split (§5.E.1 / guest-runtime
// D4.2): transport mode declares PR_SET_CHILD_SUBREAPER, so its subtree's orphans
// reparent to it AND MUST BE REAPED BY IT. They run as a plain host process — the
// reaper is factored out of transportMode precisely so the subreaper contract is
// testable without a guest, since nothing about it needs pid 1 of a namespace:
// PR_SET_CHILD_SUBREAPER is per-process and works anywhere.
//
// What they CANNOT prove (needs the real guest): that in a live guest transport is
// the only wildcard waiter besides PID 1, and the restart transient where the
// subtree briefly reparents to PID 1 instead (guest-runtime C2/C3).

// asSubreaper makes the test process a subreaper for the duration of one test, so
// a grandchild orphaned below it reparents HERE rather than to the machine's init
// — the exact standing of transport mode. Reset on cleanup so later tests in this
// binary do not inherit an orphan-attracting process.
func asSubreaper(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("PR_SET_CHILD_SUBREAPER unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
			t.Errorf("failed to clear subreaper flag: %v", err)
		}
	})
}

// procState reads a pid's state letter and ppid from /proc/<pid>/stat. ok=false
// means the pid is gone — which for a dead process is precisely "it was reaped",
// since an unreaped zombie keeps its /proc entry.
//
// The comm field can itself contain spaces and parens, so the fields after it are
// located from the LAST ')' rather than by splitting the whole line.
func procState(pid int) (state byte, ppid int, ok bool) {
	buf, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := strings.LastIndexByte(string(buf), ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(string(buf)[i+1:]) // [state, ppid, ...]
	if len(fields) < 2 {
		return 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], ppid, true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(handoffTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startOrphaner forks a child that forks a grandchild and exits immediately,
// leaving the grandchild orphaned into our subtree. Returns the child's status
// channel and the grandchild's pid (printed by the child on stdout).
//
// The grandchild's sleep only has to outlive its parent; the child exits within
// milliseconds of the echo, so the orphaning is not a race. If fractional sleep
// is unsupported the grandchild exits with an error instead — still an orphan
// that must be reaped, which is all this fixture needs.
func startOrphaner(t *testing.T, r *reapTable) (<-chan syscall.WaitStatus, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.2 & echo $!")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	_, ch, err := r.start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	_ = stdout.Close()
	gpid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse grandchild pid from %q: %v", out, err)
	}
	return ch, gpid
}

// Two phases, in the order that makes the failure visible:
//
// Phase 1 (subreaper declared, nothing draining) is the broken state: the grandchild
// is reparented to us and sits as a permanent zombie 'Z'. Declaring the subreaper
// ATTRACTS orphans, so without a Wait4(-1) drain they accumulate. This is also why
// the answer is not "drop the prctl" — phase 1 asserts the reparent itself, which is
// the behaviour §5.E.1 wants; only the collection has to accompany it.
//
// Phase 2 asserts drain() collects it: the /proc entry disappears, which for an
// exited process means reaped. Without the drain this loop never terminates.
func TestSubtreeOrphanIsReaped(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	childStatus, gpid := startOrphaner(t, r)

	// Phase 1: the leak. Nothing is draining, so the orphan must pile up as a
	// zombie parented to US (proving PR_SET_CHILD_SUBREAPER attracted it here).
	var gotPpid int
	waitFor(t, "grandchild to become an unreaped zombie under us", func() bool {
		state, ppid, ok := procState(gpid)
		gotPpid = ppid
		return ok && state == 'Z' && ppid == os.Getpid()
	})
	if gotPpid != os.Getpid() {
		t.Fatalf("grandchild ppid = %d, want %d (subreaper must attract it)", gotPpid, os.Getpid())
	}

	// Phase 2: the fix. The wildcard drain collects it, entry and all.
	waitFor(t, "drain to reap the orphaned grandchild", func() bool {
		r.drain()
		_, _, ok := procState(gpid)
		return !ok
	})

	// And the registered child's status was still delivered, not dropped with it.
	if got := exitCode(recvStatus(t, childStatus)); got != 0 {
		t.Fatalf("orphaner child exit code = %d, want 0", got)
	}
}

// THE WIRING GUARD. TestSubtreeOrphanIsReaped calls drain() itself, so it proves
// the drain works — not that transport mode ever installs one, which was the
// actual bug (the prctl was there, the waiter was not). This runs the real entry
// point, startSubtreeReaper, in a fresh process and calls drain() nowhere: the
// orphan is collected only if the SIGCHLD reaper is really wired up.
//
// Against the previous code — a bare unix.Prctl with no waiter — the helper exits
// 1 with "grandchild is a zombie" on stderr.
func TestStartSubtreeReaperReapsWithoutExplicitDrain(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestStartSubtreeReaperReapsWithoutExplicitDrain")
	cmd.Env = append(os.Environ(), subtreeReaperHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subreaper helper did not reap its subtree's orphan: %v\n%s", err, out)
	}
}

// The other half of the contract: a wildcard waiter must not STEAL a status that a
// per-process monitor owes its subscribers. This drives the real delivery path —
// proc.run's end frame — with the drain loop running concurrently, which is the
// arrangement that would break a cmd.Wait()-based monitor (one of the two waiters
// gets ECHILD and the exit code is lost).
func TestProcRunEndFrameSurvivesConcurrentDrain(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	startDraining(t, r) // the wildcard waiter, racing the per-process monitor

	cmd := exec.Command("/bin/sh", "-c", "echo hello; exit 9")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	pid, ch, err := r.start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	p := newProc(pid, nil)
	sub, _, _, _ := p.subscribe()
	go p.run(ch, []source{{r: stdout, name: "stdout", closer: stdout}}, func() {})

	frames := drain(sub.ch) // terminates when run() finishes the hub
	if len(frames) < 2 {
		t.Fatalf("frames = %v; want at least a data frame and an end frame", frames)
	}
	if got := strings.TrimSpace(string(frames[0].data)); got != "hello" {
		t.Fatalf("first frame data = %q, want %q", got, "hello")
	}
	end := frames[len(frames)-1]
	if end.kind != 'e' {
		t.Fatalf("last frame kind = %q, want 'e' (end)", end.kind)
	}
	if end.exitCode != 9 {
		t.Fatalf("end frame exit code = %d, want 9 — the wildcard drain stole the status", end.exitCode)
	}
}

// 128+signo through the same path: proc.run maps the wait status the reaper hands it,
// so a killed process reports 128+signo in its end frame (D4.1 checklist / D4.3
// per-process monitor).
func TestProcRunEndFrameIs128PlusSignoOnSignal(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	startDraining(t, r)

	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	pid, ch, err := r.start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	p := newProc(pid, nil)
	sub, _, _, _ := p.subscribe()
	go p.run(ch, []source{{r: stdout, name: "stdout", closer: stdout}}, func() {})

	// Handle released by start, so signal by raw pid — as SendSignal does.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	frames := drain(sub.ch)
	if len(frames) == 0 {
		t.Fatal("no frames; want an end frame")
	}
	end := frames[len(frames)-1]
	want := 128 + int(syscall.SIGKILL)
	if end.kind != 'e' || end.exitCode != want {
		t.Fatalf("end frame = %+v; want end with exit code %d", end, want)
	}
}

// Closing the parent ends of the pipes is the PUMPS' job (see source.closer), since
// nothing else waits on the child to do it: otherwise transport mode leaks two fds per
// process started over its whole lifetime, which for a long-lived guest daemon ends in
// EMFILE.
func TestProcRunClosesPipeParentEnds(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	startDraining(t, r)

	before := openFDCount(t)
	for i := 0; i < 20; i++ {
		cmd := exec.Command("/bin/sh", "-c", "echo x")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("StdoutPipe: %v", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatalf("StderrPipe: %v", err)
		}
		pid, ch, err := r.start(cmd)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		p := newProc(pid, nil)
		sub, _, _, _ := p.subscribe()
		done := make(chan struct{})
		go p.run(ch, []source{
			{r: stdout, name: "stdout", closer: stdout},
			{r: stderr, name: "stderr", closer: stderr},
		}, func() { close(done) })
		_ = drain(sub.ch)
		<-done
	}
	// Allowance for unrelated runtime fd churn; the leak this guards would be 40.
	if after := openFDCount(t); after > before+8 {
		t.Fatalf("open fds %d → %d over 20 processes: pipe parent ends are leaking "+
			"(cmd.Wait no longer closes them — source.closer must)", before, after)
	}
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}
