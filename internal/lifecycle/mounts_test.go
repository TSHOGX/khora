package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// mountFixture builds a mediator with all four seams faked and one volume seeded.
func mountFixture(t *testing.T) (*Mediator, *store.Memory, *fakeVolume, *fakeRuntime) {
	t.Helper()
	st := store.NewMemory()
	seedBare(t, st)
	vb := newFakeVolume()
	rt := &fakeRuntime{}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{}, Volume: vb,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Logf:          func(string, ...any) {},
	})
	return m, st, vb, rt
}

// TestCreateWritesMountRowsThenMaterializes pins the ORDER of the create hooks:
// record → mount rows → cold start, with the cold start reading the table back.
//
// The middle step cannot move: mounts.sandbox needs the record to exist, and the cold
// start needs the rows to exist in order to materialize anything.
func TestCreateWritesMountRowsThenMaterializes(t *testing.T) {
	ctx := context.Background()
	m, st, vb, rt := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}

	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	rows, err := st.ListMounts(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Volume != vid || rows[0].Path != "/data" {
		t.Fatalf("mount rows = %+v; want one row for %s at /data", rows, vid)
	}
	// The backend was asked to materialize it, and the LAUNCH carried the bind.
	var sawMaterialize bool
	for _, c := range vb.Calls() {
		if c == "Materialize:"+string(vid) {
			sawMaterialize = true
		}
	}
	if !sawMaterialize {
		t.Errorf("the volume was never materialized: %v", vb.Calls())
	}
	specs := rt.LastMounts()
	if len(specs) != 1 {
		t.Fatalf("LaunchSpec carried %d mount(s), want 1", len(specs))
	}
	if specs[0].Dest != "/data" || specs[0].SourcePath == "" {
		t.Errorf("mount spec = %+v; want Dest /data and a non-empty source", specs[0])
	}
	if specs[0].ReadOnly {
		t.Errorf("a shared-rw mount was bound READ-ONLY — an inverted bit turns a 4xx at admission into an opaque cold-start mount failure")
	}
}

// TestReadOnlyAccessReachesTheBindAsReadOnly: shared-ro is the ONE access-mode fact
// that crosses to the provider, because it is the only one that changes what the
// provider does. Exclusivity was already decided in the control plane.
func TestReadOnlyAccessReachesTheBindAsReadOnly(t *testing.T) {
	ctx := context.Background()
	m, _, _, rt := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/ro", Access: model.SharedReadOnly}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	specs := rt.LastMounts()
	if len(specs) != 1 || !specs[0].ReadOnly {
		t.Fatalf("shared-ro did not reach the bind as read-only: %+v", specs)
	}
}

// TestCreateNormalizesMountPaths: the store's uniqueness assertions are literal
// string comparisons, so the mediator normalizes before insert (the documented invariant). `/data/` and
// `/data/../data` are one mount point spelled three ways.
func TestCreateNormalizesMountPaths(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data/../data/", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create with an un-normalized path: %v", err)
	}
	rows, _ := st.ListMounts(ctx, sid)
	if len(rows) != 1 || rows[0].Path != "/data" {
		t.Errorf("stored path = %+v; want the normalized \"/data\" — an un-normalized path makes the uniqueness assertions silently not apply", rows)
	}
}

// TestCreateLosingTheLeaseLeavesNothing: a create whose binding loses the exclusive
// lease must leave no record and no rows. The lease is arbitrated by the STORE, which
// is why the mediator has no pre-check to get wrong.
func TestCreateLosingTheLeaseLeavesNothing(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	before, _ := st.ListSandboxes(ctx)

	_, err = m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if !errors.Is(err, store.ErrMountLeaseConflict) {
		t.Fatalf("second exclusive create = %v; want ErrMountLeaseConflict", err)
	}
	after, _ := st.ListSandboxes(ctx)
	if len(after) != len(before) {
		t.Errorf("a create that lost the lease left a record behind: %d sandboxes before, %d after", len(before), len(after))
	}
}

// TestUnwindDeletesMountRows is the ZOMBIE-RECORD path, and the reason unwind's mount
// deletion is a DESIGN REQUIREMENT rather than an implementation detail (the relevant internals page the documented invariant).
//
// Mount rows are written BEFORE the cold start, so every post-cold-start unwind branch
// is followed by a DeleteSandbox that hits mounts.sandbox RESTRICT. An unwind that
// dropped that error would leave a record NOBODY can ever delete — the rows block it
// and no later verb revisits a sandbox that never ran.
func TestUnwindDeletesMountRows(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	vb := newFakeVolume()
	rt := &fakeRuntime{}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{}, Volume: vb,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Logf:          func(string, ...any) {},
	})
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the launch, so the create unwinds AFTER the mount rows are in.
	rt.failLaunch = true

	_, err = m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err == nil {
		t.Fatal("create succeeded despite a failing launch")
	}

	// No sandbox record survived — which is only possible if the mount rows went first.
	recs, lerr := st.ListSandboxes(ctx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(recs) != 0 {
		t.Fatalf("a failed create left %d sandbox record(s) behind: %+v — this is the undeletable zombie unwind's mount deletion exists to prevent", len(recs), recs)
	}
	// And the volume is deletable again: no row holds it.
	if err := m.DeleteVolume(ctx, vid); err != nil {
		t.Errorf("the volume is still held after the failed create: %v — its mount row outlived the sandbox", err)
	}
}

