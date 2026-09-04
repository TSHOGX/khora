package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"connectrpc.com/connect"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// The outer-edge data plane (the outer-edge transport split): the orchestrator mounts the SAME
// process/fs Connect services the inner sandboxd exposes, and re-proxies each
// call to the sandbox's current endpoint resolved by sandbox_id via the mediator
// without exposing Placement. The inner transport.proto is reused verbatim at
// the outer edge, with no translation layer and no REST or NDJSON facade.
//
// Addressing: the proto is PID-addressed and has no sandbox ID field. The outer edge
// carries the sandbox_id in a request header; the proxy strips it and resolves
// the endpoint internally. The mediator owns the "which endpoint" verb (the lifecycle);
// the proxy is a client of the resolved transport.Client, never calling another
// subsystem directly.
const sandboxIDHeader = "Engine-Sandbox-Id"

// processProxy re-proxies ProcessService to the sandbox's sandboxd.
type processProxy struct{ e *Engine }

// fsProxy re-proxies FsService to the sandbox's sandboxd.
type fsProxy struct{ e *Engine }

// proxyErr sanitizes an inner transport-client error before it crosses to the external
// client. The inner client dials the netns veth IP (client.go), so a raw dial/transport
// error carries that IP:port, so returning it verbatim would leak Placement.
// The real cause is logged engine-side but never crosses to the wire.
//
// The leak is in the MESSAGE, never in the CODE: a Connect code is one of a fixed enum of
// ~16 values, so it cannot carry an address or a host path. That splits the sanitization:
//
//   - The inner error IS a *connect.Error — sandboxd itself classified the failure and chose
//     the code. Pass that code through with a message composed HERE from nothing but the
//     code's own name. A fresh connect.NewError is built rather than the inner error
//     forwarded, so its details/metadata do not ride along either.
//   - NOT a connect error — a raw dial/network failure, the case that genuinely IS a
//     transient outage. CodeUnavailable with a fixed message, also the honest answer for an
//     error of unknown provenance.
//
// THE CODE IS PASSED THROUGH RATHER THAN COLLAPSED to CodeUnavailable ("transient, retry
// me"): collapsing turns a malformed argument or a missing path into an invitation to retry
// forever. It also makes the RPC half agree with the REST half, which already reports the
// guest's classification (files.go bypassCode). Mid-stream terminations need it most — the
// guest reports an overflow as resource_exhausted precisely so a consumer can tell it from a
// clean end (see forwardEvents).
func proxyErr(err error) error {
	log.Printf("dataplane: transport proxy error (code passed through, message sanitized): %v", err)
	var ce *connect.Error
	if errors.As(err, &ce) {
		// ce.Code().String() is derived from the enum, not from the inner message.
		return connect.NewError(ce.Code(), errors.New("sandbox call failed: "+ce.Code().String()))
	}
	switch model.ErrorClassOf(err) {
	case model.ErrorInvalidArgument:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid sandbox request"))
	case model.ErrorCapacity:
		return connect.NewError(connect.CodeResourceExhausted, errors.New("sandbox capacity exhausted"))
	case model.ErrorNotFound:
		return connect.NewError(connect.CodeNotFound, errors.New("sandbox object not found"))
	case model.ErrorStaleEpoch:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("stale placement epoch"))
	case model.ErrorIdempotencyConflict:
		return connect.NewError(connect.CodeAlreadyExists, errors.New("sandbox request idempotency conflict"))
	case model.ErrorConflict, model.ErrorInProgress, model.ErrorCursorGap:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox request conflicts with current state"))
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("sandbox transport unavailable"))
}

// clientFor resolves the transport client for the header-named sandbox. The
// endpoint resolution stays inside the mediator (the API error contract: placement never exposed).
func (e *Engine) clientFor(hdrGet func(string) string) (transport.Client, error) {
	id := model.SandboxID(hdrGet(sandboxIDHeader))
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing "+sandboxIDHeader+" header"))
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return cl, nil
}

// mutatingClientFor is clientFor plus the FS-first pause and resume pause drain barrier, for handlers
// that WRITE. The release func must be deferred by the caller.
//
// Only writes take the barrier. Reads (Read/Stat/List, process.List, Connect)
// deliberately do not: they cannot truncate anything, and blocking them would
// stall observability exactly when a pause is in progress — the moment an operator
// most wants to look.
//
// process.Start / SendInput / SendSignal also deliberately do NOT take it. They
// mutate guest state, but not through an engine-issued write with a truncate
// window, and gating a long-lived Start stream on a drain would deadlock the
// barrier against itself: the stream is in-flight until the process exits, so the
// drain would wait for work that is itself waiting for the drain.
func (e *Engine) mutatingClientFor(hdrGet func(string) string, op string) (transport.Client, func(), error) {
	id := model.SandboxID(hdrGet(sandboxIDHeader))
	if id == "" {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing "+sandboxIDHeader+" header"))
	}
	cl, err := e.Mediator.Client(id)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeNotFound, err)
	}
	release, ok := e.Mediator.Enter(id, op)
	if !ok {
		// A pause is draining: refuse rather than queue. Queueing would starve the
		// drain — the thing it is waiting for would be arriving work.
		return nil, nil, connect.NewError(connect.CodeUnavailable,
			errors.New("sandbox is pausing; retry after it settles"))
	}
	return cl, release, nil
}

func (e *Engine) validateRequestEpoch(ctx context.Context, sandbox string, epoch uint64) error {
	if epoch == 0 {
		return nil
	}
	return e.Mediator.ValidatePlacementEpoch(ctx, model.SandboxID(sandbox), model.PlacementEpoch(epoch))
}

// --- ProcessService proxy ----------------------------------------------------

