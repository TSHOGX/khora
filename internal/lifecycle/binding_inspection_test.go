package lifecycle

import (
	"context"
	"github.com/TSHOGX/khora/internal/store"
	"testing"
	"time"
)

func TestBindingInspectionWaitsForTransitionAndReadsMatchingValidator(t *testing.T) {
	ctx := context.Background()
	mediator, records, _, _ := mountFixture(t)
	id, err := mediator.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := mediator.InspectBinding(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	lock := mediator.lock(id)
	lock.Lock()
	result := make(chan BindingInfo, 1)
	failure := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		close(entered)
		info, err := mediator.InspectBinding(ctx, id)
		if err != nil {
			failure <- err
			return
		}
		result <- info
	}()
	<-entered
	select {
	case <-result:
		lock.Unlock()
		t.Fatal("configuration read crossed active transition")
	case err := <-failure:
		lock.Unlock()
		t.Fatal(err)
	case <-time.After(20 * time.Millisecond):
	}
	duration := before.Sandbox.Retention + time.Hour
	if err := records.UpdateBinding(ctx, id, before.Sandbox.State, store.Binding{Retention: &duration}, nil); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()
	select {
	case after := <-result:
		if after.Sandbox.Retention != duration || after.MutationRevision == before.MutationRevision {
			t.Fatalf("configuration and validator did not advance together: %+v", after)
		}
		identity, err := records.Identity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.MutationRevision != mutationRevision(identity, after.Sandbox, after.Placement) {
			t.Fatal("binding validator does not cover returned record")
		}
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("configuration read remained blocked after transition")
	}
}
