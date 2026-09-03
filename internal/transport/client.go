package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

// connectClient is the host-side implementation of Client backed by the generated
// Connect-RPC stubs. It translates the DOMAIN types (StartSpec, ProcessEvent, …) to/from
// proto so the mediator never sees generated types (the transport data plane seam discipline). Stateless in
// the sense Client documents: built against one endpoint, discarded on resume, with
// nothing of its own to close.
type connectClient struct {
	health  transportv1connect.HealthServiceClient
	process transportv1connect.ProcessServiceClient
	fs      transportv1connect.FsServiceClient

	// hc/base back the large-file bypass (OpenRead / WriteStream), which is plain
	// streaming HTTP on the same guest listener rather than an RPC (transport behavior).
	hc   *http.Client
	base string
}

// sharedTransport is the ONE h2c transport every Dial hands out, and sharing it is a
// correctness requirement rather than an optimization (guest TCP transport).
//
// A transport OWNS a connection pool and nothing closes a transport, so building one per
// Dial leaks every connection it ever opened along with each one's read/write goroutines:
// measured, 50 sequential Health calls to one endpoint accepted 50 separate TCP
// connections (zero reuse) and left ~150 goroutines resident. Dial is called per
// data-plane RPC and once per running sandbox per reaper tick, so the growth is
// independent of load — fd exhaustion, not a slow leak.
//
// It does NOT weaken the transport data plane's "stateless, discarded on resume": that governs the CLIENT,
// and connectClient is still per-endpoint. The pool is keyed by host:port, so a resumed
// sandbox at a new IP gets a new connection either way.
//
// It also makes ReadIdleTimeout mean what it says: a connection abandoned after one
// unary RPC is never around long enough for a PING to come due.
var sharedTransport = &http2.Transport{
	AllowHTTP: true,
	// h2c: dial plain TCP, ignore the TLS config.
	DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	},
	// Detect a guest that died mid-stream in SECONDS, not minutes. A sandbox
	// terminated by its RAM cap (resource admission) never closes its streams — the process is
	// simply gone — so without this the host waits out the kernel's TCP
	// retransmit timeout, measured at ~150s, before the read fails. That is long
	// enough to look like a hang rather than a death.
	//
	// h2 PING is the right layer: it works even when the stream is legitimately
	// silent, so it cannot be confused with an idle-but-healthy long stream. The
	// guest's own 20s application keepalive (transport behavior) already establishes that
	// silence this long is abnormal, so probing at 30s adds no false positives.
	ReadIdleTimeout: 30 * time.Second,
	PingTimeout:     10 * time.Second,
	// REQUIRED once the pool is shared, and it is the half that bounds the pool by
	// SANDBOX CHURN rather than by request volume. Endpoints are per-placement: every
	// destroy and every resume retires one for good (the ID-reference model), and without an idle
	// timeout its connection would sit in the pool for the life of the process —
	// trading a per-call leak for a per-dead-sandbox one. Comfortably longer than the
	// PING interval above, so a healthy-but-quiet stream is probed, never reaped.
	IdleConnTimeout: 90 * time.Second,
}

// sharedClient wraps sharedTransport. Also handed to the generated Connect stubs,
// which do not close it either.
var sharedClient = &http.Client{Transport: sharedTransport}

// Dial builds a stateless Client for one sandbox's current endpoint. h2c
// (cleartext HTTP/2) so streams work without TLS — the netns channel needs no
// in-channel auth (guest TCP transport trust model).
//
// Cheap by construction: it allocates the per-endpoint stubs and shares the
// connection pool (see sharedTransport). No timeout is set on the http.Client —
// per-call deadlines ride the ctx, and a blanket client timeout would cut off
// legitimately long-lived streams (Start / Connect / the large-file bypass).
func Dial(ep Endpoint) Client {
	hc := sharedClient
	base := "http://" + string(ep)
	return &connectClient{
		health:  transportv1connect.NewHealthServiceClient(hc, base),
		process: transportv1connect.NewProcessServiceClient(hc, base),
		fs:      transportv1connect.NewFsServiceClient(hc, base),
		// Same h2c client and base URL back the large-file bypass (see the fields).
		hc:   hc,
		base: base,
	}
}

func (c *connectClient) Health(ctx context.Context) (HealthStatus, error) {
	resp, err := c.health.Check(ctx, connect.NewRequest(&v1.CheckRequest{}))
	if err != nil {
		return HealthStatus{}, err
	}
	if !resp.Msg.Ok {
		return HealthStatus{}, errors.New("transport: health not ok")
	}
	return HealthStatus{Idle: time.Duration(resp.Msg.IdleMillis) * time.Millisecond}, nil
}

