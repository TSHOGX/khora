package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/rootfs"
	"engine/internal/runtime"
	"engine/internal/transport"
)

// seedBare inserts the "bare" builtin Template into st so a sandbox create /
// insert satisfies the §4.3 template FK (M5). Production does this via the boot
// seed (§8); the L1 tests do it explicitly. Extra ids can be seeded too.
func seedBare(t *testing.T, st interface {
	CreateTemplate(context.Context, model.Template) error
}, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"bare"}
	}
	for _, id := range ids {
		if err := st.CreateTemplate(context.Background(), model.Template{
			ID: model.TemplateID(id), Builtin: true, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed template %q: %v", id, err)
		}
	}
}

// fakeRootfs / fakeRuntime / fakeClient are L1 fakes (no substrate): they record
// call order and inject failures so the mediator's lock + CAS + unwind logic can
// be tested as pure logic (L1). The real providers are covered by L2/L3.

type fakeRootfs struct {
	mu        sync.Mutex
	calls     []string
	failMount bool
	builtins  []string // §8 boot-seed list returned by ListBuiltinTemplates
}

func (f *fakeRootfs) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeRootfs) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRootfs) Name() string { return "fake_rootfs" }
func (f *fakeRootfs) CreateOverlay(_ context.Context, id string, lower []string) (*rootfs.Overlay, error) {
	f.log("CreateOverlay:" + id + ";lower=" + strings.Join(lower, "|"))
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &rootfs.Overlay{ID: id, Template: tmpl, Lower: append([]string(nil), lower...)}, nil
}
func (f *fakeRootfs) OpenOverlay(_ context.Context, id string, lower []string) (*rootfs.Overlay, error) {
	f.log("OpenOverlay:" + id + ";lower=" + strings.Join(lower, "|"))
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &rootfs.Overlay{ID: id, Template: tmpl, Lower: append([]string(nil), lower...)}, nil
}
func (f *fakeRootfs) PromoteToTemplate(_ context.Context, o *rootfs.Overlay, newID string) error {
	f.log("PromoteToTemplate:" + o.ID + "->" + newID)
	return nil
}
func (f *fakeRootfs) Mount(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	f.log("Mount:" + o.ID)
	if f.failMount {
		return nil, fmt.Errorf("injected mount failure")
	}
	return &rootfs.Merged{MergedPath: "/fake/merged/" + o.ID}, nil
}
func (f *fakeRootfs) Unmount(_ context.Context, m *rootfs.Merged) error {
	f.log("Unmount:" + m.MergedPath)
	return nil
}
func (f *fakeRootfs) DestroyOverlay(_ context.Context, o *rootfs.Overlay) error {
	f.log("DestroyOverlay:" + o.ID)
	return nil
}
func (f *fakeRootfs) AdoptMerged(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	f.log("AdoptMerged:" + o.ID)
	return &rootfs.Merged{MergedPath: "/fake/merged/" + o.ID}, nil
}

// builtins is the list ListBuiltinTemplates returns (§8 boot seed). Empty by
// default; a test sets it to drive the seed. L1 tests seed the store directly
// via seedBare, so most leave this empty.
func (f *fakeRootfs) ListBuiltinTemplates(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.builtins...), nil
}

// SweepOrphanLayers records the (live,building) keep-sets it was called with
// (§9) so the mediator's sweep wiring can be asserted as pure logic. Reclaims
// nothing in the fake (no real layer dirs).
func (f *fakeRootfs) SweepOrphanLayers(_ context.Context, live, building []string) (int, error) {
	f.log("SweepOrphanLayers:live=" + strings.Join(append([]string(nil), live...), "|") +
		";building=" + strings.Join(append([]string(nil), building...), "|"))
	return 0, nil
}

type fakeRuntime struct {
	mu         sync.Mutex
	calls      []string
	failLaunch bool
	// launchGate, if non-nil, blocks Launch until closed (to hold the lock).
	launchGate chan struct{}
	seq        int
	// adoptAlive drives fakeRuntime.Adopt per-sandbox (§4.4-A liveness).
	adoptAlive map[string]bool
	// adoptErr drives a per-sandbox Adopt PROBE error (uncertain → mediator
	// protects the placement, skip-and-log; §4.4-A).
	adoptErr map[string]error
}

