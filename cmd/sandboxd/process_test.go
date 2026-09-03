package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// Unit (no substrate): the transport behavior fan-out invariants as pure logic. The real
// runsc round-trip coverage is in test/transport_substrate_test.go; these
// pin the hub mechanics deterministically and under -race.

// blockingWriter simulates a child whose input pipe is full: Write blocks until
// release is closed. Used to prove writeInput does not hold p.mu across a
// blocking write (which would freeze the fan-out — nonblocking fan-out).
type blockingWriter struct {
	entered chan struct{} // closed-ish: signals Write was reached
	release chan struct{} // Write returns once this is closed
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return len(p), nil
}

func (b *blockingWriter) Close() error { return nil }

func drain(ch <-chan frame) []frame {
	var out []frame
	for f := range ch {
		out = append(out, f)
	}
	return out
}

func TestRingBufferDropsOldestBytes(t *testing.T) {
	r := ringBuffer{cap: 10}
	r.append(frame{kind: 'd', data: []byte("aaaa")}) // 4
	r.append(frame{kind: 'd', data: []byte("bbbb")}) // 8
	r.append(frame{kind: 'd', data: []byte("cccc")}) // 12 → drop "aaaa" → 8
	snap := r.snapshot()
	if len(snap) != 2 || string(snap[0].data) != "bbbb" || string(snap[1].data) != "cccc" {
		t.Fatalf("ring = %v; want [bbbb cccc]", snap)
	}
	if r.bytes != 8 {
		t.Fatalf("ring bytes = %d; want 8", r.bytes)
	}
}

// bounded replay: a subscriber that attaches after output was produced replays the
// ring first, then joins live.
func TestConnectReplaysRingThenLive(t *testing.T) {
	p := newProc(1, nil)
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("past1")})
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("past2")})

	sub, snap, ended, _ := p.subscribe()
	if ended || sub == nil {
		t.Fatal("subscribe returned ended for a live proc")
	}
	if len(snap) != 2 || string(snap[0].data) != "past1" || string(snap[1].data) != "past2" {
		t.Fatalf("replay snap = %v; want [past1 past2]", snap)
	}
	// now live
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("live1")})
	p.finish(0)
	got := drain(sub.ch)
	if len(got) != 2 || string(got[0].data) != "live1" || got[1].kind != 'e' {
		t.Fatalf("live frames = %v; want [live1 end]", got)
	}
}

// bounded replay (multi-connection, the sandbox process model): two subscribers each get an independent
// full copy; dropping one does not disturb the other.
func TestFanOutIndependentCopies(t *testing.T) {
	p := newProc(1, nil)
	a, _, _, _ := p.subscribe()
	b, _, _, _ := p.subscribe()
	p.broadcast(frame{kind: 'd', data: []byte("x")})
	p.unsubscribe(a) // one leaves
	p.broadcast(frame{kind: 'd', data: []byte("y")})
	p.finish(0)

	ga := drain(a.ch)
	gb := drain(b.ch)
	if len(ga) != 1 || string(ga[0].data) != "x" {
		t.Fatalf("a got %v; want [x]", ga)
	}
	if len(gb) != 3 || string(gb[0].data) != "x" || string(gb[1].data) != "y" || gb[2].kind != 'e' {
		t.Fatalf("b got %v; want [x y end]", gb)
	}
}

