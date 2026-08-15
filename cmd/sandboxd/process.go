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

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// processServer implements the the documented invariant process primitive (transport the documented invariant). It fans one process's
// output out to N subscribers (Start + any Connect), each getting an independent copy, and
// keeps a per-process bounded output ring buffer so a late Connect replays recent bytes before
// joining live (the documented invariant invariant 2).
//
// procs maps pid→proc: transport-process-local, non-authoritative, dying with the transport
// process — NOT a control-plane registry (the documented invariant: List scans /proc live). the documented invariant places the ring
// buffer inside "no *persistent* SoT" — transient, per-process, never crossing resume.
type processServer struct {
	mu    sync.Mutex
	procs map[int]*proc
	// activity is the sandbox-global last-I/O clock (the documented invariant activity signal,
	// reaper-health the documented invariant). Shared with the healthServer so Health.Check reports
	// idle duration. Every proc created here carries a reference to it and
	// stamps it on broadcast/writeInput.
	activity *activityClock
	// reaper is transport mode's single wildcard waiter (reap.go). REQUIRED — it is where
	// Start forks children and where their exit statuses come from (see proc.run for why
	// cmd.Wait() cannot be used). Set by transportMode; a nil here is a programming error.
	reaper *reapTable
}

// Tuning values. Pure numeric knobs — changing one does NOT change the the documented invariant/the documented invariant shape, and
// this is their only home. Sized so the ring holds enough tail to redraw an interactive
// screen without unbounded guest memory, and the window is deep enough for a healthy
// consumer but small enough that a stuck one is dropped promptly.
const (
	ringCapBytes      = 64 * 1024        // per-process output ring byte cap (invariant 2)
	subWindowFrames   = 256              // per-subscriber send window (invariant 1)
	keepaliveInterval = 20 * time.Second // long-stream liveness (the documented invariant)
)

func (s *processServer) register(p *proc) {
	s.mu.Lock()
	if s.procs == nil {
		s.procs = make(map[int]*proc)
	}
	p.activity = s.activity // share the sandbox-global I/O clock (reaper-health the documented invariant)
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

	// activity is the shared sandbox-global last-I/O clock (reaper-health the documented invariant),
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
// bytes silently (the documented invariant invariant 2: "oldest bytes are dropped silently"). Frames
// keep their stream
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
		// Stamp sandbox-global activity on OUTPUT (reaper-health the documented invariant), here in the path EVERY
		// output frame flows through, so a detached process producing output with no client
		// attached still counts as active (the documented invariant / reaper-health the documented invariant). Off p.mu — the atomic
		// store needs no lock.
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
// Lossy-by-design boundary (the documented invariant invariant 1 / the documented invariant): a subscriber whose window is full at
// exit loses the end frame — exit code included — for THAT copy; the channel still closes
// and serveSubscriber returns a clean end. The broadcast path's rule, extended to the exit
// instant.
//
// "You were dropped" IS observable end-to-end: serveSubscriber returns resource_exhausted on
// an overflow close, which the host surfaces as ProcessEvent{Kind:"error"}, so a consumer can
// tell it from "the process finished" — otherwise both look like a channel closing.
//
// Still NOT a health signal: the documented invariant classifies a sandbox from the L2 Health probe and its
// guest-side activity clock, never from one subscriber's copy being too slow.
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
// The blocking Write MUST happen OUTSIDE p.mu (the documented invariant invariant 1: the source never blocks).
// Holding mu across a Write that blocks on a full pipe (child busy writing stdout, not
// reading stdin) deadlocks pumpSource→broadcast on mu while the child waits for its stdout to
// drain. So: snapshot the writer handle under mu, release, write unlocked — only the handle
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
	// Stamp activity on INPUT (reaper-health the documented invariant): a keystroke is I/O even if the process
	// produces no output. BEFORE the possibly-blocking Write — the input arriving IS the
	// activity, regardless of when the child drains it.
	p.activity.stamp()
	_, err := w.Write(data)
	return err
}

// resize applies a new PTY size (transport the documented invariant Update). Errors if no pty.
//
// TIOCSWINSZ is non-blocking so it could not deadlock the fan-out, but it follows
// writeInput's snapshot-then-act-unlocked shape uniformly, so no I/O on the pty handle is
// ever done under p.mu (the documented invariant invariant 1).
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
// Like writeInput, the pty ^D Write is done outside p.mu (the documented invariant invariant 1). The stdin path
// snapshots and clears the handle before releasing, so a racing writeInput sees stdin==nil.
func (p *proc) closeStdin() error {
	p.mu.Lock()
	if p.pty != nil {
		pty := p.pty
		p.mu.Unlock()
		// The ^D keystroke is INPUT I/O — stamped like writeInput (reaper-health the documented invariant), so a
		// sandbox that was just interacted with cannot look idle.
		p.activity.stamp()
		_, err := pty.Write([]byte{0x04}) // ^D
		return err
	}
	if p.stdin != nil {
		w := p.stdin
		p.stdin = nil
		p.mu.Unlock()
		p.activity.stamp() // stdin close is input-side I/O too (reaper-health the documented invariant)
		return w.Close()
	}
	p.mu.Unlock()
	return nil
}