func (p *processProxy) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	if req == nil || req.Msg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing start request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return err
	}
	m := req.Msg
	sandboxID := model.SandboxID(req.Header().Get(sandboxIDHeader))
	epoch := model.PlacementEpoch(m.PlacementEpoch)
	if epoch == 0 {
		placement, perr := p.e.Mediator.Placement(ctx, sandboxID)
		if perr != nil {
			return proxyErr(perr)
		}
		epoch = placement.Epoch
		m.PlacementEpoch = uint64(epoch)
	}
	if err := p.e.validateRequestEpoch(ctx, string(sandboxID), uint64(epoch)); err != nil {
		return proxyErr(err)
	}
	// A reservation with an idempotency key or supplied handle is observable
	// retry identity. Serialize its reservation-to-dispatch window so a
	// concurrent retry cannot observe a row while a canceled request removes it.
	identity := processStartIdentity(sandboxID, epoch, m.IdempotencyKey, m.ProcessHandle)
	// A keyed start has a deterministic reserved handle even when the caller
	// omits one. Lock that derived handle as well, so a retry that learned the
	// handle from the durable Process list cannot race cancellation/cleanup via
	// a different alias.
	reservedHandle := m.ProcessHandle
	if reservedHandle == "" && m.IdempotencyKey != "" {
		reservedHandle = reservedProcessHandle(sandboxID, epoch, m.IdempotencyKey)
	}
	handleIdentity := processStartHandleIdentity(sandboxID, epoch, reservedHandle)
	releaseStart, err := p.e.acquireProcessStart(ctx, identity, handleIdentity)
	if err != nil {
		return err
	}
	releasedStart := false
	release := func() {
		if !releasedStart && releaseStart != nil {
			releasedStart = true
			releaseStart()
		}
	}
	defer release()
	// Reserve the logical Process row before invoking the guest. The guest
	// supervisor receives the same opaque handle, so a disconnect before the
	// first output frame cannot leave a running process without a control-plane
	// identity.
	requestHash := processStartRequestHash(m)
	reserved, created, err := p.reserveProcess(ctx, sandboxID, epoch, m.IdempotencyKey, m.ProcessHandle, requestHash)
	if err != nil {
		return proxyErr(err)
	}
	// A retry that finds a materialized logical process must reconnect to that
	// handle; starting again with the same handle would duplicate the guest
	// workload. A `starting` reservation is the one case where the guest start
	// still needs to be issued (the supervisor's start-flight CAS arbitrates
	// concurrent callers).
	if !created && reserved.State != model.ProcessStarting {
		release()
		stable, ok := cl.(transport.StableProcessClient)
		if !ok {
			return proxyErr(errors.New("stable process reconnect is unavailable"))
		}
		ch, err := stable.ConnectHandle(ctx, string(reserved.ID), uint64(reserved.PlacementEpoch), 0)
		if err != nil {
			return proxyErr(err)
		}
		return p.forwardProcessEvents(ctx, string(sandboxID), ch, stream, reserved)
	}
	spec := transport.StartSpec{Cmd: m.Cmd, Args: m.Args, Env: m.Env, Cwd: m.Cwd, IdempotencyKey: m.IdempotencyKey, PlacementEpoch: uint64(epoch), ProcessHandle: string(reserved.ID)}
	if m.Pty != nil {
		spec.Pty = &transport.PtySize{Rows: uint16(m.Pty.Rows), Cols: uint16(m.Pty.Cols)}
	}
	// A cancellation observed before dispatch is unambiguous: no guest request
	// could have forked, so remove only this attempt's reservation. Once the
	// transport call begins, cancellation is ambiguous and the reservation is
	// retained for handle-based recovery.
	if err := ctx.Err(); err != nil {
		if canceledReservationMayBeDeleted(created, m.IdempotencyKey, m.ProcessHandle) {
			deleteProcessReservation(p, reserved.ID)
		}
		return err
	}
	ch, err := cl.Start(ctx, spec)
	if err != nil {
		// A failed RPC before a stream exists did not provide a start frame. A
		// newly reserved row is removed so a definite admission failure does not
		// leave a permanent starting record; an existing row is retained because
		// the guest may already own the idempotent process.
		if created && definiteProcessStartFailure(err) {
			deleteProcessReservation(p, reserved.ID)
		}
		// Keep the identity gate held through cleanup. A retry must not observe
		// the starting row between the guest error and its conditional removal.
		release()
		return proxyErr(err)
	}
	// The guest has accepted the start and returned a stream. Release before
	// forwarding so retries can reconnect while this caller drains output.
	release()
	return p.forwardProcessEvents(ctx, string(sandboxID), ch, stream, reserved)
}

func processStartIdentity(sandbox model.SandboxID, epoch model.PlacementEpoch, key, handle string) string {
	if key == "" && handle == "" {
		return ""
	}
	kind, value := "key", key
	if key == "" {
		kind, value = "handle", handle
	}
	return fmt.Sprintf("%s\x00%d\x00%s:%s", sandbox, uint64(epoch), kind, value)
}

func processStartHandleIdentity(sandbox model.SandboxID, epoch model.PlacementEpoch, handle string) string {
	if handle == "" {
		return ""
	}
	return fmt.Sprintf("%s\x00%d\x00handle:%s", sandbox, uint64(epoch), handle)
}

func canceledReservationMayBeDeleted(created bool, key, handle string) bool {
	return created && key == "" && handle == ""
}

func deleteProcessReservation(p *processProxy, id model.ProcessID) {
	if p == nil || p.e == nil || p.e.Store == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.e.Store.DeleteProcess(cleanupCtx, id); err != nil && !errors.Is(err, store.ErrProcessNotFound) {
		log.Printf("dataplane: remove failed process reservation %s: %v", id, err)
	}
}

func definiteProcessStartFailure(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Code() {
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodePermissionDenied:
		return true
	default:
		// Unavailable/internal failures are ambiguous: the supervisor may have
		// forked the workload before the frontend lost its stream. Retain the
		// reservation so a retry can reconnect instead of orphaning the handle.
		return false
	}
}

