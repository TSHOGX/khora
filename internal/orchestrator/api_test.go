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
// (§4.3 referenced-means-undeletable); after the sandbox is gone → 204; missing
// → 404.
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

// §6.1 PATCH /v1/sandboxes/{id} — the Binding write point. These cover the WIRE
// contract (routing, body shape, status codes, applies_at); the semantics live in
// the mediator's own tests.
func TestPatchSandboxBinding(t *testing.T) {
	srv, mem := apiFixture(t)
	seedTemplate(t, mem, "bare", "", true)
	now := time.Now()
	if err := mem.CreateSandbox(context.Background(), model.Sandbox{
		ID: "sbx_p", Template: "bare", Overlay: "ov_p", State: model.Paused,
		Limits:    model.Limits{MemoryByte: 128 << 20, DiskByte: 64 << 20},
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}

	patch := func(t *testing.T, id, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/sandboxes/"+id, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	t.Run("a cap edit reports next-resume", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"memory_byte":536870912}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		var out patchResp
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		// Caps materialize at placement, so the API states the deferral rather than
		// leaving the caller to infer it (§3.4).
		if out.AppliesAt != "next-resume" {
			t.Fatalf("applies_at = %q; want next-resume", out.AppliesAt)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Limits.MemoryByte != 536870912 {
			t.Fatalf("memory not persisted: %d", rec.Limits.MemoryByte)
		}
		// omitted field untouched
		if rec.Limits.DiskByte != 64<<20 {
			t.Fatalf("disk changed to %d; want untouched", rec.Limits.DiskByte)
		}
	})

	t.Run("a TTL edit reports now and accepts duration strings", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"retention":"48h"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		var out patchResp
		_ = json.Unmarshal([]byte(body), &out)
		if out.AppliesAt != "now" {
			t.Fatalf("applies_at = %q; want now (the reaper re-reads the record)", out.AppliesAt)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Retention != 48*time.Hour {
			t.Fatalf("retention = %s; want 48h", rec.Retention)
		}
	})

	t.Run("a zero cap is accepted as unlimited, not treated as omitted", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"disk_byte":0}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Limits.DiskByte != 0 {
			t.Fatalf("disk = %d; want 0 — an explicit zero must be applied, not read as omitted", rec.Limits.DiskByte)
		}
	})

	t.Run("negative and malformed inputs are 400", func(t *testing.T) {
		for _, body := range []string{`{"memory_byte":-1}`, `{"retention":"not-a-duration"}`, `{"retention":"-5m"}`} {
			resp, got := patch(t, "sbx_p", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("body %s → status %d (%s); want 400", body, resp.StatusCode, got)
			}
		}
	})

	t.Run("unknown sandbox is 404", func(t *testing.T) {
		resp, body := patch(t, "sbx_nope", `{"memory_byte":1}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d (%s); want 404", resp.StatusCode, body)
		}
	})
}

// The two admission verdicts must NOT collapse into one status: a missing cap on a
// metered axis is the caller's request being invalid for this deployment (400),
// while a full node is a capacity condition (507). Reporting "full" for a missing
// cap would send the caller off to wait for capacity that is not the problem.
func TestAdmissionVerdictsMapToDistinctStatuses(t *testing.T) {
	t.Run("missing cap on a metered axis → 400", func(t *testing.T) {
		mem := store.NewMemory()
		med := lifecycle.New(lifecycle.Deps{
			Store:        mem,
			NodeCapacity: lifecycle.NodeCapacity{MemoryByte: 4 << 30},
		})
		eng := &Engine{Mediator: med, Store: mem}
		srv := httptest.NewServer(eng.HTTPHandler())
		t.Cleanup(srv.Close)
		seedTemplate(t, mem, "bare", "", true)

		resp, err := http.Post(srv.URL+"/v1/sandboxes", "application/json",
			strings.NewReader(`{"template_id":"bare"}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d (%s); want 400 — nothing is full, the request is invalid here", resp.StatusCode, b)
		}
		if resp.StatusCode == http.StatusInsufficientStorage {
			t.Fatal("507 would claim the node is full, which it is not")
		}
	})
}
