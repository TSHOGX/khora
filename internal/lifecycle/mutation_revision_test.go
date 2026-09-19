package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

func TestDelayedMutationCannotAffectReplacementPlacement(t *testing.T) {
	for _, verb := range []string{"pause", "destroy", "resume", "rebind"} {
		t.Run(verb, func(t *testing.T) {
			ctx := context.Background()
			m, _, _, _ := mountFixture(t)
			id, err := m.Create(ctx, CreateSpec{Template: "bare"})
			if err != nil {
				t.Fatal(err)
			}
			before, err := m.Inspect(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Pause(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := m.Resume(ctx, id); err != nil {
				t.Fatal(err)
			}
			switch verb {
			case "pause":
				err = m.PauseIfMatch(ctx, id, before.MutationRevision)
			case "destroy":
				err = m.DestroyIfMatch(ctx, id, before.MutationRevision)
			case "resume":
				err = m.ResumeIfMatch(ctx, id, before.MutationRevision)
			case "rebind":
				duration := time.Hour
				_, err = m.Rebind(ctx, id, RebindSpec{ExpectedMutationRevision: before.MutationRevision, Binding: store.Binding{Retention: &duration}})
			}
			if !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("stale %s was not refused: %v", verb, err)
			}
			after, err := m.Inspect(ctx, id)
			if err != nil || after.Sandbox.State != model.Running || after.Placement.Epoch <= before.Placement.Epoch {
				t.Fatalf("delayed request changed replacement: %+v %v", after, err)
			}
		})
	}
}

func TestMutationRevisionRejectsBindingABAAndLostResponseRetry(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := mountFixture(t)
	id, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := m.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	original := before.Sandbox.Retention
	changed := original + time.Hour
	for _, duration := range []time.Duration{changed, original} {
		if _, err := m.Rebind(ctx, id, RebindSpec{Binding: store.Binding{Retention: &duration}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.PauseIfMatch(ctx, id, before.MutationRevision); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("restored binding reused old revision: %v", err)
	}
	current, err := m.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PauseIfMatch(ctx, id, current.MutationRevision); err != nil {
		t.Fatal(err)
	}
	if err := m.PauseIfMatch(ctx, id, current.MutationRevision); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("lost-response retry changed intent: %v", err)
	}
	paused, err := m.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ResumeIfMatch(ctx, id, paused.MutationRevision); err != nil {
		t.Fatal(err)
	}
	if err := m.ResumeIfMatch(ctx, id, paused.MutationRevision); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("old resume replay accepted: %v", err)
	}
}

func TestMutationRevisionIncludesStoreAndSandboxIdentity(t *testing.T) {
	record := model.Sandbox{ID: "sandbox", Revision: 1, State: model.Paused}
	first := mutationRevision("store-a", record, nil)
	if mutationRevision("store-b", record, nil) == first {
		t.Fatal("store rebuild reused mutation revision")
	}
	record.ID = "other"
	if mutationRevision("store-a", record, nil) == first {
		t.Fatal("revision transferable to another sandbox")
	}
}

func TestMutationPreconditionIsCheckedAfterTransitionLock(t *testing.T) {
	for _, verb := range []string{"pause", "destroy"} {
		t.Run(verb, func(t *testing.T) {
			ctx := context.Background()
			m, _, _, _ := mountFixture(t)
			id, err := m.Create(ctx, CreateSpec{Template: "bare"})
			if err != nil {
				t.Fatal(err)
			}
			before, err := m.Inspect(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			lock := m.lock(id)
			lock.Lock()
			result := make(chan error, 1)
			go func() {
				if verb == "pause" {
					result <- m.PauseIfMatch(ctx, id, before.MutationRevision)
				} else {
					result <- m.DestroyIfMatch(ctx, id, before.MutationRevision)
				}
			}()
			if err := m.pauseLocked(ctx, id, model.EventPaused, nil); err != nil {
				lock.Unlock()
				t.Fatal(err)
			}
			paused, err := m.deps.Store.GetSandbox(ctx, id)
			if err != nil {
				lock.Unlock()
				t.Fatal(err)
			}
			if err := m.resumeLocked(ctx, paused); err != nil {
				lock.Unlock()
				t.Fatal(err)
			}
			lock.Unlock()
			select {
			case err := <-result:
				if !errors.Is(err, ErrPreconditionFailed) {
					t.Fatalf("queued stale mutation accepted: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("queued mutation did not finish")
			}
		})
	}
}