// source is one output reader tagged with its stream name.
//
// closer is the parent end to close once this source hits EOF, or nil when something else owns
// it. It exists because the reaper — not cmd.Wait() — waits our children (see proc.run), and
// cmd.Wait is also what closes the StdoutPipe/StderrPipe parent ends: skipping it means
// closing them here or leaking two fds per process. The pty master is nil because it is both
// output source and input sink, so finish() owns closing it.
type source struct {
	r      io.Reader
	name   string
	closer io.Closer
}

// run pumps each source into the fan-out until EOF, waits for exit, then
// finishes the hub and unregisters. Non-pty has two sources (stdout/stderr);
// pty has one ("pty" — the master merges both). The process is launched with an
// INDEPENDENT context (not the Start stream's) so it outlives a Start
// disconnect — that is what makes Connect reattach possible (the documented invariant).
//
// finish-then-unregister is the exit-status delivery boundary, and it is deliberately sharp:
// finish broadcasts `end` to whoever is attached now, then the pid stops resolving, so a later
// Connect gets not-found rather than a retained exit code. Holding the entry would retain
// state keyed by a guest pid the kernel can recycle, so the record would start describing a
// different process once the number is reused. Callers that must not miss exit status stay
// attached so the parent can reap the process group.
//
// The exit status arrives over `exited` — the reap table's handoff — NOT from cmd.Wait().
// Transport mode owns Wait4(-1) for its subtree (the documented invariant) and a wildcard wait cannot exclude a
// pid, so a per-child cmd.Wait() would race the reaper: whoever lost sees ECHILD and this
// `end` frame carries a fabricated exit code. Same single-waiter rule as PID 1's (reap.go).
func (p *proc) run(exited <-chan syscall.WaitStatus, sources []source, onExit func()) {
	var wg sync.WaitGroup
	wg.Add(len(sources))
	for _, s := range sources {
		go p.pumpSource(&wg, s)
	}
	wg.Wait()
	// "Wrote its last byte" and "the kernel published a wait status" are different instants,
	// so the status is awaited rather than assumed. A grandchild holding the pipes open past
	// the child's exit is the reverse skew and equally fine.
	p.finish(exitCode(<-exited))
	onExit()
}

func (p *proc) pumpSource(wg *sync.WaitGroup, s source) {
	defer wg.Done()
	if s.closer != nil {
		// cmd.Wait() is what closes this; the reaper owns the wait, so we do.
		defer s.closer.Close()
	}
	br := bufio.NewReader(s.r)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			p.broadcast(frame{kind: 'd', stream: s.name, data: append([]byte(nil), buf[:n]...)})
		}
		if rerr != nil {
			return
		}
	}
}

// eventSender is the one thing Start/Connect do with their stream: send events. Narrowing to
// it makes the first-frame send FAILURE injectable — connect's *ServerStream has no exported
// constructor, so with the concrete type the leak-on-Send-failure path below is unreachable
// from a root-free unit test.
//
// A plain in-package interface, NOT a build tag: nothing here is compiled-out fault injection
// in the the documented invariant `testguest` sense (that tag is for a seam that must be ABSENT from a
// production sandboxd), and *connect.ServerStream[v1.ProcessEvent] satisfies this already.
type eventSender interface {
	Send(*v1.ProcessEvent) error
}

// Start launches a process and server-streams its output events.
func (s *processServer) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	return s.start(ctx, req, stream)
}

