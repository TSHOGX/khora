package orchestrator

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/rootfs"
	"engine/internal/ruleset"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
	v1 "engine/internal/transport/gen/engine/transport/v1"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// The §4.2 drain barrier AT THE EDGE THAT TAKES IT. barrier_test.go proves the
// barrier mechanism in isolation; what was untested is the wiring in
// dataplane.go — which handlers take it, which deliberately do not, and what a
// refused call looks like on the wire.
//
// Three properties, and each has a distinct failure mode that the isolated
// barrier tests cannot see:
//
//  1. WRITES are gated. A mutating fs RPC arriving mid-drain must be REFUSED, not
//     queued: queueing starves the drain, because the thing it waits for would be
//     arriving work.
//  2. READS are NOT gated. Blocking them would stall observability exactly when a
//     pause is in progress — the moment an operator most wants to look.
//  3. process.Start is NOT gated. It is long-lived (in flight until the process
//     exits), so gating it would deadlock the barrier against itself: the drain
//     would wait for work that is waiting for the drain.
//
// (2) and (3) are the ones a well-meaning "gate everything that touches the
// guest" change would break, and nothing else in the suite would fail.

// --- fakes: the orchestrator needs a real Mediator, so the seams below it are faked.

type barrierFakeRootfs struct{}

func (barrierFakeRootfs) Name() string { return "fake" }
func (barrierFakeRootfs) CreateOverlay(_ context.Context, id string, lower []string, _ int64) (*rootfs.Overlay, error) {
	return &rootfs.Overlay{ID: id, Lower: lower}, nil
}
func (barrierFakeRootfs) OpenOverlay(_ context.Context, id string, lower []string, _ int64) (*rootfs.Overlay, error) {
	return &rootfs.Overlay{ID: id, Lower: lower}, nil
}
func (barrierFakeRootfs) Mount(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	return &rootfs.Merged{MergedPath: "/fake/" + o.ID}, nil
}
func (barrierFakeRootfs) Unmount(context.Context, *rootfs.Merged) error         { return nil }
func (barrierFakeRootfs) DestroyOverlay(context.Context, *rootfs.Overlay) error { return nil }
func (barrierFakeRootfs) AdoptMerged(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	return &rootfs.Merged{MergedPath: "/fake/" + o.ID}, nil
}
func (barrierFakeRootfs) PromoteToTemplate(context.Context, *rootfs.Overlay, string, []string) error {
	return nil
}
func (barrierFakeRootfs) ListBuiltinTemplates(context.Context) ([]string, error) {
	return nil, nil
}
func (barrierFakeRootfs) UncappedBytes(context.Context) (int64, error) { return 0, nil }
func (barrierFakeRootfs) Usage(context.Context, string) (int64, int64, error) {
	return 0, 0, nil
}
func (barrierFakeRootfs) PromoteCostBytes(context.Context, *rootfs.Overlay, []string) (int64, error) {
	return 0, nil
}
func (barrierFakeRootfs) SweepOrphanLayers(context.Context, []string, []string) (int, error) {
	return 0, nil
}
func (barrierFakeRootfs) SweepOrphanOverlays(context.Context, []string, []string) (int, error) {
	return 0, nil
}

type barrierFakeRuntime struct{}

func (barrierFakeRuntime) Name() string { return "fake" }
func (barrierFakeRuntime) Launch(context.Context, runtime.LaunchSpec) (*runtime.Placement, error) {
	return &runtime.Placement{TransportEndpoint: "10.0.0.1:9000"}, nil
}
func (barrierFakeRuntime) Stop(context.Context, *runtime.Placement) error    { return nil }
func (barrierFakeRuntime) Destroy(context.Context, *runtime.Placement) error { return nil }
func (barrierFakeRuntime) Adopt(context.Context, string) (*runtime.Placement, bool, runtime.DeathCause, error) {
	return nil, false, runtime.CauseUnknown, nil
}
func (barrierFakeRuntime) ReclaimOrphans(context.Context, []string) error { return nil }
func (barrierFakeRuntime) ReinstallRuleSet(context.Context, *runtime.Placement, runtime.SteeringSpec) error {
	return nil
}
func (barrierFakeRuntime) InstallSecondHop(context.Context, ruleset.RuleSet, *int) error {
	return nil
}

// barrierFakeClient is a transport.Client whose Write can be BLOCKED, so a test
// can hold one mutating op in flight and drive a pause into its drain wait
// deterministically. It implements the full Client surface because the data-plane
// proxy re-exports all three halves (HealthProber + ProcessClient + FsClient).
type barrierFakeClient struct {
	// writeEntered is signalled when Write is reached; writeRelease unblocks it.
	writeEntered chan struct{}
	writeRelease chan struct{}
	mu           sync.Mutex
	reads        int
	starts       int
}

