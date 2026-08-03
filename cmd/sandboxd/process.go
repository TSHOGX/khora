package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"

	v1 "engine/internal/transport/gen/engine/transport/v1"
)

// processServer implements the §5.E process primitive (transport D4.3). It fans
// one process's output out to N subscribers (Start + any Connect), each getting
// an independent copy, and keeps a per-process bounded output ring buffer so a
// late Connect replays recent bytes before joining live (D4.3 invariant 2).
//
// procs maps pid→proc. It is transport-process-local, non-authoritative, and
// dies with the transport process — NOT a control-plane
// registry (§5.E: List scans /proc live). D4.3 explicitly places the ring buffer
// inside "no *persistent* SoT": transient, per-process, gone when the process
// (or transport) dies, never crossing resume.
type processServer struct {
	mu    sync.Mutex
	procs map[int]*proc
	// activity is the sandbox-global last-I/O clock (§4.3 activity signal,
	// reaper-health D2). Shared with the healthServer so Health.Check reports
	// idle duration. Every proc created here carries a reference to it and
	// stamps it on broadcast/writeInput.
	activity *activityClock
}

// Residual tuning values (open-decisions §5.E — numeric knobs, do NOT change
// D4/D6 shape). Sized so: the ring holds enough tail to redraw an interactive
// screen without unbounded guest memory; the window is deep enough for a healthy
// consumer but small enough that a stuck one is dropped promptly.
const (
	ringCapBytes      = 64 * 1024        // per-process output ring byte cap (invariant 2)
	subWindowFrames   = 256              // per-subscriber send window (invariant 1)
	keepaliveInterval = 20 * time.Second // long-stream liveness (D4.2)
)

func (s *processServer) register(p *proc) {
	s.mu.Lock()
	if s.procs == nil {
		s.procs = make(map[int]*proc)
	}
	p.activity = s.activity // share the sandbox-global I/O clock (reaper-health D2)
	s.procs[p.pid] = p
	s.mu.Unlock()
}

func (s *processServer) get(pid int) (*proc, bool) {
	s.mu.Lock()
	p, ok := s.procs[pid]
	s.mu.Unlock()
	return p, ok
}

func (s *processServer) unregister(pid int) {
	s.mu.Lock()
	delete(s.procs, pid)
	s.mu.Unlock()
}

// frame is one broadcast item: a data chunk ('d') or the terminal exit ('e').
type frame struct {
	kind     byte // 'd' data, 'e' end
	stream   string
	data     []byte
	exitCode int
}

// subscriber is one attached output consumer. ch is its bounded send window
// (invariant 1): broadcast never blocks on it; on overflow the subscriber is
// dropped (removed from the fan-out, ch closed), backpressure never reaching
// the source. overflow distinguishes a dropped subscriber from a clean end.
type subscriber struct {
	ch       chan frame
	overflow atomic.Bool
}

// proc is the per-process fan-out hub (transient, non-authoritative — see
// processServer doc). Holds the bounded ring, the live subscriber set, and the
// input handles (stdin pipe; pty master).
type proc struct {
	pid int

	// activity is the shared sandbox-global last-I/O clock (reaper-health D2),
	// set by processServer.register. Stamped on every output (broadcast) and
	// input (writeInput). nil when a proc is built outside the server (unit
	// tests) — activityClock methods are nil-safe.
	activity *activityClock

	mu       sync.Mutex
	ring     ringBuffer
	subs     map[*subscriber]struct{}
	stdin    io.WriteCloser
	pty      *os.File // set only for pty processes
	ended    bool
	endFrame frame
}

func newProc(pid int, stdin io.WriteCloser) *proc {
	return &proc{
		pid:   pid,
		ring:  ringBuffer{cap: ringCapBytes},
		subs:  make(map[*subscriber]struct{}),
		stdin: stdin,
	}
}

// ringBuffer holds recent output frames up to a byte cap, dropping the oldest
// bytes silently (D4.3 invariant 2: "旧字节静默丢弃"). Frames keep their stream
// tag so replay preserves stdout/stderr/pty attribution.
type ringBuffer struct {
	cap    int
	bytes  int
	frames []frame
}