// reserveProcess chooses a stable handle for a Start request and records its
// initial starting state atomically through the Store seam. Idempotency keys
// are scoped by Sandbox and Placement epoch; an empty key gets an opaque
// random handle because there is no retry identity to derive from.
func (p *processProxy) reserveProcess(ctx context.Context, sandbox model.SandboxID, epoch model.PlacementEpoch, key, requestedHandle, requestHash string) (*model.Process, bool, error) {
	handle := requestedHandle
	if handle == "" {
		handle = reservedProcessHandle(sandbox, epoch, key)
	}
	// Resolve an existing key before deriving/creating the candidate handle.
	// This catches a retry that supplies a different handle: creating under that
	// new ID first would otherwise bypass the key's uniqueness constraint.
	if key != "" {
		if listed, err := p.e.Store.ListProcesses(ctx, sandbox); err != nil {
			return nil, false, err
		} else {
			for _, existing := range listed {
				if existing.PlacementEpoch != epoch || existing.StartKey != model.IdempotencyKey(key) {
					continue
				}
				if requestHash != "" && existing.StartRequestHash != requestHash {
					return nil, false, model.ErrIdempotencyConflict
				}
				if requestedHandle != "" && existing.ID != model.ProcessID(requestedHandle) {
					return nil, false, model.ErrIdempotencyConflict
				}
				return &existing, false, nil
			}
		}
	}
	for attempt := 0; ; attempt++ {
		if key == "" && requestedHandle == "" && attempt > 0 {
			handle = reservedProcessHandle(sandbox, epoch, "")
		}
		now := time.Now()
		candidate := model.Process{ID: model.ProcessID(handle), Sandbox: sandbox, PlacementEpoch: epoch, State: model.ProcessStarting, StartedAt: now, OutputCursor: model.ProcessOutputCursor(model.ProcessID(handle), epoch, 0), StartKey: model.IdempotencyKey(key), StartRequestHash: requestHash}
		if err := p.e.Mediator.CreateProcess(ctx, candidate); err == nil {
			return &candidate, true, nil
		} else if !errors.Is(err, store.ErrProcessConflict) {
			return nil, false, err
		}
		existing, gerr := p.e.Mediator.Process(ctx, candidate.ID)
		// A retry can carry a caller-supplied handle that differs from the
		// first request's handle. Resolve the existing logical row by its
		// scoped idempotency key so the conflict is deterministic rather than
		// creating a second row under the new handle.
		if gerr != nil && key != "" {
			if listed, lerr := p.e.Store.ListProcesses(ctx, sandbox); lerr == nil {
				for _, candidateProcess := range listed {
					if candidateProcess.PlacementEpoch == epoch && candidateProcess.StartKey == model.IdempotencyKey(key) {
						existing, gerr = candidateProcess, nil
						break
					}
				}
			}
		}
		if gerr != nil {
			if key == "" && requestedHandle == "" && attempt < 3 {
				continue
			}
			return nil, false, gerr
		}
		if existing.Sandbox != sandbox || existing.PlacementEpoch != epoch {
			if requestedHandle != "" {
				// A caller-supplied handle is globally opaque identity. A row
				// belonging to another sandbox or epoch is a handle collision,
				// not permission to reinterpret it as a stale placement.
				return nil, false, model.ErrConflict
			}
			return nil, false, model.ErrStaleEpoch
		}
		if requestHash != "" && existing.StartRequestHash != requestHash {
			return nil, false, model.ErrIdempotencyConflict
		}
		if requestedHandle != "" && existing.ID != model.ProcessID(requestedHandle) {
			return nil, false, model.ErrIdempotencyConflict
		}
		return &existing, false, nil
	}
}

func processStartRequestHash(m *v1.StartRequest) string {
	if m == nil {
		return ""
	}
	args := append([]string{}, m.Args...)
	env := append([]string{}, m.Env...)
	canonical := struct {
		Cmd            string      `json:"cmd"`
		Args           []string    `json:"args"`
		Env            []string    `json:"env"`
		Cwd            string      `json:"cwd"`
		Pty            *v1.PtySize `json:"pty,omitempty"`
		PlacementEpoch uint64      `json:"placement_epoch"`
	}{m.Cmd, args, env, m.Cwd, m.Pty, m.PlacementEpoch}
	hash, _ := model.RequestHash(canonical)
	return hash
}

func reservedProcessHandle(sandbox model.SandboxID, epoch model.PlacementEpoch, key string) string {
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err == nil {
			return "proc_" + hex.EncodeToString(b[:])
		}
		return "proc_unknown"
	}
	sum := sha256.Sum256([]byte(string(sandbox) + "\x00" + fmt.Sprint(uint64(epoch)) + "\x00" + key))
	return "proc_" + hex.EncodeToString(sum[:12])
}

func (p *processProxy) Connect(ctx context.Context, req *connect.Request[v1.ConnectRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	if req == nil || req.Msg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing connect request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return err
	}
	var ch <-chan transport.ProcessEvent
	var durableProc *model.Process
	if req.Msg.ProcessHandle != "" {
		id := model.SandboxID(req.Header().Get(sandboxIDHeader))
		proc, perr := p.e.Mediator.Process(ctx, model.ProcessID(req.Msg.ProcessHandle))
		if perr != nil {
			return proxyErr(perr)
		}
		if proc.Sandbox != id {
			return connect.NewError(connect.CodeNotFound, errors.New("process handle not found"))
		}
		durableProc = &proc
		if req.Msg.PlacementEpoch > 0 && req.Msg.PlacementEpoch != uint64(proc.PlacementEpoch) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("stale placement epoch"))
		}
		if stable, ok := cl.(transport.StableProcessClient); ok {
			ch, err = stable.ConnectHandle(ctx, req.Msg.ProcessHandle, uint64(proc.PlacementEpoch), req.Msg.AfterSequence)
		} else {
			ch, err = cl.Connect(ctx, proc.PID)
		}
	} else {
		if err := p.e.validateRequestEpoch(ctx, req.Header().Get(sandboxIDHeader), req.Msg.PlacementEpoch); err != nil {
			return proxyErr(err)
		}
		ch, err = cl.Connect(ctx, int(req.Msg.Pid))
	}
	if err != nil {
		return proxyErr(err)
	}
	// The durable row is the expected stream identity. Passing it through lets
	// forwarding validate every replay/live frame, not only a Start frame, and
	// persist terminal/output updates against the requested logical handle.
	return p.forwardProcessEvents(ctx, req.Header().Get(sandboxIDHeader), ch, stream, durableProc)
}