func (c *barrierFakeClient) Health(context.Context) (transport.HealthStatus, error) {
	return transport.HealthStatus{}, nil
}
func (c *barrierFakeClient) Write(_ context.Context, _ string, _ []byte) error {
	if c.writeEntered != nil {
		select {
		case c.writeEntered <- struct{}{}:
		default:
		}
		<-c.writeRelease
	}
	return nil
}
func (c *barrierFakeClient) Read(context.Context, string, int64, int64) ([]byte, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return []byte("read-ok"), nil
}
func (c *barrierFakeClient) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}
func (c *barrierFakeClient) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}
func (c *barrierFakeClient) Start(context.Context, transport.StartSpec) (<-chan transport.ProcessEvent, error) {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	ch := make(chan transport.ProcessEvent, 1)
	ch <- transport.ProcessEvent{Kind: "start", PID: 4242}
	close(ch)
	return ch, nil
}
func (c *barrierFakeClient) Connect(context.Context, int) (<-chan transport.ProcessEvent, error) {
	ch := make(chan transport.ProcessEvent)
	close(ch)
	return ch, nil
}
func (c *barrierFakeClient) StreamInput(context.Context, int) (transport.InputWriter, error) {
	return nil, nil
}
func (c *barrierFakeClient) SendInput(context.Context, int, []byte, bool) error   { return nil }
func (c *barrierFakeClient) SendSignal(context.Context, int, int) error           { return nil }
func (c *barrierFakeClient) Update(context.Context, int, transport.PtySize) error { return nil }
func (c *barrierFakeClient) CloseStdin(context.Context, int) error                { return nil }
func (c *barrierFakeClient) List(context.Context) ([]int, error)                  { return nil, nil }
func (c *barrierFakeClient) Stat(context.Context, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *barrierFakeClient) ListDir(context.Context, string, uint32) ([]transport.EntryInfo, error) {
	return nil, nil
}
func (c *barrierFakeClient) MakeDir(context.Context, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *barrierFakeClient) Remove(context.Context, string) error { return nil }
func (c *barrierFakeClient) Move(context.Context, string, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *barrierFakeClient) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (c *barrierFakeClient) WriteStream(_ context.Context, _ string, src io.Reader) (int64, error) {
	return io.Copy(io.Discard, src)
}

// h2cClient speaks cleartext HTTP/2, which the orchestrator's handler requires
// for Connect server-streams. (test/ has an identical helper, but that is a
// different package behind the `substrate` tag — these tests need no substrate.)
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// sidHeader sets the sandbox-id header on unary calls — the only addressing an
// outer-edge client uses (§6.2: placement stays inside the engine).
func sidHeader(sid string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(sandboxIDHeader, sid)
			return next(ctx, req)
		}
	}))
}

// barrierFixture assembles an Engine over fake seams with one `running` sandbox,
// and returns the httptest server plus the sandbox id.
func barrierFixture(t *testing.T, cl *barrierFakeClient, drain time.Duration) (*httptest.Server, *lifecycle.Mediator, model.SandboxID) {
	t.Helper()
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:         mem,
		Runtime:       barrierFakeRuntime{},
		Rootfs:        barrierFakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return cl },
		HealthTimeout: 2 * time.Second,
		DrainGrace:    drain,
	})
	sid, err := med.Create(context.Background(), lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, med, sid
}