func (r *ringBuffer) append(f frame) {
	r.frames = append(r.frames, f)
	r.bytes += len(f.data)
	for r.bytes > r.cap && len(r.frames) > 0 {
		r.bytes -= len(r.frames[0].data)
		r.frames[0] = frame{} // release the byte slice for GC before resliding
		r.frames = r.frames[1:]
	}
}

func (r *ringBuffer) snapshot() []frame {
	return append([]frame(nil), r.frames...)
}

// broadcast appends a data frame to the ring and non-blockingly delivers it to
// every live subscriber. A subscriber whose window is full is dropped
// (invariant 1) — the source NEVER blocks here.
func (p *proc) broadcast(f frame) {
	if f.kind == 'd' {
		// Stamp sandbox-global activity on OUTPUT (reaper-health D2). Done here,
		// in the path EVERY output frame flows through, so a detached process
		// producing output with no client attached still counts as active
		// (§3.2 / reaper-health D1 — the whole reason the signal is guest-side).
		// Off p.mu: the atomic store needs no lock, and keeping it out avoids any
		// question of holding the fan-out mutex a beat longer.
		p.activity.stamp()
	}
	p.mu.Lock()
	if f.kind == 'd' {
		p.ring.append(f)
	}
	for sub := range p.subs {
		select {
		case sub.ch <- f:
		default:
			sub.overflow.Store(true)
			delete(p.subs, sub)
			close(sub.ch)
		}
	}
	p.mu.Unlock()
}