func (p *processProxy) StreamInput(ctx context.Context, stream *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	if stream == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing stream input request"))
	}
	cl, err := p.e.clientFor(stream.RequestHeader().Get)
	if err != nil {
		return nil, err
	}
	var in transport.InputWriter
	for stream.Receive() {
		m := stream.Msg()
		if in == nil {
			if err := p.e.validateRequestEpoch(ctx, stream.RequestHeader().Get(sandboxIDHeader), m.PlacementEpoch); err != nil {
				return nil, proxyErr(err)
			}
			if m.ProcessHandle != "" {
				if stable, ok := cl.(transport.StableProcessClient); ok {
					in, err = stable.StreamInputHandle(ctx, m.ProcessHandle, m.PlacementEpoch)
				} else {
					in, err = cl.StreamInput(ctx, int(m.Pid))
				}
			} else {
				in, err = cl.StreamInput(ctx, int(m.Pid))
			}
			if err != nil {
				return nil, proxyErr(err)
			}
		}
		if err := in.Send(m.Data, m.PtyIn); err != nil {
			_ = in.Close()
			return nil, proxyErr(err)
		}
	}
	if in != nil {
		if err := in.Close(); err != nil {
			return nil, proxyErr(err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StreamInputResponse{}), nil
}

func (p *processProxy) SendInput(ctx context.Context, req *connect.Request[v1.SendInputRequest]) (*connect.Response[v1.SendInputResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing send input request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := p.e.validateRequestEpoch(ctx, req.Header().Get(sandboxIDHeader), req.Msg.PlacementEpoch); err != nil {
		return nil, proxyErr(err)
	}
	var callErr error
	if req.Msg.ProcessHandle != "" {
		if stable, ok := cl.(transport.StableProcessClient); ok {
			callErr = stable.SendInputHandle(ctx, req.Msg.ProcessHandle, req.Msg.PlacementEpoch, req.Msg.Data, req.Msg.PtyIn)
		} else {
			callErr = cl.SendInput(ctx, int(req.Msg.Pid), req.Msg.Data, req.Msg.PtyIn)
		}
	} else {
		callErr = cl.SendInput(ctx, int(req.Msg.Pid), req.Msg.Data, req.Msg.PtyIn)
	}
	if callErr != nil {
		return nil, proxyErr(callErr)
	}
	return connect.NewResponse(&v1.SendInputResponse{}), nil
}

// signalPidErr mirrors the guest's message verbatim (cmd/sandboxd/transport.go) so a caller
// gets the same explanation whichever end refuses. DUPLICATED rather than shared through a
// leaf: what the two ends must agree on is the RULE, and the rule is one comparison — where
// guestport's bar is a fact neither side can derive alone (the port, the upload prefix). A
// test pins the copies together instead (dataplane_signal_test.go).
const signalPidErr = "pid must be > 0: SendSignal carries DIRECTED signals only " +
	"(one process, defaulting to its group). pid <= 0 is POSIX fan-out — " +
	"kill(0)/kill(-1) address a whole group or the whole pid namespace — and " +
	"container-level fan-out belongs to guest PID 1, not the transport channel " +
	"(guest supervision / the guest startup topology)"

// SendSignal proxies a DIRECTED signal. The pid > 0 check here is fail-fast only
// (one less RPC): the authoritative check is the guest's, because the guest startup topology's premise
// is that the guest can write its own fs and so must validate for itself. pid <= 0
// is POSIX fan-out, which belongs to guest PID 1 rather than this channel
// (guest supervision).
func (p *processProxy) SendSignal(ctx context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing send signal request"))
	}
	if req.Msg.ProcessHandle == "" && req.Msg.Pid <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(signalPidErr))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := p.e.validateRequestEpoch(ctx, req.Header().Get(sandboxIDHeader), req.Msg.PlacementEpoch); err != nil {
		return nil, proxyErr(err)
	}
	var callErr error
	if req.Msg.ProcessHandle != "" {
		if stable, ok := cl.(transport.StableProcessClient); ok {
			callErr = stable.SendSignalHandle(ctx, req.Msg.ProcessHandle, req.Msg.PlacementEpoch, int(req.Msg.Signal))
		} else {
			callErr = cl.SendSignal(ctx, int(req.Msg.Pid), int(req.Msg.Signal))
		}
	} else {
		callErr = cl.SendSignal(ctx, int(req.Msg.Pid), int(req.Msg.Signal))
	}
	if callErr != nil {
		return nil, proxyErr(callErr)
	}
	return connect.NewResponse(&v1.SendSignalResponse{}), nil
}

func (p *processProxy) Update(ctx context.Context, req *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing update request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := p.e.validateRequestEpoch(ctx, req.Header().Get(sandboxIDHeader), req.Msg.PlacementEpoch); err != nil {
		return nil, proxyErr(err)
	}
	if req.Msg.Pty == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("Update requires pty size"))
	}
	sz := transport.PtySize{Rows: uint16(req.Msg.Pty.Rows), Cols: uint16(req.Msg.Pty.Cols)}
	var callErr error
	if req.Msg.ProcessHandle != "" {
		if stable, ok := cl.(transport.StableProcessClient); ok {
			callErr = stable.UpdateHandle(ctx, req.Msg.ProcessHandle, req.Msg.PlacementEpoch, sz)
		} else {
			callErr = cl.Update(ctx, int(req.Msg.Pid), sz)
		}
	} else {
		callErr = cl.Update(ctx, int(req.Msg.Pid), sz)
	}
	if callErr != nil {
		return nil, proxyErr(callErr)
	}
	return connect.NewResponse(&v1.UpdateResponse{}), nil
}

func (p *processProxy) CloseStdin(ctx context.Context, req *connect.Request[v1.CloseStdinRequest]) (*connect.Response[v1.CloseStdinResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing close stdin request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := p.e.validateRequestEpoch(ctx, req.Header().Get(sandboxIDHeader), req.Msg.PlacementEpoch); err != nil {
		return nil, proxyErr(err)
	}
	var callErr error
	if req.Msg.ProcessHandle != "" {
		if stable, ok := cl.(transport.StableProcessClient); ok {
			callErr = stable.CloseStdinHandle(ctx, req.Msg.ProcessHandle, req.Msg.PlacementEpoch)
		} else {
			callErr = cl.CloseStdin(ctx, int(req.Msg.Pid))
		}
	} else {
		callErr = cl.CloseStdin(ctx, int(req.Msg.Pid))
	}
	if callErr != nil {
		return nil, proxyErr(callErr)
	}
	return connect.NewResponse(&v1.CloseStdinResponse{}), nil
}

func (p *processProxy) List(ctx context.Context, req *connect.Request[v1.ListRequest]) (*connect.Response[v1.ListResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing list request"))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	pids, err := cl.List(ctx)
	if err != nil {
		return nil, proxyErr(err)
	}
	out := make([]int32, 0, len(pids))
	for _, p := range pids {
		out = append(out, int32(p))
	}
	resp := &v1.ListResponse{Pids: out}
	if stable, ok := cl.(transport.StableProcessClient); ok {
		infos, serr := stable.ListStable(ctx)
		if serr != nil {
			return nil, proxyErr(serr)
		}
		resp.Processes = make([]*v1.ProcessInfo, 0, len(infos))
		for _, info := range infos {
			pi := &v1.ProcessInfo{ProcessHandle: info.Handle, Pid: int32(info.PID), State: info.State, PlacementEpoch: info.Epoch, OutputSequence: info.OutputSequence}
			if info.ExitCode != nil {
				code := int32(*info.ExitCode)
				pi.ExitCode = &code
			}
			resp.Processes = append(resp.Processes, pi)
		}
	}
	return connect.NewResponse(resp), nil
}

// --- FsService proxy ---------------------------------------------------------

func (f *fsProxy) Read(ctx context.Context, req *connect.Request[v1.ReadRequest]) (*connect.Response[v1.ReadResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing read request"))
	}
	cl, err := f.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	data, err := cl.Read(ctx, req.Msg.Path, req.Msg.Offset, req.Msg.Length)
	if err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.ReadResponse{Data: data}), nil
}

func (f *fsProxy) Write(ctx context.Context, req *connect.Request[v1.WriteRequest]) (*connect.Response[v1.WriteResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing write request"))
	}
	// Barrier-gated: this is THE call the drain exists for. The guest's write is
	// truncate-then-write, so a pause landing mid-call empties the file (the transport contract).
	cl, release, err := f.e.mutatingClientFor(req.Header().Get, "fs.Write "+req.Msg.Path)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := cl.Write(ctx, req.Msg.Path, req.Msg.Data); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.WriteResponse{BytesWritten: int64(len(req.Msg.Data))}), nil
}

