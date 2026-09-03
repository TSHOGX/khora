// Package transport is subsystem the transport data plane: the host↔sandbox channel. The engine
// exposes exactly two stateless RPC services — process and fs — plus a Health
// probe (the transport data plane / the wire protocol). This package is the HOST-SIDE client of those
// services; the in-guest server lives in cmd/sandboxd (transport mode).
//
// Carrier: netns-internal TCP, no vsock (the transport data plane; validated on both substrate arches per
// the root-required deployment). sandboxd listens on a TCP port inside the guest netns; the host dials the
// netns veth IP — unary over HTTP, streams over h2c with immediate flush. Framework is
// Connect-RPC over h2c (streaming transport). Cold start discovers the current netns IP
// before dialing (the runtime provider contract placement); the carrier encodes no fixed address, so an IP change
// across resume invalidates the old endpoint and reconnect just re-resolves (the ID-reference model / health and reclamation
// IP-as-epoch).
//
// Stateless (the transport data plane): the engine keeps no persistent SoT for connections. "What's
// running now" = a live process.List, never a control-plane registry. Bare pty
// byte streams are explicitly lossy (shell has no persistent SoT); any
// "don't drop" guarantee is the upper layer's job (a session adapter, outside the
// engine).
//
// Surface: the full process data plane — Start / Connect (server-stream) /
// StreamInput (client-stream) / SendInput / SendSignal / Update / CloseStdin /
// List; the full fs surface — Read / Write / Stat / ListDir / MakeDir / Remove /
// Move plus the streaming large-file bypass (OpenRead / WriteStream, transport behavior); and
// Health. Bounded ring replay on Connect and slow-subscriber non-blocking drop
// (transport behavior's two invariants) live in the guest server (cmd/sandboxd).
package transport

// Wire stubs are generated from proto/khora/transport/v1/transport.proto via
// buf (schema-first). Regenerate with: cd proto && buf generate.
// Requires buf + protoc-gen-go + protoc-gen-connect-go on PATH. The generated
// code lives in pkg/transport/gen/... — OUTSIDE internal/ so a Go consumer can
// import the protocol (the thin-client contract) — and is committed (buf/protoc are not
// guaranteed present on every checkout). This Client interface is the host-side
// DOMAIN seam; the concrete Connect client translates domain↔proto so
// the mediator never sees generated types.

import (
	"context"
	"io"
	"time"
)

// Endpoint is the current transport endpoint (netns veth IP:port), discovered
// at placement time. It is never cached across resume because a resumed sandbox
// receives a new placement endpoint.
type Endpoint string

// ProcessEvent is one item of a process output stream: oneof{start,data,end,
// keepalive} (process transport) — start(pid) / data(stdout|stderr) /
// end(exit_code) / keepalive.
type ProcessEvent struct {
	Kind        string // "start" | "data" | "end" | "keepalive" | "error"
	PID         int    // set on start
	Handle      string // opaque logical process handle, when the stable contract is used
	Epoch       uint64 // placement epoch carried by stable process events
	Sequence    uint64 // output sequence; zero for non-data control frames
	Replay      bool   // true when the frame came from bounded replay
	Gap         bool   // true when replay could not cover the requested cursor
	ResetReason string
	Stream      string // "stdout" | "stderr" | "pty" — set on data
	Data        []byte // set on data
	ExitCode    int    // set on end
	// Err is set on Kind == "error": the stream ended WITHOUT an end frame, commonly the
	// overflow disconnect (a subscriber that could not keep up is
	// dropped, reported as resource_exhausted). Without it a consumer watching only for
	// "end" cannot tell "the process finished" from "your copy of the output was
	// dropped", and those call for opposite responses — see drainEvents.
	Err error
}

// StartSpec starts a process (the transport data plane process service). PTY is the Pty!=nil special
// case (process transport: pty not a separate service). guest-native pid addressing.
type StartSpec struct {
	Cmd            string
	Args           []string
	Env            []string
	Cwd            string
	Pty            *PtySize // non-nil = allocate a PTY
	IdempotencyKey string
	PlacementEpoch uint64
	// ProcessHandle is an optional stable handle allocated by the control plane
	// before the guest start. Empty lets the guest supervisor allocate one.
	ProcessHandle string
}

type PtySize struct{ Rows, Cols uint16 }

// EntryType is what a filesystem path IS (the transport data plane fs / fs transport). Mirrors the
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

// EntryInfo describes one filesystem entry (the transport data plane fs / fs transport). Carries no
// owner/group: the engine does not model guest users (the guest identity model / the product-neutral sandbox boundary).
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

