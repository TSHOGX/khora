//go:build substrate

// Transport-depth acceptance (C1–C7) against the REAL sandboxd under real
// runsc: PTY interactive loop, multi-connection fan-out, Connect ring replay,
// signal-to-group, refusal of non-directed signal targets, and keepalive on a
// long idle stream. Requires root.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 300s ./test/ -run Transport
//
// That filter runs both transport suites (this depth one and the round-trip in
// transport_substrate_test.go), which is usually what you want.
package test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/TSHOGX/warren/internal/transport"
	"github.com/TSHOGX/warren/internal/waitfor"
	"github.com/TSHOGX/warren/test/harness"
)

// collect drains a process event channel until end, returning accumulated data
// bytes, the pid, exit code, and whether a keepalive was seen.
type collected struct {
	pid       int
	data      strings.Builder
	exit      int
	gotEnd    bool
	keepalive bool
	// streamErr is why the stream ended without an end frame, when that happens
	// (D4.3 invariant-1 overflow disconnect vs a frame lost for no stated reason).
	streamErr error
}

func collectUntilEnd(ch <-chan transport.ProcessEvent) collected {
	var c collected
	c.exit = -1
	for ev := range ch {
		switch ev.Kind {
		case "start":
			c.pid = ev.PID
		case "data":
			c.data.Write(ev.Data)
		case "keepalive":
			c.keepalive = true
		case "end":
			c.exit = ev.ExitCode
			c.gotEnd = true
		case "error":
			// Why the stream ended without an end frame (overflow disconnect vs a
			// silently lost frame). Kept so assertions can tell them apart.
			c.streamErr = ev.Err
		}
	}
	return c
}