// entryToWire re-encodes a domain EntryInfo for the outer edge. A straight projection: the
// outer wire is the SAME proto as the inner one (the transport contract one-wire), and this exists only
// because the proxy speaks the domain Client interface in between.
func entryToWire(e transport.EntryInfo) *v1.EntryInfo {
	out := &v1.EntryInfo{
		Name:          e.Name,
		Path:          e.Path,
		Type:          v1.EntryType_ENTRY_TYPE_UNSPECIFIED,
		Size:          e.Size,
		Mode:          e.Mode,
		MtimeUnixNano: e.MTime.UnixNano(),
	}
	switch e.Type {
	case transport.EntryFile:
		out.Type = v1.EntryType_ENTRY_TYPE_FILE
	case transport.EntryDirectory:
		out.Type = v1.EntryType_ENTRY_TYPE_DIRECTORY
	case transport.EntrySymlink:
		out.Type = v1.EntryType_ENTRY_TYPE_SYMLINK
	}
	if e.SymlinkTarget != "" {
		t := e.SymlinkTarget
		out.SymlinkTarget = &t
	}
	return out
}

func (f *fsProxy) Stat(ctx context.Context, req *connect.Request[v1.StatRequest]) (*connect.Response[v1.StatResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing stat request"))
	}
	cl, err := f.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	e, err := cl.Stat(ctx, req.Msg.Path)
	if err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.StatResponse{Entry: entryToWire(e)}), nil
}

func (f *fsProxy) List(ctx context.Context, req *connect.Request[v1.FsServiceListRequest]) (*connect.Response[v1.FsServiceListResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing list request"))
	}
	cl, err := f.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	entries, err := cl.ListDir(ctx, req.Msg.Path, req.Msg.Depth)
	if err != nil {
		return nil, proxyErr(err)
	}
	out := make([]*v1.EntryInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryToWire(e))
	}
	return connect.NewResponse(&v1.FsServiceListResponse{Entries: out}), nil
}

func (f *fsProxy) MakeDir(ctx context.Context, req *connect.Request[v1.MakeDirRequest]) (*connect.Response[v1.MakeDirResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing make dir request"))
	}
	cl, release, err := f.e.mutatingClientFor(req.Header().Get, "fs.MakeDir "+req.Msg.Path)
	if err != nil {
		return nil, err
	}
	defer release()
	e, err := cl.MakeDir(ctx, req.Msg.Path)
	if err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.MakeDirResponse{Entry: entryToWire(e)}), nil
}

func (f *fsProxy) Remove(ctx context.Context, req *connect.Request[v1.RemoveRequest]) (*connect.Response[v1.RemoveResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing remove request"))
	}
	cl, release, err := f.e.mutatingClientFor(req.Header().Get, "fs.Remove "+req.Msg.Path)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := cl.Remove(ctx, req.Msg.Path); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.RemoveResponse{}), nil
}

func (f *fsProxy) Move(ctx context.Context, req *connect.Request[v1.MoveRequest]) (*connect.Response[v1.MoveResponse], error) {
	if req == nil || req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing move request"))
	}
	cl, release, err := f.e.mutatingClientFor(req.Header().Get, "fs.Move "+req.Msg.Source)
	if err != nil {
		return nil, err
	}
	defer release()
	e, err := cl.Move(ctx, req.Msg.Source, req.Msg.Destination)
	if err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.MoveResponse{Entry: entryToWire(e)}), nil
}

