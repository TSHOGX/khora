//go:build substrate

package test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TSHOGX/khora/pkg/khora"
)

func TestConditionalMutationCannotReachReplacementRealRunsc(t *testing.T) {
	ctx := context.Background()
	engine, _ := setupEngine(t, 20*time.Second)
	server := httptest.NewServer(engine.HTTPHandler())
	defer server.Close()
	client, err := khora.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.Create(ctx, khora.CreateRequest{TemplateID: "bare", IdleTimeout: "0s", Retention: "0s"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Destroy(ctx, created.SandboxID) })
	first, err := client.Get(ctx, created.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, first.MutationRevision); err != nil {
		t.Fatal(err)
	}
	paused, err := client.Get(ctx, created.SandboxID)
	if err != nil || paused.PlacementState != "lost" {
		t.Fatalf("unverified pause: %+v %v", paused, err)
	}
	if _, err := client.ResumeIfMatch(ctx, created.SandboxID, paused.MutationRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, first.MutationRevision); !khora.IsPreconditionFailed(err) {
		t.Fatalf("delayed pause: %v", err)
	}
	if err := client.DestroyIfMatch(ctx, created.SandboxID, first.MutationRevision); !khora.IsPreconditionFailed(err) {
		t.Fatalf("delayed destroy: %v", err)
	}
	retention := "1h"
	if _, err := client.PatchIfMatch(ctx, created.SandboxID, first.MutationRevision, khora.PatchRequest{Retention: &retention}); !khora.IsPreconditionFailed(err) {
		t.Fatalf("delayed patch: %v", err)
	}
	current, err := client.Get(ctx, created.SandboxID)
	if err != nil || current.State != "running" || current.PlacementEpoch <= first.PlacementEpoch {
		t.Fatalf("replacement disturbed: %+v %v", current, err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, current.MutationRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, current.MutationRevision); !khora.IsPreconditionFailed(err) {
		t.Fatalf("lost-response retry reused a new revision: %v", err)
	}
}