// TestResumeReMountsFromTheCurrentTable is what makes the mount relationship a
// BINDING (the documented invariant / the documented invariant): a resume re-materializes per the table as it stands NOW, not
// per whatever it said when the sandbox last ran, and not from any frozen byte.
func TestResumeReMountsFromTheCurrentTable(t *testing.T) {
	ctx := context.Background()
	m, st, vb, rt := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The ROWS SURVIVE the pause — the lease is not released by pausing (the documented invariant).
	rows, _ := st.ListMounts(ctx, sid)
	if len(rows) != 1 {
		t.Fatalf("mount rows after pause = %d, want 1 — pause must not release the binding", len(rows))
	}
	// Release WAS called for the materialization, though: that is placement-local.
	var released bool
	for _, c := range vb.Calls() {
		if c == "Release:"+string(vid) {
			released = true
		}
	}
	if !released {
		t.Errorf("pause did not release the volume materialization: %v", vb.Calls())
	}

	// Now CHANGE the table while paused, then resume: the new topology must be what is
	// materialized.
	vid2, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteMounts(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMounts(ctx, []model.Mount{
		{Sandbox: sid, Volume: vid2, Path: "/newplace", Access: model.SharedReadOnly},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	specs := rt.LastMounts()
	if len(specs) != 1 || specs[0].Dest != "/newplace" || !specs[0].ReadOnly {
		t.Fatalf("resume materialized %+v; want the CURRENT table's /newplace shared-ro — the binding is re-read, never restored from frozen state", specs)
	}
}

// TestDestroyDeletesMountRowsFirst: the order is required (RESTRICT fires
// immediately), and the row deletion IS the lease release (the documented invariant).
func TestDestroyDeletesMountRowsFirst(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy: %v — the mount rows must be deleted first or RESTRICT blocks the record", err)
	}
	if rows, _ := st.ListMounts(ctx, sid); len(rows) != 0 {
		t.Errorf("mount rows survived destroy: %+v", rows)
	}
	// The LEASE is released, because the row is gone — there is no separate release.
	sid2, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if err != nil {
		t.Fatalf("re-attaching the volume exclusively after destroy: %v — the lease IS the row's existence", err)
	}
	_ = sid2
	// And the volume itself outlived the sandbox (the documented invariant lifecycle decoupling).
	if _, err := st.GetVolume(ctx, vid); err != nil {
		t.Errorf("the volume was destroyed with its sandbox: %v", err)
	}
}

// TestReconcileDeletesMountRowsOfACrashedCreate is the CRASH twin of the unwind path,
// and the one where getting it wrong is PERMANENT: rows referencing a `creating`
// record make RESTRICT block the cleanup delete on EVERY subsequent boot, and nothing
// else in the engine knows to remove them.
func TestReconcileDeletesMountRowsOfACrashedCreate(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Hand-build the crash residue: a `creating` record with mount rows, exactly what a
	// process death between CreateMounts and the committing CAS leaves.
	const sid = model.SandboxID("sbx_crashed")
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: sid, Template: "bare", Overlay: "ov_crashed", State: model.Creating,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMounts(ctx, []model.Mount{
		{Sandbox: sid, Volume: vid, Path: "/data", Access: model.Exclusive},
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := st.GetSandbox(ctx, sid); err != store.ErrNotFound {
		t.Fatalf("the crashed `creating` record survived reconcile (%v) — its mount rows block the delete on every future boot, and nothing else removes them", err)
	}
	if rows, _ := st.ListMounts(ctx, sid); len(rows) != 0 {
		t.Errorf("mount rows of the crashed create survived: %+v", rows)
	}
	// The lease it held is released, so the volume is usable again.
	if err := m.DeleteVolume(ctx, vid); err != nil {
		t.Errorf("the volume is still held after reconcile: %v", err)
	}
}

// TestPromoteExcludesThisSandboxsMountDests: the mediator threads the mount
// destinations to BOTH Promote calls, which is what keeps the documented invariant from ever asking the documented invariant
// (the documented invariant: no subsystem edge) and what keeps the forecast modelling the same freeze.
func TestPromoteExcludesThisSandboxsMountDests(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	vb := newFakeVolume()
	rp := &fakeRootfs{}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp, Volume: vb,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Logf:          func(string, ...any) {},
	})
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/workspace", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Promote(ctx, sid); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// BOTH calls saw the destination, and they saw the SAME one.
	var costSet, freezeSet string
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "PromoteCostBytes:") {
			costSet = c
		}
		if strings.HasPrefix(c, "PromoteToTemplate:") {
			freezeSet = c
		}
	}
	for name, call := range map[string]string{"PromoteCostBytes": costSet, "PromoteToTemplate": freezeSet} {
		if call == "" {
			t.Fatalf("%s was never called", name)
		}
		if !strings.Contains(call, "/workspace") {
			t.Errorf("%s did not receive the mount destination: %q — a static package list cannot know a per-sandbox dest (rootfs-overlay the documented invariant)", name, call)
		}
	}
}

