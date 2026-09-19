package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/TSHOGX/khora/pkg/khora"
)

func TestPublicConditionalLifecycleRejectsStaleRequests(t *testing.T) {
	ctx := context.Background()
	server, _ := bindingFixture(t)
	client, err := khora.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.Create(ctx, khora.CreateRequest{TemplateID: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Get(ctx, created.SandboxID)
	if err != nil || first.MutationRevision == "" {
		t.Fatalf("missing mutation revision: %+v %v", first, err)
	}
	if _, err := client.PauseIfMatch(ctx, created.SandboxID, first.MutationRevision); err != nil {
		t.Fatal(err)
	}
	paused, err := client.Get(ctx, created.SandboxID)
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("delayed binding update: %v", err)
	}
	current, err := client.Get(ctx, created.SandboxID)
	if err != nil || current.State != khora.SandboxState("running") {
		t.Fatalf("replacement changed: %+v %v", current, err)
	}
	if err := client.DestroyIfMatch(ctx, created.SandboxID, current.MutationRevision); err != nil {
		t.Fatal(err)
	}
	if err := client.DestroyIfMatch(ctx, created.SandboxID, current.MutationRevision); !khora.IsNotFound(err) {
		t.Fatalf("destroy retry should report absent object: %v", err)
	}
}

func TestConditionalHeaderRefusesWildcardsAndMultipleValidators(t *testing.T) {
	server, _ := bindingFixture(t)
	for _, header := range []string{"*", "unquoted", `W/"weak"`, `"one", "two"`} {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sandboxes/absent/pause", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("If-Match", header)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid precondition %q: %d", header, response.StatusCode)
		}
	}
}