func (s *processServer) start(ctx context.Context, req *connect.Request[v1.StartRequest], stream eventSender) error {
	m := req.Msg
	if m.Cmd == "" {
		return connect.NewError(connect.CodeInvalidArgument, errString("empty cmd"))
	}

	// Independent context: the process must outlive this Start stream so a later Connect can
	// reattach (the documented invariant). Signals/exit end it, not a client disconnect.
	//
	// Processes run as guest root (the documented invariant: guest identity is not a host-isolation pillar —
	// sentry virtualizes guest uid 0). "Run as a different user" is a guest-native concern
	// (su/sudo), not an engine primitive (the documented invariant).
	cmd := exec.Command(m.Cmd, m.Args...)
	cmd.Env = m.Env
	cmd.Dir = m.Cwd

	// Every fork goes through the reap table: it registers the waiter under the
	// same lock as the fork, so the reaper cannot collect this child before we
	// are entitled to its status (the documented invariant single waiter — see reapTable.start).
	// It also Releases the os.Process handle, so cmd.Process.Pid is cleared and
	// the returned pid is the only handle; SendSignal targets it by number.
	var p *proc
	var sources []source
	var exited <-chan syscall.WaitStatus
	if m.Pty != nil {
		// PTY branch (transport the documented invariant): pure-Go pty pair. The slave is the
		// child's controlling terminal; the master is the single merged output
		// source AND the keyboard-input sink. Setsid makes the child a session
		// leader; Setctty binds the slave (fd 0) as its controlling tty.
		master, slave, err := openPTY(uint16(m.Pty.Rows), uint16(m.Pty.Cols))
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
		pid, ch, err := s.reaper.start(cmd)
		if err != nil {
			master.Close()
			slave.Close()
			return connect.NewError(connect.CodeInternal, err)
		}
		_ = slave.Close() // parent keeps only the master
		p = newProc(pid, master)
		p.pty = master
		// closer nil: the master is also the input sink, so finish() closes it.
		sources = []source{{r: master, name: "pty"}}
		exited = ch
	} else {
		// Own pgid so SendSignal can target the whole tree (transport the documented invariant).
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
		pid, ch, err := s.reaper.start(cmd)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		p = newProc(pid, stdin)
		// These two parent ends are exactly what cmd.Wait() closes; the reaper owns
		// the wait instead, so each pump closes its own on EOF. stdin's parent end is
		// closed by finish.
		sources = []source{
			{r: stdout, name: "stdout", closer: stdout},
			{r: stderr, name: "stderr", closer: stderr},
		}
		exited = ch
	}
	pid := p.pid
	s.register(p)

	// Subscribe BEFORE the source pumps start so Start sees everything live
	// (ring replay is empty for a fresh Start).
	sub, snap, ended, endF := p.subscribe()

	// UNCONDITIONALLY before the first Send, because run() is the only thing that drains this
	// child, closes its pipe parent ends, and calls unregister. Gated behind a successful Send
	// (as it was), a first-frame failure left the pumps unstarted, so the child filled the
	// 64 KiB pipe buffer on an unread stdout and blocked THERE forever, unable even to exit —
	// plus a permanent s.procs entry and three unclosed fds. Ordering is safe: subscribe()
	// above already registered this subscriber, so no frame is missed.
	//
	// The already-forked process is left to run and be reaped, NOT killed. the documented invariant gives Start an
	// independent context precisely so a process outlives a client disconnect; "kill on
	// first-frame failure" cannot be a contract, because the same disconnect one frame later
	// leaves the process alive — dead-vs-alive would depend on invisible timing.
	go p.run(exited, sources, func() { s.unregister(pid) })

	if err := stream.Send(&v1.ProcessEvent{
		Kind: &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(pid)}},
	}); err != nil {
		if sub != nil {
			p.unsubscribe(sub)
		}
		return err
	}

	if sub != nil {
		defer p.unsubscribe(sub)
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF)
}

// Connect reattaches to a running process: replay the bounded ring, then live
// (the documented invariant invariant 2). Independent per-subscriber copy (the documented invariant fan-out).
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
// idle (the documented invariant — no idle deadline on the long output stream).
func serveSubscriber(ctx context.Context, stream eventSender, sub *subscriber, snap []frame, ended bool, endF frame) error {
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

func sendFrame(stream eventSender, f frame) error {
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
// single stream's (transport the documented invariant). engine does NOT arbitrate multiple writers
// (the documented invariant: bytes interleave, Unix semantics — that is the product layer's call).
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

// Update resizes a process's PTY (transport the documented invariant). No-op-erroring on a non-pty
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

// SendSignal delivers a DIRECTED signal — one process target, defaulting to its
// group (transport the documented invariant / the documented invariant).
//
// It validates `pid > 0` and nothing more — NO registry membership, unlike
// Connect/SendInput/Update/CloseStdin, which touch per-process state. Demanding a registry hit
// would break the the documented invariant List-then-signal pairing, since List scans /proc live and returns pids
// that were never in s.procs.
//
// The check is HERE (guest side) because the documented invariant's premise is that the guest can write its own
// fs and must therefore validate for itself; the orchestrator's copy is only fail-fast.
func (s *processServer) SendSignal(_ context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	m := req.Msg
	if err := validateSignalPid(int(m.Pid)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString(err.Error()))
	}
	if err := killProcessGroup(int(m.Pid), int(m.Signal)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SendSignalResponse{}), nil
}

// List scans /proc for live pids (no registry, the documented invariant).
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
