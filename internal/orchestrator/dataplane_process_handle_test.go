package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

func processReservationFixture(t *testing.T) (*processProxy, model.SandboxID) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now()
	if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sid := model.SandboxID("sbx_process_reservation")
	if err := st.CreateSandbox(ctx, model.Sandbox{ID: sid, Template: "bare", Overlay: "ov_process_reservation", State: model.Paused, CreatedAt: now, UpdatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	return &processProxy{e: &Engine{Mediator: lifecycle.New(lifecycle.Deps{Store: st}), Store: st}}, sid
}

func TestReserveProcessHonorsCallerSuppliedHandle(t *testing.T) {
	p, sid := processReservationFixture(t)
	req := &v1.StartRequest{Cmd: "/bin/sh", ProcessHandle: "proc_caller", PlacementEpoch: 7}
	got, created, err := p.reserveProcess(context.Background(), sid, 7, "", req.ProcessHandle, processStartRequestHash(req))
	if err != nil {
		t.Fatal(err)
	}
	if !created || got.ID != model.ProcessID(req.ProcessHandle) {
		t.Fatalf("reservation = id=%q created=%v; want caller handle %q", got.ID, created, req.ProcessHandle)
	}
}

func TestReserveProcessRejectsKeyRetryWithDifferentCallerHandle(t *testing.T) {
	p, sid := processReservationFixture(t)
	first := &v1.StartRequest{Cmd: "/bin/sh", ProcessHandle: "proc_first", IdempotencyKey: "start-key", PlacementEpoch: 7}
	if _, _, err := p.reserveProcess(context.Background(), sid, 7, first.IdempotencyKey, first.ProcessHandle, processStartRequestHash(first)); err != nil {
		t.Fatal(err)
	}
	second := &v1.StartRequest{Cmd: "/bin/sh", ProcessHandle: "proc_second", IdempotencyKey: first.IdempotencyKey, PlacementEpoch: 7}
	if _, _, err := p.reserveProcess(context.Background(), sid, 7, second.IdempotencyKey, second.ProcessHandle, processStartRequestHash(second)); !errors.Is(err, model.ErrIdempotencyConflict) {
		t.Fatalf("changed handle retry = %v; want idempotency conflict", err)
	}
}