// nonblocking fan-out: a subscriber that never drains its window is dropped on overflow;
// the source (broadcast) NEVER blocks, and a subscriber that keeps up is
// unaffected. The fast reader drains in lockstep with each broadcast so the
// assertion is deterministic (a concurrently-scheduled reader can be transiently
// outpaced — that is not an invariant violation, only source-blocking is).
func TestSlowSubscriberDroppedSourceNeverBlocks(t *testing.T) {
	p := newProc(1, nil)
	slow, _, _, _ := p.subscribe() // never drained
	fast, _, _, _ := p.subscribe() // drained after each broadcast

	total := subWindowFrames * 4 // far past the window
	var fastFrames []frame
	for i := 0; i < total; i++ {
		// If broadcast blocked on the stuck subscriber, this loop never
		// completes and -timeout kills the test — the failure signal for
		// "source blocked".
		p.broadcast(frame{kind: 'd', data: []byte("z")})
		for {
			select {
			case f := <-fast.ch:
				fastFrames = append(fastFrames, f)
				continue
			default:
			}
			break
		}
	}
	p.finish(0)
	fastFrames = append(fastFrames, drain(fast.ch)...)

	if !slow.overflow.Load() {
		t.Fatal("slow subscriber was not marked overflow/dropped")
	}
	_ = drain(slow.ch) // closed by the drop; must terminate
	if n := len(fastFrames); n != total+1 {
		t.Fatalf("fast subscriber got %d frames; want %d (all data + end)", n, total+1)
	}
	if fastFrames[len(fastFrames)-1].kind != 'e' {
		t.Fatal("fast subscriber missing terminal end frame")
	}
}

// nonblocking fan-out boundary (lossy-by-design, nonblocking fan-out / transport behavior): a
// subscriber whose window is exactly full at process exit cannot receive the end
// frame. Per the best-effort-tail contract that dropped end frame — exit code
// included — is silently lost for THAT copy; the channel still closes cleanly and
// finish must NOT block or panic. Making this loss observable would change the
// best-effort-tail contract; this test pins the current deliberate behavior.
func TestFinishOnFullWindowIsSilentlyLossy(t *testing.T) {
	p := newProc(1, nil)
	sub, _, _, _ := p.subscribe()
	// Fill the window to the brim without draining, staying just under the
	// broadcast-drop threshold (broadcast drops on the send that can't fit).
	for i := 0; i < subWindowFrames; i++ {
		p.broadcast(frame{kind: 'd', data: []byte("z")})
	}
	if sub.overflow.Load() {
		t.Fatal("subscriber dropped before finish; window fill overshot the cap")
	}
	// Exit with a full window: end frame has nowhere to go and is dropped for this
	// copy. finish must complete (source never blocks) and the channel must close.
	p.finish(0)
	got := drain(sub.ch) // must terminate (channel closed by finish)
	if len(got) != subWindowFrames {
		t.Fatalf("drained %d frames; want %d data frames (end frame silently lost, by design)", len(got), subWindowFrames)
	}
}

// a subscriber attaching after the process ended replays the ring and gets end.
func TestSubscribeAfterEnded(t *testing.T) {
	p := newProc(1, nil)
	p.broadcast(frame{kind: 'd', data: []byte("tail")})
	p.finish(7)
	sub, snap, ended, endF := p.subscribe()
	if !ended || sub != nil {
		t.Fatal("subscribe after finish should report ended with nil sub")
	}
	if len(snap) != 1 || string(snap[0].data) != "tail" {
		t.Fatalf("post-end replay = %v; want [tail]", snap)
	}
	if endF.kind != 'e' || endF.exitCode != 7 {
		t.Fatalf("end frame = %v; want end exit=7", endF)
	}
}

func TestStableProcessHandleSequenceAndGap(t *testing.T) {
	p := newProc(17, nil)
	if p.handle == "" || p.epoch == 0 {
		t.Fatalf("new process handle/epoch = %q/%d", p.handle, p.epoch)
	}
	for i := 0; i < 20; i++ {
		p.broadcast(frame{kind: 'd', data: []byte("x")})
	}
	_, snap, _, _, gap := p.subscribeFrom(0)
	if gap {
		t.Fatal("initial replay reported a gap")
	}
	if len(snap) != 20 || snap[0].sequence != 1 || snap[len(snap)-1].sequence != 20 {
		t.Fatalf("sequences = %+v", snap)
	}
	_, _, _, _, gap = p.subscribeFrom(1)
	if gap {
		t.Fatal("cursor at retained boundary reported a gap")
	}
	// Force the bounded ring to discard old frames.
	p.ring.cap = 4
	for i := 0; i < 10; i++ {
		p.broadcast(frame{kind: 'd', data: []byte("xx")})
	}
	_, _, _, _, gap = p.subscribeFrom(1)
	if !gap {
		t.Fatal("stale output cursor did not report a gap")
	}
}

