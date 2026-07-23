package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
)

// The mediator's SweepOrphanLayers reads the authoritative store for live layer
// IDs and hands the FS provider (live, building). §9: only Promote-built
// (non-builtin) rows are live candidates — a builtin's dir is under TemplatesDir,
// never a sweep candidate — and the in-flight-Promote keep-set is included.
func TestMediatorSweepPassesLiveAndBuilding(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	rp := &fakeRootfs{}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})

	// a builtin (should NOT appear in live) + a Promote-built layer (should).
	if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTemplate(ctx, model.Template{ID: "tmpl_promoted", Parent: "bare", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// pretend a Promote is in flight.
	m.mu.Lock()
	m.buildingLayers["tmpl_inflight"] = true
	m.mu.Unlock()

	if err := m.SweepOrphanLayers(ctx); err != nil {
		t.Fatalf("SweepOrphanLayers: %v", err)
	}

	var got string
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "SweepOrphanLayers:") {
			got = c
		}
	}
	if got == "" {
		t.Fatal("provider SweepOrphanLayers was not called")
	}
	// live must contain the Promote-built layer, NOT the builtin.
	if !strings.Contains(got, "live=tmpl_promoted") {
		t.Errorf("live set wrong: %q; want live=tmpl_promoted (builtin excluded)", got)
	}
	if strings.Contains(got, "bare") {
		t.Errorf("builtin leaked into live set: %q", got)
	}
	// building keep-set forwarded.
	if !strings.Contains(got, "building=tmpl_inflight") {
		t.Errorf("building set not forwarded: %q", got)
	}
}