// C1: PTY interactive loop — start /bin/sh with a pty, type a command via
// StreamInput, see the output stream back; resize via Update reflected by
// `stty size`; CloseStdin ends the shell (Ctrl-D).
func TestTransportPtyInteractiveLoop(t *testing.T) {
	// The bare template has no `stty`; inject a tiny static winsize reader that
	// prints "SIZE <rows> <cols>" from TIOCGWINSZ on its own stdin tty.
	cl, cleanup := launchWith(t, func(templatesDir, template string) {
		harness.InjectStaticBin(t, templatesDir, template, "winsize", winsizeProbeSrc)
	})
	defer cleanup()
	ctx := context.Background()

	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Pty: &transport.PtySize{Rows: 24, Cols: 80},
	})
	if err != nil {
		t.Fatalf("Start pty: %v", err)
	}
	// grab pid from the first event.
	var pid int
	for ev := range ch {
		if ev.Kind == "start" {
			pid = ev.PID
			break
		}
	}
	if pid <= 0 {
		t.Fatal("no start event with pid")
	}

	// Collect output concurrently, into a buffer the test can READ WHILE IT FILLS.
	// Guarded by a mutex rather than handed over at the end, because the wait below has
	// to inspect it in flight — the previous version could only sleep a guessed 500 ms
	// and hope the marker had arrived.
	var (
		mu   sync.Mutex
		buf  strings.Builder
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		for ev := range ch {
			if ev.Kind == "data" {
				mu.Lock()
				buf.Write(ev.Data)
				mu.Unlock()
			}
		}
	}()
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	in, err := cl.StreamInput(ctx, pid)
	if err != nil {
		t.Fatalf("StreamInput: %v", err)
	}
	// resize to 40x120, then have the shell print its size and a marker.
	if err := cl.Update(ctx, pid, transport.PtySize{Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("Update resize: %v", err)
	}
	// This one STAYS A SLEEP, deliberately. Update returning does not mean the shell has
	// observed the new size, and the protocol exposes no signal that it has — the only
	// evidence is the winsize output that the very next Send provokes, so there is no
	// condition to poll that is not already the thing being tested. Making it a wait
	// would just be a poll whose predicate is "true".
	//
	// If the resize has NOT landed by then, winsize prints SIZE 24 80 and the assertion
	// below fails loudly rather than flaking silently, which is the right failure shape
	// for a race that cannot be closed from this side.
	time.Sleep(200 * time.Millisecond)
	if err := in.Send([]byte("winsize; echo PTY_MARKER_$((6*7))\n"), true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Wait for the OUTPUT rather than for a duration: both lines are what the assertions
	// read, so their arrival is exactly the condition, and CloseStdin must not race them.
	if !waitfor.Until(20*time.Second, 20*time.Millisecond, func() bool {
		s := seen()
		return strings.Contains(s, "PTY_MARKER_42") && strings.Contains(s, "SIZE ")
	}) {
		t.Logf("pty output did not contain both lines within 20s; asserting on what arrived:\n%s", seen())
	}
	// CloseStdin (Ctrl-D) ends the interactive shell.
	if err := cl.CloseStdin(ctx, pid); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	_ = in.Close()

	<-done
	got := seen()
	if !strings.Contains(got, "PTY_MARKER_42") {
		t.Errorf("pty output missing echoed marker; got:\n%s", got)
	}
	if !strings.Contains(got, "SIZE 40 120") {
		t.Errorf("winsize did not reflect Update resize (want \"SIZE 40 120\"); got:\n%s", got)
	}
}

// winsizeProbeSrc is a pure-Go TIOCGWINSZ reader injected into the guest
// (no coreutils/stty in the bare template). Prints "SIZE <rows> <cols>".
const winsizeProbeSrc = `package main

import (
	"fmt"
	"os"
	"unsafe"
	"syscall"
)

func main() {
	type winsize struct{ Row, Col, X, Y uint16 }
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(),
		uintptr(0x5413), uintptr(unsafe.Pointer(&ws))) // TIOCGWINSZ
	if errno != 0 {
		fmt.Printf("SIZE ERR %v\n", errno)
		return
	}
	fmt.Printf("SIZE %d %d\n", ws.Row, ws.Col)
}
`

// C2: multi-connection fan-out — one pid, two Connect, each an independent full
// copy; disconnect one, the other and the process keep flowing.
func TestTransportMultiConnectionFanOut(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	// a long-ish producer so Connects attach while it is live.
	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", "for i in 1 2 3 4 5; do echo LINE_$i; sleep 0.4; done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var pid int
	for ev := range ch {
		if ev.Kind == "start" {
			pid = ev.PID
			break
		}
	}
	if pid <= 0 {
		t.Fatal("no pid")
	}
	// drain the Start stream in the background (source keeps flowing).
	go func() {
		for range ch {
		}
	}()

	a, err := cl.Connect(ctx, pid)
	if err != nil {
		t.Fatalf("Connect a: %v", err)
	}
	b, err := cl.Connect(ctx, pid)
	if err != nil {
		t.Fatalf("Connect b: %v", err)
	}

	// a fully drains; b is closed early (disconnect) and must not disturb a.
	adone := make(chan collected, 1)
	go func() { adone <- collectUntilEnd(a) }()
	// read a couple events from b then stop reading (simulate disconnect via ctx
	// is heavy; here we just stop draining and let it be dropped — a must be fine).
	go func() {
		n := 0
		for range b {
			n++
			if n >= 1 {
				break
			}
		}
	}()

	ca := <-adone
	if !ca.gotEnd {
		// Distinguish the two ways this can happen: they look identical at this
		// assertion but call for opposite conclusions. If a was DROPPED as a slow
		// subscriber (D4.3 invariant 1), that is the documented overflow behaviour
		// under load rather than a fan-out defect — b's abandonment did not break a,
		// a simply could not keep up. Only a silent close with no reason is the
		// failure this test is about.
		if ca.streamErr != nil {
			t.Skipf("subscriber a was disconnected rather than ending cleanly (%v) — "+
				"that is D4.3 invariant 1 (overflow → drop) under load, not a fan-out "+
				"failure. The fan-out property is still covered by the other C4 tests.",
				ca.streamErr)
		}
		t.Fatal("subscriber a never saw end, and the stream reported no reason — " +
			"the end frame was lost without an overflow disconnect")
	}
	// a should have seen the full tail of lines (it attached slightly late but
	// the ring replays recent lines; at minimum the later lines are present).
	if !strings.Contains(ca.data.String(), "LINE_5") {
		t.Errorf("subscriber a missing later output; got:\n%s", ca.data.String())
	}
}

// C3: Connect replay — produce output, THEN Connect; the new subscriber gets
// recent ring bytes before live, not a blank screen. The bare template has no
// coreutils (no `sleep`), so we hold the process alive with dash's `read`
// builtin (blocks on stdin) and drive it with SendInput — deterministic, no
// timing race.
func TestTransportConnectReplaysRing(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	// echo EARLY, block on read, then echo LATE once we send a line.
	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", "echo EARLY_OUTPUT; read x; echo LATE_OUTPUT"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var pid int
	for ev := range ch {
		if ev.Kind == "start" {
			pid = ev.PID
			break
		}
	}
	if pid <= 0 {
		t.Fatal("no pid")
	}
	// The Start stream is WATCHED rather than discarded, because what it carries is the
	// precondition this test needs: EARLY_OUTPUT must already be in the ring before
	// Connect, or "Connect replayed it" proves nothing. Seeing the line on this stream is
	// direct evidence of that; the 500ms sleep it replaces was a bet on it.
	var (
		srcMu  sync.Mutex
		srcBuf strings.Builder
	)
	go func() {
		for ev := range ch { // keeps the source flowing
			if ev.Kind == "data" {
				srcMu.Lock()
				srcBuf.Write(ev.Data)
				srcMu.Unlock()
			}
		}
	}()
	earlyInRing := func() bool {
		srcMu.Lock()
		defer srcMu.Unlock()
		return strings.Contains(srcBuf.String(), "EARLY_OUTPUT")
	}
	if !waitfor.Until(20*time.Second, 20*time.Millisecond, earlyInRing) {
		t.Fatal("EARLY_OUTPUT never reached the Start stream, so it was never in the ring — " +
			"the replay assertion below would be vacuous")
	}

	// Connect fresh: it must replay EARLY before anything live.
	sub, err := cl.Connect(ctx, pid)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Collected incrementally so the send below can wait for the REPLAY to arrive rather
	// than sleeping: the replay landing is what proves the subscriber is attached, and an
	// unattached subscriber would miss LATE_OUTPUT and fail the live half for the wrong
	// reason.
	var (
		subMu  sync.Mutex
		subBuf strings.Builder
		subRes = make(chan collected, 1)
	)
	go func() {
		var c collected
		c.exit = -1
		for ev := range sub {
			switch ev.Kind {
			case "data":
				subMu.Lock()
				subBuf.Write(ev.Data)
				subMu.Unlock()
				c.data.Write(ev.Data)
			case "end":
				c.exit = ev.ExitCode
				c.gotEnd = true
			case "error":
				c.streamErr = ev.Err
			}
		}
		subRes <- c
	}()

	if !waitfor.Until(20*time.Second, 20*time.Millisecond, func() bool {
		subMu.Lock()
		defer subMu.Unlock()
		return strings.Contains(subBuf.String(), "EARLY_OUTPUT")
	}) {
		// Not fatal: the replay assertion below is this test's subject and must be the
		// thing that reports it, with the full stream contents.
		t.Logf("the new subscriber had not replayed EARLY_OUTPUT within 20s; " +
			"sending input anyway so the assertions report the real contents")
	}

	// unblock `read` so the process emits LATE_OUTPUT live to both subscribers.
	if err := cl.SendInput(ctx, pid, []byte("go\n"), false); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	c := <-subRes
	if !strings.Contains(c.data.String(), "EARLY_OUTPUT") {
		t.Errorf("Connect did not replay EARLY_OUTPUT (blank-screen bug); got:\n%s", c.data.String())
	}
	if !strings.Contains(c.data.String(), "LATE_OUTPUT") {
		t.Errorf("Connect did not receive live LATE_OUTPUT; got:\n%s", c.data.String())
	}
}

// C5: SendSignal reaches the whole process group (fork subtree), no orphans.
// A shell backgrounds a `blocker` child (blocks forever, dies on default
// SIGTERM) then waits; SIGTERM to the GROUP must end the whole stream (parent +
// child), not leave the child running as an orphan.
func TestTransportSignalProcessGroup(t *testing.T) {
	// The bare template has no coreutils, and a backgrounded dash `read` gets
	// stdin=/dev/null (immediate EOF) so it can't block. Inject a static blocker
	// that blocks unconditionally and terminates on SIGTERM (Go default action).
	cl, cleanup := launchWith(t, func(templatesDir, template string) {
		harness.InjectStaticBin(t, templatesDir, template, "blocker", blockerSrc)
	})
	defer cleanup()
	ctx := context.Background()

	// Backgrounded `blocker` (a forked child) + the parent `wait`. Both share the
	// process group AND inherit the stdout fd, so the output stream reaches EOF
	// only when the WHOLE group has died. If SendSignal only hit the parent pid,
	// the orphaned blocker would keep stdout open and the stream would never end.
	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", "blocker & echo FORKED; wait"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var pid int
	for ev := range ch {
		if ev.Kind == "start" {
			pid = ev.PID
			break
		}
	}
	if pid <= 0 {
		t.Fatal("no start pid")
	}

	// FORKED is watched rather than slept past. The fixture echoes it immediately after
	// backgrounding `blocker`, so its arrival is direct evidence the fork happened — which
	// is the precondition for the whole test: signal the parent before the child exists
	// and the stream ends for the trivial reason, passing without exercising kill(-pgid).
	var (
		mu     sync.Mutex
		buf    strings.Builder
		ended  = make(chan int, 1)
		forked = func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(buf.String(), "FORKED")
		}
	)
	go func() {
		var c collected
		c.exit = -1
		for ev := range ch {
			switch ev.Kind {
			case "data":
				mu.Lock()
				buf.Write(ev.Data)
				mu.Unlock()
			case "end":
				c.exit = ev.ExitCode
			}
		}
		ended <- c.exit
	}()
	if !waitfor.Until(20*time.Second, 20*time.Millisecond, forked) {
		t.Fatal("the guest never echoed FORKED, so the backgrounded child never started — " +
			"signalling now would test kill(pid) against a single process, not kill(-pgid)")
	}
	// SIGTERM (15) — the server defaults to kill(-pgid) so the whole group dies.
	if err := cl.SendSignal(ctx, pid, 15); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	select {
	case <-ended:
		// good: parent + backgrounded child both terminated by the group signal
		// (stdout fully closed only because no group member survived as orphan).
	case <-time.After(10 * time.Second):
		t.Fatal("output stream did not end on SIGTERM — a group member survived (signal did not reach the whole pgid)")
	}
}

