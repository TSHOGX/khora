package orchestrator

import (
	"context"
	"errors"
	"log"

	"connectrpc.com/connect"

	"engine/internal/model"
	"engine/internal/transport"
	v1 "engine/internal/transport/gen/engine/transport/v1"
)

// The outer-edge data plane (transport D6): the orchestrator mounts the SAME
// process/fs Connect services the inner sandboxd exposes, and re-proxies each
// call to the sandbox's current endpoint resolved by sandbox_id via the mediator
// (placement NEVER leaves the engine, §6.2 / D6.1). One wire — the inner
// transport.proto is reused verbatim at the outer edge, no translation layer
// and no REST/NDJSON facade (D6.2 / D6.3).
//
// Addressing: the proto is pid-addressed (no sandbox_id field — D4.6 vocabulary
// boundary keeps product/placement concepts out of the wire). The outer edge
// carries the sandbox_id in a request header; the proxy strips it and resolves
// the endpoint internally. The mediator owns the "which endpoint" verb (§4);
// the proxy is a client of the resolved transport.Client, never calling another
// subsystem directly.
const sandboxIDHeader = "Engine-Sandbox-Id"

// processProxy re-proxies ProcessService to the sandbox's sandboxd (D6).
type processProxy struct{ e *Engine }

// fsProxy re-proxies FsService to the sandbox's sandboxd (D6).
type fsProxy struct{ e *Engine }

// proxyErr sanitizes an inner transport-client error before it crosses to the
// external client. The inner client dials the netns veth IP (client.go), so a
// raw dial/transport error carries that IP:port — returning it verbatim would
// leak placement to the engine's edge (§6.2 / D6.1: placement never exposed).
// Map to CodeUnavailable with a placement-free message; the real cause is logged
// engine-side (so it stays debuggable) but never crosses to the wire. This also
// carries mid-stream terminations — an overflow disconnect (D4.3 invariant 1)
// reaches forwardEvents as a ProcessEvent{Kind:"error"} and ends the outer stream
// through here, so the external client learns the stream died rather than seeing
// it close as if the process had finished.
func proxyErr(err error) error {
	log.Printf("dataplane: transport proxy error (sanitized to client): %v", err)
	return connect.NewError(connect.CodeUnavailable, errors.New("sandbox transport unavailable"))
}

// clientFor resolves the transport client for the header-named sandbox. The
// endpoint resolution stays inside the mediator (§6.2: placement never exposed).
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

// mutatingClientFor is clientFor plus the §4.2 pause drain barrier, for handlers
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
// drain would wait for work that is waiting for the drain (D4.4).
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

// --- ProcessService proxy ----------------------------------------------------

func (p *processProxy) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return err
	}
	m := req.Msg
	spec := transport.StartSpec{Cmd: m.Cmd, Args: m.Args, Env: m.Env, Cwd: m.Cwd}
	if m.Pty != nil {
		spec.Pty = &transport.PtySize{Rows: uint16(m.Pty.Rows), Cols: uint16(m.Pty.Cols)}
	}
	ch, err := cl.Start(ctx, spec)
	if err != nil {
		return proxyErr(err)
	}
	return forwardEvents(stream, ch)
}

func (p *processProxy) Connect(ctx context.Context, req *connect.Request[v1.ConnectRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return err
	}
	ch, err := cl.Connect(ctx, int(req.Msg.Pid))
	if err != nil {
		return proxyErr(err)
	}
	return forwardEvents(stream, ch)
}