func (f *fsProxy) Watch(ctx context.Context, req *connect.Request[v1.WatchRequest], stream *connect.ServerStream[v1.WatchEvent]) error {
	if req == nil || req.Msg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing watch request"))
	}
	id := model.SandboxID(req.Header().Get(sandboxIDHeader))
	if id == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing "+sandboxIDHeader+" header"))
	}
	pl, perr := f.e.Mediator.Placement(ctx, id)
	if perr != nil {
		return proxyErr(perr)
	}
	epoch := req.Msg.PlacementEpoch
	if epoch == 0 {
		epoch = uint64(pl.Epoch)
	} else if err := f.e.Mediator.ValidatePlacementEpoch(ctx, id, model.PlacementEpoch(epoch)); err != nil {
		return proxyErr(err)
	}
	cl, err := f.e.clientFor(req.Header().Get)
	if err != nil {
		return err
	}
	watcher, ok := cl.(transport.WatchClient)
	if !ok {
		return connect.NewError(connect.CodeUnimplemented, errors.New("filesystem watch is unavailable"))
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := watcher.Watch(watchCtx, req.Msg.Path, req.Msg.Recursive, epoch, req.Msg.AfterSequence)
	if err != nil {
		return proxyErr(err)
	}
	// The inner stream can remain connected while the control plane advances the
	// Placement epoch (for example, during a pause/resume race). A periodic
	// control-plane check makes the reset observable without waiting for the
	// transport child to close its socket.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastReset := false
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if lastReset {
					return nil
				}
				if current, err := f.e.Mediator.Placement(ctx, id); err == nil && current.Epoch != model.PlacementEpoch(epoch) {
					// The old transport disappeared during Pause/Resume. Make the epoch
					// reset visible before closing so a watcher can rescan on the new
					// placement rather than interpreting a disconnect as an empty stream.
					_ = stream.Send(&v1.WatchEvent{PlacementEpoch: uint64(current.Epoch), Reset_: true, ResetReason: "placement"})
				} else {
					// A transport-child restart can close the inner stream while
					// retaining the same Placement epoch. The guest journal remains
					// resumable, but the caller must reconnect to resume delivery.
					_ = stream.Send(&v1.WatchEvent{PlacementEpoch: uint64(epoch), Reset_: true, ResetReason: "transport"})
				}
				return nil
			}
			if ev.Operation == "error" {
				if ev.Err != nil {
					// A raw transport termination is the expected shape when the
					// guest transport child is restarted in place. Preserve the
					// placement-scoped stream contract by making that reconnect
					// boundary explicit; typed cursor-gap errors still pass through
					// as resource-exhausted below.
					var ce *connect.Error
					// Only an unclassified/non-wire Unavailable means the inner
					// transport endpoint disappeared and is safe to represent as a
					// reconnect reset. A wire-sent Unavailable is an actual guest
					// verdict (for example, a corrupt watch journal) and must remain
					// an error rather than being mislabeled as a recoverable restart.
					if !errors.As(ev.Err, &ce) || (ce.Code() == connect.CodeUnavailable && !connect.IsWireError(ev.Err)) {
						_ = stream.Send(&v1.WatchEvent{PlacementEpoch: uint64(epoch), Reset_: true, ResetReason: "transport"})
						return nil
					}
					return proxyErr(ev.Err)
				}
				return connect.NewError(connect.CodeUnavailable, errors.New("filesystem watch unavailable"))
			}
			if ev.Epoch != 0 && ev.Epoch != epoch {
				lastReset = true
				_ = stream.Send(&v1.WatchEvent{PlacementEpoch: ev.Epoch, Reset_: true, ResetReason: "placement"})
				return nil
			}
			if current, err := f.e.Mediator.Placement(ctx, id); err == nil && (current.Epoch != model.PlacementEpoch(epoch) || current.State != model.PlacementActive) {
				lastReset = true
				_ = stream.Send(&v1.WatchEvent{PlacementEpoch: uint64(current.Epoch), Reset_: true, ResetReason: "placement"})
				return nil
			}
			kind := v1.EntryType_ENTRY_TYPE_UNSPECIFIED
			switch ev.Type {
			case transport.EntryFile:
				kind = v1.EntryType_ENTRY_TYPE_FILE
			case transport.EntryDirectory:
				kind = v1.EntryType_ENTRY_TYPE_DIRECTORY
			case transport.EntrySymlink:
				kind = v1.EntryType_ENTRY_TYPE_SYMLINK
			}
			if err := stream.Send(&v1.WatchEvent{Path: ev.Path, OldPath: ev.OldPath, Operation: ev.Operation, Type: kind, Size: ev.Size, MtimeUnixNano: ev.MTime.UnixNano(), Sequence: ev.Sequence, PlacementEpoch: ev.Epoch, Overflow: ev.Overflow, Reset_: ev.Reset, ResetReason: ev.ResetReason}); err != nil {
				return err
			}
			lastReset = ev.Reset && (ev.ResetReason == "placement" || ev.ResetReason == "transport")
		case <-ticker.C:
			current, err := f.e.Mediator.Placement(ctx, id)
			if err != nil {
				continue
			}
			if current.Epoch != model.PlacementEpoch(epoch) || current.State != model.PlacementActive {
				_ = stream.Send(&v1.WatchEvent{PlacementEpoch: uint64(current.Epoch), Reset_: true, ResetReason: "placement"})
				return nil
			}
		}
	}
}

// forwardEvents pumps the domain output channel to the outer proto server-stream
// (transparent passthrough: the outer edge sees the exact same frames the inner
// sandboxd produced — one wire, the transport contract).
func forwardEvents(stream *connect.ServerStream[v1.ProcessEvent], ch <-chan transport.ProcessEvent) error {
	for ev := range ch {
		var pe v1.ProcessEvent
		switch ev.Kind {
		case "start":
			pe.ProcessHandle, pe.PlacementEpoch, pe.Sequence, pe.Replay, pe.Gap, pe.ResetReason = ev.Handle, ev.Epoch, ev.Sequence, ev.Replay, ev.Gap, ev.ResetReason
			pe.Kind = &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(ev.PID), ProcessHandle: ev.Handle, PlacementEpoch: ev.Epoch}}
		case "data":
			pe.ProcessHandle, pe.PlacementEpoch, pe.Sequence, pe.Replay, pe.Gap, pe.ResetReason = ev.Handle, ev.Epoch, ev.Sequence, ev.Replay, ev.Gap, ev.ResetReason
			pe.Kind = &v1.ProcessEvent_Data{Data: &v1.DataEvent{Stream: ev.Stream, Data: ev.Data}}
		case "end":
			pe.ProcessHandle, pe.PlacementEpoch, pe.Sequence, pe.Replay, pe.Gap, pe.ResetReason = ev.Handle, ev.Epoch, ev.Sequence, ev.Replay, ev.Gap, ev.ResetReason
			pe.Kind = &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(ev.ExitCode)}}
		case "keepalive":
			pe.ProcessHandle, pe.PlacementEpoch, pe.Sequence, pe.Replay, pe.Gap, pe.ResetReason = ev.Handle, ev.Epoch, ev.Sequence, ev.Replay, ev.Gap, ev.ResetReason
			pe.Kind = &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}}
		case "error":
			// The inner stream ended WITHOUT an end frame — typically an overflow disconnect
			// overflow disconnect. Terminate the outer stream with the same error rather than
			// closing it cleanly, which would tell the external client "the process finished".
			return proxyErr(ev.Err)
		default:
			continue
		}
		if err := stream.Send(&pe); err != nil {
			return err
		}
	}
	return nil
}

// processEventSender is the narrow stream operation used by the forwarding
// projection. Keeping the seam small lets the terminal/replay contract be
// tested without constructing a live Connect server stream.
type processEventSender interface {
	Send(*v1.ProcessEvent) error
}

