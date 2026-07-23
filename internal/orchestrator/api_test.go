package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
)

// apiFixture assembles an Engine over a Memory store with no real providers —
// the Template GET/DELETE endpoints are pure store operations (§4.3: Template is
// not a state-machine object, so DeleteTemplate takes no placement / lock), so
// the runtime/rootfs seams are never touched by these routes.
func apiFixture(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	mem := store.NewMemory()
	med := lifecycle.New(lifecycle.Deps{Store: mem})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, mem
}

func seedTemplate(t *testing.T, mem *store.Memory, id, parent string, builtin bool) {
	t.Helper()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: model.TemplateID(id), Parent: model.TemplateID(parent),
		Builtin: builtin, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template %q: %v", id, err)
	}
}

func do(t *testing.T, method, url string) *http.Response {
	t.Helper()
	return doBody(t, method, url, "")
}

func doBody(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// GET /v1/templates lists the rows with id/parent/builtin.
func TestAPIListTemplates(t *testing.T) {
	srv, mem := apiFixture(t)
	seedTemplate(t, mem, "bare", "", true)
	seedTemplate(t, mem, "tmpl_child", "bare", false)

	resp := do(t, http.MethodGet, srv.URL+"/v1/templates")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET templates = %d; want 200", resp.StatusCode)
	}
	var got []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d templates; want 2", len(got))
	}
}

// DELETE /v1/templates/{id}: unreferenced → 204; referenced by a sandbox → 409
// (§4.3 被引用即拒删); after the sandbox is gone → 204; missing → 404.
func TestAPIDeleteTemplateReferenceProtection(t *testing.T) {
	srv, mem := apiFixture(t)
	ctx := context.Background()
	seedTemplate(t, mem, "bare", "", true)
	seedTemplate(t, mem, "unused", "", true)

	// unreferenced → 204.
	resp := do(t, http.MethodDelete, srv.URL+"/v1/templates/unused")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete unreferenced = %d; want 204", resp.StatusCode)
	}

	// referenced by a sandbox → 409 template in use.
	if err := mem.CreateSandbox(ctx, model.Sandbox{
		ID: "sbx_a", Template: "bare", Overlay: "ov_a", State: model.Running,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/bare")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete referenced = %d; want 409", resp.StatusCode)
	}

	// remove the referrer → now deletable (204).
	if err := mem.DeleteSandbox(ctx, "sbx_a", nil); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/bare")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after referrer gone = %d; want 204", resp.StatusCode)
	}

	// missing → 404.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/ghost")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing = %d; want 404", resp.StatusCode)
	}
}

// POST /v1/templates {from_sandbox_id} is the Promote endpoint (§6.1). An empty
// body → 400 (from_sandbox_id required); a promote of an unknown sandbox → 404
// (ErrNotFound). The full freeze path is exercised on the substrate (needs a
// real Overlay upper).
func TestAPIPromoteValidation(t *testing.T) {
	srv, _ := apiFixture(t)

	// missing from_sandbox_id → 400.
	resp := do(t, http.MethodPost, srv.URL+"/v1/templates")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST templates (empty) = %d; want 400", resp.StatusCode)
	}

	// unknown sandbox → 404.
	resp = doBody(t, http.MethodPost, srv.URL+"/v1/templates", `{"from_sandbox_id":"ghost"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST templates (unknown sandbox) = %d; want 404", resp.StatusCode)
	}
}