func (p *processProxy) StreamInput(ctx context.Context, stream *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	cl, err := p.e.clientFor(stream.RequestHeader().Get)
	if err != nil {
		return nil, err
	}
	var in transport.InputWriter
	for stream.Receive() {
		m := stream.Msg()
		if in == nil {
			in, err = cl.StreamInput(ctx, int(m.Pid))
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
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := cl.SendInput(ctx, int(req.Msg.Pid), req.Msg.Data, req.Msg.PtyIn); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.SendInputResponse{}), nil
}

// signalPidErr mirrors the guest's message verbatim (cmd/sandboxd/transport.go)
// so a caller gets the same explanation whichever end refuses. It is duplicated
// rather than shared through a leaf package deliberately: what the two ends must
// agree on is the RULE, and the rule is one comparison. A leaf existing only to
// hold an error string would couple the guest binary to the host tree for
// prose — and guestport's bar is a fact neither side can derive alone (the port,
// the upload prefix), which this is not. A test pins the copies together instead
// (dataplane_signal_test.go).
const signalPidErr = "pid must be > 0: SendSignal carries DIRECTED signals only " +
	"(one process, defaulting to its group). pid <= 0 is POSIX fan-out — " +
	"kill(0)/kill(-1) address a whole group or the whole pid namespace — and " +
	"container-level fan-out belongs to guest PID 1, not the transport channel " +
	"(guest-runtime D4.3 / §5.E.1)"

// SendSignal proxies a DIRECTED signal. The pid > 0 check here is fail-fast only
// (one less RPC): the authoritative check is the guest's, because §5.E.1's premise
// is that the guest can write its own fs and so must validate for itself. pid <= 0
// is POSIX fan-out, which belongs to guest PID 1 rather than this channel
// (guest-runtime D4.3).
func (p *processProxy) SendSignal(ctx context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	if req.Msg.Pid <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(signalPidErr))
	}
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := cl.SendSignal(ctx, int(req.Msg.Pid), int(req.Msg.Signal)); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.SendSignalResponse{}), nil
}

func (p *processProxy) Update(ctx context.Context, req *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if req.Msg.Pty == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("Update requires pty size"))
	}
	if err := cl.Update(ctx, int(req.Msg.Pid), transport.PtySize{Rows: uint16(req.Msg.Pty.Rows), Cols: uint16(req.Msg.Pty.Cols)}); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.UpdateResponse{}), nil
}

func (p *processProxy) CloseStdin(ctx context.Context, req *connect.Request[v1.CloseStdinRequest]) (*connect.Response[v1.CloseStdinResponse], error) {
	cl, err := p.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := cl.CloseStdin(ctx, int(req.Msg.Pid)); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.CloseStdinResponse{}), nil
}

func (p *processProxy) List(ctx context.Context, req *connect.Request[v1.ListRequest]) (*connect.Response[v1.ListResponse], error) {
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
	return connect.NewResponse(&v1.ListResponse{Pids: out}), nil
}

// --- FsService proxy ---------------------------------------------------------

func (f *fsProxy) Read(ctx context.Context, req *connect.Request[v1.ReadRequest]) (*connect.Response[v1.ReadResponse], error) {
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
	// Barrier-gated: this is THE call the drain exists for. The guest's write is
	// truncate-then-write, so a pause landing mid-call empties the file (D4.4).
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

// entryToWire re-encodes a domain EntryInfo for the outer edge. The outer wire is
// the SAME proto as the inner one (D6.2 one-wire), so this is a straight
// projection — it exists only because the proxy speaks the domain Client
// interface in between, never because the two edges differ.
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

// forwardEvents pumps the domain output channel to the outer proto server-stream
// (transparent passthrough: the outer edge sees the exact same frames the inner
// sandboxd produced — one wire, D6.2).
func forwardEvents(stream *connect.ServerStream[v1.ProcessEvent], ch <-chan transport.ProcessEvent) error {
	for ev := range ch {
		var pe v1.ProcessEvent
		switch ev.Kind {
		case "start":
			pe.Kind = &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(ev.PID)}}
		case "data":
			pe.Kind = &v1.ProcessEvent_Data{Data: &v1.DataEvent{Stream: ev.Stream, Data: ev.Data}}
		case "end":
			pe.Kind = &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(ev.ExitCode)}}
		case "keepalive":
			pe.Kind = &v1.ProcessEvent_Keepalive{Keepalive: &v1.KeepaliveEvent{}}
		case "error":
			// The inner stream ended WITHOUT an end frame — typically the D4.3
			// invariant-1 overflow disconnect. Terminate the outer stream with the
			// same error rather than closing it cleanly: a clean close would tell the
			// external client "the process finished", which is exactly wrong, and it
			// would undo the distinction the guest went to the trouble of reporting.
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
