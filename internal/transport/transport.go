// Package transport is subsystem §5.E: the host↔sandbox channel. The engine
// exposes exactly two stateless RPC services — process and fs — plus a Health
// probe (§5.E / transport D4). This package is the HOST-SIDE client of those
// services; the in-guest server lives in cmd/sandboxd (transport mode).
//
// Carrier: netns-internal TCP, no vsock (§5.E; validated on both substrate
// arches per ADR-0002). sandboxd listens
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
// List; the full fs surface — Read / Write / Stat / ListDir / MakeDir / Remove /
// Move plus the streaming large-file bypass (OpenRead / WriteStream, D4.4); and
// Health. Bounded ring replay on Connect and slow-subscriber non-blocking drop
// (D4.3's two invariants) live in the guest server (cmd/sandboxd).
package transport

// Wire stubs are generated from proto/engine/transport/v1/transport.proto via
// buf (schema-first). Regenerate with: cd proto && buf generate.
// Requires buf + protoc-gen-go + protoc-gen-connect-go on PATH. The generated
// code lives in internal/transport/gen/... and is committed (buf/protoc are not
// guaranteed present on every checkout). This Client interface is the host-side
// DOMAIN seam; the concrete Connect client translates domain↔proto so
// the mediator never sees generated types.

import (
	"context"
	"io"
	"time"
)

// Endpoint is the current transport endpoint (netns veth IP:port), discovered
// at placement time (§5.A ⑥). Never cached across resume (§4.3 要点③).
type Endpoint string

// ProcessEvent is one item of a process output stream: oneof{start,data,end,
// keepalive} (transport D4.3) — start(pid) / data(stdout|stderr) /
// end(exit_code) / keepalive.
type ProcessEvent struct {
	Kind     string // "start" | "data" | "end" | "keepalive" | "error"
	PID      int    // set on start
	Stream   string // "stdout" | "stderr" | "pty" — set on data
	Data     []byte // set on data
	ExitCode int    // set on end
	// Err is set on Kind == "error": the stream ended WITHOUT an end frame. The
	// common cause is the D4.3 invariant-1 overflow disconnect — a subscriber that
	// could not keep up is dropped, and the guest reports resource_exhausted to
	// distinguish that from a clean end.
	//
	// This exists because the distinction is otherwise LOST at this seam: the guest
	// is careful to signal it, and a consumer that only watches for "end" cannot
	// tell "the process finished" from "your copy of the output was dropped". Those
	// call for opposite responses — accept the exit code, or reconnect and re-read.
	Err error
}

// StartSpec starts a process (§5.E process service). PTY is the Pty!=nil special
// case (transport D4.3: pty not a separate service). guest-native pid addressing.
type StartSpec struct {
	Cmd  string
	Args []string
	Env  []string
	Cwd  string
	Pty  *PtySize // non-nil = allocate a PTY
}

type PtySize struct{ Rows, Cols uint16 }

// EntryType is what a filesystem path IS (§5.E fs / transport D4.4). Mirrors the
// wire enum as a domain type so the Client interface does not leak protobuf.
type EntryType string

const (
	// EntryUnknown is the zero value: a type this engine version does not model.
	// Callers should skip such an entry rather than guess — that is what makes
	// adding a type later additive.
	EntryUnknown   EntryType = ""
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "dir"
	EntrySymlink   EntryType = "symlink"
)

// EntryInfo describes one filesystem entry (§5.E fs / transport D4.4). Carries no
// owner/group: the engine does not model guest users (§2.2 / §3.2.1).
type EntryInfo struct {
	Name  string // base name
	Path  string // absolute in the GUEST namespace
	Type  EntryType
	Size  int64
	Mode  uint32 // full st_mode incl. type bits
	MTime time.Time
	// SymlinkTarget is set iff Type == EntrySymlink, absolutized against the
	// link's own directory (a relative target means nothing to a host caller).
	SymlinkTarget string
}

// HealthStatus is the domain projection of a successful Health probe (§4.3 L2 +
// activity signal). It carries the sandbox-global idle duration the reaper's
// classifier reads to tell healthy from idle (reaper-health D2). Only valid
// when Health returned a nil error — a failed probe yields no trustworthy
// activity reading, which is exactly the unreadable-idle guard: the classifier
// must never read Idle from a failed probe (reaper-health D2).
type HealthStatus struct {
	// Idle is how long since the sandbox's last I/O across all processes + ptys
	// (reaper-health D2). Zero for callers/guests that never did I/O since the
	// transport (re)started ("idle since start").
	Idle time.Duration
}

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

// The channel is split by CONCERN into three interfaces, composed into Client.
// This is not decoration: each consumer should depend on what it actually uses.
//
//   - The §4 mediator uses HealthProber and nothing else — both its dial sites
//     only probe readiness/liveness. Typed as the full 18-method Client, every L1
//     fake would owe 17 methods it never calls, and one added fs RPC would churn
//     every lifecycle test file that has no opinion about fs.
//   - The orchestrator's data plane is the real consumer of ProcessClient and
//     FsClient — it proxies them to external callers.
//
// The concrete client implements all three, so nothing loses capability; only the
// declared dependencies narrow.

