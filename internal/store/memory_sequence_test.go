package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

func seedSequenceSandbox(t *testing.T, m *Memory, id model.SandboxID) {
	t.Helper()
	ctx := context.Background()
	if err := m.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSandbox(ctx, model.Sandbox{ID: id, Template: "bare", Overlay: model.OverlayID("ov_" + string(id)), State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
}

func sequenceEvent(id model.SandboxID, kind model.EventKind, state model.Lifecycle) *Event {
	return &Event{ID: "evt_sequence", Sandbox: id, Kind: kind, State: state, OccurredAt: time.Now()}
}

func TestMemoryEventSequenceExhaustionLeavesCreateAtomic(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	seedSequenceSandbox(t, m, "seed")
	m.mu.Lock()
	m.nextSeq = maxEventSequence
	m.mu.Unlock()

	id := model.SandboxID("new")
	err := m.CreateSandbox(ctx, model.Sandbox{ID: id, Template: "bare", Overlay: "ov_new", State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now()}, sequenceEvent(id, model.EventCreated, model.Running))
	if !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatalf("CreateSandbox at max sequence = %v; want ErrEventSequenceExhausted", err)
	}
	if _, err := m.GetSandbox(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sandbox row after exhausted create = %v; want no row", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nextSeq != maxEventSequence || len(m.outbox) != 0 {
		t.Fatalf("outbox after exhausted create: nextSeq=%d rows=%d; want unchanged max/0", m.nextSeq, len(m.outbox))
	}
}

func TestMemoryEventSequenceExhaustionLeavesTransitionAtomic(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	seedSequenceSandbox(t, m, "sbx")
	m.mu.Lock()
	m.nextSeq = maxEventSequence
	m.mu.Unlock()

	err := m.CompareAndSetState(ctx, "sbx", model.Creating, model.Running, sequenceEvent("sbx", model.EventCreated, model.Running), nil)
	if !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatalf("CompareAndSetState at max sequence = %v; want ErrEventSequenceExhausted", err)
	}
	rec, err := m.GetSandbox(ctx, "sbx")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Creating {
		t.Fatalf("state after exhausted transition = %s; want creating", rec.State)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nextSeq != maxEventSequence || len(m.outbox) != 0 {
		t.Fatalf("outbox after exhausted transition: nextSeq=%d rows=%d; want unchanged max/0", m.nextSeq, len(m.outbox))
	}
}

func TestMemoryEventSequenceExhaustionLeavesDeleteAtomic(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	seedSequenceSandbox(t, m, "sbx")
	m.mu.Lock()
	m.nextSeq = maxEventSequence
	m.mu.Unlock()

	err := m.DeleteSandbox(ctx, "sbx", sequenceEvent("sbx", model.EventDestroyed, model.Destroyed))
	if !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatalf("DeleteSandbox at max sequence = %v; want ErrEventSequenceExhausted", err)
	}
	if rec, err := m.GetSandbox(ctx, "sbx"); err != nil || rec.State != model.Creating {
		t.Fatalf("sandbox after exhausted delete = %+v, %v; want original row", rec, err)
	}
}

func TestMemoryEventSequenceExhaustionDoesNotBlockUnpublishedMutation(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	seedSequenceSandbox(t, m, "sbx")
	m.mu.Lock()
	m.nextSeq = maxEventSequence
	m.mu.Unlock()
	if err := m.CompareAndSetState(ctx, "sbx", model.Creating, model.Running, nil, nil); err != nil {
		t.Fatalf("state-only transition at max sequence = %v; want success", err)
	}
}