// finish marks the process ended, delivers the end frame to remaining
// subscribers, and closes their channels. A subscriber that Connects after this
// sees ended=true from subscribe and replays the ring + end directly.
//
// Lossy-by-design boundary (D4.3 invariant 1 / D1.3): a per-subscriber copy
// is best-effort. If a subscriber's window is full at exit (too slow to receive
// even the end frame), that end frame — exit code included — is silently dropped
// for THAT copy; the channel still closes and serveSubscriber returns a clean
// end. This is the same "lossy is the slow subscriber's copy, not the source"
// rule the broadcast path enforces, extended to the exit instant.
//
// NOTE: "you were dropped" is not observable end-to-end anyway — the domain
// seam (client.go drainEvents ignores stream.Err(); ProcessEvent has no error
// field) cannot carry a terminal error across the fan-out channel. Subscriber
// drop is deliberately NOT a health signal: §4.3 classifies a sandbox from the
// L2 Health probe and its guest-side activity clock, never from one
// subscriber's copy being too slow.
func (p *proc) finish(exitCode int) {
	p.mu.Lock()
	p.ended = true
	p.endFrame = frame{kind: 'e', exitCode: exitCode}
	for sub := range p.subs {
		select {
		case sub.ch <- p.endFrame:
		default:
		}
		delete(p.subs, sub)
		close(sub.ch)
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	if p.pty != nil {
		_ = p.pty.Close()
		p.pty = nil
	}
	p.mu.Unlock()
}

// subscribe atomically snapshots the ring and (if the process is still live)
// registers a new subscriber, so no frame is duplicated or missed at the
// replay→live seam. Returns sub==nil when the process has already ended.
func (p *proc) subscribe() (sub *subscriber, snap []frame, ended bool, endFrame frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap = p.ring.snapshot()
	if p.ended {
		return nil, snap, true, p.endFrame
	}
	sub = &subscriber{ch: make(chan frame, subWindowFrames)}
	p.subs[sub] = struct{}{}
	return sub, snap, false, frame{}
}

// unsubscribe removes a subscriber (idempotent with the overflow/finish paths).
func (p *proc) unsubscribe(sub *subscriber) {
	p.mu.Lock()
	if _, ok := p.subs[sub]; ok {
		delete(p.subs, sub)
		close(sub.ch)
	}
	p.mu.Unlock()
}

// writeInput writes to the process's pty master (ptyIn) or stdin pipe.
//
// The blocking Write MUST happen OUTSIDE p.mu (D4.3 invariant 1: the source
// never blocks). If we held mu across a Write that blocks on a full pipe (child
// busy writing stdout, not reading stdin), pumpSource→broadcast would deadlock
// waiting for mu while the child deadlocks waiting for its stdout to drain. So
// snapshot the writer handle under mu, release, then write unlocked. io.Writer
// is safe to use concurrently with the mu-guarded fan-out; only the handle
// pointer needs lock protection.
func (p *proc) writeInput(data []byte, ptyIn bool) error {
	p.mu.Lock()
	var w io.Writer
	if ptyIn {
		if p.pty == nil {
			p.mu.Unlock()
			return errString("process has no pty")
		}
		w = p.pty
	} else {
		if p.stdin == nil {
			p.mu.Unlock()
			return errString("stdin closed")
		}
		w = p.stdin
	}
	p.mu.Unlock()
	// Stamp sandbox-global activity on INPUT (reaper-health D2): a keystroke /
	// stdin write is I/O even if the process produces no output in response.
	// Stamped before the (possibly blocking) Write — the input arriving IS the
	// activity, regardless of when the child drains it.
	p.activity.stamp()
	_, err := w.Write(data)
	return err
}

// resize applies a new PTY size (transport D4.3 Update). Errors if no pty.
//
// The TIOCSWINSZ ioctl is non-blocking so it wouldn't deadlock the fan-out, but
// we follow writeInput's snapshot-then-act-unlocked shape uniformly so no I/O on
// the pty handle is ever done under p.mu (D4.3 invariant 1).
func (p *proc) resize(rows, cols uint16) error {
	p.mu.Lock()
	if p.pty == nil {
		p.mu.Unlock()
		return errString("process has no pty")
	}
	pty := p.pty
	p.mu.Unlock()
	return setWinsize(pty, rows, cols)
}

// closeStdin closes the stdin pipe (non-pty) or writes VEOF (pty Ctrl-D). For a
// pty the master stays open (still the output source) — only EOF is injected.
//
// Like writeInput, the pty ^D Write is done outside p.mu so it can't block the
// fan-out (D4.3 invariant 1). The stdin Close path nils the handle, so it stays
// under the lock (a pointer mutation, not a blocking op) — but snapshots and
// clears before releasing so a racing writeInput sees stdin==nil.
func (p *proc) closeStdin() error {
	p.mu.Lock()
	if p.pty != nil {
		pty := p.pty
		p.mu.Unlock()
		// The ^D keystroke is INPUT I/O — stamp it like writeInput (reaper-health
		// D2), so closing stdin counts as activity and can't leave a sandbox that
		// was just interacted with looking idle.
		p.activity.stamp()
		_, err := pty.Write([]byte{0x04}) // ^D
		return err
	}
	if p.stdin != nil {
		w := p.stdin
		p.stdin = nil
		p.mu.Unlock()
		p.activity.stamp() // stdin close is input-side I/O too (reaper-health D2)
		return w.Close()
	}
	p.mu.Unlock()
	return nil
}

// source is one output reader tagged with its stream name.
type source struct {
	r    io.Reader
	name string
}

// run pumps each source into the fan-out until EOF, waits for exit, then
// finishes the hub and unregisters. Non-pty has two sources (stdout/stderr);
// pty has one ("pty" — the master merges both). The process is launched with an
// INDEPENDENT context (not the Start stream's) so it outlives a Start
// disconnect — that is what makes Connect reattach possible (§3.2).
func (p *proc) run(cmd *exec.Cmd, sources []source, onExit func()) {
	var wg sync.WaitGroup
	wg.Add(len(sources))
	for _, s := range sources {
		go p.pumpSource(&wg, s.r, s.name)
	}
	wg.Wait()
	p.finish(waitExit(cmd))
	onExit()
}

func (p *proc) pumpSource(wg *sync.WaitGroup, r io.Reader, name string) {
	defer wg.Done()
	br := bufio.NewReader(r)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			p.broadcast(frame{kind: 'd', stream: name, data: append([]byte(nil), buf[:n]...)})
		}
		if rerr != nil {
			return
		}
	}
}

