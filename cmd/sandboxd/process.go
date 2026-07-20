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
	"syscall"

	"connectrpc.com/connect"

	v1 "engine/internal/transport/gen/engine/transport/v1"
)

// processServer implements the §5.E process primitive (transport D4.3). M1
// scope: Start (server-stream stdout/stderr), SendInput (unary stdin),
// SendSignal (pgid-directed), List (scan /proc — no registry, §5.E). PTY is in
// the wire schema but NOT in M1 acceptance (A3 is `sh -c echo`); a pty request
// is rejected rather than silently downgraded.
//
// A small in-process table maps pid→stdin pipe so SendInput can reach a running
// process. It is transport-process-local, non-authoritative, dies with the
// process — not a control-plane registry (§5.E: List scans /proc live).
type processServer struct {
	mu    sync.Mutex
	stdin map[int]io.WriteCloser
}

func (s *processServer) track(pid int, w io.WriteCloser) {
	s.mu.Lock()
	if s.stdin == nil {
		s.stdin = make(map[int]io.WriteCloser)
	}
	s.stdin[pid] = w
	s.mu.Unlock()
}

func (s *processServer) untrack(pid int) {
	s.mu.Lock()
	if w, ok := s.stdin[pid]; ok {
		_ = w.Close()
		delete(s.stdin, pid)
	}
	s.mu.Unlock()
}

// Start launches a process and server-streams start/data/end events.
func (s *processServer) Start(ctx context.Context, req *connect.Request[v1.StartRequest], stream *connect.ServerStream[v1.ProcessEvent]) error {
	m := req.Msg
	if m.Pty != nil {
		return connect.NewError(connect.CodeUnimplemented, errString("pty not in M1 scope"))
	}
	if m.Cmd == "" {
		return connect.NewError(connect.CodeInvalidArgument, errString("empty cmd"))
	}

	cmd := exec.CommandContext(ctx, m.Cmd, m.Args...)
	cmd.Env = m.Env
	cmd.Dir = m.Cwd
	// Non-PTY process gets its own pgid so SendSignal can target the whole tree
	// (transport D4.3 C1: PTY would be the asymmetric case, not here).
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
	pid := cmd.Process.Pid
	s.track(pid, stdin)
	defer s.untrack(pid)

	if err := stream.Send(&v1.ProcessEvent{
		Kind: &v1.ProcessEvent_Start{Start: &v1.StartEvent{Pid: int32(pid)}},
	}); err != nil {
		return err
	}

	// Pump stdout+stderr concurrently into data events. A mutex serializes
	// stream.Send (Connect server-streams are not safe for concurrent Send).
	var sendMu sync.Mutex
	var wg sync.WaitGroup
	pump := func(r io.Reader, name string) {
		defer wg.Done()
		br := bufio.NewReader(r)
		buf := make([]byte, 16*1024)
		for {
			n, rerr := br.Read(buf)
			if n > 0 {
				sendMu.Lock()
				_ = stream.Send(&v1.ProcessEvent{
					Kind: &v1.ProcessEvent_Data{Data: &v1.DataEvent{
						Stream: name, Data: append([]byte(nil), buf[:n]...),
					}},
				})
				sendMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")
	wg.Wait()

	// exit code (128+signo on signal, per convention).
	exitCode := 0
	if werr := cmd.Wait(); werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			if exitCode < 0 {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					exitCode = 128 + int(ws.Signal())
				}
			}
		} else {
			return connect.NewError(connect.CodeInternal, werr)
		}
	}
	return stream.Send(&v1.ProcessEvent{
		Kind: &v1.ProcessEvent_End{End: &v1.EndEvent{ExitCode: int32(exitCode)}},
	})
}

// --- M3 wire methods (Task A scaffolding; filled in Tasks B/C/D) -------------

func (s *processServer) Connect(_ context.Context, _ *connect.Request[v1.ConnectRequest], _ *connect.ServerStream[v1.ProcessEvent]) error {
	return connect.NewError(connect.CodeUnimplemented, errString("Connect: pending Task B"))
}

func (s *processServer) StreamInput(_ context.Context, _ *connect.ClientStream[v1.StreamInputRequest]) (*connect.Response[v1.StreamInputResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errString("StreamInput: pending Task D"))
}

func (s *processServer) Update(_ context.Context, _ *connect.Request[v1.UpdateRequest]) (*connect.Response[v1.UpdateResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errString("Update: pending Task D"))
}

func (s *processServer) CloseStdin(_ context.Context, _ *connect.Request[v1.CloseStdinRequest]) (*connect.Response[v1.CloseStdinResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errString("CloseStdin: pending Task D"))
}

func (s *processServer) SendInput(_ context.Context, req *connect.Request[v1.SendInputRequest]) (*connect.Response[v1.SendInputResponse], error) {
	m := req.Msg
	s.mu.Lock()
	w, ok := s.stdin[int(m.Pid)]
	s.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errString("no such live process pid "+strconv.Itoa(int(m.Pid))))
	}
	if _, err := w.Write(m.Data); err != nil {
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
