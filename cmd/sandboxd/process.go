package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// processServer implements the transport data plane process primitive (process transport). It fans one process's
// output out to N subscribers (Start + any Connect), each getting an independent copy, and
// keeps a per-process bounded output ring buffer so a late Connect replays recent bytes before
// joining live (bounded replay).
//
// procs maps pid→proc: transport-process-local, non-authoritative, dying with the transport
// process — NOT a control-plane registry (the transport data plane: List scans /proc live). The
// hub is owned by guest PID 1 and is reached by transport children through
// the private supervisor socket; descriptors and replay state therefore
// survive a transport-child restart.
type processServer struct {
	mu              sync.Mutex
	procs           map[int]*proc
	byHandle        map[string]*proc
	idempotency     map[string]string
	idempotencySpec map[string]string
	// activity is the sandbox-global last-I/O clock (health and reclamation activity signal,
	// the activity signal). Shared with the healthServer so Health.Check reports
	// idle duration. Every proc created here carries a reference to it and
	// stamps it on broadcast/writeInput.
	activity *activityClock
	// reaper is the guest supervisor's single wildcard waiter (reap.go). REQUIRED —
	// it is where Start forks children and where their exit statuses come from
	// (see proc.run for why cmd.Wait() cannot be used). It is owned by PID 1 and
	// remains alive while replaceable transport children come and go.
	reaper *reapTable
	// retention bounds how long an exited logical handle remains addressable.  It
	// is deliberately owned by the process hub (the durable supervisor), not by
	// each transport child, so reconnecting a transport cannot reset the clock.
	retention    time.Duration
	startFlights map[string]*startFlight
	// handleFlights closes the fork-before-register window for a caller-supplied
	// logical handle. A retry must wait for the original materialization rather
	// than overwrite byHandle and orphan the first workload.
	handleFlights    map[string]*startFlight
	persistMu        sync.Mutex
	persistScheduled bool
	persistDirty     bool
	statePath        string
	// remote is set only by a transport child.  PID 1 owns the process hub and
	// the transport role talks to it over the private guest Unix socket.  Keeping
	// this seam optional preserves the pure in-process hub used by unit tests.
	remote *supervisorClient
}

type startFlight struct {
	done   chan struct{}
	handle string
	err    error
	claims []string
	hash   string
}

func startClaimAlias(key, handle string) string {
	if key != "" {
		return "key:" + key
	}
	if handle != "" {
		return "handle:" + handle
	}
	return ""
}

// claimStart serializes first materialization for one idempotency key.  A
// lookup followed by fork is not enough: two retries arriving together could
// both pass the lookup and create two logical processes.  The flight remains
// in memory only while the durable handle is being registered; the resulting
// handle and request hash are persisted with the process row.
func (s *processServer) claimStart(key, handle, hash string) (existing *proc, wait <-chan struct{}, owner bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idempotency == nil {
		s.idempotency = make(map[string]string)
	}
	if s.idempotencySpec == nil {
		s.idempotencySpec = make(map[string]string)
	}
	if s.startFlights == nil {
		s.startFlights = make(map[string]*startFlight)
	}
	if s.handleFlights == nil {
		s.handleFlights = make(map[string]*startFlight)
	}
	if prior := s.idempotencySpec[key]; prior != "" && prior != hash {
		return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("idempotency key was reused with a different process request"))
	}
	if mapped := s.idempotency[key]; mapped != "" {
		if handle != "" && mapped != handle {
			return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("idempotency key is bound to a different process handle"))
		}
		if p := s.byHandle[mapped]; p != nil {
			if hash != "" && p.specHash != "" && p.specHash != hash {
				return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("idempotency key was reused with a different process request"))
			}
			return p, nil, false, nil
		}
	}
	if handle != "" {
		if p := s.byHandle[handle]; p != nil {
			if hash != "" && p.specHash != "" && p.specHash != hash {
				return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("process handle was reused with a different process request"))
			}
			return p, nil, false, nil
		}
	}
	aliases := make([]string, 0, 2)
	if alias := startClaimAlias(key, ""); alias != "" {
		aliases = append(aliases, alias)
	}
	if handle != "" {
		aliases = append(aliases, startClaimAlias("", handle))
	}
	for _, alias := range aliases {
		if flight := s.startFlights[alias]; flight != nil {
			if flight.hash != "" && flight.hash != hash {
				return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("process start was retried with a different request"))
			}
			return nil, flight.done, false, nil
		}
		if flight := s.handleFlights[alias]; flight != nil {
			if flight.hash != "" && flight.hash != hash {
				return nil, nil, false, connect.NewError(connect.CodeAlreadyExists, errString("process start was retried with a different request"))
			}
			return nil, flight.done, false, nil
		}
	}
	flight := &startFlight{done: make(chan struct{}), claims: aliases, hash: hash}
	for _, alias := range aliases {
		if strings.HasPrefix(alias, "handle:") {
			s.handleFlights[alias] = flight
		} else {
			s.startFlights[alias] = flight
		}
	}
	return nil, flight.done, true, nil
}

func (s *processServer) finishStartFlight(key string, handle string, err error) {
	aliases := make([]string, 0, 2)
	if alias := startClaimAlias(key, ""); alias != "" {
		aliases = append(aliases, alias)
	}
	if handle != "" {
		aliases = append(aliases, startClaimAlias("", handle))
	}
	if len(aliases) == 0 {
		return
	}
	s.mu.Lock()
	var flight *startFlight
	for _, alias := range aliases {
		if f := s.startFlights[alias]; f != nil {
			flight = f
			break
		}
		if f := s.handleFlights[alias]; f != nil {
			flight = f
			break
		}
	}
	if flight != nil {
		flight.handle = handle
		flight.err = err
		for _, alias := range flight.claims {
			if strings.HasPrefix(alias, "handle:") {
				delete(s.handleFlights, alias)
			} else {
				delete(s.startFlights, alias)
			}
		}
		close(flight.done)
	}
	s.mu.Unlock()
}

