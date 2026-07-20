package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	v1 "engine/internal/transport/gen/engine/transport/v1"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// connectClient is the host-side implementation of Client backed by the
// generated Connect-RPC stubs. It translates the DOMAIN types (StartSpec,
// ProcessEvent, …) to/from proto so the mediator never sees generated types
// (§5.E seam discipline). Stateless: built against one endpoint, discarded on
// resume (the endpoint's netns IP changes, §3.3 / §4.3 IP-as-epoch).
type connectClient struct {
	health  transportv1connect.HealthServiceClient
	process transportv1connect.ProcessServiceClient
	fs      transportv1connect.FsServiceClient
}

// Dial builds a stateless Client for one sandbox's current endpoint. h2c
// (cleartext HTTP/2) so streams work without TLS — the netns channel needs no
// in-channel auth (transport D4.1 trust model).
func Dial(ep Endpoint) Client {
	hc := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			// h2c: dial plain TCP, ignore the TLS config.
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
	base := "http://" + string(ep)
	return &connectClient{
		health:  transportv1connect.NewHealthServiceClient(hc, base),
		process: transportv1connect.NewProcessServiceClient(hc, base),
		fs:      transportv1connect.NewFsServiceClient(hc, base),
	}
}

// DialFunc adapts Dial to the mediator's Deps.Dial signature.
func DialFunc(ep Endpoint) Client { return Dial(ep) }

func (c *connectClient) Health(ctx context.Context) error {
	resp, err := c.health.Check(ctx, connect.NewRequest(&v1.CheckRequest{}))
	if err != nil {
		return err
	}
	if !resp.Msg.Ok {
		return errors.New("transport: health not ok")
	}
	return nil
}

func (c *connectClient) Start(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error) {
	req := &v1.StartRequest{
		Cmd: spec.Cmd, Args: spec.Args, Env: spec.Env, Cwd: spec.Cwd,
		GuestUser: spec.GuestUser,
	}
	if spec.Pty != nil {
		req.Pty = &v1.PtySize{Rows: uint32(spec.Pty.Rows), Cols: uint32(spec.Pty.Cols)}
	}
	stream, err := c.process.Start(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return drainEvents(stream), nil
}

func (c *connectClient) Connect(ctx context.Context, pid int) (<-chan ProcessEvent, error) {
	stream, err := c.process.Connect(ctx, connect.NewRequest(&v1.ConnectRequest{Pid: int32(pid)}))
	if err != nil {
		return nil, err
	}
	return drainEvents(stream), nil
}

// eventStream is the shared shape of Start's and Connect's server-streams.
type eventStream interface {
	Receive() bool
	Msg() *v1.ProcessEvent
	Close() error
}

// drainEvents translates a proto ProcessEvent server-stream into the domain
// channel, mapping oneof variants to domain Kind strings (§5.E seam: the
// mediator never sees generated types).
func drainEvents(stream eventStream) <-chan ProcessEvent {
	out := make(chan ProcessEvent, 16)
	go func() {
		defer close(out)
		for stream.Receive() {
			ev := stream.Msg()
			switch k := ev.Kind.(type) {
			case *v1.ProcessEvent_Start:
				out <- ProcessEvent{Kind: "start", PID: int(k.Start.Pid)}
			case *v1.ProcessEvent_Data:
				out <- ProcessEvent{Kind: "data", Stream: k.Data.Stream, Data: k.Data.Data}
			case *v1.ProcessEvent_End:
				out <- ProcessEvent{Kind: "end", ExitCode: int(k.End.ExitCode)}
			case *v1.ProcessEvent_Keepalive:
				out <- ProcessEvent{Kind: "keepalive"}
			}
		}
		_ = stream.Close()
	}()
	return out
}

// streamInputWriter adapts a Connect client-stream to the domain InputWriter.
type streamInputWriter struct {
	stream *connect.ClientStreamForClient[v1.StreamInputRequest, v1.StreamInputResponse]
	pid    int
}

func (w *streamInputWriter) Send(data []byte, ptyIn bool) error {
	return w.stream.Send(&v1.StreamInputRequest{Pid: int32(w.pid), Data: data, PtyIn: ptyIn})
}

func (w *streamInputWriter) Close() error {
	_, err := w.stream.CloseAndReceive()
	return err
}

func (c *connectClient) StreamInput(ctx context.Context, pid int) (InputWriter, error) {
	return &streamInputWriter{stream: c.process.StreamInput(ctx), pid: pid}, nil
}

func (c *connectClient) Update(ctx context.Context, pid int, size PtySize) error {
	_, err := c.process.Update(ctx, connect.NewRequest(&v1.UpdateRequest{
		Pid: int32(pid), Pty: &v1.PtySize{Rows: uint32(size.Rows), Cols: uint32(size.Cols)},
	}))
	return err
}

func (c *connectClient) CloseStdin(ctx context.Context, pid int) error {
	_, err := c.process.CloseStdin(ctx, connect.NewRequest(&v1.CloseStdinRequest{Pid: int32(pid)}))
	return err
}

func (c *connectClient) SendInput(ctx context.Context, pid int, data []byte, ptyIn bool) error {
	_, err := c.process.SendInput(ctx, connect.NewRequest(&v1.SendInputRequest{
		Pid: int32(pid), Data: data, PtyIn: ptyIn,
	}))
	return err
}

func (c *connectClient) SendSignal(ctx context.Context, pid int, signal int) error {
	_, err := c.process.SendSignal(ctx, connect.NewRequest(&v1.SendSignalRequest{
		Pid: int32(pid), Signal: int32(signal),
	}))
	return err
}

func (c *connectClient) List(ctx context.Context) ([]int, error) {
	resp, err := c.process.List(ctx, connect.NewRequest(&v1.ListRequest{}))
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(resp.Msg.Pids))
	for _, p := range resp.Msg.Pids {
		pids = append(pids, int(p))
	}
	return pids, nil
}

func (c *connectClient) Read(ctx context.Context, path string, offset, length int64) ([]byte, error) {
	resp, err := c.fs.Read(ctx, connect.NewRequest(&v1.ReadRequest{
		Path: path, Offset: offset, Length: length,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Data, nil
}

func (c *connectClient) Write(ctx context.Context, path string, data []byte) error {
	_, err := c.fs.Write(ctx, connect.NewRequest(&v1.WriteRequest{
		Path: path, Data: data,
	}))
	return err
}

// compile-time: connectClient satisfies Client.
var _ Client = (*connectClient)(nil)
