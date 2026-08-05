package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
	"engine/internal/volume"
)

// bindingFixture assembles an Engine with fake runtime/rootfs seams and a REAL
// plain-directory volume backend, so a create can actually reach the cold-start chain
// and carry a binding through it.
func bindingFixture(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	vb, err := volume.NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:         mem,
		Runtime:       barrierFakeRuntime{},
		Rootfs:        barrierFakeRootfs{},
		Volume:        vb,
		Dial:          func(transport.Endpoint) transport.Client { return &barrierFakeClient{} },
		HealthTimeout: 2 * time.Second,
		Logf:          func(string, ...any) {},
	})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, mem
}

func newVolume(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create volume = %d", resp.StatusCode)
	}
	return decodeVolume(t, resp).VolumeID
}

// TestCreateWithVolumeBinding: `volumes` on create reaches the mount table, with
// `access` given on the MOUNT rather than on the volume record (§5.C).
func TestCreateWithVolumeBinding(t *testing.T) {
	srv, mem := bindingFixture(t)
	vid := newVolume(t, srv)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+vid+`","path":"/data","access":"shared-rw"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var cr createResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, err := mem.ListMounts(context.Background(), model.SandboxID(cr.SandboxID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "/data" || rows[0].Access != model.SharedReadWrite {
		t.Fatalf("mount rows = %+v; want one shared-rw row at /data", rows)
	}
}

// TestCreateBindingValidationIs400: a malformed or self-contradictory binding is 400,
// and the refusal leaves no sandbox behind.
func TestCreateBindingValidationIs400(t *testing.T) {
	srv, mem := bindingFixture(t)
	vid := newVolume(t, srv)
	vid2 := newVolume(t, srv)

	for _, tc := range []struct{ name, body string }{
		{"missing access", `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/data"}]}`},
		{"unknown access", `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/data","access":"rw"}]}`},
		{"missing path", `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","access":"shared-rw"}]}`},
		{"missing volume_id", `{"template_id":"bare","volumes":[{"path":"/data","access":"shared-rw"}]}`},
		{"duplicate destination", `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/d","access":"shared-rw"},{"volume_id":"` + vid2 + `","path":"/d","access":"shared-rw"}]}`},
		{"nested destinations", `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/d","access":"shared-rw"},{"volume_id":"` + vid2 + `","path":"/d/sub","access":"shared-rw"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("= %d, want 400 (a self-contradictory binding is decidable without reading another row, and no retry makes it legal — §6.2)", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
	recs, _ := mem.ListSandboxes(context.Background())
	if len(recs) != 0 {
		t.Errorf("rejected bindings left %d sandbox record(s) behind", len(recs))
	}
}

// TestAttachContendedLeaseIs409: attaching a volume already held exclusively is 409 —
// a RETRYABLE state conflict, distinct from the 400s above.
func TestAttachContendedLeaseIs409(t *testing.T) {
	srv, _ := bindingFixture(t)
	vid := newVolume(t, srv)
	body := `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/data","access":"exclusive"}]}`

	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d", resp.StatusCode)
	}
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", body)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second exclusive attach = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestDeleteMountedVolumeIs409: §6.2 verdict ① through the wire.
func TestDeleteMountedVolumeIs409(t *testing.T) {
	srv, _ := bindingFixture(t)
	vid := newVolume(t, srv)
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+vid+`","path":"/data","access":"shared-rw"}]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp := do(t, http.MethodDelete, srv.URL+"/v1/volumes/"+vid)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete a mounted volume = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPatchRebindsAndSplitsTheEffectTimes is the §6.1 detach shape: the response says
// next-resume for the TOPOLOGY and lease_released for the LEASE, because the two do
// not happen at the same time and one value cannot say both.
func TestPatchRebindsAndSplitsTheEffectTimes(t *testing.T) {
	srv, mem := bindingFixture(t)
	vid := newVolume(t, srv)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+vid+`","path":"/data","access":"exclusive"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var cr createResp
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	// Detach everything: an EMPTY array is a legitimate request, distinguishable from
	// omitting the field.
	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID, `{"volumes":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch detach = %d, want 200", resp.StatusCode)
	}
	var pr patchResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pr.AppliesAt != "next-resume" {
		t.Errorf("applies_at = %q, want next-resume — runsc takes mounts at startup, so topology changes on resume (§5.C)", pr.AppliesAt)
	}
	if !pr.LeaseReleased {
		t.Errorf("lease_released = false after detaching an exclusive mount — the lease IS the row, so deleting it releases NOW, which applies_at cannot express (§6.1)")
	}
	if rows, _ := mem.ListMounts(context.Background(), model.SandboxID(cr.SandboxID)); len(rows) != 0 {
		t.Errorf("rows survived the detach: %+v", rows)
	}
	// And the released lease is immediately usable by another sandbox, even though the
	// detaching sandbox has not resumed.
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+vid+`","path":"/data","access":"exclusive"}]}`); resp.StatusCode != http.StatusCreated {
		t.Errorf("re-attaching the released volume = %d, want 201 — the lease released at detach, not at the next resume", resp.StatusCode)
	}

	// Omitting `volumes` leaves the binding alone, and reports no release.
	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID, `{"idle_timeout":"10m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch without volumes = %d", resp.StatusCode)
	}
	// A FRESH value: decoding into the previous one would leave lease_released true from
	// the earlier response, since `omitempty` means the field is simply absent here.
	var pr2 patchResp
	_ = json.NewDecoder(resp.Body).Decode(&pr2)
	resp.Body.Close()
	if pr2.LeaseReleased {
		t.Errorf("lease_released = true for a PATCH that did not touch volumes")
	}
}

// TestPatchRefusedRebindRestoresThePreviousBinding: a rejected rebind must leave the
// sandbox with the binding it had — silently detaching everything because a NEW
// binding was invalid would be far worse than the refusal.
func TestPatchRefusedRebindRestoresThePreviousBinding(t *testing.T) {
	srv, mem := bindingFixture(t)
	vid := newVolume(t, srv)
	other := newVolume(t, srv)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+vid+`","path":"/data","access":"shared-rw"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var cr createResp
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	// A second sandbox takes `other` exclusively, so the rebind below loses the lease.
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+other+`","path":"/x","access":"exclusive"}]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second create = %d", resp.StatusCode)
	}

	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID,
		`{"volumes":[{"volume_id":"`+other+`","path":"/y","access":"exclusive"}]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rebind onto a contended lease = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	rows, _ := mem.ListMounts(context.Background(), model.SandboxID(cr.SandboxID))
	if len(rows) != 1 || rows[0].Volume != model.VolumeID(vid) || rows[0].Path != "/data" {
		t.Errorf("after a REFUSED rebind the binding is %+v; want the original /data mount restored — a refusal must not silently detach everything", rows)
	}
}
