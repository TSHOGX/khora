package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"engine/internal/rootfs"
	"engine/internal/runtime"
	"engine/internal/transport"
)

// fakeRootfs / fakeRuntime / fakeClient are L1 fakes (no substrate): they record
// call order and inject failures so the mediator's lock + CAS + unwind logic can
// be tested as pure logic (M1 §8 L1). The real providers are covered by L2/L3.

type fakeRootfs struct {
	mu        sync.Mutex
	calls     []string
	failMount bool
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
func (f *fakeRootfs) CreateOverlay(_ context.Context, id, tmpl string) (*rootfs.Overlay, error) {
	f.log("CreateOverlay:" + id)
	return &rootfs.Overlay{ID: id, Template: tmpl}, nil
}
func (f *fakeRootfs) OpenOverlay(_ context.Context, id, tmpl string) (*rootfs.Overlay, error) {
	f.log("OpenOverlay:" + id)
	return &rootfs.Overlay{ID: id, Template: tmpl}, nil
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

type fakeRuntime struct {
	mu         sync.Mutex
	calls      []string
	failLaunch bool
	// launchGate, if non-nil, blocks Launch until closed (to hold the lock).
	launchGate chan struct{}
	seq        int
	// adoptAlive drives fakeRuntime.Adopt per-sandbox (§4.4-A liveness).
	adoptAlive map[string]bool
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
// calls so Task F's reconciliation orchestration can be tested as pure logic.
func (f *fakeRuntime) Adopt(_ context.Context, sandbox string) (*runtime.Placement, bool, error) {
	f.log("Adopt:" + sandbox)
	f.mu.Lock()
	alive := f.adoptAlive[sandbox]
	f.seq++
	ep := fmt.Sprintf("10.9.9.%d:9000", f.seq)
	f.mu.Unlock()
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
type fakeClient struct{ healthy bool }

func (c *fakeClient) Health(context.Context) error {
	if c.healthy {
		return nil
	}
	return fmt.Errorf("not healthy")
}
func (c *fakeClient) Start(context.Context, transport.StartSpec) (<-chan transport.ProcessEvent, error) {
	return nil, nil
}
func (c *fakeClient) SendInput(context.Context, int, []byte, bool) error { return nil }
func (c *fakeClient) SendSignal(context.Context, int, int) error         { return nil }
func (c *fakeClient) List(context.Context) ([]int, error)                { return nil, nil }
func (c *fakeClient) Read(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}
func (c *fakeClient) Write(context.Context, string, []byte) error { return nil }