func TestRingBufferEmptyAfterOversizeFrameStillReportsCursorGap(t *testing.T) {
	r := ringBuffer{cap: 2}
	r.append(frame{kind: 'd', data: []byte("oversize"), sequence: 1})
	if len(r.frames) != 0 || r.droppedBefore != 2 {
		t.Fatalf("ring after oversize frame = frames=%v droppedBefore=%d", r.frames, r.droppedBefore)
	}
	p := newProc(1, nil)
	p.ring = r
	_, _, _, _, gap := p.subscribeFrom(0)
	if !gap {
		t.Fatal("empty ring after dropped output did not report a cursor gap")
	}
}

func TestStableProcessHandleRetainsExitAfterPidUnregister(t *testing.T) {
	s := &processServer{}
	p := newProc(19, nil)
	s.register(p)
	handle, epoch := p.handle, p.epoch
	p.finish(23)
	s.unregister(p.pid)
	got, err := s.resolveProcess(handle, epoch, 0)
	if err != nil || got != p || !got.ended || got.endFrame.exitCode != 23 {
		t.Fatalf("retained handle = %v, %+v", err, got)
	}
}

func TestStableProcessEpochMismatchFailsClosed(t *testing.T) {
	s := &processServer{}
	p := newProc(21, nil)
	s.register(p)
	if _, err := s.resolveProcess(p.handle, p.epoch+1, 0); err == nil {
		t.Fatal("stale process epoch accepted")
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	p := newProc(1, nil)
	p.finish(7)
	seq := p.endFrame.sequence
	p.finish(9)
	if p.endFrame.sequence != seq || p.endFrame.exitCode != 7 {
		t.Fatalf("second finish changed terminal result: %+v", p.endFrame)
	}
}

func TestPersistedRingAndExitSurviveReload(t *testing.T) {
	old := processStatePath
	t.Cleanup(func() { processStatePath = old })
	processStatePath = filepath.Join(t.TempDir(), "processes.json")
	s := &processServer{retention: time.Hour}
	p := newProc(1, nil)
	s.register(p)
	s.mu.Lock()
	s.idempotency["retry"] = p.handle
	s.idempotencySpec["retry"] = "hash"
	s.savePersistentLocked()
	s.mu.Unlock()
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("persisted")})
	p.finish(12)
	s.unregister(p.pid)
	if _, err := os.Stat(processStatePath); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	r := &processServer{retention: time.Hour}
	r.loadPersistent()
	r.mu.Lock()
	got := r.byHandle[p.handle]
	r.mu.Unlock()
	if got == nil || !got.ended || got.endFrame.exitCode != 12 {
		t.Fatalf("reloaded process = %+v", got)
	}
	_, snap, ended, end, gap := got.subscribeFrom(0)
	if gap || !ended || len(snap) != 1 || string(snap[0].data) != "persisted" || end.exitCode != 12 {
		t.Fatalf("reloaded replay = snap=%+v ended=%v end=%+v gap=%v", snap, ended, end, gap)
	}
}

func TestStartSpecHashIncludesPlacementEpoch(t *testing.T) {
	a := startSpecHash(&v1.StartRequest{Cmd: "/bin/true", PlacementEpoch: 1})
	b := startSpecHash(&v1.StartRequest{Cmd: "/bin/true", PlacementEpoch: 2})
	if a == b {
		t.Fatal("start idempotency hash ignored placement epoch")
	}
}

func TestSuppliedHandleClaimRejectsDifferentRequest(t *testing.T) {
	s := &processServer{}
	_, _, owner, err := s.claimStart("", "proc_fixed", "hash-a")
	if err != nil || !owner {
		t.Fatalf("first claim owner=%v err=%v", owner, err)
	}
	if _, _, _, err := s.claimStart("", "proc_fixed", "hash-b"); err == nil {
		t.Fatal("supplied handle was reused with a different request")
	}
	s.finishStartFlight("", "proc_fixed", nil)
}