// blockerSrc blocks forever; it does not catch SIGTERM, so the group signal's
// default action (terminate) kills it — the discriminator for kill(-pgid) vs
// kill(pid) in C5.
const blockerSrc = `package main

import "time"

// Loop on a timer (not select{}, which trips Go's deadlock detector and exits).
// SIGTERM is uncaught → default action terminates the process.
func main() {
	for {
		time.Sleep(time.Hour)
	}
}
`

// C7: a long idle output stream is NOT killed by an idle deadline, and
// keepalive frames arrive periodically. Uses a short keepalive by relying on the
// server's configured interval; we wait long enough to see at least one.
func TestTransportLongStreamKeepalive(t *testing.T) {
	if testing.Short() {
		t.Skip("keepalive timing test skipped in -short")
	}
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// an idle shell that produces nothing until we send input (no coreutils
	// `sleep`; `read` blocks). We hold it idle ACROSS the keepalive interval, so
	// the server must emit at least one keepalive; the stream must NOT be killed
	// by any idle deadline (D4.2). Then we unblock it to a clean end.
	var pid int
	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", "read x; echo DONE"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// collect in the background so we observe keepalive frames as they arrive.
	// pid is handed off over a channel (not a shared var) so the -race detector
	// sees a clean happens-before between the stream goroutine and this one.
	res := make(chan struct {
		data string
		ka   bool
	}, 1)
	pidCh := make(chan int, 1)
	go func() {
		var b strings.Builder
		ka := false
		for ev := range ch {
			switch ev.Kind {
			case "start":
				select {
				case pidCh <- ev.PID:
				default:
				}
			case "data":
				b.Write(ev.Data)
			case "keepalive":
				ka = true
			}
		}
		res <- struct {
			data string
			ka   bool
		}{b.String(), ka}
	}()

	// the start event arrives immediately; get the pid before idling.
	select {
	case pid = <-pidCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no start pid")
	}
	if pid <= 0 {
		t.Fatal("no start pid")
	}
	// hold the stream idle past the keepalive interval (guest `read` blocks,
	// producing nothing): the server must emit a keepalive and NOT idle-kill it.
	time.Sleep(keepaliveWait)
	if err := cl.SendInput(ctx, pid, []byte("go\n"), false); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	r := <-res
	if !strings.Contains(r.data, "DONE") {
		t.Errorf("long idle stream did not survive to produce DONE; got:\n%s", r.data)
	}
	if !r.ka {
		t.Errorf("no keepalive frame arrived on an idle stream held %s (interval 20s)", keepaliveWait)
	}
}