func (p *processProxy) forwardProcessEvents(ctx context.Context, sandbox string, ch <-chan transport.ProcessEvent, stream processEventSender, reserved *model.Process) error {
	var rec *model.Process = reserved
	var localSeq uint64
	var streamEpoch uint64
	var streamHandle string
	if rec != nil {
		streamHandle = string(rec.ID)
		streamEpoch = uint64(rec.PlacementEpoch)
	}
	lookupDone := rec != nil
	for ev := range ch {
		if ev.Kind != "error" {
			// Bind every frame to the same logical process and placement. Older
			// transports may omit the envelope fields on data/end frames, so fill
			// omissions from the identity established by Start or the first frame.
			var err error
			streamHandle, streamEpoch, err = normalizeProcessEventIdentity(&ev, streamHandle, streamEpoch)
			if err != nil {
				return err
			}
			if streamEpoch == 0 && streamHandle != "" {
				// A legacy guest may carry the stable handle but omit the epoch on
				// its first frame. Resolve the current placement once before any
				// durable Process update; subsequent frames are then fenced by the
				// established epoch.
				placement, placementErr := p.e.Mediator.Placement(ctx, model.SandboxID(sandbox))
				if placementErr != nil {
					return proxyErr(placementErr)
				}
				streamEpoch = uint64(placement.Epoch)
				ev.Epoch = streamEpoch
			}
			// A PID-based Connect has no reserved row in this proxy call, but a
			// stable handle in its first frame should still update the durable row
			// when one exists. Resolve it once; absence is valid for legacy guests.
			if !lookupDone && streamHandle != "" {
				lookupDone = true
				if existing, lookupErr := p.e.Mediator.Process(ctx, model.ProcessID(streamHandle)); lookupErr == nil {
					if existing.Sandbox != model.SandboxID(sandbox) {
						return connect.NewError(connect.CodeNotFound, errors.New("process handle not found"))
					}
					if streamEpoch != 0 && existing.PlacementEpoch != model.PlacementEpoch(streamEpoch) {
						return connect.NewError(connect.CodeFailedPrecondition, errors.New("stale placement epoch"))
					}
					rec = &existing
				}
			}
		}
		if ev.Sequence > localSeq {
			localSeq = ev.Sequence
		} else if ev.Sequence == 0 && (ev.Kind == "data" || ev.Kind == "end") {
			// Legacy transports may omit sequence numbers. Only that explicit
			// absence gets a synthetic local increment; a duplicate or stale
			// numbered frame must never manufacture cursor progress.
			localSeq++
		}
		if ev.Handle != "" {
			switch ev.Kind {
			case "start":
				pid := ev.PID
				epoch := ev.Epoch
				if epoch == 0 && rec != nil {
					epoch = uint64(rec.PlacementEpoch)
				}
				// A stable Connect to an exited handle replays the guest's
				// synthetic start marker before its retained output and terminal
				// frame. The durable terminal row is authoritative; projecting
				// that marker as running would attempt to resurrect it and is
				// correctly rejected by the Store fence. Advance only a newer
				// cursor while preserving terminal metadata.
				if rec != nil && rec.State == model.ProcessExited {
					if localSeq > rec.OutputCursor.Sequence {
						rec.OutputCursor.Sequence = localSeq
						if err := p.e.Mediator.UpdateProcess(ctx, *rec); err != nil {
							return processProjectionError("persist process replay start", rec.ID, err)
						}
					}
					break
				}
				if rec != nil && (rec.State == model.ProcessPaused || rec.State == model.ProcessReclaimed) {
					return processProjectionError("persist process replay start", rec.ID, store.ErrProcessConflict)
				}
				now := time.Now()
				if rec == nil {
					candidate := model.Process{ID: model.ProcessID(ev.Handle), Sandbox: model.SandboxID(sandbox), PlacementEpoch: model.PlacementEpoch(epoch), PID: pid, State: model.ProcessRunning, StartedAt: now, OutputCursor: model.ProcessOutputCursor(model.ProcessID(ev.Handle), model.PlacementEpoch(epoch), localSeq)}
					if err := p.e.Mediator.CreateProcess(ctx, candidate); err == nil {
						rec = &candidate
					} else if errors.Is(err, store.ErrProcessConflict) {
						existing, gerr := p.e.Mediator.Process(ctx, candidate.ID)
						if gerr != nil {
							return processProjectionError("resolve process start", candidate.ID, gerr)
						}
						if existing.Sandbox != candidate.Sandbox || existing.PlacementEpoch != candidate.PlacementEpoch {
							return connect.NewError(connect.CodeFailedPrecondition, errors.New("process stream placement mismatch"))
						}
						rec = &existing
					} else {
						return processProjectionError("persist process start", candidate.ID, err)
					}
				} else {
					if ev.Handle != string(rec.ID) {
						return connect.NewError(connect.CodeFailedPrecondition, errors.New("process stream handle mismatch"))
					}
					rec.PID = pid
					rec.State = model.ProcessRunning
					if localSeq > rec.OutputCursor.Sequence {
						rec.OutputCursor = model.ProcessOutputCursor(rec.ID, rec.PlacementEpoch, localSeq)
					}
					if err := p.e.Mediator.UpdateProcess(ctx, *rec); err != nil {
						return processProjectionError("persist process start", rec.ID, err)
					}
				}
			case "data":
				if rec != nil {
					if localSeq > rec.OutputCursor.Sequence {
						if err := persistProcessProgress(ctx, p, rec, localSeq); err != nil {
							return processProjectionError("persist process output", rec.ID, err)
						}
					}
				}
			case "end":
				if rec != nil {
					if err := persistProcessEnd(ctx, p, rec, ev, localSeq); err != nil {
						return processProjectionError("persist process exit", rec.ID, err)
					}
				}
			}
		}
		// Reuse the ordinary projection after persistence bookkeeping.
		if err := sendProcessEvent(stream, ev); err != nil {
			return err
		}
	}
	if streamEpoch > 0 {
		if current, err := p.e.Mediator.Placement(ctx, model.SandboxID(sandbox)); err == nil && uint64(current.Epoch) != streamEpoch {
			_ = stream.Send(&v1.ProcessEvent{ProcessHandle: streamHandle, PlacementEpoch: uint64(current.Epoch), Gap: true, ResetReason: "placement", Kind: &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}}})
		}
	}
	return nil
}

// applyProcessEnd projects an end frame onto the durable logical Process.
// Replay frames are advisory for terminal metadata: once the control plane has
// observed an exit, its timestamp, code, and historical PID are immutable. A
// replayed end therefore only advances the output cursor. Live ends transition
// starting/running records and preserve the existing PID when the transport
// omits it (the ordinary End wire variant carries only an exit code).
func applyProcessEnd(rec *model.Process, ev transport.ProcessEvent, now time.Time) error {
	if rec == nil {
		return nil
	}
	switch rec.State {
	case model.ProcessExited:
		// A durable exit may intentionally have an unknown code. The wire End
		// field is an int32 (whose zero is indistinguishable from an omitted
		// value to older transports), so never turn that unknown into a claimed
		// success merely because a replay carries 0. If a code was recorded,
		// conflicting replay data is rejected.
		if rec.ExitCode != nil && *rec.ExitCode != ev.ExitCode {
			return store.ErrProcessConflict
		}
		if ev.PID != 0 && rec.PID != 0 && ev.PID != rec.PID {
			return store.ErrProcessConflict
		}
		return nil
	case model.ProcessPaused, model.ProcessReclaimed:
		// A stale stream cannot turn a placement-loss or reclaimed record back
		// into a live/terminal process. The Store remains the final write fence.
		return store.ErrProcessConflict
	}
	code := ev.ExitCode
	rec.State = model.ProcessExited
	rec.ExitCode = &code
	if now.IsZero() {
		now = time.Now()
	}
	rec.ExitedAt = &now
	if ev.PID != 0 {
		rec.PID = ev.PID
	}
	return nil
}

