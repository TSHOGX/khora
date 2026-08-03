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
// IDs and hands the rootfs provider (live, building). §5.B / §4.4-A: only Promote-built
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

// in-flight guard: while a Promote is mid-freeze (bytes down, row not
// yet inserted), a concurrent sweep must see the layer in the buildingLayers
// keep-set and NOT reclaim it. Deterministic via a gate that blocks the freeze.
func TestInFlightPromoteProtectedFromSweep(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	gate := make(chan struct{})
	reached := make(chan struct{})
	rp := &fakeRootfs{promoteGate: gate, promoteReached: reached}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})

	// a paused source to promote (no cold-start needed).
	seedPaused(t, st, "sbx_p", "ov_p", "bare")

	// start Promote in the background; it will block inside the freeze.
	promoted := make(chan model.TemplateID, 1)
	go func() {
		tid, err := m.Promote(ctx, "sbx_p")
		if err != nil {
			t.Errorf("promote: %v", err)
		}
		promoted <- tid
	}()

	// wait until the freeze is entered → the layer id is registered in
	// buildingLayers (markBuilding ran before PromoteToTemplate).
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("promote never reached the freeze")
	}

	// the building keep-set now holds exactly the in-flight layer.
	building := m.buildingLayerIDs()
	if len(building) != 1 {
		t.Fatalf("buildingLayers = %v; want exactly the in-flight layer", building)
	}
	// a concurrent sweep must forward that id as protected (not reclaim it).
	if err := m.SweepOrphanLayers(ctx); err != nil {
		t.Fatalf("concurrent sweep: %v", err)
	}
	var swept string
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "SweepOrphanLayers:") {
			swept = c
		}
	}
	if !strings.Contains(swept, "building="+building[0]) {
		t.Fatalf("in-flight layer not protected during sweep: %q (building=%v)", swept, building)
	}

	// release the freeze → Promote completes, row inserted, keep-set cleared.
	close(gate)
	tid := <-promoted
	if _, err := st.GetTemplate(ctx, tid); err != nil {
		t.Fatalf("promoted template row missing after completion: %v", err)
	}
	if len(m.buildingLayerIDs()) != 0 {
		t.Fatalf("buildingLayers not cleared after promote: %v", m.buildingLayerIDs())
	}
}