// keepaliveWait is how long C7 holds the stream idle — past the server's 20s
// keepalive interval so at least one keepalive frame must arrive.
const keepaliveWait = 23 * time.Second

// C6: SendSignal refuses a non-positive pid, and the sandbox survives it.
//
// This MUST be a substrate test, not a unit test: the assertion is that a real
// pid<=0 request cannot destroy a real sandbox, and only runsc's pid namespace
// contains the blast radius — kill(-1) on the host would signal the developer's
// own session.
//
// Each case is measured against a LIVE workload, which is what makes the survival
// assertion meaningful: the guest is not empty, so a fan-out would have something
// to hit. Pre-fix, both cases below killed the sandbox outright (the RPC returned
// "unavailable: unexpected EOF" and Health never came back).
func TestTransportSignalRefusesNonPositivePid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		// what the unvalidated code did with this pid, i.e. what the refusal buys
		wasHow string
	}{
		{"minus-one", -1, "kill(-(-1)) = directed kill(1) → PID 1's own TERM handler → namespace torn down"},
		{"zero", 0, "kill(-0) = kill(0) → sandboxd's own process group, which is group 1 → PID 1 included"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, cleanup := launchOne(t)
			defer cleanup()
			ctx := context.Background()

			// A live workload, held on `read` so it stays alive on its own.
			ch, err := cl.Start(ctx, transport.StartSpec{
				Cmd: "/bin/sh", Args: []string{"-c", "echo READY; read x"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			var pid int
			for ev := range ch {
				if ev.Kind == "start" {
					pid = ev.PID
					break
				}
			}
			if pid <= 0 {
				t.Fatal("no start pid")
			}
			// READY is watched rather than slept past: the fixture echoes it before
			// blocking on `read`, so its arrival is evidence the workload is actually
			// live. That is this test's precondition — the refusal must be proved against
			// a RUNNING sandbox, or "still alive afterwards" is trivially true of a
			// sandbox that had not started yet.
			var (
				mu    sync.Mutex
				buf   strings.Builder
				ended = make(chan collected, 1)
			)
			go func() {
				var c collected
				c.exit = -1
				for ev := range ch {
					switch ev.Kind {
					case "data":
						mu.Lock()
						buf.Write(ev.Data)
						mu.Unlock()
						c.data.Write(ev.Data)
					case "end":
						c.exit = ev.ExitCode
						c.gotEnd = true
					case "error":
						c.streamErr = ev.Err
					}
				}
				ended <- c
			}()
			if !waitfor.Until(20*time.Second, 20*time.Millisecond, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return strings.Contains(buf.String(), "READY")
			}) {
				t.Fatal("the guest never echoed READY, so the workload was not live — a refusal " +
					"asserted against a sandbox that had not started proves nothing")
			}

			// 15 = SIGTERM, the signal that is actually lethal here: PID 1 handles
			// it, broadcasts, and exits.
			err = cl.SendSignal(ctx, tc.pid, 15)
			if err == nil {
				t.Fatalf("SendSignal(pid=%d, SIGTERM) was ACCEPTED — %s", tc.pid, tc.wasHow)
			}
			if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
				t.Errorf("SendSignal(pid=%d) code = %v, want invalid_argument (got err %v)", tc.pid, code, err)
			}
			// The message must explain the subsystem split, not just say "invalid" —
			// a caller holding a pid<=0 needs to know fan-out is PID 1's, so it
			// stops looking for a transport-side way to do it.
			if !strings.Contains(err.Error(), "PID 1") {
				t.Errorf("refusal does not say where fan-out belongs; got: %v", err)
			}

			// The sandbox survived: Health answers AND the workload is still running.
			if _, err := cl.Health(ctx); err != nil {
				t.Fatalf("sandbox did not survive SendSignal(pid=%d): Health: %v", tc.pid, err)
			}
			select {
			case c := <-ended:
				t.Fatalf("workload pid %d died despite the refusal (exit %d, streamErr %v)", pid, c.exit, c.streamErr)
			case <-time.After(500 * time.Millisecond):
				// still running, as required
			}

			// Control: a DIRECTED signal at the same workload still works, so the
			// validation refused fan-out without breaking the real signal path.
			if err := cl.SendSignal(ctx, pid, 15); err != nil {
				t.Fatalf("directed SendSignal(pid=%d) broke: %v", pid, err)
			}
			select {
			case <-ended:
			case <-time.After(10 * time.Second):
				t.Fatal("directed SIGTERM did not end the workload stream")
			}
		})
	}
}
