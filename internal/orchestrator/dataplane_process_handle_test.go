package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
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

func TestProcessStartIdentityGateSerializesRetryWindow(t *testing.T) {
	e := &Engine{}
	identity := processStartIdentity("sbx_a", 3, "retry-key", "")
	release, err := e.acquireProcessStart(context.Background(), identity)
	if err != nil || release == nil {
		t.Fatalf("first gate = release=%v err=%v", release != nil, err)
	}
	defer release()
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		second, err := e.acquireProcessStart(waitCtx, identity)
		if second != nil {
			second()
		}
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatal("retry acquired gate before initial dispatch released it")
	}
}

func TestCanceledReservationDeletionRequiresAnonymousStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created bool
		key     string
		handle  string
		want    bool
	}{
		{name: "anonymous newly-created", created: true, want: true},
		{name: "keyed retry identity", created: true, key: "retry", want: false},
		{name: "caller handle identity", created: true, handle: "proc_fixed", want: false},
		{name: "existing row", created: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canceledReservationMayBeDeleted(tc.created, tc.key, tc.handle); got != tc.want {
				t.Fatalf("canceledReservationMayBeDeleted(%v,%q,%q) = %v; want %v", tc.created, tc.key, tc.handle, got, tc.want)
			}
		})
	}
}

func TestProcessStartRequestHashIncludesEpochWithoutKey(t *testing.T) {
	a := processStartRequestHash(&v1.StartRequest{Cmd: "/bin/true", PlacementEpoch: 1})
	b := processStartRequestHash(&v1.StartRequest{Cmd: "/bin/true", PlacementEpoch: 2})
	if a == "" || b == "" || a == b {
		t.Fatalf("request hashes = %q/%q; want non-empty epoch-specific fingerprints", a, b)
	}
}

func TestReserveProcessRetryByReturnedHandleConvergesWithoutKey(t *testing.T) {
	p, sid := processReservationFixture(t)
	first := &v1.StartRequest{Cmd: "/bin/sh", Args: []string{"-c", "printf ok"}, PlacementEpoch: 7}
	reserved, created, err := p.reserveProcess(context.Background(), sid, 7, "", "", processStartRequestHash(first))
	if err != nil || !created {
		t.Fatalf("initial reservation = %v, created=%v", err, created)
	}
	retry := &v1.StartRequest{Cmd: first.Cmd, Args: append([]string(nil), first.Args...), ProcessHandle: string(reserved.ID), PlacementEpoch: first.PlacementEpoch}
	got, created, err := p.reserveProcess(context.Background(), sid, 7, "", retry.ProcessHandle, processStartRequestHash(retry))
	if err != nil || created || got.ID != reserved.ID {
		t.Fatalf("retry reservation = id=%q created=%v err=%v; want original reservation", got.ID, created, err)
	}
	changed := &v1.StartRequest{
		Cmd:            retry.Cmd,
		Args:           []string{"-c", "printf changed"},
		ProcessHandle:  retry.ProcessHandle,
		PlacementEpoch: retry.PlacementEpoch,
	}
	if _, _, err := p.reserveProcess(context.Background(), sid, 7, "", changed.ProcessHandle, processStartRequestHash(changed)); !errors.Is(err, model.ErrIdempotencyConflict) {
		t.Fatalf("changed command retry = %v; want idempotency conflict", err)
	}
}

func TestReserveProcessKeylessHandleRetryUsesCanonicalRepeatedFields(t *testing.T) {
	p, sid := processReservationFixture(t)
	first := &v1.StartRequest{Cmd: "/bin/true", PlacementEpoch: 7}
	reserved, created, err := p.reserveProcess(context.Background(), sid, 7, "", "", processStartRequestHash(first))
	if err != nil || !created {
		t.Fatalf("initial reservation = %v, created=%v", err, created)
	}
	retry := &v1.StartRequest{Cmd: first.Cmd, Args: []string{}, Env: []string{}, ProcessHandle: string(reserved.ID), PlacementEpoch: 7}
	got, created, err := p.reserveProcess(context.Background(), sid, 7, "", retry.ProcessHandle, processStartRequestHash(retry))
	if err != nil || created || got.ID != reserved.ID {
		t.Fatalf("retry with omitted/empty repeated fields = id=%q created=%v err=%v; want original reservation", got.ID, created, err)
	}
}

func TestNormalizeProcessEventIdentityFillsLegacyFields(t *testing.T) {
	ev := &transport.ProcessEvent{Kind: "data", Sequence: 2}
	handle, epoch, err := normalizeProcessEventIdentity(ev, "proc_a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if handle != "proc_a" || epoch != 7 || ev.Handle != "proc_a" || ev.Epoch != 7 {
		t.Fatalf("normalized legacy event = handle=%q epoch=%d event=%+v", handle, epoch, ev)
	}
}

func TestNormalizeProcessEventIdentityRejectsMismatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   transport.ProcessEvent
		want string
	}{
		{name: "handle", ev: transport.ProcessEvent{Kind: "data", Handle: "proc_b", Epoch: 7}, want: "process stream handle mismatch"},
		{name: "epoch", ev: transport.ProcessEvent{Kind: "data", Handle: "proc_a", Epoch: 8}, want: "process stream placement epoch mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := normalizeProcessEventIdentity(&tc.ev, "proc_a", 7)
			if err == nil || !strings.Contains(err.Error(), tc.want) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("error = %v; want failed_precondition %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeProcessEventIdentityRejectsNil(t *testing.T) {
	_, _, err := normalizeProcessEventIdentity(nil, "", 0)
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("nil event error = %v; want invalid_argument", err)
	}
}
