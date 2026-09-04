package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

type renewalFailureStore struct{ store.Store }

func (s renewalFailureStore) RenewOperation(context.Context, model.OperationID, uint32, time.Time, time.Duration) error {
	return store.ErrOperationOwnershipConflict
}

func TestOperationLeaseRenewsPastOriginalDeadline(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hash, _ := model.RequestHash(map[string]any{"lease": "renew"})
	now := time.Now().UTC()
	op, err := model.BeginOperation("op_lease", "volume.create", "lease-key", hash, now, 60*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st, Volume: newFakeVolume(), Logf: func(string, ...any) {}})
	ownedCtx, claim, stop := m.startOperationLeaseWith(ctx, operationClaim{op: &op}, 60*time.Millisecond, 10*time.Millisecond)
	defer stop()
	time.Sleep(110 * time.Millisecond)
	if err := ownedCtx.Err(); err != nil {
		t.Fatalf("renewed operation context canceled: %v", err)
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LeaseUntil.After(now.Add(60*time.Millisecond)) || claim.lease.Err() != nil {
		t.Fatalf("lease was not renewed: lease_until=%s err=%v", got.LeaseUntil, claim.lease.Err())
	}
}

func TestOperationLeaseCancelsOnOwnershipLoss(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hash, _ := model.RequestHash(map[string]any{"lease": "loss"})
	op, err := model.BeginOperation("op_lease_loss", "volume.create", "lease-loss-key", hash, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: renewalFailureStore{Store: st}, Logf: func(string, ...any) {}})
	ownedCtx, claim, stop := m.startOperationLeaseWith(ctx, operationClaim{op: &op}, time.Minute, 5*time.Millisecond)
	defer stop()
	select {
	case <-ownedCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ownership loss did not cancel operation context")
	}
	if !errors.Is(claim.lease.Err(), store.ErrOperationOwnershipConflict) {
		t.Fatalf("lease error = %v; want ownership conflict", claim.lease.Err())
	}
}

type digestRootfs struct {
	*fakeRootfs
	digest string
}

func (r *digestRootfs) TemplateDigest(context.Context, string) (string, error) { return r.digest, nil }