// HealthProber is the readiness/liveness half (§4.3 L2 / transport D4.5). It is
// the ONLY part of the channel the §4 mediator needs.
type HealthProber interface {
	// Health is the unary readiness/liveness probe. First success is the resume
	// readiness gate that flips state to running (guest-runtime D5.2 要点①) — the
	// engine does not trust bare TCP accept. The returned HealthStatus carries the
	// sandbox-global idle duration (§4.3 activity signal); it is only meaningful
	// when err == nil. Readiness callers (cold-start ⑦ gate, adopt probe) ignore it
	// and check err alone; the reaper classifier reads Idle (reaper-health D2).
	Health(ctx context.Context) (HealthStatus, error)
}

// ProcessClient is the §5.E process primitive (transport D4.3): run, reattach,
// input, signal, resize, list. Addressed by guest-native pid throughout.
type ProcessClient interface {
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
	//
	// DIRECTED only: pid must be > 0, or the guest returns invalid_argument. pid <= 0
	// is the POSIX fan-out form (a whole group, or the whole pid namespace) and
	// container-level fan-out belongs to guest PID 1, not this channel (D4.3).
	SendSignal(ctx context.Context, pid int, signal int) error

	// Update resizes a process's PTY (unary; transport D4.3).
	Update(ctx context.Context, pid int, size PtySize) error

	// CloseStdin closes a process's stdin — under a pty, equivalent to Ctrl-D.
	CloseStdin(ctx context.Context, pid int) error

	// List enumerates processes live (no registry, §5.E).
	List(ctx context.Context) ([]int, error)
}

// FsClient is the §5.E fs primitive (transport D4.4): the unary metadata surface
// plus the streaming large-file bypass. No watch — consumers poll Stat+Read.
type FsClient interface {
	// Read reads sandbox file bytes at an offset; engine keeps no read cursor,
	// no cross-resume offset continuity (transport D4.4). No watch — consumers
	// poll Stat+Read (§ render-vs-interact asymmetry, transport D2).
	Read(ctx context.Context, path string, offset, length int64) ([]byte, error)

	// Write writes sandbox file bytes.
	Write(ctx context.Context, path string, data []byte) error

	// Stat describes one path with LSTAT semantics — a symlink is reported as
	// itself, with SymlinkTarget set (transport D4.4).
	Stat(ctx context.Context, path string) (EntryInfo, error)

	// ListDir enumerates a directory up to depth levels, FLATTENED (each entry
	// carries its absolute path). depth must be >= 1; 0 is invalid, not
	// "unlimited". Named ListDir, not List, because List is the PROCESS listing
	// on this same interface.
	ListDir(ctx context.Context, path string, depth uint32) ([]EntryInfo, error)

	// MakeDir creates a directory including parents (mkdir -p). An existing path
	// is an already-exists error.
	MakeDir(ctx context.Context, path string) (EntryInfo, error)

	// Remove deletes a path recursively, and is idempotent — removing something
	// absent succeeds.
	Remove(ctx context.Context, path string) error

	// Move renames source→destination, creating destination parents and
	// overwriting an existing destination. A cross-device move is refused
	// (rename(2) cannot span filesystems) rather than silently copied.
	Move(ctx context.Context, source, destination string) (EntryInfo, error)

	// OpenRead streams a guest file out — the large-file half of the fs surface
	// (transport D4.4). Unlike Read it never materializes the file in memory at
	// either end, so it is the path for anything big. The caller MUST Close the
	// returned reader; the underlying HTTP response body stays open until it does.
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)

	// WriteStream streams src into a guest file, returning the bytes written. The
	// guest writes to a temp file in the destination directory and renames, so an
	// interrupted transfer cannot leave a partial file where a whole one is
	// expected — which is why this path, unlike Write, has no truncate window.
	WriteStream(ctx context.Context, path string, src io.Reader) (int64, error)
}

// Client is the whole host-side channel to one sandbox's current endpoint —
// everything the concrete client can do. Stateless: constructed against an
// endpoint, discarded on resume (the endpoint's netns IP changes, §3.3 / §4.3
// IP-as-epoch).
//
// "Stateless" means THIS VALUE carries no state worth keeping — it does not mean
// dialing is free of process-wide resources. The connection pool underneath is
// shared and outlives any one Client (see Dial); that is what makes discarding
// them freely safe, since a Client has nothing to close.
//
// Depend on the NARROWEST of the three above that your caller actually uses; take
// this composed form only if you genuinely need all three (the data-plane proxy
// does, since it re-exports the full surface).
type Client interface {
	HealthProber
	ProcessClient
	FsClient
}