const processStateFile = "/tmp/.sandboxd-processes.json"

// processStatePath is a variable rather than a second test-only code path so
// persistence tests exercise the exact atomic file writer used in the guest.
// Production never changes it.
var processStatePath = processStateFile

type persistedProcess struct {
	Key           string           `json:"key,omitempty"`
	SpecHash      string           `json:"spec_hash,omitempty"`
	Handle        string           `json:"handle"`
	PID           int              `json:"pid"`
	Epoch         uint64           `json:"epoch"`
	State         string           `json:"state"`
	ExitCode      *int             `json:"exit_code,omitempty"`
	Sequence      uint64           `json:"sequence"`
	StartTime     uint64           `json:"start_time,omitempty"`
	Frames        []persistedFrame `json:"frames,omitempty"`
	DroppedBefore uint64           `json:"dropped_before,omitempty"`
	EndFrame      *persistedFrame  `json:"end_frame,omitempty"`
	ExitedAt      int64            `json:"exited_at,omitempty"`
}

type persistedFrame struct {
	Kind     string `json:"kind"`
	Stream   string `json:"stream,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Sequence uint64 `json:"sequence"`
}

func (s *processServer) loadPersistent() {
	if s.statePath == "" {
		s.statePath = processStatePath
	}
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		return
	}
	var saved []persistedProcess
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.procs == nil {
		s.procs = make(map[int]*proc)
	}
	if s.byHandle == nil {
		s.byHandle = make(map[string]*proc)
	}
	if s.idempotency == nil {
		s.idempotency = make(map[string]string)
	}
	if s.idempotencySpec == nil {
		s.idempotencySpec = make(map[string]string)
	}
	dirty := false
	for _, saved := range saved {
		if saved.Handle == "" {
			continue
		}
		// A running row cannot be resumed by a newly booted PID 1: its pipes,
		// PTY master, and wait-status handoff belonged to the old supervisor.
		// Convert it to a terminal, fail-closed record rather than exposing an
		// inert handle that reports closed input and silently loses output.  The
		// live transport-restart path never reaches this branch because PID 1's
		// supervisor remains in place.
		if saved.State != "exited" {
			saved.State = "exited"
			code := 255
			saved.ExitCode = &code
			dirty = true
		}
		exitedAt := time.Time{}
		if saved.ExitedAt != 0 {
			exitedAt = time.Unix(0, saved.ExitedAt)
		}
		p := &proc{pid: saved.PID, handle: saved.Handle, specHash: saved.SpecHash, epoch: saved.Epoch, startTime: saved.StartTime, exitedAt: exitedAt, ring: ringBuffer{cap: ringCapBytes, droppedBefore: saved.DroppedBefore}, subs: make(map[*subscriber]struct{}), sequence: saved.Sequence}
		p.restored = true
		for _, persisted := range saved.Frames {
			p.ring.append(frame{kind: byte(firstByte(persisted.Kind, 'd')), stream: persisted.Stream, data: append([]byte(nil), persisted.Data...), exitCode: persisted.ExitCode, sequence: persisted.Sequence})
		}
		if saved.EndFrame != nil {
			p.endFrame = frame{kind: byte(firstByte(saved.EndFrame.Kind, 'e')), stream: saved.EndFrame.Stream, data: append([]byte(nil), saved.EndFrame.Data...), exitCode: saved.EndFrame.ExitCode, sequence: saved.EndFrame.Sequence}
		}
		if saved.State == "exited" {
			p.ended = true
			code := 0
			if saved.ExitCode != nil {
				code = *saved.ExitCode
			}
			p.endFrame = frame{kind: 'e', exitCode: code, sequence: saved.Sequence}
		} else if saved.StartTime == 0 {
			// A pre-supervisor state file cannot authenticate a live PID.  Keep
			// the durable handle as an exited/reclaimed record rather than risking
			// delivery to a reused PID.
			p.ended = true
			p.exitedAt = time.Now()
			p.endFrame = frame{kind: 'e', exitCode: 255, sequence: saved.Sequence + 1}
		}
		s.byHandle[p.handle] = p
		if saved.Key != "" {
			s.idempotency[saved.Key] = p.handle
			s.idempotencySpec[saved.Key] = saved.SpecHash
		}
	}
	for _, p := range s.byHandle {
		if p.ended {
			s.scheduleRetentionLocked(p)
		}
	}
	if dirty {
		s.savePersistentLocked()
	}
}

func firstByte(s string, fallback byte) byte {
	if s == "" {
		return fallback
	}
	return s[0]
}

func (s *processServer) watchRestored() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		pids := make([]int, 0, len(s.procs))
		for pid := range s.procs {
			pids = append(pids, pid)
		}
		s.mu.Unlock()
		for _, pid := range pids {
			p, ok := s.get(pid)
			if !ok {
				continue
			}
			// Processes started by this still-live supervisor already have a
			// registered wait-status handoff and output pumps.  Only metadata
			// reconstructed from disk needs this fallback liveness probe; probing
			// active entries races the real reaper and can replace a valid exit
			// status with the synthetic 255 code.
			p.mu.Lock()
			restored := p.restored
			p.mu.Unlock()
			if !restored {
				continue
			}
			if p.identityValid() {
				continue
			}
			p.finish(255)
			s.unregisterProcess(pid, p)
		}
	}
}

func (s *processServer) savePersistentLocked() {
	s.savePersistentLockedAt(s.persistencePath())
}

func (s *processServer) persistencePath() string {
	if s.statePath != "" {
		return s.statePath
	}
	return processStatePath
}

func (s *processServer) savePersistentLockedAt(path string) {
	all := make([]persistedProcess, 0, len(s.byHandle))
	for _, p := range s.byHandle {
		p.mu.Lock()
		state := "running"
		var code *int
		if p.ended {
			state = "exited"
			c := p.endFrame.exitCode
			code = &c
		}
		key := ""
		for candidate, handle := range s.idempotency {
			if handle == p.handle {
				key = candidate
				break
			}
		}
		frames := make([]persistedFrame, 0, len(p.ring.frames))
		for _, f := range p.ring.frames {
			frames = append(frames, persistedFrame{Kind: string([]byte{f.kind}), Stream: f.stream, Data: append([]byte(nil), f.data...), ExitCode: f.exitCode, Sequence: f.sequence})
		}
		var end *persistedFrame
		if p.endFrame.kind != 0 {
			v := persistedFrame{Kind: string([]byte{p.endFrame.kind}), Stream: p.endFrame.stream, Data: append([]byte(nil), p.endFrame.data...), ExitCode: p.endFrame.exitCode, Sequence: p.endFrame.sequence}
			end = &v
		}
		var exitedAt int64
		if !p.exitedAt.IsZero() {
			exitedAt = p.exitedAt.UnixNano()
		}
		specHash := p.specHash
		if specHash == "" {
			specHash = s.idempotencySpec[key]
		}
		all = append(all, persistedProcess{Key: key, SpecHash: specHash, Handle: p.handle, PID: p.pid, Epoch: p.epoch, State: state, ExitCode: code, Sequence: p.sequence, StartTime: p.startTime, Frames: frames, DroppedBefore: p.ring.droppedBefore, EndFrame: end, ExitedAt: exitedAt})
		p.mu.Unlock()
	}
	b, err := json.Marshal(all)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// Tuning values. Pure numeric knobs — changing one does NOT change the transport behavior/transport behavior shape, and
// this is their only home. Sized so the ring holds enough tail to redraw an interactive
// screen without unbounded guest memory, and the window is deep enough for a healthy
// consumer but small enough that a stuck one is dropped promptly.
const (
	ringCapBytes      = 64 * 1024        // per-process output ring byte cap (bounded replay)
	subWindowFrames   = 256              // per-subscriber send window (nonblocking fan-out)
	keepaliveInterval = 20 * time.Second // long-stream liveness (transport behavior)
)

func (s *processServer) register(p *proc) {
	s.mu.Lock()
	if s.procs == nil {
		s.procs = make(map[int]*proc)
	}
	if s.byHandle == nil {
		s.byHandle = make(map[string]*proc)
	}
	if s.idempotency == nil {
		s.idempotency = make(map[string]string)
	}
	if s.idempotencySpec == nil {
		s.idempotencySpec = make(map[string]string)
	}
	p.activity = s.activity // share the sandbox-global I/O clock (the activity signal)
	if s.statePath == "" {
		s.statePath = processStatePath
	}
	path := s.statePath
	p.persist = func() { s.persistSoonAt(path) }
	p.persistNow = func() { s.persistAt(path) }
	if p.startTime == 0 {
		p.startTime = procStartTime(p.pid)
	}
	s.procs[p.pid] = p
	s.byHandle[p.handle] = p
	s.savePersistentLocked()
	s.mu.Unlock()
}

func (s *processServer) persist() {
	s.persistAt(s.persistencePath())
}

// persistSoon coalesces high-rate output updates.  Serializing JSON and a
// tmpfs write on every output frame would make the child reader wait on the
// persistence path and would turn a slow disk into source backpressure.  A
// short coalescing window still records the latest bounded ring promptly while
// keeping fan-out and pipe draining independent of storage latency.
func (s *processServer) persistSoon() {
	s.persistSoonAt(s.persistencePath())
}

func (s *processServer) persistAt(path string) {
	s.mu.Lock()
	s.savePersistentLockedAt(path)
	s.mu.Unlock()
}

func (s *processServer) persistSoonAt(path string) {
	s.persistMu.Lock()
	s.persistDirty = true
	if s.persistScheduled {
		s.persistMu.Unlock()
		return
	}
	s.persistScheduled = true
	s.persistMu.Unlock()
	time.AfterFunc(10*time.Millisecond, func() { s.flushPersist(path) })
}

func (s *processServer) flushPersist(path string) {
	for {
		s.persistMu.Lock()
		if !s.persistDirty {
			s.persistScheduled = false
			s.persistMu.Unlock()
			return
		}
		s.persistDirty = false
		s.persistMu.Unlock()

		s.persistAt(path)
		// If output arrived while the snapshot was being written, loop once
		// more before releasing the scheduled slot.  There is never more than
		// one writer or one timer for a process hub.
	}
}

func (s *processServer) get(pid int) (*proc, bool) {
	s.mu.Lock()
	p, ok := s.procs[pid]
	s.mu.Unlock()
	return p, ok
}

// unregister removes the process currently occupying pid. It is kept as a
// compatibility helper for tests and callers that do not retain the proc
// pointer; lifecycle callbacks should use unregisterProcess so a recycled PID
// cannot let an older workload delete a newer one.
func (s *processServer) unregister(pid int) {
	s.unregisterProcess(pid, nil)
}

// unregisterProcess removes p only when it is still the process mapped at pid.
// The kernel may recycle a PID while an old process's final pipe-drain callback
// is still running; pointer identity fences that callback from deleting the new
// occupant. The logical handle map is retained for exit replay as before.
func (s *processServer) unregisterProcess(pid int, expected *proc) {
	s.mu.Lock()
	p := s.procs[pid]
	if expected != nil && p != expected {
		// The PID slot was recycled before the old process's drain callback
		// arrived. Keep the replacement untouched, but complete the old
		// process's logical lifecycle and retention timer.
		if !expected.removed.CompareAndSwap(false, true) {
			s.mu.Unlock()
			return
		}
		s.scheduleRetentionLocked(expected)
		s.savePersistentLocked()
		s.mu.Unlock()
		return
	}
	delete(s.procs, pid)
	// Keep the logical handle and exit status for the bounded retention window;
	// only the placement-local PID map is removed immediately.
	if p != nil {
		if !p.removed.CompareAndSwap(false, true) {
			s.mu.Unlock()
			return
		}
		s.scheduleRetentionLocked(p)
	}
	s.savePersistentLocked()
	s.mu.Unlock()
}

func (s *processServer) scheduleRetentionLocked(p *proc) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.exitedAt.IsZero() {
		p.exitedAt = time.Now()
	}
	deadline := p.exitedAt.Add(s.retentionDuration())
	p.mu.Unlock()
	d := time.Until(deadline)
	if d < 0 {
		d = 0
	}
	time.AfterFunc(d, func() {
		s.mu.Lock()
		if s.byHandle[p.handle] == p {
			delete(s.byHandle, p.handle)
			for key, handle := range s.idempotency {
				if handle == p.handle {
					delete(s.idempotency, key)
					delete(s.idempotencySpec, key)
				}
			}
			s.savePersistentLocked()
		}
		s.mu.Unlock()
	})
}

func (s *processServer) retentionDuration() time.Duration {
	if s.retention > 0 {
		return s.retention
	}
	return 24 * time.Hour
}

// frame is one broadcast item: a data chunk ('d') or the terminal exit ('e').
type frame struct {
	kind     byte // 'd' data, 'e' end
	stream   string
	data     []byte
	exitCode int
	sequence uint64
}

// subscriber is one attached output consumer. ch is its bounded send window
// (nonblocking fan-out): broadcast never blocks on it; on overflow the subscriber is
// dropped (removed from the fan-out, ch closed), backpressure never reaching
// the source. overflow distinguishes a dropped subscriber from a clean end.
type subscriber struct {
	ch       chan frame
	overflow atomic.Bool
}

// proc is the per-process fan-out hub (transient, non-authoritative — see
// processServer doc). Holds the bounded ring, the live subscriber set, and the
// input handles (stdin pipe; pty master). In production this hub is owned by
// guest PID 1; transport children only proxy to it.
type proc struct {
	pid int
	// startTime is the kernel start-time tick read from /proc/<pid>/stat at
	// launch.  A PID alone is reusable; every restored/live lookup compares this
	// token before treating a locator as the same process.
	startTime uint64
	restored  bool
	handle    string
	specHash  string
	epoch     uint64

	// activity is the shared sandbox-global last-I/O clock (the activity signal),
	// set by processServer.register. Stamped on every output (broadcast) and
	// input (writeInput). nil when a proc is built outside the server (unit
	// tests) — activityClock methods are nil-safe.
	activity *activityClock

	mu         sync.Mutex
	ring       ringBuffer
	subs       map[*subscriber]struct{}
	stdin      io.WriteCloser
	pty        *os.File // set only for pty processes
	ended      bool
	exitedAt   time.Time
	endFrame   frame
	sequence   uint64
	persist    func()
	persistNow func()
	removed    atomic.Bool
}

func newProc(pid int, stdin io.WriteCloser) *proc {
	return &proc{
		pid:       pid,
		startTime: procStartTime(pid),
		handle:    newProcessHandle(),
		epoch:     1,
		ring:      ringBuffer{cap: ringCapBytes},
		subs:      make(map[*subscriber]struct{}),
		stdin:     stdin,
	}
}

// procStartTime returns field 22 (starttime) from /proc/<pid>/stat.  The comm
// field may contain spaces and ')' characters, so parsing starts after the last
// closing parenthesis.  A zero result means the locator cannot be authenticated
// and is intentionally treated as unavailable by restored-process checks.
func procStartTime(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(b)[i+1:])
	// fields[0] is state; field 22 in the full record is index 19 after
	// the comm field.
	if len(fields) <= 19 {
		return 0
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func (p *proc) identityValid() bool {
	if p == nil || p.pid <= 0 {
		return false
	}
	if p.startTime == 0 {
		if p.restored {
			return false
		}
		// Unit-only synthetic procs do not have a kernel locator.  Keep their
		// in-memory behavior.
		_, err := os.Stat("/proc/" + strconv.Itoa(p.pid))
		return err == nil
	}
	return procStartTime(p.pid) == p.startTime
}

func newProcessHandle() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "proc_unknown"
	}
	return "proc_" + hex.EncodeToString(b[:])
}

func startSpecHash(m *v1.StartRequest) string {
	// Keep repeated fields in the same canonical representation as the
	// control-plane fingerprint.  encoding/json distinguishes nil slices
	// (`null`) from empty slices (`[]`), while protobuf decoding commonly
	// leaves omitted repeated fields nil.  Normalizing both edges prevents a
	// retry hash from changing merely because the request crossed the wire.
	args := append([]string{}, m.Args...)
	env := append([]string{}, m.Env...)
	b, _ := json.Marshal(struct {
		Cmd            string      `json:"cmd"`
		Args           []string    `json:"args"`
		Env            []string    `json:"env"`
		Cwd            string      `json:"cwd"`
		Pty            *v1.PtySize `json:"pty,omitempty"`
		PlacementEpoch uint64      `json:"placement_epoch"`
	}{m.Cmd, args, env, m.Cwd, m.Pty, m.PlacementEpoch})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ringBuffer holds recent output frames up to a byte cap, dropping the oldest
// bytes silently (bounded replay: "oldest bytes are dropped silently"). Frames
// keep their stream
// tag so replay preserves stdout/stderr/pty attribution.
type ringBuffer struct {
	cap           int
	bytes         int
	frames        []frame
	droppedBefore uint64
}

func (r *ringBuffer) append(f frame) {
	r.frames = append(r.frames, f)
	r.bytes += len(f.data)
	for r.bytes > r.cap && len(r.frames) > 0 {
		if r.frames[0].sequence+1 > r.droppedBefore {
			r.droppedBefore = r.frames[0].sequence + 1
		}
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
// (nonblocking fan-out) — the source NEVER blocks here.
func (p *proc) broadcast(f frame) {
	if f.kind == 'd' {
		// Stamp sandbox-global activity on OUTPUT (the activity signal), here in the path EVERY
		// output frame flows through, so a detached process producing output with no client
		// attached still counts as active (the sandbox process model / guest-side activity measurement). Off p.mu — the atomic
		// store needs no lock.
		p.activity.stamp()
	}
	p.mu.Lock()
	p.sequence++
	f.sequence = p.sequence
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
	if p.persist != nil {
		p.persist()
	}
}

// finish marks the process ended, delivers the end frame to remaining
// subscribers, and closes their channels. A subscriber that Connects after this
// sees ended=true from subscribe and replays the ring + end directly.
//
// Lossy-by-design boundary (nonblocking fan-out / transport behavior): a subscriber whose window is full at
// exit loses the end frame — exit code included — for THAT copy; the channel still closes
// and serveSubscriber returns a clean end. The broadcast path's rule, extended to the exit
// instant.
//
// "You were dropped" IS observable end-to-end: serveSubscriber returns resource_exhausted on
// an overflow close, which the host surfaces as ProcessEvent{Kind:"error"}, so a consumer can
// tell it from "the process finished" — otherwise both look like a channel closing.
//
// Still NOT a health signal: health and reclamation classifies a sandbox from the L2 Health probe and its
// guest-side activity clock, never from one subscriber's copy being too slow.
func (p *proc) finish(exitCode int) {
	p.mu.Lock()
	if p.ended {
		p.mu.Unlock()
		return
	}
	p.ended = true
	p.exitedAt = time.Now()
	p.sequence++
	p.endFrame = frame{kind: 'e', exitCode: exitCode, sequence: p.sequence}
	for sub := range p.subs {
		select {
		case sub.ch <- p.endFrame:
		default:
			// The terminal frame carries the only exit code for this live copy.
			// Do not let a full queue turn a dropped terminal into a clean EOF.
			sub.overflow.Store(true)
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
	if p.persistNow != nil {
		p.persistNow()
	} else if p.persist != nil {
		p.persist()
	}
}

// subscribe atomically snapshots the ring and (if the process is still live)
// registers a new subscriber, so no frame is duplicated or missed at the
// replay→live seam. Returns sub==nil when the process has already ended.
func (p *proc) subscribe() (sub *subscriber, snap []frame, ended bool, endFrame frame) {
	sub, snap, ended, endFrame, _ = p.subscribeFrom(0)
	return
}

func (p *proc) subscribeFrom(after uint64) (sub *subscriber, snap []frame, ended bool, endFrame frame, gap bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap = p.ring.snapshot()
	first := uint64(0)
	if len(snap) > 0 {
		first = snap[0].sequence
	} else {
		first = p.ring.droppedBefore
	}
	// Sequence zero is the initial cursor: it asks for whatever bounded tail
	// is currently retained, even when older frames have already been evicted.
	// A nonzero cursor is invalid both when it is behind the retained prefix and
	// when it points into the future. Returning an empty live stream for a future
	// cursor would suppress output (and, for an already-ended process, its end
	// frame) until a caller happened to rescan by hand.
	if after > 0 && (after > p.sequence || (first > 0 && after < first-1)) {
		gap = true
	}
	if after > 0 {
		filtered := snap[:0]
		for _, f := range snap {
			if f.sequence > after {
				filtered = append(filtered, f)
			}
		}
		snap = append([]frame(nil), filtered...)
	}
	if p.ended {
		if after > 0 && p.endFrame.sequence <= after {
			// The caller has already consumed the terminal frame at this cursor.
			// Returning it again would violate the strictly-after cursor contract.
			return nil, snap, true, frame{}, gap
		}
		return nil, snap, true, p.endFrame, gap
	}
	sub = &subscriber{ch: make(chan frame, subWindowFrames)}
	p.subs[sub] = struct{}{}
	return sub, snap, false, frame{}, gap
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
// The blocking Write MUST happen OUTSIDE p.mu (nonblocking fan-out: the source never blocks).
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
	// Stamp activity on INPUT (the activity signal): a keystroke is I/O even if the process
	// produces no output. BEFORE the possibly-blocking Write — the input arriving IS the
	// activity, regardless of when the child drains it.
	p.activity.stamp()
	_, err := w.Write(data)
	return err
}

// resize applies a new PTY size (process transport Update). Errors if no pty.
//
// TIOCSWINSZ is non-blocking so it could not deadlock the fan-out, but it follows
// writeInput's snapshot-then-act-unlocked shape uniformly, so no I/O on the pty handle is
// ever done under p.mu (nonblocking fan-out).
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
// Like writeInput, the pty ^D Write is done outside p.mu (nonblocking fan-out). The stdin path
// snapshots and clears the handle before releasing, so a racing writeInput sees stdin==nil.
func (p *proc) closeStdin() error {
	p.mu.Lock()
	if p.pty != nil {
		pty := p.pty
		p.mu.Unlock()
		// The ^D keystroke is INPUT I/O — stamped like writeInput (the activity signal), so a
		// sandbox that was just interacted with cannot look idle.
		p.activity.stamp()
		_, err := pty.Write([]byte{0x04}) // ^D
		return err
	}
	if p.stdin != nil {
		w := p.stdin
		p.stdin = nil
		p.mu.Unlock()
		p.activity.stamp() // stdin close is input-side I/O too (the activity signal)
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
// disconnect — that is what makes Connect reattach possible (the sandbox process model).
//
// finish-then-unregister is the exit-status delivery boundary, and it is deliberately sharp:
// finish broadcasts `end` to whoever is attached now, then the pid stops resolving, so a later
// Connect gets not-found rather than a retained exit code. Holding the entry would retain
// state keyed by a guest pid the kernel can recycle, so the record would start describing a
// different process once the number is reused. Callers that must not miss exit status stay
// attached so the parent can reap the process group.
//
// The exit status arrives over `exited` — the supervisor reaper's handoff — NOT
// from cmd.Wait().  PID 1 owns Wait4(-1), so a per-child cmd.Wait() would race
// the reaper: whoever lost sees ECHILD and this `end` frame carries a fabricated
// exit code.  Transport children never wait for workload processes.
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
// in the guest startup topology `testguest` sense (that tag is for a seam that must be ABSENT from a
// production sandboxd), and *connect.ServerStream[v1.ProcessEvent] satisfies this already.
type eventSender interface {
	Send(*v1.ProcessEvent) error
}

// Start launches a process and server-streams its output events.
func (s *processServer) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	return s.start(ctx, req, stream)
}

func (s *processServer) start(ctx context.Context, req *connect.Request[v1.StartRequest], stream eventSender) (retErr error) {
	m := req.Msg
	if m.Cmd == "" {
		return connect.NewError(connect.CodeInvalidArgument, errString("empty cmd"))
	}
	// A caller key is scoped to this transport endpoint. Repeating it returns
	// the same logical process stream while the process is still known here;
	// the control plane persists the durable handle across endpoint changes.
	flightOwner := false
	// Seed the cleanup handle from the caller before any fork can fail.  If
	// setup (PTY allocation, pipe creation, or exec) fails, both the key and
	// handle aliases must be released or every retry would remain blocked.
	flightHandle := m.ProcessHandle
	if m.IdempotencyKey != "" || m.ProcessHandle != "" {
		hash := startSpecHash(m)
		for {
			existing, wait, owner, err := s.claimStart(m.IdempotencyKey, m.ProcessHandle, hash)
			if err != nil {
				return err
			}
			if existing != nil {
				return serveExistingProcess(ctx, stream, existing)
			}
			if owner {
				flightOwner = true
				defer func() {
					if flightOwner {
						s.finishStartFlight(m.IdempotencyKey, flightHandle, retErr)
					}
				}()
				break
			}
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	// Independent context: the process must outlive this Start stream so a later Connect can
	// reattach (the sandbox process model). Signals/exit end it, not a client disconnect.
	//
	// Processes run as guest root (the guest identity model: guest identity is not a host-isolation pillar —
	// sentry virtualizes guest uid 0). "Run as a different user" is a guest-native concern
	// (su/sudo), not an engine primitive (the product-neutral sandbox boundary).
	cmd := exec.Command(m.Cmd, m.Args...)
	cmd.Env = m.Env
	cmd.Dir = m.Cwd

	// Every fork goes through the reap table: it registers the waiter under the
	// same lock as the fork, so the reaper cannot collect this child before we
	// are entitled to its status (the guest startup topology single waiter — see reapTable.start).
	// It also Releases the os.Process handle, so cmd.Process.Pid is cleared and
	// the returned pid is the only handle; SendSignal targets it by number.
	var p *proc
	var sources []source
	var exited <-chan syscall.WaitStatus
	if m.Pty != nil {
		// PTY branch (process transport): pure-Go pty pair. The slave is the
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
		if m.ProcessHandle != "" {
			p.handle = m.ProcessHandle
		}
		p.pty = master
		// closer nil: the master is also the input sink, so finish() closes it.
		sources = []source{{r: master, name: "pty"}}
		exited = ch
	} else {
		// Own pgid so SendSignal can target the whole tree (process transport).
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
		if m.ProcessHandle != "" {
			p.handle = m.ProcessHandle
		}
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
	if m.PlacementEpoch > 0 {
		p.epoch = m.PlacementEpoch
	}
	// Keep the canonical request fingerprint on the in-memory hub as soon as
	// the caller-supplied handle is claimed. This lets a retry validate both
	// handle identity and request semantics before it can attach or fork.
	p.specHash = startSpecHash(m)
	s.register(p)
	flightHandle = p.handle
	if m.IdempotencyKey != "" {
		s.mu.Lock()
		s.idempotency[m.IdempotencyKey] = p.handle
		s.idempotencySpec[m.IdempotencyKey] = p.specHash
		s.savePersistentLocked()
		s.mu.Unlock()
		// The durable handle is now visible.  Release the creation flight before
		// streaming output so a concurrent retry can attach immediately instead
		// of waiting for the process to exit.
		s.finishStartFlight(m.IdempotencyKey, p.handle, nil)
		flightOwner = false
	} else if m.ProcessHandle != "" {
		// A caller-supplied handle without an idempotency key is still protected
		// by the handle flight. Release it only after the process is registered.
		s.finishStartFlight("", p.handle, nil)
		flightOwner = false
	}

	// Subscribe BEFORE the source pumps start so Start sees everything live
	// (ring replay is empty for a fresh Start).
	sub, snap, ended, endF, gap := p.subscribeFrom(0)

	// UNCONDITIONALLY before the first Send, because run() is the only thing that drains this
	// child, closes its pipe parent ends, and calls unregister. Gated behind a successful Send
	// (as it was), a first-frame failure left the pumps unstarted, so the child filled the
	// 64 KiB pipe buffer on an unread stdout and blocked THERE forever, unable even to exit —
	// plus a permanent s.procs entry and three unclosed fds. Ordering is safe: subscribe()
	// above already registered this subscriber, so no frame is missed.
	//
	// The already-forked process is left to run and be reaped, NOT killed. the sandbox process model gives Start an
	// independent context precisely so a process outlives a client disconnect; "kill on
	// first-frame failure" cannot be a contract, because the same disconnect one frame later
	// leaves the process alive — dead-vs-alive would depend on invisible timing.
	go p.run(exited, sources, func() { s.unregisterProcess(pid, p) })

	if err := stream.Send(&v1.ProcessEvent{
		ProcessHandle: p.handle, PlacementEpoch: p.epoch,
		Kind: &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(pid), ProcessHandle: p.handle, PlacementEpoch: p.epoch}},
	}); err != nil {
		if sub != nil {
			p.unsubscribe(sub)
		}
		return err
	}

	if sub != nil {
		defer p.unsubscribe(sub)
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF, p.handle, p.epoch, gap)
}

// Connect reattaches to a running process: replay the bounded ring, then live
// (bounded replay). Independent per-subscriber copy (the sandbox process model fan-out).
func (s *processServer) Connect(ctx context.Context, req *connect.Request[v1.ConnectRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	var p *proc
	var ok bool
	if req.Msg.ProcessHandle != "" {
		s.mu.Lock()
		p, ok = s.byHandle[req.Msg.ProcessHandle]
		s.mu.Unlock()
		if ok {
			p.mu.Lock()
			ended := p.ended
			p.mu.Unlock()
			if !ended && !p.identityValid() {
				ok = false
			}
		}
		if ok && req.Msg.PlacementEpoch > 0 && p.epoch != req.Msg.PlacementEpoch {
			return connect.NewError(connect.CodeFailedPrecondition, errString("stale placement epoch"))
		}
	} else {
		p, ok = s.get(int(req.Msg.Pid))
	}
	if !ok {
		if req.Msg.ProcessHandle != "" {
			return connect.NewError(connect.CodeNotFound, errString("no such process handle"))
		}
		return errNoSuchPid(int(req.Msg.Pid))
	}
	sub, snap, ended, endF, gap := p.subscribeFrom(req.Msg.AfterSequence)
	if sub != nil {
		defer p.unsubscribe(sub)
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF, p.handle, p.epoch, gap)
}

func (s *processServer) resolveProcess(handle string, epoch uint64, pid int) (*proc, error) {
	s.mu.Lock()
	var p *proc
	if handle != "" {
		p = s.byHandle[handle]
	} else {
		p = s.procs[pid]
	}
	s.mu.Unlock()
	if p == nil {
		if handle != "" {
			return nil, connect.NewError(connect.CodeNotFound, errString("no such process handle"))
		}
		return nil, errNoSuchPid(pid)
	}
	if epoch > 0 && p.epoch != epoch {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errString("stale placement epoch"))
	}
	// A PID is only a placement-local locator.  A restored record must carry
	// the same kernel start-time token as the current /proc entry; otherwise a
	// recycled number could route input, resize, or signals to an unrelated
	// process before the periodic restored-state sweep notices it.
	p.mu.Lock()
	ended := p.ended
	p.mu.Unlock()
	if !ended && !p.identityValid() {
		if handle != "" {
			return nil, connect.NewError(connect.CodeNotFound, errString("process handle is no longer live"))
		}
		return nil, errNoSuchPid(pid)
	}
	return p, nil
}

func serveExistingProcess(ctx context.Context, stream eventSender, p *proc) error {
	sub, snap, ended, endF, gap := p.subscribeFrom(0)
	if sub != nil {
		defer p.unsubscribe(sub)
	}
	if err := stream.Send(&v1.ProcessEvent{ProcessHandle: p.handle, PlacementEpoch: p.epoch, Kind: &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(p.pid), ProcessHandle: p.handle, PlacementEpoch: p.epoch}}}); err != nil {
		return err
	}
	return serveSubscriber(ctx, stream, sub, snap, ended, endF, p.handle, p.epoch, gap)
}

// serveSubscriber replays the ring snapshot, then either sends the end frame (if
// the process already ended) or pumps the live window, emitting keepalives on
// idle (transport behavior — no idle deadline on the long output stream).
func serveSubscriber(ctx context.Context, stream eventSender, sub *subscriber, snap []frame, ended bool, endF frame, handle string, epoch uint64, gap bool) error {
	if gap {
		// The caller's cursor fell behind the bounded ring. Make the reset
		// explicit instead of silently returning a partial replay.
		if err := stream.Send(&v1.ProcessEvent{ProcessHandle: handle, PlacementEpoch: epoch, Gap: true, ResetReason: "overflow", Kind: &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}}}); err != nil {
			return err
		}
		return connect.NewError(connect.CodeResourceExhausted, errString("process output cursor gap; full rescan required"))
	}
	for _, f := range snap {
		if err := sendFrame(stream, f, handle, epoch, true); err != nil {
			return err
		}
	}
	if ended || sub == nil {
		return sendFrame(stream, endF, handle, epoch, false)
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
			if err := sendFrame(stream, f, handle, epoch, false); err != nil {
				return err
			}
		case <-ka.C:
			if err := stream.Send(&v1.ProcessEvent{
				ProcessHandle: handle, PlacementEpoch: epoch,
				Kind: &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}},
			}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func sendFrame(stream eventSender, f frame, handle string, epoch uint64, replay bool) error {
	switch f.kind {
	case 'd':
		return stream.Send(&v1.ProcessEvent{
			ProcessHandle: handle, PlacementEpoch: epoch, Sequence: f.sequence, Replay: replay,
			Kind: &v1.ProcessEvent_Data{Data: &v1.DataEvent{Stream: f.stream, Data: f.data}},
		})
	case 'e':
		return stream.Send(&v1.ProcessEvent{
			ProcessHandle: handle, PlacementEpoch: epoch, Sequence: f.sequence, Replay: replay,
			Kind: &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(f.exitCode)}},
		})
	}
	return nil
}

// StreamInput writes an ordered client-stream of input frames to one process
// (keystrokes to a pty). pid is taken from the first frame; ordering is the
// single stream's (process transport). engine does NOT arbitrate multiple writers
// (transport behavior: bytes interleave, Unix semantics — that is the product layer's call).
func (s *processServer) StreamInput(_ context.Context, stream *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	var p *proc
	for stream.Receive() {
		m := stream.Msg()
		if p == nil {
			var err error
			p, err = s.resolveProcess(m.ProcessHandle, m.PlacementEpoch, int(m.Pid))
			if err != nil {
				return nil, err
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

// Update resizes a process's PTY (process transport). No-op-erroring on a non-pty
// process (nothing to resize).
func (s *processServer) Update(_ context.Context, req *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	m := req.Msg
	p, err := s.resolveProcess(m.ProcessHandle, m.PlacementEpoch, int(m.Pid))
	if err != nil {
		return nil, err
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
	p, err := s.resolveProcess(req.Msg.ProcessHandle, req.Msg.PlacementEpoch, int(req.Msg.Pid))
	if err != nil {
		return nil, err
	}
	if err := p.closeStdin(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.CloseStdinResponse{}), nil
}

func (s *processServer) SendInput(_ context.Context, req *connect.Request[v1.SendInputRequest]) (*connect.Response[v1.SendInputResponse], error) {
	m := req.Msg
	p, err := s.resolveProcess(m.ProcessHandle, m.PlacementEpoch, int(m.Pid))
	if err != nil {
		return nil, err
	}
	if err := p.writeInput(m.Data, m.PtyIn); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SendInputResponse{}), nil
}

// SendSignal delivers a DIRECTED signal — one process target, defaulting to its
// group (process transport / the guest startup topology).
//
// It validates `pid > 0` and nothing more — NO registry membership, unlike
// Connect/SendInput/Update/CloseStdin, which touch per-process state. Demanding a registry hit
// would break the control-plane API List-then-signal pairing, since List scans /proc live and returns pids
// that were never in s.procs.
//
// The check is HERE (guest side) because the guest startup topology's premise is that the guest can write its own
// fs and must therefore validate for itself; the orchestrator's copy is only fail-fast.
func (s *processServer) SendSignal(_ context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	m := req.Msg
	if m.ProcessHandle != "" {
		p, err := s.resolveProcess(m.ProcessHandle, m.PlacementEpoch, int(m.Pid))
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		ended := p.ended
		p.mu.Unlock()
		if ended {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errString("process has exited"))
		}
		m.Pid = int32(p.pid)
	}
	if err := validateSignalPid(int(m.Pid)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString(err.Error()))
	}
	if err := killProcessGroup(int(m.Pid), int(m.Signal)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SendSignalResponse{}), nil
}

// List scans /proc for live pids (no registry, the transport data plane).
func (s *processServer) List(_ context.Context, _ *connect.Request[v1.ListRequest]) (*connect.Response[v1.ListResponse], error) {
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
	stable := make([]*v1.ProcessInfo, 0)
	s.mu.Lock()
	for _, p := range s.procs {
		p.mu.Lock()
		info := &v1.ProcessInfo{ProcessHandle: p.handle, Pid: int32(p.pid), State: "running", PlacementEpoch: p.epoch, OutputSequence: p.sequence}
		if p.ended {
			info.State = "exited"
			code := int32(p.endFrame.exitCode)
			info.ExitCode = &code
		}
		p.mu.Unlock()
		stable = append(stable, info)
	}
	s.mu.Unlock()
	return connect.NewResponse(&v1.ListResponse{Pids: pids, Processes: stable}), nil
}

type errString string

func (e errString) Error() string { return string(e) }

// errNoSuchPid is the NotFound returned when a pid has no live process (List
// scans /proc, so a stale pid simply isn't there).
func errNoSuchPid(pid int) error {
	return connect.NewError(connect.CodeNotFound, errString("no such live process pid "+strconv.Itoa(pid)))
}
