package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/sys/unix"

	"engine/internal/guestport"
	v1 "engine/internal/transport/gen/engine/transport/v1"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// transportMode is the §5.E process/fs endpoint (the child of PID 1). It
// declares itself a subreaper (so its process subtree's orphans reparent to it,
// not PID 1 — guest-runtime D4.2), then serves Connect-RPC over h2c on a fixed
// TCP port inside the guest netns. It knows NOTHING of agent/turn/session
// (transport D1) — pure process/fs mechanism.
func transportMode() {
	exitIfCrashLoopSentinel()

	// Declare ourselves subreaper so our subtree's orphans reparent here, not to
	// PID 1 (guest-runtime D4.2 / §5.E.1).
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		logf("transport: PR_SET_CHILD_SUBREAPER failed: %v", err)
	}

	// One sandbox-global activity clock (reaper-health D2), shared by the
	// process server (stamps I/O) and the health server (reports idle). It is
	// transient/process-local — reborn here on every transport (re)start, which
	// is correct: a fresh transport child that has served no I/O yet is "idle
	// since (re)start", and the reaper reads the guest-observable truth, not a
	// cross-restart memory (§5.E stateless).
	activity := newActivityClock()

	mux := http.NewServeMux()
	mux.Handle(transportv1connect.NewHealthServiceHandler(&healthServer{activity: activity}))
	mux.Handle(transportv1connect.NewProcessServiceHandler(&processServer{activity: activity}))
	mux.Handle(transportv1connect.NewFsServiceHandler(&fsServer{}))

	// Publish our pid so supervision/tests can target the transport child
	// deterministically (the guest pid is not fixed — the Go runtime consumes
	// low pids, so it is NOT pid 2). Best-effort; not authoritative.
	_ = os.WriteFile(transportPidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)

	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(guestport.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf("transport: listen %s: %v", addr, err)
		os.Exit(1)
	}
	logf("transport(pid %d): serving Connect-RPC on %s inside netns", os.Getpid(), addr)

	// h2c so bidi/server-streams work over cleartext HTTP/2 (transport D4.2).
	srv := &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
	if err := srv.Serve(ln); err != nil {
		logf("transport: serve: %v", err)
		os.Exit(1)
	}
}

// transportPidFile is where transport mode publishes its own pid (on tmpfs
// /tmp, which is mounted rw in the guest). Non-authoritative; for supervision
// and test targeting of the transport child (its pid is not fixed).
//
// This and crashLoopSentinel are engine-injected runtime files that Promote must
// exclude from a frozen layer: rootfs.promoteExcludedPaths lists their
// upper-relative forms and MUST be kept in sync with the paths here.
const transportPidFile = "/tmp/.sandboxd-transport.pid"

// --- Health service (§4.3 L2 / transport D4.5) -------------------------------

type healthServer struct {
	// activity is the shared sandbox-global last-I/O clock (reaper-health D2).
	// Check reports now-lastIO as idle_millis — the §4.3 activity signal riding
	// the same probe that answers L2 liveness (one RPC, no second round-trip).
	activity *activityClock
}

func (h *healthServer) Check(_ context.Context, _ *connect.Request[v1.CheckRequest]) (*connect.Response[v1.CheckResponse], error) {
	return connect.NewResponse(&v1.CheckResponse{
		Ok:         true,
		IdleMillis: h.activity.idleMillis(),
	}), nil
}

// --- fs service (§5.E fs primitive) ------------------------------------------

type fsServer struct{}

func (*fsServer) Read(_ context.Context, req *connect.Request[v1.ReadRequest]) (*connect.Response[v1.ReadResponse], error) {
	m := req.Msg
	f, err := os.Open(m.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	defer f.Close()
	if m.Offset > 0 {
		if _, err := f.Seek(m.Offset, 0); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	var data []byte
	if m.Length > 0 {
		data = make([]byte, m.Length)
		n, err := io.ReadFull(f, data)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		data = data[:n]
	} else {
		b, err := io.ReadAll(f)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		data = b
	}
	return connect.NewResponse(&v1.ReadResponse{Data: data}), nil
}

func (*fsServer) Write(_ context.Context, req *connect.Request[v1.WriteRequest]) (*connect.Response[v1.WriteResponse], error) {
	m := req.Msg
	if err := os.WriteFile(m.Path, m.Data, 0o644); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.WriteResponse{BytesWritten: int64(len(m.Data))}), nil
}

// killProcessGroup signals pgid first (kill(-pgid)), falling back to pid
// (transport D4.3 / §5.E.1: directed signals belong to transport mode).
func killProcessGroup(pid, sig int) error {
	if err := syscall.Kill(-pid, syscall.Signal(sig)); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.Signal(sig))
}