// While a pause is draining, a mutating fs RPC is REFUSED with `unavailable`
// (not queued, not silently succeeding). Refusal is what keeps the drain from
// being starved by arriving work.
//
// Also pins the two deliberate NON-gates in the same drain window, because they
// are only observable while a drain is actually open: a Read must still succeed,
// and process.Start must still succeed.
func TestDrainRefusesWritesButNotReadsOrProcessStart(t *testing.T) {
	ctx := context.Background()
	cl := &barrierFakeClient{
		writeEntered: make(chan struct{}, 1),
		writeRelease: make(chan struct{}),
	}
	// A long grace so the drain is still waiting while we probe it. The pause is
	// released explicitly below, so this bounds nothing the test waits on.
	srv, med, sid := barrierFixture(t, cl, 30*time.Second)

	hc := h2cClient()
	fs := transportv1connect.NewFsServiceClient(hc, srv.URL, sidHeader(string(sid)))
	proc := transportv1connect.NewProcessServiceClient(hc, srv.URL, sidHeader(string(sid)))

	// Write #1 goes in flight and parks inside the guest call, holding the barrier.
	w1 := make(chan error, 1)
	go func() {
		_, err := fs.Write(ctx, connect.NewRequest(&v1.WriteRequest{Path: "/root/held", Data: []byte("x")}))
		w1 <- err
	}()
	select {
	case <-cl.writeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write #1 never reached the guest client — fixture wiring is wrong")
	}

	// Start the pause. It marks the sandbox draining, then waits for write #1.
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- med.Pause(ctx, sid) }()

	// POLL until a second write is refused, rather than sleeping a guessed
	// interval: the only thing we need is "the drain has been marked", and polling
	// the observable is what means that in both fast and slow scheduling. A bare
	// sleep would either race the pause goroutine or pad every run.
	var refused error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := fs.Write(ctx, connect.NewRequest(&v1.WriteRequest{Path: "/root/late", Data: []byte("y")}))
		if err != nil {
			refused = err
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if refused == nil {
		t.Fatal("a mutating fs.Write was ACCEPTED while a pause was draining — the barrier is not wired into the data-plane proxy, so a pause can be starved by arriving writes (§4.2 / D4.4)")
	}
	// The pause must still be WAITING here: write #1 is registered in the barrier
	// and has not released. This is the load-bearing half — if fs.Write never took
	// the barrier, the pause would have sailed straight past the in-flight write
	// and frozen the sandbox mid-call, which is the truncation D4.4 exists to stop.
	select {
	case err := <-pauseDone:
		t.Fatalf("Pause COMPLETED (err=%v) while a write was still in flight — the drain did not wait for it, so a pause can land mid-write and truncate the file (§4.2 / D4.4)", err)
	default:
	}
	if got := connect.CodeOf(refused); got != connect.CodeUnavailable {
		t.Errorf("refused write code = %v (%v); want unavailable — the caller must be able to tell 'retry shortly' from a real fault", got, refused)
	}

	// (2) READS are deliberately NOT gated: observability must survive a pause.
	readsBefore := cl.readCount()
	if _, err := fs.Read(ctx, connect.NewRequest(&v1.ReadRequest{Path: "/root/held"})); err != nil {
		t.Errorf("fs.Read was refused during a drain: %v — reads cannot truncate anything, and blocking them stalls observability exactly when an operator most wants to look (D4.4)", err)
	}
	if cl.readCount() == readsBefore {
		t.Error("fs.Read never reached the guest during a drain — it was gated somewhere before the client")
	}

	// (3) process.Start is deliberately NOT gated: it is in flight until the
	// process exits, so gating it would deadlock the drain against itself.
	startsBefore := cl.startCount()
	req := connect.NewRequest(&v1.StartRequest{Cmd: "/bin/true"})
	req.Header().Set(sandboxIDHeader, string(sid))
	stream, err := proc.Start(ctx, req)
	if err != nil {
		t.Errorf("process.Start was refused during a drain: %v — a long-lived Start gated on the drain deadlocks the barrier against itself (D4.4)", err)
	} else {
		for stream.Receive() {
		}
		_ = stream.Close()
	}
	if cl.startCount() == startsBefore {
		t.Error("process.Start never reached the guest during a drain — it was gated, which is the self-deadlock D4.4 warns about")
	}

	// Release write #1 → the drain completes and the pause lands.
	close(cl.writeRelease)
	if err := <-w1; err != nil {
		t.Errorf("the in-flight write failed: %v; the drain should have let it finish", err)
	}
	if err := <-pauseDone; err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if stt, _ := med.State(ctx, sid); stt != model.Paused {
		t.Fatalf("state = %s; want paused", stt)
	}
}

// After the pause settles, mutating writes are accepted again. The pause path
// undrains unconditionally, so one pause must not leave a sandbox permanently
// read-only — the same wedge TestFailedPauseReopensTheDrainBarrier guards on the
// mediator side, here observed through the wire.
func TestWritesAcceptedAgainAfterPauseSettles(t *testing.T) {
	ctx := context.Background()
	cl := &barrierFakeClient{}
	srv, med, sid := barrierFixture(t, cl, time.Second)

	if err := med.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := med.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	fs := transportv1connect.NewFsServiceClient(h2cClient(), srv.URL, sidHeader(string(sid)))
	if _, err := fs.Write(ctx, connect.NewRequest(&v1.WriteRequest{Path: "/root/after", Data: []byte("z")})); err != nil {
		t.Fatalf("fs.Write refused after the pause settled: %v — the barrier was left closed, so the sandbox is permanently read-only (§4.2: undrain must always run)", err)
	}
}