// waitExit maps cmd.Wait into an exit code (128+signo on signal, convention).
func waitExit(cmd *exec.Cmd) int {
	werr := cmd.Wait()
	if werr == nil {
		return 0
	}
	if ee, ok := werr.(*exec.ExitError); ok {
		code := ee.ExitCode()
		if code < 0 {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
		}
		return code
	}
	return -1
}

// Start launches a process and server-streams its output events.
func (s *processServer) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	m := req.Msg
	if m.Cmd == "" {
		return connect.NewError(connect.CodeInvalidArgument, errString("empty cmd"))
	}

	// Independent context: the process must outlive this Start stream so a later
	// Connect can reattach (§3.2). Signals/exit end it, not a client disconnect.
	//
	// Processes run as guest root — the engine defaults to root and does not model
	// guest users (§2.2: guest identity is not a host-isolation pillar; sentry
	// virtualizes guest uid 0, so guest-root costs nothing on the isolation axis).
	// "Run as a different user" is a guest-native concern (su/sudo), not an engine
	// primitive (§3.2.1).
	cmd := exec.Command(m.Cmd, m.Args...)
	cmd.Env = m.Env
	cmd.Dir = m.Cwd

	var p *proc
	var sources []source
	if m.Pty != nil {
		// PTY branch (transport D4.3): pure-Go pty pair. The slave is the
		// child's controlling terminal; the master is the single merged output
		// source AND the keyboard-input sink. Setsid makes the child a session
		// leader; Setctty binds the slave (fd 0) as its controlling tty.
		master, slave, err := openPTY(uint16(m.Pty.Rows), uint16(m.Pty.Cols))
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
		if err := cmd.Start(); err != nil {
			master.Close()
			slave.Close()
			return connect.NewError(connect.CodeInternal, err)
		}
		_ = slave.Close() // parent keeps only the master
		p = newProc(cmd.Process.Pid, master)
		p.pty = master
		sources = []source{{r: master, name: "pty"}}
	} else {
		// Own pgid so SendSignal can target the whole tree (transport D4.3).
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if err := cmd.Start(); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		p = newProc(cmd.Process.Pid, stdin)
		sources = []source{{r: stdout, name: "stdout"}, {r: stderr, name: "stderr"}}
	}
	pid := p.pid
	s.register(p)

	// Subscribe BEFORE the source pumps start so Start sees everything live
	// (ring replay is empty for a fresh Start).
	sub, snap, ended, endF := p.subscribe()
	if err := stream.Send(&v1.ProcessEvent{
		Kind: &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(pid)}},
	}); err != nil {
		if sub != nil {
			p.unsubscribe(sub)
		}
		return err
	}
	go p.run(cmd, sources, func() { s.unregister(pid) })

	if sub != nil {
		defer p.unsubscribe(sub)
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF)
}