func TestCreatePauseResumeAdvancesPlacementEpoch(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.GetPlacement(ctx, sid)
	if err != nil || first.Epoch != model.FirstPlacementEpoch || first.State != model.PlacementActive {
		t.Fatalf("first placement = %+v, %v", first, err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatal(err)
	}
	lost, err := st.GetPlacement(ctx, sid)
	if err != nil || lost.Epoch != first.Epoch || lost.State != model.PlacementLost {
		t.Fatalf("paused placement = %+v, %v", lost, err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatal(err)
	}
	second, err := st.GetPlacement(ctx, sid)
	if err != nil || second.Epoch != first.Epoch+1 || second.State != model.PlacementActive {
		t.Fatalf("resumed placement = %+v, %v", second, err)
	}
	if err := m.ValidatePlacementEpoch(ctx, sid, first.Epoch); model.ErrorClassOf(err) != model.ErrorStaleEpoch {
		t.Fatalf("stale epoch = %v; want stale_epoch", err)
	}
	if err := m.ValidatePlacementEpoch(ctx, sid, second.Epoch); err != nil {
		t.Fatalf("current epoch refused: %v", err)
	}
	events, _, _, err := st.Events(ctx, 0, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if events[0].PlacementEpoch != first.Epoch || events[2].PlacementEpoch != second.Epoch {
		t.Fatalf("event epochs = %d/%d, want %d/%d", events[0].PlacementEpoch, events[2].PlacementEpoch, first.Epoch, second.Epoch)
	}
}

func TestColdStartCarriesProcessRetentionToRuntime(t *testing.T) {
	ctx := context.Background()
	m, _, _, rt := mountFixture(t)
	want := 37 * time.Minute
	m.deps.ProcessRetention = model.ProcessRetentionPolicy{ExitedFor: want}
	if _, err := m.Create(ctx, CreateSpec{Template: "bare"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(rt.launchProcessRetention) != 1 || rt.launchProcessRetention[0] != want {
		t.Fatalf("runtime process retention = %v; want [%s]", rt.launchProcessRetention, want)
	}
}

func TestAdoptPreservesPlacementEpoch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	rt := &fakeRuntime{adoptAlive: map[string]bool{"sbx_a": true}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{}, Volume: newFakeVolume(),
		Dial: func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		Logf: func(string, ...any) {},
	})
	now := time.Now()
	rec := model.Sandbox{ID: "sbx_a", Template: "bare", Overlay: "ov_a", State: model.Running, Network: networkZero(), CreatedAt: now, UpdatedAt: now}
	if err := st.CreateSandbox(ctx, rec, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlacement(ctx, model.Placement{Sandbox: rec.ID, Epoch: 7, Provider: rt.Name(), State: model.PlacementActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	adopted, err := m.adoptOrDemote(ctx, rec)
	if err != nil || !adopted {
		t.Fatalf("adopt = %v, %v", adopted, err)
	}
	got, err := st.GetPlacement(ctx, rec.ID)
	if err != nil || got.Epoch != 7 {
		t.Fatalf("adopted placement = %+v, %v; epoch must be preserved", got, err)
	}
}

func networkZero() model.NetworkDecl {
	return model.NetworkDecl{Egress: model.NetEgress{Default: model.NetDeny}}.Normalize()
}

func TestPlacementLossPausesLogicalProcesses(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	pl, _ := st.GetPlacement(ctx, sid)
	p := model.Process{ID: "proc_a", Sandbox: sid, PlacementEpoch: pl.Epoch, PID: 42, State: model.ProcessRunning, StartedAt: time.Now(), OutputCursor: model.ProcessOutputCursor("proc_a", pl.Epoch, 0)}
	if err := st.CreateProcess(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidatePlacementEpoch(ctx, sid, 0); !errors.Is(err, model.ErrInvalidArgument) {
		t.Fatalf("zero epoch = %v; want invalid argument", err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProcess(ctx, p.ID)
	if err != nil || got.State != model.ProcessPaused || got.PID != 0 {
		t.Fatalf("process after pause = %+v, %v", got, err)
	}
}

func TestSandboxCreateIdempotencyConvergesAndConflicts(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	spec := CreateSpec{Template: "bare", IdempotencyKey: "sandbox-key"}
	first, err := m.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Create(ctx, spec)
	if err != nil || second != first {
		t.Fatalf("retry = %s, %v; want %s", second, err, first)
	}
	recs, _ := st.ListSandboxes(ctx)
	if len(recs) != 1 {
		t.Fatalf("idempotent retry left %d sandbox records", len(recs))
	}
	changed := spec
	changed.Limits.MemoryByte = 1
	if _, err := m.Create(ctx, changed); model.ErrorClassOf(err) != model.ErrorIdempotencyConflict {
		t.Fatalf("key reuse = %v; want idempotency_conflict", err)
	}
}

func TestVolumeCreateIdempotencyConvergesAndConflicts(t *testing.T) {
	ctx := context.Background()
	m, st, vb, _ := mountFixture(t)
	first, err := m.CreateVolumeWithKey(ctx, 0, "volume-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.CreateVolumeWithKey(ctx, 0, "volume-key")
	if err != nil || second != first {
		t.Fatalf("retry = %s, %v; want %s", second, err, first)
	}
	vols, _ := st.ListVolumes(ctx)
	if len(vols) != 1 {
		t.Fatalf("idempotent retry left %d volumes", len(vols))
	}
	if _, err := m.CreateVolumeWithKey(ctx, 1, "volume-key"); model.ErrorClassOf(err) != model.ErrorIdempotencyConflict {
		t.Fatalf("key reuse = %v; want idempotency_conflict", err)
	}
	creates := 0
	for _, call := range vb.Calls() {
		if strings.HasPrefix(call, "CreateVolume:") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("backend create calls = %d, want 1", creates)
	}
}

func TestStateCoreRestartTakeoverPreservesCleanupPhase(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	m := New(Deps{Store: st, Volume: newFakeVolume(), Logf: func(string, ...any) {}})
	hash, _ := model.RequestHash(map[string]any{"size": 0})
	op, err := model.BeginOperation("op_stale", "volume.create", "key", hash, time.Now().Add(-time.Minute), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = model.OperationMaterialized
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileStateCore(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 2 || got.Phase != model.OperationRecorded || got.RecoveryAction() != model.RecoveryRetry {
		t.Fatalf("takeover = attempt %d phase %s action %s; restart cleanup should converge the side-effect window back to recorded", got.Attempt, got.Phase, got.RecoveryAction())
	}
}

func TestStateCoreRestartReclaimsMaterializedVolumeWithoutRetry(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	vb := newFakeVolume()
	m := New(Deps{Store: st, Volume: vb, Logf: func(string, ...any) {}})
	hash, _ := model.RequestHash(map[string]any{"size": 0})
	started := time.Now().Add(-time.Minute)
	op, err := model.BeginOperation("op_reconcile_cleanup", "volume.create", "cleanup-key", hash, started, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = model.OperationMaterialized
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	orphanID := deterministicObjectID("vol_", op.ID)
	vb.mu.Lock()
	vb.units[orphanID] = true
	vb.mu.Unlock()
	if err := m.ReconcileStateCore(ctx); err != nil {
		t.Fatal(err)
	}
	vb.mu.Lock()
	stillPresent := vb.units[orphanID]
	vb.mu.Unlock()
	if stillPresent {
		t.Fatalf("materialized volume %s survived restart reconciliation; cleanup must not require a caller retry", orphanID)
	}
	if !contains(vb.Calls(), "DestroyVolume:"+orphanID) {
		t.Fatalf("restart reconciliation did not call backend cleanup for %s: %v", orphanID, vb.Calls())
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != model.OperationRecorded || got.State != model.OperationInProgress {
		t.Fatalf("operation after cleanup = %+v; want recorded in_progress for a future explicit retry", got)
	}
}

func TestStateCoreRestartReplaysCommittedVolumeInsteadOfCleaningIt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	vb := newFakeVolume()
	m := New(Deps{Store: st, Volume: vb, Logf: func(string, ...any) {}})
	hash, _ := model.RequestHash(map[string]any{"size": 0})
	started := time.Now().Add(-time.Minute)
	op, err := model.BeginOperation("op_reconcile_committed", "volume.create", "committed-key", hash, started, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = model.OperationMaterialized
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	id := deterministicObjectID("vol_", op.ID)
	vb.mu.Lock()
	vb.units[id] = true
	vb.mu.Unlock()
	if err := st.CreateVolume(ctx, model.Volume{ID: model.VolumeID(id), Backend: vb.Name(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileStateCore(ctx); err != nil {
		t.Fatal(err)
	}
	vb.mu.Lock()
	stillPresent := vb.units[id]
	vb.mu.Unlock()
	if !stillPresent {
		t.Fatalf("committed volume %s was cleaned during restart reconciliation", id)
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.OperationSucceeded || got.Result == nil || got.Result.ID != id {
		t.Fatalf("operation after committed replay = %+v; want succeeded result %s", got, id)
	}
}

func TestTemplateManifestDigestMismatchRefusesLaunch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now()
	manifest := defaultTemplateManifest()
	manifest.DigestAlgorithm = model.DigestSHA256
	manifest.ContentDigest = model.DigestBytes([]byte("expected"))
	if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, Manifest: manifest, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	rp := &digestRootfs{fakeRootfs: &fakeRootfs{}, digest: model.DigestBytes([]byte("different"))}
	m := New(Deps{Store: st, Runtime: rt, Rootfs: rp, Volume: newFakeVolume(), Dial: func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} }, Logf: func(string, ...any) {}})
	if _, err := m.Create(ctx, CreateSpec{Template: "bare"}); err == nil {
		t.Fatal("digest mismatch launch succeeded")
	}
	for _, call := range rt.Calls() {
		if strings.HasPrefix(call, "Launch:") {
			t.Fatalf("runtime launched despite manifest mismatch: %v", rt.Calls())
		}
	}
}

func TestPromoteRejectsDeclaredDigestMismatchBeforeTemplateCommit(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	digest := model.DigestBytes([]byte("actual"))
	rp := &digestRootfs{fakeRootfs: &fakeRootfs{}, digest: digest}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp, Volume: newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond, Logf: func(string, ...any) {},
	})
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wanted := model.DigestBytes([]byte("different"))
	manifest := defaultTemplateManifest()
	manifest.DigestAlgorithm = model.DigestSHA256
	manifest.ContentDigest = wanted
	if _, err := m.PromoteWithManifest(ctx, sid, &manifest); model.ErrorClassOf(err) != model.ErrorConflict {
		t.Fatalf("promote digest mismatch = %v; want conflict", err)
	}
	// Digest verification is against the newly materialized layer, so this
	// refusal is intentionally post-quiesce. The source is left at rest and the
	// unclaimed layer is recovered by the orphan-layer sweep; no template row is
	// exposed.
	rec, err := st.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Paused {
		t.Fatalf("source state after post-materialization digest refusal = %s; want paused", rec.State)
	}
	templates, err := st.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range templates {
		if !tm.Builtin {
			t.Fatalf("mismatched manifest committed template row: %+v", tm)
		}
	}
}
