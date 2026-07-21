// Package transport is subsystem §5.E: the host↔sandbox channel. The engine
// exposes exactly two stateless RPC services — process and fs — plus a Health
// probe (§5.E / transport D4). This package is the HOST-SIDE client of those
// services; the in-guest server lives in cmd/sandboxd (transport mode).
//
// Carrier: netns-internal TCP, no vsock (§5.E, ECS-validated). sandboxd listens
// on a TCP port inside the guest netns; the host dials the netns veth IP —
// unary over HTTP, streams over h2c with immediate flush. Framework is
// Connect-RPC over h2c (transport D4.2). Cold start discovers the current netns
// IP before dialing (§5.A placement); IP change across resume invalidates the
// old endpoint (§3.3 / §4.3 IP-as-epoch), and reconnect just re-resolves — the
// carrier does not encode a fixed address (§3.3).
//
// Stateless (§5.E): the engine keeps no persistent SoT for connections. "What's
// running now" = a live process.List, never a control-plane registry. Bare pty
// byte streams are explicitly lossy (shell has no persistent SoT); any
// "don't drop" guarantee is the upper layer's job (session adapter, engine-外).
//
// Surface: the full process data plane — Start / Connect (server-stream) /
// StreamInput (client-stream) / SendInput / SendSignal / Update / CloseStdin /
// List, plus fs Read / Write and Health. Bounded ring replay on Connect and
// slow-subscriber non-blocking drop (D4.3's two invariants) live in the guest
// server (cmd/sandboxd). fs breadth (list/stat/mkdir/…) + multipart bypass are a
// later increment.
package transport

// Wire stubs are generated from proto/engine/transport/v1/transport.proto via
// buf (schema-first, Task S). Regenerate with: cd proto && buf generate.
// Requires buf + protoc-gen-go + protoc-gen-connect-go on PATH. The generated
// code lives in internal/transport/gen/... and is committed (buf/protoc are not
// guaranteed present on every checkout). This Client interface is the host-side
// DOMAIN seam; the concrete Connect client (Task D) translates domain↔proto so
// the mediator never sees generated types.

import "context"

// Endpoint is the current transport endpoint (netns veth IP:port), discovered
// at placement time (§5.A ⑥). Never cached across resume (§4.3 要点③).
type Endpoint string

// Port is the fixed TCP port sandboxd (transport mode) listens on inside the
// guest netns. It can be constant across sandboxes because each sandbox has its
// own netns — the veth IP disambiguates, not the port (§5.E netns-internal TCP).
const Port = 9000

// ProcessEvent is one item of a process output stream: oneof{start,data,end,
// keepalive} (transport D4.3). M1 carries start(pid) / data(stdout|stderr) /
// end(exit_code).
type ProcessEvent struct {
	Kind     string // "start" | "data" | "end" | "keepalive"
	PID      int    // set on start
	Stream   string // "stdout" | "stderr" | "pty" — set on data
	Data     []byte // set on data
	ExitCode int    // set on end
}

// StartSpec starts a process (§5.E process service). PTY is the Pty!=nil special
// case (transport D4.3: pty not a separate service). guest-native pid addressing.
type StartSpec struct {
	Cmd       string
	Args      []string
	Env       []string
	Cwd       string
	GuestUser string   // per-Start run-as, like `sudo -u` (§6.2) — not the sandbox default
	Pty       *PtySize // non-nil = allocate a PTY
}

type PtySize struct{ Rows, Cols uint16 }

// InputWriter is an ordered input stream to one process (stdin / pty-in). It is
// the domain projection of the StreamInput client-stream (transport D4.3):
// keystroke ordering is guaranteed by the single stream. Close ends the stream
// (it does NOT close the process's stdin — use CloseStdin for Ctrl-D).
type InputWriter interface {
	// Send writes one ordered frame. ptyIn selects the pty branch vs stdin.
	Send(data []byte, ptyIn bool) error
	// Close ends the input stream and returns the RPC's terminal error, if any.
	Close() error
}

// Client is the host-side transport client for one sandbox's current endpoint.
// Stateless: constructed against an endpoint, discarded on resume.
type Client interface {
	// Health is the unary readiness/liveness probe (§4.3 L2 / transport D4.5).
	// First success is the resume readiness gate that flips state to running
	// (guest-runtime D5.2 要点①) — the engine does not trust bare TCP accept.
	Health(ctx context.Context) error

	// Start launches a process and returns a channel of output events
	// (server-stream). Addressed by the returned pid thereafter.
	Start(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error)

	// Connect reattaches to a running process: the returned stream replays the
	// bounded output ring buffer first, then joins live (transport D4.3
	// invariant 2). Many Connect can attach one pid, each an independent copy.
	Connect(ctx context.Context, pid int) (<-chan ProcessEvent, error)

	// StreamInput opens an ordered input stream to a process (client-stream).
	// The interactive input path — keystrokes to a pty, in order.
	StreamInput(ctx context.Context, pid int) (InputWriter, error)

	// SendInput writes stdin/pty-in once (unary; for when no stream is held).
	// ptyIn selects the pty branch.
	SendInput(ctx context.Context, pid int, data []byte, ptyIn bool) error

	// SendSignal signals a process; defaults to the process group, falling back
	// to the pid (transport D4.3 / §5.E.1 directed signals belong to transport mode).
	SendSignal(ctx context.Context, pid int, signal int) error

	// Update resizes a process's PTY (unary; transport D4.3).
	Update(ctx context.Context, pid int, size PtySize) error

	// CloseStdin closes a process's stdin — under a pty, equivalent to Ctrl-D.
	CloseStdin(ctx context.Context, pid int) error

	// List enumerates processes live (no registry, §5.E).
	List(ctx context.Context) ([]int, error)

	// Read reads sandbox file bytes at an offset; engine keeps no read cursor,
	// no cross-resume offset continuity (transport D4.4). No watch — consumers
	// poll Stat+Read (§ render-vs-interact asymmetry, transport D2).
	Read(ctx context.Context, path string, offset, length int64) ([]byte, error)

	// Write writes sandbox file bytes.
	Write(ctx context.Context, path string, data []byte) error
}