// Connect reattaches to a running process: replay the bounded ring, then live
// (D4.3 invariant 2). Independent per-subscriber copy (§3.2 fan-out).
func (s *processServer) Connect(ctx context.Context, req *connect.Request[v1.ConnectRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	p, ok := s.get(int(req.Msg.Pid))
	if !ok {
		return errNoSuchPid(int(req.Msg.Pid))
	}
	sub, snap, ended, endF := p.subscribe()
	if sub != nil {
		defer p.unsubscribe(sub)
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF)
}

// serveSubscriber replays the ring snapshot, then either sends the end frame (if
// the process already ended) or pumps the live window, emitting keepalives on
// idle (D4.2 — no idle deadline on the long output stream).
func serveSubscriber(ctx context.Context, stream *connect.ServerStream[v1.ProcessEvent], sub *subscriber, snap []frame, ended bool, endF frame) error {
	for _, f := range snap {
		if err := sendFrame(stream, f); err != nil {
			return err
		}
	}
	if ended || sub == nil {
		return sendFrame(stream, endF)
	}
	ka := time.NewTicker(keepaliveInterval)
	defer ka.Stop()
	for {
		select {
		case f, ok := <-sub.ch:
			if !ok {
				if sub.overflow.Load() {
					return connect.NewError(connect.CodeResourceExhausted, errString("subscriber send window overflow — dropped"))
				}
				return nil // clean end: end frame delivered, then channel closed
			}
			if err := sendFrame(stream, f); err != nil {
				return err
			}
		case <-ka.C:
			if err := stream.Send(&v1.ProcessEvent{
				Kind: &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}},
			}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func sendFrame(stream *connect.ServerStream[v1.ProcessEvent], f frame) error {
	switch f.kind {
	case 'd':
		return stream.Send(&v1.ProcessEvent{
			Kind: &v1.ProcessEvent_Data{Data: &v1.DataEvent{Stream: f.stream, Data: f.data}},
		})
	case 'e':
		return stream.Send(&v1.ProcessEvent{
			Kind: &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(f.exitCode)}},
		})
	}
	return nil
}

// StreamInput writes an ordered client-stream of input frames to one process
// (keystrokes to a pty). pid is taken from the first frame; ordering is the
// single stream's (transport D4.3). engine does NOT arbitrate multiple writers
// (D4.3: bytes interleave, Unix semantics — that is the product layer's call).
func (s *processServer) StreamInput(_ context.Context, stream *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	var p *proc
	for stream.Receive() {
		m := stream.Msg()
		if p == nil {
			var ok bool
			if p, ok = s.get(int(m.Pid)); !ok {
				return nil, errNoSuchPid(int(m.Pid))
			}
		}
		if err := p.writeInput(m.Data, m.PtyIn); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StreamInputResponse{}), nil
}

// Update resizes a process's PTY (transport D4.3). No-op-erroring on a non-pty
// process (nothing to resize).
func (s *processServer) Update(_ context.Context, req *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	m := req.Msg
	p, ok := s.get(int(m.Pid))
	if !ok {
		return nil, errNoSuchPid(int(m.Pid))
	}
	if m.Pty == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("Update requires pty size"))
	}
	if err := p.resize(uint16(m.Pty.Rows), uint16(m.Pty.Cols)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.UpdateResponse{}), nil
}

// CloseStdin closes stdin (non-pty) or sends EOF (pty, Ctrl-D equivalent).
func (s *processServer) CloseStdin(_ context.Context, req *connect.Request[v1.CloseStdinRequest]) (*connect.Response[v1.CloseStdinResponse], error) {
	p, ok := s.get(int(req.Msg.Pid))
	if !ok {
		return nil, errNoSuchPid(int(req.Msg.Pid))
	}
	if err := p.closeStdin(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.CloseStdinResponse{}), nil
}

func (s *processServer) SendInput(_ context.Context, req *connect.Request[v1.SendInputRequest]) (*connect.Response[v1.SendInputResponse], error) {
	m := req.Msg
	p, ok := s.get(int(m.Pid))
	if !ok {
		return nil, errNoSuchPid(int(m.Pid))
	}
	if err := p.writeInput(m.Data, m.PtyIn); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SendInputResponse{}), nil
}

func (s *processServer) SendSignal(_ context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	m := req.Msg
	if err := killProcessGroup(int(m.Pid), int(m.Signal)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SendSignalResponse{}), nil
}

// List scans /proc for live pids (no registry, §5.E).
func (*processServer) List(_ context.Context, _ *connect.Request[v1.ListRequest]) (*connect.Response[v1.ListResponse], error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var pids []int32
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(e.Name())); err == nil {
			pids = append(pids, int32(pid))
		}
	}
	return connect.NewResponse(&v1.ListResponse{Pids: pids}), nil
}

type errString string

func (e errString) Error() string { return string(e) }

// errNoSuchPid is the NotFound returned when a pid has no live process (List
// scans /proc, so a stale pid simply isn't there).
func errNoSuchPid(pid int) error {
	return connect.NewError(connect.CodeNotFound, errString("no such live process pid "+strconv.Itoa(pid)))
}
