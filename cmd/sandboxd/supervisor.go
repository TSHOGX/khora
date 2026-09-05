package main

// The process supervisor lives with guest PID 1, not with a transport child.
// Transport children are intentionally replaceable: a crash/restart must not
// orphan the workload's pipes, PTY master, output cursor, or logical handle.
// This file is the private in-guest RPC seam between those two roles.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

// processSupervisorSocket is private guest state.  It is deliberately separate
// from the guest TCP listener: only the transport child in this sandbox can
// reach it, and it never becomes part of the host-facing wire contract.
const processSupervisorSocket = "/tmp/.sandboxd-process.sock"

// supervisorServer owns the one processServer instance for the whole guest
// lifetime.  The Unix listener can outlive any number of transport children.
type supervisorServer struct {
	svc *processServer
	ln  net.Listener
	srv *http.Server
	mu  sync.Mutex
}

func newSupervisorServer(svc *processServer) (*supervisorServer, error) {
	// A stale socket can only be a file left by an unclean PID 1 shutdown.  It is
	// safe to remove after a read-only liveness probe; never unlink a live peer.
	if st, err := os.Stat(processSupervisorSocket); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("process supervisor socket path is not a socket")
		}
		c, derr := net.Dial("unix", processSupervisorSocket)
		if derr == nil {
			_ = c.Close()
			return nil, errors.New("process supervisor socket is already in use")
		}
		if err := os.Remove(processSupervisorSocket); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", processSupervisorSocket)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(processSupervisorSocket, 0o600)
	s := &supervisorServer{svc: svc, ln: ln}
	path, handler := transportv1connect.NewProcessServiceHandler(svc)
	_ = path // the Unix listener uses the generated handler's procedure paths.
	healthPath, healthHandler := transportv1connect.NewHealthServiceHandler(&healthServer{activity: svc.activity})
	_ = healthPath
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.Handle(healthPath, healthHandler)
	s.srv = &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
	return s, nil
}

func (s *supervisorServer) serve() {
	if s == nil || s.srv == nil || s.ln == nil {
		return
	}
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logf("process supervisor: serve: %v", err)
	}
}

func (s *supervisorServer) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	return os.Remove(processSupervisorSocket)
}

// supervisorClient is intentionally connection-pool backed.  A transport
// child creates a new client on each restart, but every request dials the same
// Unix endpoint and reaches the PID-1-owned process hub.
type supervisorClient struct {
	process transportv1connect.ProcessServiceClient
	health  transportv1connect.HealthServiceClient
}

func newSupervisorClient() *supervisorClient {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", processSupervisorSocket)
		},
	}
	hc := &http.Client{Transport: tr}
	return &supervisorClient{
		process: transportv1connect.NewProcessServiceClient(hc, "http://sandbox-supervisor"),
		health:  transportv1connect.NewHealthServiceClient(hc, "http://sandbox-supervisor"),
	}
}

type healthForwarder struct{ c *supervisorClient }

func (h *healthForwarder) Check(ctx context.Context, req *connect.Request[v1.CheckRequest]) (*connect.Response[v1.CheckResponse], error) {
	return h.c.health.Check(ctx, req)
}

func (h *healthForwarder) RecordActivity(ctx context.Context, req *connect.Request[v1.RecordActivityRequest]) (*connect.Response[v1.RecordActivityResponse], error) {
	return h.c.health.RecordActivity(ctx, req)
}

// processForwarder is the transport child's externally mounted ProcessService.
// Every method is a transparent RPC hop to PID 1.  In particular, server
// streams are copied frame-for-frame, while the child process never owns a
// workload descriptor or a replay buffer.
type processForwarder struct{ c *supervisorClient }

func (p *processForwarder) Start(ctx context.Context, req *connect.Request[v1.StartRequest], out *connect.ServerStream[v1.ProcessEvent]) error {
	in, err := p.c.process.Start(ctx, req)
	if err != nil {
		return err
	}
	defer in.Close()
	for in.Receive() {
		if err := out.Send(in.Msg()); err != nil {
			return err
		}
	}
	return in.Err()
}

func (p *processForwarder) Connect(ctx context.Context, req *connect.Request[v1.ConnectRequest], out *connect.ServerStream[v1.ProcessEvent]) error {
	in, err := p.c.process.Connect(ctx, req)
	if err != nil {
		return err
	}
	defer in.Close()
	for in.Receive() {
		if err := out.Send(in.Msg()); err != nil {
			return err
		}
	}
	return in.Err()
}

func (p *processForwarder) StreamInput(ctx context.Context, in *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	out := p.c.process.StreamInput(ctx)
	for in.Receive() {
		if err := out.Send(in.Msg()); err != nil {
			if conn, cerr := out.Conn(); cerr == nil {
				_ = conn.CloseRequest()
				_ = conn.CloseResponse()
			}
			return nil, err
		}
	}
	if err := in.Err(); err != nil {
		if conn, cerr := out.Conn(); cerr == nil {
			_ = conn.CloseRequest()
			_ = conn.CloseResponse()
		}
		return nil, err
	}
	return out.CloseAndReceive()
}

func (p *processForwarder) SendInput(ctx context.Context, req *connect.Request[v1.SendInputRequest]) (*connect.Response[v1.SendInputResponse], error) {
	return p.c.process.SendInput(ctx, req)
}

func (p *processForwarder) SendSignal(ctx context.Context, req *connect.Request[v1.SendSignalRequest]) (*connect.Response[v1.SendSignalResponse], error) {
	return p.c.process.SendSignal(ctx, req)
}

func (p *processForwarder) Update(ctx context.Context, req *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	return p.c.process.Update(ctx, req)
}

func (p *processForwarder) CloseStdin(ctx context.Context, req *connect.Request[v1.CloseStdinRequest]) (*connect.Response[v1.CloseStdinResponse], error) {
	return p.c.process.CloseStdin(ctx, req)
}

func (p *processForwarder) List(ctx context.Context, req *connect.Request[v1.ListRequest]) (*connect.Response[v1.ListResponse], error) {
	return p.c.process.List(ctx, req)
}
