package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/store"
	"github.com/TSHOGX/warren/internal/transport"
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
		Volume:        newFakeVolume(),
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
		Volume:        newFakeVolume(),
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

// TestBuildingOverlaysProtectsAnInFlightCreate is the missing third of the keep-set
// trio. buildingLayers is covered by TestInFlightPromoteProtectedFromSweep above and
// buildingVolumes in volume_test.go; buildingOverlays had no test at all.
//
// WHY IT IS SILENT: an Overlay unit is created on the backend BEFORE the sandbox row is
// visible to a sweep's `live` snapshot, so a healthy in-flight create legitimately
// presents as "bytes with no row" — the sweep's definition of an orphan (the hazard is
// the STALE `live` set, not write ordering: see buildingOverlays' own doc). If the
// keep-set stops being threaded, the sweep destroys the upper of a sandbox whose create
// then "succeeds", and nothing raises an error because reclaiming bytes is the sweep's job.
//
// Asserted from INSIDE the create window via FaultAfterColdStart, which is the only
// point where the marker is provably still held (markBuildingOverlay is deferred
// across the whole create).
func TestBuildingOverlaysProtectsAnInFlightCreate(t *testing.T) {
	rp := &fakeRootfs{}
	rt := &fakeRuntime{}
	var sweptDuring string
	var sawHook bool

	m, st := newLimitsMediator(t, rp, rt, NodeCapacity{})
	m.deps.FaultAfterColdStart = func(id model.SandboxID) {
		sawHook = true
		// A concurrent sweep, exactly as the reaper would run it mid-create.
		if err := m.SweepOrphanOverlays(context.Background()); err != nil {
			t.Errorf("sweep during create: %v", err)
		}
		for _, c := range rp.Calls() {
			if strings.HasPrefix(c, "SweepOrphanOverlays:") {
				sweptDuring = c
			}
		}
	}
	id, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Non-vacuity: if the hook never ran, everything below would pass trivially.
	if !sawHook {
		t.Fatal("FaultAfterColdStart never fired — the sweep never ran inside the create window, so this test proves nothing")
	}
	if sweptDuring == "" {
		t.Fatal("no SweepOrphanOverlays call was recorded during the create window")
	}

	rec, err := st.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	ov := string(rec.Overlay)
	if ov == "" {
		t.Fatal("sandbox has no Overlay id")
	}
	// THE ASSERTION: the in-flight unit must be named in the keep-set handed to the
	// provider. Being in `live` too is fine and expected once the row is visible —
	// what must never happen is the unit appearing in NEITHER set, which is what the
	// provider reads as "orphan, reclaim it".
	live, building := parseKeepSets(t, sweptDuring)
	if !building[ov] && !live[ov] {
		t.Fatalf("Overlay %s was in NEITHER keep-set during its own create: %s\n"+
			"the provider reads that as an orphan and destroys the upper of a sandbox whose row exists — "+
			"and reclaiming bytes is the sweep's JOB, so nothing errors (§4.4-A in-flight build keep-set)", ov, sweptDuring)
	}
	if !building[ov] {
		t.Errorf("Overlay %s was protected only by `live`, not by `building`: %s\n"+
			"that is the stale-snapshot hazard buildingOverlays exists for — a sweep whose `live` read happened "+
			"BEFORE the row landed would have no protection left", ov, sweptDuring)
	}
}

// parseKeepSets splits the fake's recorded "…:live=a|b;building=c" call.
func parseKeepSets(t *testing.T, call string) (live, building map[string]bool) {
	t.Helper()
	live, building = map[string]bool{}, map[string]bool{}
	_, args, ok := strings.Cut(call, ":")
	if !ok {
		t.Fatalf("unparseable sweep call: %q", call)
	}
	lpart, bpart, ok := strings.Cut(args, ";building=")
	if !ok {
		t.Fatalf("unparseable sweep call: %q", call)
	}
	for _, s := range strings.Split(strings.TrimPrefix(lpart, "live="), "|") {
		if s != "" {
			live[s] = true
		}
	}
	for _, s := range strings.Split(bpart, "|") {
		if s != "" {
			building[s] = true
		}
	}
	return live, building
}
