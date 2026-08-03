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
// engine-side (so it stays debuggable) but never crosses to the wire. (Stream
// errors that arrive via drainEvents are a separate seam item tied to the health
// health state — see transport.md D4.3.)
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

func (p *processProxy) SendSignal(ctx context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
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
	cl, err := f.e.clientFor(req.Header().Get)
	if err != nil {
		return nil, err
	}
	if err := cl.Write(ctx, req.Msg.Path, req.Msg.Data); err != nil {
		return nil, proxyErr(err)
	}
	return connect.NewResponse(&v1.WriteResponse{BytesWritten: int64(len(req.Msg.Data))}), nil
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
		default:
			continue
		}
		if err := stream.Send(&pe); err != nil {
			return err
		}
	}
	return nil
}