// HealthStatus is the domain projection of a successful Health probe (health and reclamation L2 +
// activity signal). It carries the sandbox-global idle duration the reaper's classifier
// reads to tell healthy from idle. ONLY VALID WHEN Health RETURNED A NIL ERROR: the
// classifier must never read Idle from a failed probe, which is the unreadable-idle
// guard (the activity signal).
type HealthStatus struct {
	// Idle is how long since the sandbox's last I/O across all processes + ptys
	// (the activity signal). Zero for callers/guests that never did I/O since the
	// transport (re)started ("idle since start").
	Idle time.Duration
}

// InputWriter is an ordered input stream to one process (stdin / pty-in). It is
// the domain projection of the StreamInput client-stream (process transport):
// keystroke ordering is guaranteed by the single stream. Close ends the stream
// (it does NOT close the process's stdin — use CloseStdin for Ctrl-D).
type InputWriter interface {
	// Send writes one ordered frame. ptyIn selects the pty branch vs stdin.
	Send(data []byte, ptyIn bool) error
	// Close ends the input stream and returns the RPC's terminal error, if any.
	Close() error
}

// The channel is split by CONCERN into three interfaces, composed into Client, so each
// consumer depends on what it actually uses. The lifecycle mediator uses HealthProber and
// nothing else — typed as the full 18-method Client, every L1 fake would owe 17 methods
// it never calls, and one added fs RPC would churn every lifecycle test file that has no
// opinion about fs. The orchestrator's data plane is the real consumer of ProcessClient
// and FsClient. The concrete client implements all three, so nothing loses capability;
// only the declared dependencies narrow.

// HealthProber is the readiness/liveness half (health and reclamation L2 / the Health probe). It is
// the ONLY part of the channel the lifecycle mediator needs.
type HealthProber interface {
	// Health is the unary readiness/liveness probe. First success is the resume
	// readiness gate that flips state to running; the
	// engine does not trust bare TCP accept. The returned HealthStatus carries the
	// sandbox-global idle duration (health and reclamation activity signal); it is only meaningful
	// when err == nil. Readiness callers (cold-start gate, adopt probe) ignore it
	// and check err alone; the reaper classifier reads Idle (the activity signal).
	Health(ctx context.Context) (HealthStatus, error)
}

// ActivityRecorder is the WRITE half of the same one activity signal HealthProber reads
// (the activity signal). Split off rather than folded in because the lifecycle mediator only ever
// READS the signal: its classifier must never stamp it, or every probed sandbox would
// look permanently active (reaper serialization: an observation must never be
// indistinguishable from the thing observed). The only consumer is the control-plane API's
// POST /v1/sandboxes/{id}/activity.
type ActivityRecorder interface {
	// RecordActivity stamps the guest's activity clock NOW and reads it back. The
	// returned HealthStatus.Idle is the reading AFTER the stamp — positive
	// confirmation the clock moved, not a pre-stamp value.
	//
	// It is an in-guest RPC and that is load-bearing (guest-side activity measurement: the signal
	// is measured guest-side, never host-side), so it is unavailable in exactly the
	// cases a Health probe is — a caller cannot use it to keep a sandbox whose L2 is
	// down looking alive.
	//
	// NOT a lease / keepalive / pause-inhibit: it carries no duration and grants no
	// protected period, so it needs no per-caller state, no overlap arbitration and
	// no expiry. Those shapes stay rejected (the activity signal).
	RecordActivity(ctx context.Context) (HealthStatus, error)
}

// ProcessClient is the transport data plane process primitive (process transport): run, reattach,
// input, signal, resize, list. Addressed by guest-native pid throughout.
type ProcessClient interface {
	// Start launches a process and returns a channel of output events
	// (server-stream). Addressed by the returned pid thereafter.
	Start(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error)

	// Connect reattaches to a running process: the returned stream replays the
	// bounded output ring buffer first, then joins live (process transport
	// bounded replay). Many Connect can attach one pid, each an independent copy.
	Connect(ctx context.Context, pid int) (<-chan ProcessEvent, error)

	// StreamInput opens an ordered input stream to a process (client-stream).
	// The interactive input path — keystrokes to a pty, in order.
	StreamInput(ctx context.Context, pid int) (InputWriter, error)

	// SendInput writes stdin/pty-in once (unary; for when no stream is held).
	// ptyIn selects the pty branch.
	SendInput(ctx context.Context, pid int, data []byte, ptyIn bool) error

	// SendSignal signals a process; defaults to the process group, falling back
	// to the pid (process transport / the guest startup topology directed signals belong to transport mode).
	//
	// DIRECTED only: pid must be > 0, or the guest returns invalid_argument. pid <= 0
	// is the POSIX fan-out form (a whole group, or the whole pid namespace) and
	// container-level fan-out belongs to guest PID 1, not this channel (transport behavior).
	SendSignal(ctx context.Context, pid int, signal int) error

	// Update resizes a process's PTY (unary; process transport).
	Update(ctx context.Context, pid int, size PtySize) error

	// CloseStdin closes a process's stdin — under a pty, equivalent to Ctrl-D.
	CloseStdin(ctx context.Context, pid int) error

	// List enumerates processes live (no registry, the transport data plane).
	List(ctx context.Context) ([]int, error)
}