func TestSuppliedHandleStartFlightSerializesConcurrentRetries(t *testing.T) {
	s := &processServer{}
	_, firstWait, firstOwner, err := s.claimStart("", "proc_fixed", "hash")
	if err != nil || !firstOwner || firstWait == nil {
		t.Fatalf("first supplied-handle claim = owner=%v wait=%v err=%v", firstOwner, firstWait, err)
	}
	_, secondWait, secondOwner, err := s.claimStart("", "proc_fixed", "hash")
	if err != nil || secondOwner || secondWait == nil {
		t.Fatalf("concurrent supplied-handle claim = owner=%v wait=%v err=%v", secondOwner, secondWait, err)
	}
	s.finishStartFlight("", "proc_fixed", nil)
	select {
	case <-secondWait:
	case <-time.After(time.Second):
		t.Fatal("supplied-handle retry was not released after the owner finished")
	}
	_, _, owner, err := s.claimStart("", "proc_fixed", "hash")
	if err != nil || !owner {
		t.Fatalf("post-flight supplied-handle claim = owner=%v err=%v", owner, err)
	}
}

// nonblocking fan-out (source never blocks): a writeInput that blocks in Write (child not
// draining its input) must NOT hold p.mu, so broadcast/subscribe/finish stay live
// meanwhile. Holding p.mu across the write deadlocks this test — broadcast waits on
// the mu the stuck write is holding — which is what makes it a real guard.
func TestWriteInputDoesNotBlockFanOut(t *testing.T) {
	bw := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := newProc(1, bw)
	sub, _, _, _ := p.subscribe()

	// Kick off a write that will block inside bw.Write (input pipe "full").
	writeDone := make(chan error, 1)
	go func() { writeDone <- p.writeInput([]byte("stuck"), false) }()
	<-bw.entered // write is now parked inside Write

	// With the write parked, the fan-out must still flow: broadcast delivers,
	// subscribe works, finish completes. If writeInput held p.mu, all three
	// would block here and the test would hang to -timeout.
	done := make(chan struct{})
	go func() {
		p.broadcast(frame{kind: 'd', data: []byte("live")})
		s2, _, _, _ := p.subscribe()
		if s2 != nil {
			p.unsubscribe(s2)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fan-out blocked while writeInput parked in Write — source blocked, nonblocking fan-out violated")
	}

	// Let the write finish and confirm it returned cleanly.
	close(bw.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("writeInput returned error: %v", err)
	}
	_ = sub
}

// The transport behavior subsystem boundary as pure logic: only pid > 0 is a directed target.
// The substrate proof that a refused pid cannot destroy a real sandbox is in
// test/transport_depth_substrate_test.go; this pins the predicate itself,
// including that the message says WHERE fan-out belongs — a caller that only
// learns "invalid" keeps hunting for a transport-side way to do it.
func TestValidateSignalPid(t *testing.T) {
	for _, pid := range []int{-2147483648, -1, 0} {
		err := validateSignalPid(pid)
		if err == nil {
			t.Errorf("validateSignalPid(%d) = nil, want refusal (pid <= 0 is POSIX fan-out)", pid)
			continue
		}
		if !strings.Contains(err.Error(), "PID 1") {
			t.Errorf("validateSignalPid(%d) message does not say fan-out belongs to PID 1: %v", pid, err)
		}
	}
	for _, pid := range []int{1, 2, 12345} {
		if err := validateSignalPid(pid); err != nil {
			t.Errorf("validateSignalPid(%d) = %v, want nil (a directed target)", pid, err)
		}
	}
}

// failingSender fails the Nth Send (1 = the very first, the StartEvent). It is the
// injectable half of the eventSender seam: connect's *ServerStream has no exported
// constructor, so without it the Start first-frame failure arm is unreachable from
// a unit test — and that arm is exactly where the leak below lived.
type failingSender struct {
	failAt int
	n      int
	sent   []*v1.ProcessEvent
}

func (f *failingSender) Send(e *v1.ProcessEvent) error {
	f.n++
	if f.n == f.failAt {
		return errString("injected send failure")
	}
	f.sent = append(f.sent, e)
	return nil
}

// A first-frame Send failure must not skip `go p.run(...)`, because run's
// onExit is unregister's ONLY call site. Three leaks follow if it does, worst first: the
// pumps never start, so the child blocks forever writing into a 64 KiB pipe nobody
// reads — it cannot even exit; the s.procs entry stays for the daemon's life; and
// three fds stay open (cmd.Wait is deliberately skipped, so pumpSource's
// defer closer.Close() is what closes them).
//
// Drives the real handler through a real fork + real reaper. The child writes far
// more than a pipe buffer, so "the pumps ran" and "the child could exit" are the
// same observation: it can only finish if someone drained it.
func TestStartSendFailureDoesNotLeakProcOrWedgeChild(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	startDraining(t, r)
	s := &processServer{activity: &activityClock{}, reaper: r}

	fs := &failingSender{failAt: 1} // fail the StartEvent itself
	err := s.start(context.Background(), connect.NewRequest(&v1.StartRequest{
		Cmd: "/bin/sh",
		// >>64 KiB on stdout: only a running pump lets this child ever exit.
		Args: []string{"-c", "i=0; while [ $i -lt 400 ]; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; i=$((i+1)); done"},
	}), fs)
	if err == nil {
		t.Fatal("start returned nil; want the injected send failure")
	}
	if !strings.Contains(err.Error(), "injected send failure") {
		t.Fatalf("start error = %v; want the injected send failure", err)
	}

	// Leak 2: the registry entry must drain, which can only happen via run's onExit.
	waitFor(t, "s.procs to drop the pid after a first-frame Send failure", func() bool {
		s.mu.Lock()
		n := len(s.procs)
		s.mu.Unlock()
		return n == 0
	})

	// Leak 1 (the worst): the child had to be drained enough to EXIT and be reaped.
	// A wedged child sits in /proc forever with a full pipe.
	s.mu.Lock()
	if len(s.procs) != 0 {
		t.Fatalf("procs still holds %d entries", len(s.procs))
	}
	s.mu.Unlock()
}

// Same seam, the OTHER side of the boundary: a process that was started must not be
// killed because a client went away. the sandbox process model gives Start an independent context so the
// process outlives a disconnect (the premise of Connect reattach), and "kill on
// first-frame failure" cannot be a contract — the same disconnect one frame later
// leaves it alive, so special-casing the first frame would make dead-vs-alive turn
// on invisible timing. So: still live and still reattachable right after the failure.
func TestStartSendFailureLeavesProcessRunning(t *testing.T) {
	asSubreaper(t)
	r := newReapTable()
	startDraining(t, r)
	s := &processServer{activity: &activityClock{}, reaper: r}

	fs := &failingSender{failAt: 1}
	err := s.start(context.Background(), connect.NewRequest(&v1.StartRequest{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "sleep 30"},
	}), fs)
	if err == nil {
		t.Fatal("start returned nil; want the injected send failure")
	}

	s.mu.Lock()
	var pid int
	for p := range s.procs {
		pid = p
	}
	s.mu.Unlock()
	if pid == 0 {
		t.Fatal("no proc registered; the process should still be tracked while it runs")
	}
	if _, _, ok := procState(pid); !ok {
		t.Fatalf("pid %d is gone; a first-frame Send failure must not kill the process", pid)
	}
	// Reattachable: the whole point of not killing it.
	if _, ok := s.get(pid); !ok {
		t.Fatalf("pid %d not resolvable for Connect", pid)
	}
	// dash forks a grandchild for `sleep`, so kill the GROUP — a pid-directed kill
	// leaves the grandchild holding the pipes and run() never returns.
	_ = killProcessGroup(pid, int(syscall.SIGKILL))
	waitFor(t, "the killed process to unregister", func() bool {
		_, ok := s.get(pid)
		return !ok
	})
}