func (f *fakeRuntime) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeRuntime) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRuntime) Name() string { return "fake_runsc" }
func (f *fakeRuntime) Launch(ctx context.Context, spec runtime.LaunchSpec) (*runtime.Placement, error) {
	f.log("Launch:" + spec.Sandbox)
	if f.launchGate != nil {
		select {
		case <-f.launchGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failLaunch {
		return nil, fmt.Errorf("injected launch failure")
	}
	f.mu.Lock()
	f.seq++
	ep := fmt.Sprintf("10.0.0.%d:9000", f.seq)
	f.mu.Unlock()
	pl := &runtime.Placement{TransportEndpoint: ep}
	return pl, nil
}
func (f *fakeRuntime) Stop(_ context.Context, _ *runtime.Placement) error {
	f.log("Stop")
	return nil
}
func (f *fakeRuntime) Destroy(_ context.Context, _ *runtime.Placement) error {
	f.log("Destroy")
	return nil
}

// Adopt/ReclaimOrphans back the §4.4-A reconciliation path. The fake is driven
// by adoptAlive (per-sandbox liveness the test sets up) and records reclaim
// calls so the reconciliation orchestration can be tested as pure logic.
func (f *fakeRuntime) Adopt(_ context.Context, sandbox string) (*runtime.Placement, bool, error) {
	f.log("Adopt:" + sandbox)
	f.mu.Lock()
	alive := f.adoptAlive[sandbox]
	err := f.adoptErr[sandbox]
	f.seq++
	ep := fmt.Sprintf("10.9.9.%d:9000", f.seq)
	f.mu.Unlock()
	if err != nil {
		return nil, false, err
	}
	if !alive {
		return nil, false, nil
	}
	return &runtime.Placement{TransportEndpoint: ep}, true, nil
}
func (f *fakeRuntime) ReclaimOrphans(_ context.Context, keep []string) error {
	f.log("ReclaimOrphans:keep=" + strings.Join(append([]string(nil), keep...), "|"))
	return nil
}

// fakeClient satisfies transport.Client; only Health matters for the mediator.
// idle is the sandbox-global idle duration a successful Health reports back
// (reaper-health D2 activity signal) — the classifier reads it to tell healthy
// from idle. Zero unless a test sets it. idleFn, if set, overrides idle and is
// called per probe — used to simulate a sandbox that becomes active BETWEEN the
// sweep classify and the lock-held re-read (the D3 activity-race guard test).
type fakeClient struct {
	healthy   bool
	idle      time.Duration
	idleFn    func() time.Duration
	healthyFn func() bool // if set, overrides healthy per probe (D4b escalation test)
}

func (c *fakeClient) Health(context.Context) (transport.HealthStatus, error) {
	healthy := c.healthy
	if c.healthyFn != nil {
		healthy = c.healthyFn()
	}
	if !healthy {
		return transport.HealthStatus{}, fmt.Errorf("not healthy")
	}
	idle := c.idle
	if c.idleFn != nil {
		idle = c.idleFn()
	}
	return transport.HealthStatus{Idle: idle}, nil
}
func (c *fakeClient) Start(context.Context, transport.StartSpec) (<-chan transport.ProcessEvent, error) {
	return nil, nil
}
func (c *fakeClient) Connect(context.Context, int) (<-chan transport.ProcessEvent, error) {
	return nil, nil
}
func (c *fakeClient) StreamInput(context.Context, int) (transport.InputWriter, error) {
	return nil, nil
}
func (c *fakeClient) SendInput(context.Context, int, []byte, bool) error { return nil }
func (c *fakeClient) SendSignal(context.Context, int, int) error         { return nil }
func (c *fakeClient) Update(context.Context, int, transport.PtySize) error { return nil }
func (c *fakeClient) CloseStdin(context.Context, int) error              { return nil }
func (c *fakeClient) List(context.Context) ([]int, error)                { return nil, nil }
func (c *fakeClient) Read(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}
func (c *fakeClient) Write(context.Context, string, []byte) error { return nil }
