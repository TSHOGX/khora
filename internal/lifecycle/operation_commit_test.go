package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// succeedLedgerFault fails the first UpdateOperation that would record a
// successful idempotency result, then lets the retry of that write through.
type succeedLedgerFault struct {
	*store.Memory
	failsLeft int
}

func (s *succeedLedgerFault) UpdateOperation(ctx context.Context, op model.Operation) error {
	if op.State == model.OperationSucceeded && s.failsLeft > 0 {
		s.failsLeft--
		return errors.New("injected operation ledger failure")
	}
	return s.Memory.UpdateOperation(ctx, op)
}

func TestSandboxCreateSurvivesOperationLedgerFailure(t *testing.T) {
	ctx := context.Background()
	st := &succeedLedgerFault{Memory: store.NewMemory(), failsLeft: 1}
	seedBare(t, st)
	m := New(Deps{
		Store:         st,
		Runtime:       &fakeRuntime{},
		Rootfs:        &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
		Deployment:    network.Deployment{HostLocal: viaHostLocal},
	})
	key := model.IdempotencyKey("sandbox-ledger-fault")
	sid, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key})
	if err != nil || sid == "" {
		t.Fatalf("create = %s, %v; want the running sandbox id", sid, err)
	}
	rec, err := st.GetSandbox(ctx, sid)
	if err != nil || rec.State != model.Running {
		t.Fatalf("sandbox after ledger fault = %+v, %v; want running", rec, err)
	}
	op, err := st.GetOperationByKey(ctx, "sandbox.create", key)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != model.OperationSucceeded || op.Result == nil || op.Result.ID != string(sid) {
		t.Fatalf("operation = %+v; want succeeded result %s", op, sid)
	}
	replayed, err := m.Create(ctx, CreateSpec{Template: "bare", IdempotencyKey: key})
	if err != nil || replayed != sid {
		t.Fatalf("retry = %s, %v; want replay of %s", replayed, err, sid)
	}
}

func TestFailOperationDoesNotTerminalizeCommittedSandbox(t *testing.T) {
	ctx := context.Background()
	m, st := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	semantic := struct {
		N int `json:"n"`
	}{1}
	hash, err := model.RequestHash(semantic)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	op, err := model.BeginOperation("op_committed", "sandbox.create", "committed-key", hash, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = model.OperationMaterialized
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	sid := model.SandboxID(deterministicObjectID("sbx_", op.ID))
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: sid, Template: "bare", State: model.Running, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	m.failOperation(ctx, operationClaim{op: &op}, errors.New("ledger update failed"))
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.OperationSucceeded || got.Result == nil || got.Result.ID != string(sid) {
		t.Fatalf("committed failure recorded as %+v; want succeeded %s", got, sid)
	}
	replay, err := m.claimOperation(ctx, "sandbox.create", "committed-key", semantic)
	if err != nil {
		t.Fatal(err)
	}
	if replay.replay == nil || replay.replay.ID != string(sid) {
		t.Fatalf("replay = %+v; want %s", replay.replay, sid)
	}
}

func TestFailOperationStillTerminalizesUncommittedWork(t *testing.T) {
	ctx := context.Background()
	m, st := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	hash, err := model.RequestHash(struct {
		N int `json:"n"`
	}{2})
	if err != nil {
		t.Fatal(err)
	}
	op, err := model.BeginOperation("op_open", "sandbox.create", "open-key", hash, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	m.failOperation(ctx, operationClaim{op: &op}, errors.New("overlay create failed"))
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.OperationFailed || got.ErrorClass != model.ErrorInternal {
		t.Fatalf("uncommitted failure = state %q class %q; want failed/internal", got.State, got.ErrorClass)
	}
}

func TestCreateVolumeSurvivesOperationLedgerFailure(t *testing.T) {
	ctx := context.Background()
	st := &succeedLedgerFault{Memory: store.NewMemory(), failsLeft: 1}
	vb := newFakeVolume()
	m := New(Deps{
		Store:   st,
		Runtime: &fakeRuntime{},
		Rootfs:  &fakeRootfs{},
		Volume:  vb,
		Logf:    func(string, ...any) {},
	})
	key := model.IdempotencyKey("volume-ledger-fault")
	vid, err := m.CreateVolumeWithKey(ctx, 0, key)
	if err != nil || vid == "" {
		t.Fatalf("create volume = %s, %v; want the volume id", vid, err)
	}
	if _, err := st.GetVolume(ctx, vid); err != nil {
		t.Fatalf("volume row: %v", err)
	}
	op, err := st.GetOperationByKey(ctx, "volume.create", key)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != model.OperationSucceeded || op.Result == nil || op.Result.ID != string(vid) {
		t.Fatalf("operation = %+v; want succeeded result %s", op, vid)
	}
	replayed, err := m.CreateVolumeWithKey(ctx, 0, key)
	if err != nil || replayed != vid {
		t.Fatalf("retry = %s, %v; want replay of %s", replayed, err, vid)
	}
}