// TestAdoptRecoversVolumeMaterializations closes the the documented invariant / the documented invariant gap that
// AdoptMaterialized existed for without a consumer: an adopted survivor's liveState must
// carry its volume handles, or the next pause/destroy Releases nothing.
//
// THE FAILURE IS INVISIBLE ON THE LOCAL ARM, which is why it needs a test rather than a
// reviewer: Release is a documented no-op there, so a lost handle changes nothing
// observable. On a remote arm the handle owns a live FUSE process and losing it strands
// that process — the exact leak the backend-private handle exists to prevent.
func TestAdoptRecoversVolumeMaterializations(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	vb := newFakeVolume()
	rt := &fakeRuntime{}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{}, Volume: vb,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Logf:          func(string, ...any) {},
	})
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A `running` sandbox with a mount row, and NO live handle — exactly what a
	// control-plane crash leaves behind for reconcile to adopt.
	const sid = model.SandboxID("sbx_survivor")
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: sid, Template: "bare", Overlay: "ov_survivor", State: model.Running,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMounts(ctx, []model.Mount{
		{Sandbox: sid, Volume: vid, Path: "/data", Access: model.Exclusive},
	}); err != nil {
		t.Fatal(err)
	}
	rt.adoptAlive = map[string]bool{string(sid): true}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !m.hasLive(sid) {
		t.Fatalf("the survivor was not adopted, so this test proves nothing about its volumes")
	}
	// The backend was asked to RE-RECOGNIZE, not to materialize afresh: Materialize is
	// create-if-absent and would have invented an empty unit for a volume whose storage
	// was gone, reporting a clean adopt while the guest holds the old directory.
	var sawAdopt, sawMaterialize bool
	for _, c := range vb.Calls() {
		if c == "AdoptMaterialized:"+string(vid) {
			sawAdopt = true
		}
		if c == "Materialize:"+string(vid) {
			sawMaterialize = true
		}
	}
	if !sawAdopt {
		t.Errorf("adopt did not re-recognize the volume: %v", vb.Calls())
	}
	if sawMaterialize {
		t.Errorf("adopt called Materialize (create-if-absent) — that would invent an empty unit for a volume whose storage is gone and report a clean adopt while the guest holds the old one")
	}
	// And the recovered handle is USED: a pause now Releases it.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause the adopted survivor: %v", err)
	}
	var released bool
	for _, c := range vb.Calls() {
		if c == "Release:"+string(vid) {
			released = true
		}
	}
	if !released {
		t.Errorf("pausing the adopted survivor Released nothing: %v — a liveState without its volume handles owes a Release it can never make", vb.Calls())
	}
}

// TestAdoptRefusesWhenAVolumeIsGone: an absent unit makes the adopt FAIL rather than
// silently succeed, and the sandbox is left alone (the documented invariant uncertain → protect).
//
// "Recreate it empty" is not a recovery for a LIVE sandbox: the running guest cannot be
// re-pointed at a new directory, so it would keep writing into the old unlinked one with
// zero errors while the control plane reported everything healthy — the measured mis-sweep
// blast radius (the relevant internals page the documented invariant). A resume later legitimately creates-if-absent, because
// that restarts the guest.
func TestAdoptRefusesWhenAVolumeIsGone(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	vb := newFakeVolume()
	rt := &fakeRuntime{}
	var logged []string
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{}, Volume: vb,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Logf:          func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	})
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	const sid = model.SandboxID("sbx_survivor")
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: sid, Template: "bare", Overlay: "ov_survivor", State: model.Running,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMounts(ctx, []model.Mount{
		{Sandbox: sid, Volume: vid, Path: "/data", Access: model.Exclusive},
	}); err != nil {
		t.Fatal(err)
	}
	rt.adoptAlive = map[string]bool{string(sid): true}
	// The volume's STORAGE is gone while its record and mount row remain — the residue a
	// mis-sweep or an external deletion leaves.
	vb.mu.Lock()
	delete(vb.units, string(vid))
	vb.mu.Unlock()

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile returned fatal: %v — one un-adoptable sandbox must be skipped and logged, not abort recovery of the rest (the documented invariant)", err)
	}
	// NOT adopted, and NOT demoted either: the record stays `running` and its placement
	// stays protected, so nothing was torn down and the next pass retries.
	if m.hasLive(sid) {
		t.Errorf("the sandbox was adopted despite its volume storage being gone — that reports a clean adopt while the guest holds an unlinked directory, which the control plane cannot see")
	}
	rec, err := st.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Running {
		t.Errorf("state = %s; want running left untouched (uncertain → protect: refusing to adopt must not tear anything down)", rec.State)
	}
	if len(logged) == 0 {
		t.Errorf("the refusal was silent — skip-and-log is the the documented invariant discipline, and the log is the only signal")
	}
}