// persistProcessEnd writes a terminal frame while tolerating the expected
// duplicate-end race between independent subscribers. The first subscriber
// wins the terminal timestamp; a concurrent replay with the same exit result
// may still advance the cursor, but a different result remains a conflict.
func persistProcessEnd(ctx context.Context, p *processProxy, rec *model.Process, ev transport.ProcessEvent, sequence uint64) error {
	if err := applyProcessEnd(rec, ev, time.Now()); err != nil {
		return err
	}
	if sequence > rec.OutputCursor.Sequence {
		rec.OutputCursor.Sequence = sequence
	}
	if err := p.e.Mediator.UpdateProcess(ctx, *rec); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrProcessConflict) {
		return err
	} else {
		// A second stream may have committed the same terminal result between
		// our read and write. Reconcile only that exact identity/result; never
		// turn a reclaimed or mismatched row into a successful replay.
		current, readErr := p.e.Mediator.Process(ctx, rec.ID)
		if readErr != nil || !sameProcessIdentity(*rec, current) || !sameProcessExit(current, ev) {
			return err
		}
		if sequence <= current.OutputCursor.Sequence {
			*rec = current
			return nil
		}
		current.OutputCursor.Sequence = sequence
		if updateErr := p.e.Mediator.UpdateProcess(ctx, current); updateErr != nil {
			return updateErr
		}
		*rec = current
		return nil
	}
}

func sameProcessIdentity(a, b model.Process) bool {
	return a.ID == b.ID && a.Sandbox == b.Sandbox && a.PlacementEpoch == b.PlacementEpoch &&
		a.StartedAt.Equal(b.StartedAt) && a.StartKey == b.StartKey && a.StartRequestHash == b.StartRequestHash
}

func sameProcessExit(p model.Process, ev transport.ProcessEvent) bool {
	if p.State != model.ProcessExited {
		return false
	}
	if p.ExitCode != nil && *p.ExitCode != ev.ExitCode {
		return false
	}
	return ev.PID == 0 || p.PID == 0 || p.PID == ev.PID
}

func processProjectionError(op string, id model.ProcessID, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrProcessConflict) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("process state changed; reconnect with the current handle"))
	}
	if errors.Is(err, store.ErrProcessNotFound) || errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("process handle no longer exists"))
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("process state persistence timed out"))
	}
	if model.ErrorClassOf(err) != model.ErrorInternal {
		return proxyErr(err)
	}
	log.Printf("dataplane: %s %s: process state persistence failed: %v", op, id, err)
	return connect.NewError(connect.CodeUnavailable, errors.New("process state persistence unavailable"))
}

// persistProcessProgress advances only the output cursor. A second subscriber
// can legitimately hold a stale Running copy after another subscriber commits
// the terminal Exited state; reconcile that race against the durable row rather
// than terminating the otherwise valid fan-out stream. Reclaimed or mismatched
// rows remain a hard fence.
func persistProcessProgress(ctx context.Context, p *processProxy, rec *model.Process, sequence uint64) error {
	if rec == nil || sequence <= rec.OutputCursor.Sequence {
		return nil
	}
	rec.OutputCursor.Sequence = sequence
	if err := p.e.Mediator.UpdateProcess(ctx, *rec); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrProcessConflict) {
		return err
	}
	current, readErr := p.e.Mediator.Process(ctx, rec.ID)
	if readErr != nil || !sameProcessIdentity(*rec, current) {
		return store.ErrProcessConflict
	}
	switch current.State {
	case model.ProcessExited:
		if current.OutputCursor.Sequence < sequence {
			current.OutputCursor.Sequence = sequence
			if err := p.e.Mediator.UpdateProcess(ctx, current); err != nil && !errors.Is(err, store.ErrProcessConflict) {
				return err
			}
		}
		*rec = current
		return nil
	default:
		return store.ErrProcessConflict
	}
}

// normalizeProcessEventIdentity establishes and validates the identity carried
// by a process stream. Handle and epoch are envelope fields, so every data/end
// frame must agree with the stream's first identity; empty legacy fields are
// filled from the established values before persistence or forwarding.
func normalizeProcessEventIdentity(ev *transport.ProcessEvent, expectedHandle string, expectedEpoch uint64) (string, uint64, error) {
	if ev == nil {
		return expectedHandle, expectedEpoch, connect.NewError(connect.CodeInvalidArgument, errors.New("nil process stream event"))
	}
	if ev.Handle == "" {
		ev.Handle = expectedHandle
	} else if expectedHandle != "" && ev.Handle != expectedHandle {
		return expectedHandle, expectedEpoch, connect.NewError(connect.CodeFailedPrecondition, errors.New("process stream handle mismatch"))
	}
	if ev.Handle != "" {
		expectedHandle = ev.Handle
	}
	if ev.Epoch == 0 {
		ev.Epoch = expectedEpoch
	} else if expectedEpoch != 0 && ev.Epoch != expectedEpoch {
		return expectedHandle, expectedEpoch, connect.NewError(connect.CodeFailedPrecondition, errors.New("process stream placement epoch mismatch"))
	}
	if ev.Epoch != 0 {
		expectedEpoch = ev.Epoch
	}
	return expectedHandle, expectedEpoch, nil
}

func sendProcessEvent(stream processEventSender, ev transport.ProcessEvent) error {
	var pe v1.ProcessEvent
	pe.ProcessHandle, pe.PlacementEpoch, pe.Sequence, pe.Replay, pe.Gap, pe.ResetReason = ev.Handle, ev.Epoch, ev.Sequence, ev.Replay, ev.Gap, ev.ResetReason
	switch ev.Kind {
	case "start":
		pe.Kind = &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(ev.PID), ProcessHandle: ev.Handle, PlacementEpoch: ev.Epoch}}
	case "data":
		pe.Kind = &v1.ProcessEvent_Data{Data: &v1.DataEvent{Stream: ev.Stream, Data: ev.Data}}
	case "end":
		pe.Kind = &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(ev.ExitCode)}}
	case "keepalive":
		pe.Kind = &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}}
	case "error":
		return proxyErr(ev.Err)
	default:
		return nil
	}
	return stream.Send(&pe)
}
