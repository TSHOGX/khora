package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/TSHOGX/khora/internal/guestport"
	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

// transportMode is the transport data plane process/fs endpoint (the child of PID 1). It becomes
// the reaper of its own process subtree (so its orphans reparent to it, not
// PID 1, AND are collected by it — the init and sandboxd split / the guest startup topology), then serves
// Connect-RPC over h2c on a fixed TCP port inside the guest netns. It knows
// NOTHING of agent/turn/session (the process/fs boundary) — pure process/fs mechanism.
func transportMode() {
	exitIfCrashLoopSentinel()

	// Subreaper declaration + the wildcard drain that collects what it attracts, in one call
	// (the init and sandboxd split / the guest startup topology — see startSubtreeReaper). This table is now this process's
	// ONLY waiter, so the processServer carries it to get its children's exit codes.
	reaper := startSubtreeReaper()

	// One sandbox-global activity clock (the activity signal), shared by the process server
	// (stamps I/O) and the health server (reports idle). Transient/process-local, reborn on
	// every transport (re)start: a fresh transport child that has served no I/O is "idle since
	// (re)start", and the reaper reads guest-observable truth, not a cross-restart memory
	// (the transport data plane stateless).
	activity := newActivityClock()

	procSvc := &processServer{activity: activity, reaper: reaper}
	procSvc.loadPersistent()
	go procSvc.watchRestored()
	mux := http.NewServeMux()
	mux.Handle(transportv1connect.NewHealthServiceHandler(&healthServer{activity: activity}))
	mux.Handle(transportv1connect.NewProcessServiceHandler(procSvc))
	mux.Handle(transportv1connect.NewFsServiceHandler(&fsServer{activity: activity}))
	// The large-file bypass shares this mux, listener and port — a plain HTTP
	// route beside the Connect handlers (files.go / fs transport).
	registerFileRoutes(mux)

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

	// h2c so bidi/server-streams work over cleartext HTTP/2 (streaming transport).
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

// --- Health service (health and reclamation L2 / the Health probe) -------------------------------

type healthServer struct {
	// activity is the shared sandbox-global last-I/O clock (the activity signal).
	// Check reports now-lastIO as idle_millis — the health and reclamation activity signal riding
	// the same probe that answers L2 liveness (one RPC, no second round-trip).
	activity *activityClock
}

func (h *healthServer) Check(_ context.Context, _ *connect.Request[v1.CheckRequest]) (*connect.Response[v1.CheckResponse], error) {
	return connect.NewResponse(&v1.CheckResponse{
		Ok:         true,
		IdleMillis: h.activity.idleMillis(),
	}), nil
}

// RecordActivity stamps the clock Check reports (health and reclamation / the activity signal) — the write half of
// the same one signal, which is why it shares this service and this clock.
//
// The stamp is the ONLY thing that happens: no duration is accepted, nothing is held, nothing
// expires, so afterwards this is indistinguishable from a keystroke. The pause-inhibit /
// keepalive / lease shapes stay rejected — each carries a protected PERIOD, and the period is
// what would force per-caller state.
//
// The probe itself does NOT stamp: an observation must not be indistinguishable from the thing
// observed, and a Check that stamped would make every sandbox permanently active. This RPC is a
// caller ASSERTING activity, which is a different act.
func (h *healthServer) RecordActivity(_ context.Context, _ *connect.Request[v1.RecordActivityRequest]) (*connect.Response[v1.RecordActivityResponse], error) {
	h.activity.stamp()
	// Read back AFTER the stamp (~0), so the caller gets positive confirmation that the
	// clock the reaper reads is the one that moved.
	return connect.NewResponse(&v1.RecordActivityResponse{
		IdleMillis: h.activity.idleMillis(),
	}), nil
}

// The fs service (the transport data plane) lives in fs.go.

// signalPidErr is the message both ends use when a caller addresses SendSignal
// at a non-positive pid. It says WHY rather than just "invalid": the pid is not
// merely dangerous, it is addressed to the wrong subsystem.
const signalPidErr = "pid must be > 0: SendSignal carries DIRECTED signals only " +
	"(one process, defaulting to its group). pid <= 0 is POSIX fan-out — " +
	"kill(0)/kill(-1) address a whole group or the whole pid namespace — and " +
	"container-level fan-out belongs to guest PID 1, not the transport channel " +
	"(guest supervision / the guest startup topology)"

// validateSignalPid enforces the transport behavior subsystem split at the transport entry
// point. Only pid > 0 is a directed target; everything else is fan-out.
//
// This is the whole check on purpose — see SendSignal on why registry membership
// is deliberately NOT required.
func validateSignalPid(pid int) error {
	if pid <= 0 {
		return errString(signalPidErr)
	}
	return nil
}

// killProcessGroup signals pgid first (kill(-pgid)), falling back to pid
// (process transport / the guest startup topology: directed signals belong to transport mode).
//
// It REQUIRES a caller-validated pid > 0 (validateSignalPid); it does not
// re-check. Both of its arms turn hostile on a non-positive pid — kill(-pid)
// becomes the kill(-1) namespace broadcast at pid=1, and the fallback becomes a
// directed kill(1) at pid=-1, which lands on PID 1's own TERM handler and tears
// down the namespace.
//
// The pid=1 arm is the worse of the two BECAUSE it looks survivable: Linux's
// kill(-1) excludes PID 1 and the caller itself, so measured, the sandbox stays up
// while every worker inside it dies. A caller sees a live sandbox with its work
// gone, rather than a sandbox that died and can be recreated.
func killProcessGroup(pid, sig int) error {
	if err := syscall.Kill(-pid, syscall.Signal(sig)); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.Signal(sig))
}