// StableProcessClient is the handle/epoch-aware extension of ProcessClient.
// It is separate so existing PID-based callers remain source-compatible while
// new callers can require explicit stale-epoch and replay semantics.
type StableProcessClient interface {
	StartHandle(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error)
	ConnectHandle(ctx context.Context, handle string, epoch, after uint64) (<-chan ProcessEvent, error)
	StreamInputHandle(ctx context.Context, handle string, epoch uint64) (InputWriter, error)
	SendInputHandle(ctx context.Context, handle string, epoch uint64, data []byte, ptyIn bool) error
	SendSignalHandle(ctx context.Context, handle string, epoch uint64, signal int) error
	UpdateHandle(ctx context.Context, handle string, epoch uint64, size PtySize) error
	CloseStdinHandle(ctx context.Context, handle string, epoch uint64) error
	ListStable(ctx context.Context) ([]StableProcessInfo, error)
}

type StableProcessInfo struct {
	Handle         string
	PID            int
	State          string
	ExitCode       *int
	Epoch          uint64
	OutputSequence uint64
}

// FsClient is the transport data plane fs primitive (fs transport): the unary metadata surface
// plus the streaming large-file bypass. No watch — consumers poll Stat+Read.
type FsClient interface {
	// Read reads sandbox file bytes at an offset; engine keeps no read cursor,
	// no cross-resume offset continuity (fs transport). No watch — consumers
	// poll Stat+Read: rendering state is separate from interactive stream state.
	Read(ctx context.Context, path string, offset, length int64) ([]byte, error)

	// Write writes sandbox file bytes.
	Write(ctx context.Context, path string, data []byte) error

	// Stat describes one path with LSTAT semantics — a symlink is reported as
	// itself, with SymlinkTarget set (fs transport).
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
	// (fs transport). Unlike Read it never materializes the file in memory at
	// either end, so it is the path for anything big. The caller MUST Close the
	// returned reader; the underlying HTTP response body stays open until it does.
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)

	// WriteStream streams src into a guest file, returning the bytes written. The
	// guest writes to a temp file in the destination directory and renames, so an
	// interrupted transfer cannot leave a partial file where a whole one is
	// expected — which is why this path, unlike Write, has no truncate window.
	WriteStream(ctx context.Context, path string, src io.Reader) (int64, error)
}

type WatchEvent struct {
	Path        string
	OldPath     string
	Operation   string
	Type        EntryType
	Size        int64
	MTime       time.Time
	Sequence    uint64
	Epoch       uint64
	Overflow    bool
	Reset       bool
	ResetReason string
	// Err is set only on the synthetic error event emitted when the inner
	// stream terminates without a normal close.  Keeping the typed error at
	// the domain seam lets the outer proxy preserve cursor-gap/resource codes
	// while sanitizing transport diagnostics.
	Err error
}

// WatchClient is the placement-scoped change-detection extension. It is kept
// separate from FsClient so existing unary filesystem consumers do not need to
// implement a stream they do not use.
type WatchClient interface {
	Watch(ctx context.Context, path string, recursive bool, epoch, after uint64) (<-chan WatchEvent, error)
}

// Client is the whole host-side channel to one sandbox's current endpoint —
// everything the concrete client can do. Stateless: constructed against an
// endpoint, discarded on resume (the endpoint's netns IP changes, the ID-reference model / health and reclamation
// IP-as-epoch).
//
// "Stateless" means THIS VALUE carries no state worth keeping — not that dialing is free
// of process-wide resources: the connection pool underneath is shared and outlives any
// one Client (see sharedTransport), which is what makes discarding them freely safe.
//
// Depend on the NARROWEST of the three above that your caller actually uses; take this
// composed form only if you genuinely need all three (the data-plane proxy does).
type Client interface {
	HealthProber
	ActivityRecorder
	ProcessClient
	FsClient
}