// RecordActivity has no Ok field to check, unlike Check: a stamp either happened
// (the RPC returned) or the transport failed. The reading it returns is taken
// after the stamp, so a success is expected to be near-zero idle.
func (c *connectClient) RecordActivity(ctx context.Context) (HealthStatus, error) {
	resp, err := c.health.RecordActivity(ctx, connect.NewRequest(&v1.RecordActivityRequest{}))
	if err != nil {
		return HealthStatus{}, err
	}
	return HealthStatus{Idle: time.Duration(resp.Msg.IdleMillis) * time.Millisecond}, nil
}

func (c *connectClient) Start(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error) {
	req := &v1.StartRequest{
		Cmd: spec.Cmd, Args: spec.Args, Env: spec.Env, Cwd: spec.Cwd,
		IdempotencyKey: spec.IdempotencyKey, PlacementEpoch: spec.PlacementEpoch,
		ProcessHandle: spec.ProcessHandle,
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

func (c *connectClient) StartHandle(ctx context.Context, spec StartSpec) (<-chan ProcessEvent, error) {
	return c.Start(ctx, spec)
}

func (c *connectClient) Connect(ctx context.Context, pid int) (<-chan ProcessEvent, error) {
	stream, err := c.process.Connect(ctx, connect.NewRequest(&v1.ConnectRequest{Pid: int32(pid)}))
	if err != nil {
		return nil, err
	}
	return drainEvents(stream), nil
}

func (c *connectClient) ConnectHandle(ctx context.Context, handle string, epoch, after uint64) (<-chan ProcessEvent, error) {
	stream, err := c.process.Connect(ctx, connect.NewRequest(&v1.ConnectRequest{ProcessHandle: handle, PlacementEpoch: epoch, AfterSequence: after}))
	if err != nil {
		return nil, err
	}
	return drainEvents(stream), nil
}

// eventStream is the shared shape of Start's and Connect's server-streams.
type eventStream interface {
	Receive() bool
	Msg() *v1.ProcessEvent
	// Err reports why Receive stopped. Part of the interface because the reason is
	// load-bearing: an overflow disconnect and a clean end both stop Receive, and
	// only this tells them apart (nonblocking fan-out).
	Err() error
	Close() error
}

// drainEvents translates a proto ProcessEvent server-stream into the domain
// channel, mapping oneof variants to domain Kind strings (the transport data plane seam: the
// mediator never sees generated types).
func drainEvents(stream eventStream) <-chan ProcessEvent {
	out := make(chan ProcessEvent, 16)
	go func() {
		defer close(out)
		for stream.Receive() {
			ev := stream.Msg()
			switch k := ev.Kind.(type) {
			case *v1.ProcessEvent_Start:
				out <- ProcessEvent{Kind: "start", PID: int(k.Start.Pid), Handle: k.Start.ProcessHandle, Epoch: k.Start.PlacementEpoch, Sequence: ev.Sequence, Replay: ev.Replay, Gap: ev.Gap, ResetReason: ev.ResetReason}
			case *v1.ProcessEvent_Data:
				out <- ProcessEvent{Kind: "data", Stream: k.Data.Stream, Data: k.Data.Data, Handle: ev.ProcessHandle, Epoch: ev.PlacementEpoch, Sequence: ev.Sequence, Replay: ev.Replay, Gap: ev.Gap, ResetReason: ev.ResetReason}
			case *v1.ProcessEvent_End:
				out <- ProcessEvent{Kind: "end", ExitCode: int(k.End.ExitCode), Handle: ev.ProcessHandle, Epoch: ev.PlacementEpoch, Sequence: ev.Sequence, Replay: ev.Replay, Gap: ev.Gap, ResetReason: ev.ResetReason}
			case *v1.ProcessEvent_Keepalive:
				out <- ProcessEvent{Kind: "keepalive", Handle: ev.ProcessHandle, Epoch: ev.PlacementEpoch, Sequence: ev.Sequence, Replay: ev.Replay, Gap: ev.Gap, ResetReason: ev.ResetReason}
			}
		}
		// Surface WHY the stream ended: without it, two opposite outcomes collapse into
		// one silent channel close — a clean end (the process finished; its exit code
		// already arrived) and a overflow disconnect (this subscriber
		// could not keep up and its copy was dropped). The guest reports the latter as
		// resource_exhausted precisely so the consumer can tell them apart, reconnecting
		// being the right response to one and wrong for the other.
		if err := stream.Err(); err != nil {
			out <- ProcessEvent{Kind: "error", Err: err}
		}
		_ = stream.Close()
	}()
	return out
}

// streamInputWriter adapts a Connect client-stream to the domain InputWriter.
type streamInputWriter struct {
	stream *connect.ClientStreamForClient[v1.StreamInputRequest, v1.StreamInputResponse]
	pid    int
	handle string
	epoch  uint64
}

func (w *streamInputWriter) Send(data []byte, ptyIn bool) error {
	return w.stream.Send(&v1.StreamInputRequest{Pid: int32(w.pid), ProcessHandle: w.handle, PlacementEpoch: w.epoch, Data: data, PtyIn: ptyIn})
}

func (c *connectClient) StreamInputHandle(ctx context.Context, handle string, epoch uint64) (InputWriter, error) {
	return &streamInputWriter{stream: c.process.StreamInput(ctx), handle: handle, epoch: epoch}, nil
}

func (c *connectClient) SendInputHandle(ctx context.Context, handle string, epoch uint64, data []byte, ptyIn bool) error {
	_, err := c.process.SendInput(ctx, connect.NewRequest(&v1.SendInputRequest{ProcessHandle: handle, PlacementEpoch: epoch, Data: data, PtyIn: ptyIn}))
	return err
}

func (c *connectClient) SendSignalHandle(ctx context.Context, handle string, epoch uint64, signal int) error {
	_, err := c.process.SendSignal(ctx, connect.NewRequest(&v1.SendSignalRequest{ProcessHandle: handle, PlacementEpoch: epoch, Signal: int32(signal)}))
	return err
}

func (c *connectClient) UpdateHandle(ctx context.Context, handle string, epoch uint64, size PtySize) error {
	_, err := c.process.Update(ctx, connect.NewRequest(&v1.UpdateRequest{ProcessHandle: handle, PlacementEpoch: epoch, Pty: &v1.PtySize{Rows: uint32(size.Rows), Cols: uint32(size.Cols)}}))
	return err
}

func (c *connectClient) CloseStdinHandle(ctx context.Context, handle string, epoch uint64) error {
	_, err := c.process.CloseStdin(ctx, connect.NewRequest(&v1.CloseStdinRequest{ProcessHandle: handle, PlacementEpoch: epoch}))
	return err
}

func (c *connectClient) ListStable(ctx context.Context) ([]StableProcessInfo, error) {
	resp, err := c.process.List(ctx, connect.NewRequest(&v1.ListRequest{}))
	if err != nil {
		return nil, err
	}
	out := make([]StableProcessInfo, 0, len(resp.Msg.Processes))
	for _, p := range resp.Msg.Processes {
		info := StableProcessInfo{Handle: p.ProcessHandle, PID: int(p.Pid), State: p.State, Epoch: p.PlacementEpoch, OutputSequence: p.OutputSequence}
		if p.ExitCode != nil {
			code := int(*p.ExitCode)
			info.ExitCode = &code
		}
		out = append(out, info)
	}
	return out, nil
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

// entryFromWire converts a wire EntryInfo to the domain type. One place, so Stat
// and ListDir cannot decode the same entry differently.
func entryFromWire(e *v1.EntryInfo) EntryInfo {
	if e == nil {
		return EntryInfo{}
	}
	out := EntryInfo{
		Name:  e.Name,
		Path:  e.Path,
		Size:  e.Size,
		Mode:  e.Mode,
		MTime: time.Unix(0, e.MtimeUnixNano),
	}
	switch e.Type {
	case v1.EntryType_ENTRY_TYPE_FILE:
		out.Type = EntryFile
	case v1.EntryType_ENTRY_TYPE_DIRECTORY:
		out.Type = EntryDirectory
	case v1.EntryType_ENTRY_TYPE_SYMLINK:
		out.Type = EntrySymlink
	default:
		// Unknown on the wire stays unknown here (EntryUnknown), rather than being
		// coerced into "file" — a wrong type is worse than an admitted unknown.
		out.Type = EntryUnknown
	}
	if e.SymlinkTarget != nil {
		out.SymlinkTarget = *e.SymlinkTarget
	}
	return out
}

func (c *connectClient) Stat(ctx context.Context, path string) (EntryInfo, error) {
	resp, err := c.fs.Stat(ctx, connect.NewRequest(&v1.StatRequest{Path: path}))
	if err != nil {
		return EntryInfo{}, err
	}
	return entryFromWire(resp.Msg.Entry), nil
}

func (c *connectClient) ListDir(ctx context.Context, path string, depth uint32) ([]EntryInfo, error) {
	resp, err := c.fs.List(ctx, connect.NewRequest(&v1.FsServiceListRequest{
		Path: path, Depth: depth,
	}))
	if err != nil {
		return nil, err
	}
	out := make([]EntryInfo, 0, len(resp.Msg.Entries))
	for _, e := range resp.Msg.Entries {
		out = append(out, entryFromWire(e))
	}
	return out, nil
}

func (c *connectClient) Watch(ctx context.Context, path string, recursive bool, epoch, after uint64) (<-chan WatchEvent, error) {
	stream, err := c.fs.Watch(ctx, connect.NewRequest(&v1.WatchRequest{Path: path, Recursive: recursive, PlacementEpoch: epoch, AfterSequence: after}))
	if err != nil {
		return nil, err
	}
	out := make(chan WatchEvent, 16)
	go func() {
		defer close(out)
		for stream.Receive() {
			e := stream.Msg()
			kind := EntryUnknown
			switch e.Type {
			case v1.EntryType_ENTRY_TYPE_FILE:
				kind = EntryFile
			case v1.EntryType_ENTRY_TYPE_DIRECTORY:
				kind = EntryDirectory
			case v1.EntryType_ENTRY_TYPE_SYMLINK:
				kind = EntrySymlink
			}
			event := WatchEvent{Path: e.Path, OldPath: e.OldPath, Operation: e.Operation, Type: kind, Size: e.Size, MTime: time.Unix(0, e.MtimeUnixNano), Sequence: e.Sequence, Epoch: e.PlacementEpoch, Overflow: e.Overflow, Reset: e.Reset_, ResetReason: e.ResetReason}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		if err := stream.Err(); err != nil {
			event := WatchEvent{Operation: "error", ResetReason: err.Error(), Err: err}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		_ = stream.Close()
	}()
	return out, nil
}

func (c *connectClient) MakeDir(ctx context.Context, path string) (EntryInfo, error) {
	resp, err := c.fs.MakeDir(ctx, connect.NewRequest(&v1.MakeDirRequest{Path: path}))
	if err != nil {
		return EntryInfo{}, err
	}
	return entryFromWire(resp.Msg.Entry), nil
}

func (c *connectClient) Remove(ctx context.Context, path string) error {
	_, err := c.fs.Remove(ctx, connect.NewRequest(&v1.RemoveRequest{Path: path}))
	return err
}

func (c *connectClient) Move(ctx context.Context, source, destination string) (EntryInfo, error) {
	resp, err := c.fs.Move(ctx, connect.NewRequest(&v1.MoveRequest{
		Source: source, Destination: destination,
	}))
	if err != nil {
		return EntryInfo{}, err
	}
	return entryFromWire(resp.Msg.Entry), nil
}

// filesURL builds the bypass URL for a guest path. The path travels as a query
// parameter, url-escaped, so a filename with spaces or '#' survives intact.
func (c *connectClient) filesURL(path string) string {
	return c.base + "/files?path=" + url.QueryEscape(path)
}

// bypassErr turns a non-2xx bypass response into an error, mapping the status back
// to the same classification the RPC path uses so a caller sees one contract
// regardless of which path carried the bytes.
func bypassErr(op, path string, resp *http.Response) error {
	// The guest's body is a short diagnostic; cap it so a misbehaving endpoint
	// cannot stream an unbounded "error" into engine memory.
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	base := op + " " + path + ": " + strings.TrimSpace(string(msg))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return connect.NewError(connect.CodeNotFound, errors.New(base))
	case http.StatusForbidden:
		return connect.NewError(connect.CodePermissionDenied, errors.New(base))
	case http.StatusInsufficientStorage:
		return connect.NewError(connect.CodeResourceExhausted, errors.New(base))
	case http.StatusBadRequest:
		return connect.NewError(connect.CodeInvalidArgument, errors.New(base))
	default:
		return connect.NewError(connect.CodeInternal,
			errors.New(base+" (status "+resp.Status+")"))
	}
}

func (c *connectClient) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.filesURL(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, bypassErr("download", path, resp)
	}
	// The body is handed over UNREAD — that is the point of this path. The caller
	// closes it.
	return resp.Body, nil
}

func (c *connectClient) WriteStream(ctx context.Context, path string, src io.Reader) (int64, error) {
	// counted wraps src so the byte total comes from what was actually consumed off
	// the reader, not from a length the caller asserted.
	counted := &countingReader{r: src}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.filesURL(path), counted)
	if err != nil {
		return 0, err
	}
	// octet-stream: the raw-body framing, so the request streams instead of being
	// buffered into a multipart envelope.
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return counted.n, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return counted.n, bypassErr("upload", path, resp)
	}
	return counted.n, nil
}

// countingReader counts bytes passed through, so WriteStream can report what it
// actually sent for a reader of unknown length.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// compile-time: connectClient satisfies Client.
var _ Client = (*connectClient)(nil)
