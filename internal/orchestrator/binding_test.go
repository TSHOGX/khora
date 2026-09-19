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

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/volume"
)

// bindingFixture assembles an Engine with fake runtime/rootfs seams and a REAL
// plain-directory volume backend, so a create can actually reach the cold-start chain
// and carry a binding through it.
func bindingFixture(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now(),
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
// `access` given on the MOUNT rather than on the volume record (the Volume subsystem).
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

func TestSandboxCreateIdempotencyHeaderAtHTTPBoundary(t *testing.T) {
	srv, mem := bindingFixture(t)
	post := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "sandbox-http-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out createResp
		if resp.StatusCode/100 == 2 {
			_ = json.NewDecoder(resp.Body).Decode(&out)
		}
		resp.Body.Close()
		return resp.StatusCode, out.SandboxID
	}
	firstCode, first := post(`{"template_id":"bare"}`)
	secondCode, second := post(`{"template_id":"bare","network":{"egress":{"default":"deny"}}}`)
	if firstCode != http.StatusCreated || secondCode != http.StatusCreated || first != second {
		t.Fatalf("idempotent HTTP create = %d/%s and %d/%s", firstCode, first, secondCode, second)
	}
	if recs, _ := mem.ListSandboxes(context.Background()); len(recs) != 1 {
		t.Fatalf("HTTP retry created %d sandboxes", len(recs))
	}
	code, _ := post(`{"template_id":"bare","memory_byte":1}`)
	if code != http.StatusConflict {
		t.Fatalf("changed HTTP request status = %d, want 409", code)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("= %d, want 400 (a self-contradictory binding is decidable without reading another row, and no retry makes it legal — the API error contract)", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
	recs, _ := mem.ListSandboxes(context.Background())
	if len(recs) != 0 {
		t.Errorf("rejected bindings left %d sandbox record(s) behind", len(recs))
	}

	// Nested destinations are now a supported mount tree. The compiler orders
	// the parent before the child and preserves both bindings.
	nested := `{"template_id":"bare","volumes":[{"volume_id":"` + vid + `","path":"/d","access":"shared-rw"},{"volume_id":"` + vid2 + `","path":"/d/sub","access":"shared-rw"}]}`
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", nested)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("nested binding = %d: %s; want supported mount tree", resp.StatusCode, b)
	}
	resp.Body.Close()
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

// TestDeleteMountedVolumeIs409: the API error contract verdict  through the wire.
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

// TestPatchRebindsAndSplitsTheEffectTimes is the control-plane API detach shape: the response says
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

	rejected := doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID, `{"volumes":[]}`)
	if rejected.StatusCode != http.StatusConflict {
		t.Fatalf("live detach = %d, want 409", rejected.StatusCode)
	}
	rejected.Body.Close()
	paused := do(t, http.MethodPost, srv.URL+"/v1/sandboxes/"+cr.SandboxID+"/pause")
	if paused.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d", paused.StatusCode)
	}
	paused.Body.Close()
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
		t.Errorf("applies_at = %q, want next-resume — runsc takes mounts at startup, so topology changes on resume (the Volume subsystem)", pr.AppliesAt)
	}
	if !pr.LeaseReleased {
		t.Errorf("lease_released = false after detaching an exclusive mount — the lease IS the row, so deleting it releases NOW, which applies_at cannot express (the control-plane API)")
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

// TestPatch400ChangesNothing: a PATCH that answers 400 left the record EXACTLY as it
// was — asserted by reading the record back, not by trusting the status code.
//
// The defect a status-code-only test cannot see: were `handlePatch` to commit the binding and
// only THEN decode and validate the declaration, `{"memory_byte":N, <invalid network>}` would
// answer 400 with the memory cap ALREADY COMMITTED. the API error contract requires validation to precede the
// admission gate so a refusal touches nothing, and the record is the only witness.
//
// Every case pairs a LEGAL first field with an ILLEGAL declaration, which is the whole design:
// with only the illegal field present there is nothing to leak, so the test would pass against
// the defective ordering too.
func TestPatch400ChangesNothing(t *testing.T) {
	srv, mem := bindingFixture(t)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","memory_byte":134217728,"retention":"24h"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed create = %d", resp.StatusCode)
	}
	var cr createResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// PAUSED, so a cap edit is legal on its own terms (resource admission — caps are editable only
	// from `paused`). Without this the caps write would be refused by the STATE gate and
	// the test would pass even with the defect present, for the wrong reason.
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes/"+cr.SandboxID+"/pause", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	before, err := mem.GetSandbox(context.Background(), model.SandboxID(cr.SandboxID))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, body string }{
		// A cap that WOULD be accepted alone, riding with a declaration that is the API error contract's
		// ClassDefaultMissing (`network` given, `egress.default` absent).
		{"cap + declaration missing egress.default",
			`{"memory_byte":536870912,"network":{"egress":{"allow":[]}}}`},
		{"cap + declaration with an empty egress.default",
			`{"memory_byte":536870912,"network":{"egress":{"default":""}}}`},
		{"cap + declaration with an unknown egress.default",
			`{"memory_byte":536870912,"network":{"egress":{"default":"maybe"}}}`},
		// A TTL is legal on either state, so this one isolates the ordering from the
		// state gate entirely.
		{"ttl + invalid declaration",
			`{"retention":"1h","network":{"egress":{"allow":[]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("PATCH = %d, want 400 (the declaration is invalid, the API error contract)", resp.StatusCode)
			}

			after, err := mem.GetSandbox(context.Background(), model.SandboxID(cr.SandboxID))
			if err != nil {
				t.Fatal(err)
			}
			if after.Limits.MemoryByte != before.Limits.MemoryByte {
				t.Errorf("memory_byte = %d after a 400; want %d unchanged — the cap was committed and "+
					"THEN the declaration was refused, so the caller was told the request changed "+
					"nothing while the record disagrees (the API error contract: validation precedes the write)",
					after.Limits.MemoryByte, before.Limits.MemoryByte)
			}
			if after.Retention != before.Retention {
				t.Errorf("retention = %s after a 400; want %s unchanged", after.Retention, before.Retention)
			}
			if after.IdleTimeout != before.IdleTimeout {
				t.Errorf("idle_timeout = %s after a 400; want %s unchanged", after.IdleTimeout, before.IdleTimeout)
			}
			// The declaration itself must be untouched too — it is the field the refused
			// request named.
			if after.Network.Egress.Default != before.Network.Egress.Default {
				t.Errorf("egress.default = %q after a 400; want %q unchanged",
					after.Network.Egress.Default, before.Network.Egress.Default)
			}
			if after.State != before.State {
				t.Errorf("state = %s after a 400; want %s (a refused PATCH is not a transition)",
					after.State, before.State)
			}
		})
	}
}

// TestPatch409ChangesNothing is TestPatch400ChangesNothing's harder twin, over the wire.
//
// The two differ in WHERE the refusal comes from. A 400 is decidable from the request, so
// hoisting the decode above the writes closed it. This one cannot be hoisted: a contended
// exclusive lease is arbitrated by the store AT COMMIT (store.Store.CreateMounts — judgment and
// insert are inseparable, measured), so the only way a `{cap, losing volumes}` PATCH answers 409
// having changed nothing is for the mount commit to be ordered FIRST and to share one transition
// lock with the cap write. Three separately-committing calls could not deliver that.
func TestPatch409ChangesNothing(t *testing.T) {
	srv, mem := bindingFixture(t)
	mine := newVolume(t, srv)
	held := newVolume(t, srv)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","memory_byte":134217728,"volumes":[{"volume_id":"`+mine+`","path":"/data","access":"exclusive"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed create = %d", resp.StatusCode)
	}
	var cr createResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Another sandbox takes `held` exclusively, so the rebind below must lose the lease.
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
		`{"template_id":"bare","volumes":[{"volume_id":"`+held+`","path":"/x","access":"exclusive"}]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("holder create = %d", resp.StatusCode)
	}

	// PAUSED, so the cap edit is legal on its own terms (resource admission) and the request reaches the
	// mount commit rather than being turned away by the state gate.
	if resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes/"+cr.SandboxID+"/pause", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	before, err := mem.GetSandbox(context.Background(), model.SandboxID(cr.SandboxID))
	if err != nil {
		t.Fatal(err)
	}

	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID,
		`{"memory_byte":536870912,"idle_timeout":"7m","volumes":[{"volume_id":"`+held+`","path":"/y","access":"exclusive"}]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("PATCH onto a contended lease = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	after, err := mem.GetSandbox(context.Background(), model.SandboxID(cr.SandboxID))
	if err != nil {
		t.Fatal(err)
	}
	if after.Limits.MemoryByte != before.Limits.MemoryByte {
		t.Errorf("memory_byte = %d after a 409; want %d unchanged — the cap committed and the mount "+
			"binding was THEN refused by the store, so the caller was told the request failed while "+
			"the record disagrees (per-sandbox transition serialization: one PATCH is one lock and one decision)",
			after.Limits.MemoryByte, before.Limits.MemoryByte)
	}
	if after.IdleTimeout != before.IdleTimeout {
		t.Errorf("idle_timeout = %s after a 409; want %s unchanged", after.IdleTimeout, before.IdleTimeout)
	}
	// The sandbox still holds the binding it had — the refusal did not detach it.
	rows, _ := mem.ListMounts(context.Background(), model.SandboxID(cr.SandboxID))
	if len(rows) != 1 || rows[0].Volume != model.VolumeID(mine) || rows[0].Path != "/data" {
		t.Errorf("mount rows = %+v after a 409; want the original exclusive /data mount", rows)
	}
}

// TestBothTTLWritePointsAgree: create and PATCH give the SAME verdict for the SAME
// value, on both TTL axes.
//
// Not implied by the two tests either side of it. The bug was never "nobody checks" — PATCH
// checked and create did not — so a per-endpoint test passes on each endpoint's own terms while
// the two disagree about one field. Asserting the AGREEMENT is what makes the shared `parseTTL`
// structural rather than a coincidence.
func TestBothTTLWritePointsAgree(t *testing.T) {
	srv, mem := bindingFixture(t)

	// A sandbox to PATCH, created with a legal TTL so the read-back below has a known
	// prior value to still be holding after each refusal.
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", `{"template_id":"bare","retention":"24h"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed create = %d", resp.StatusCode)
	}
	var cr createResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, tc := range []struct{ field, value string }{
		{"retention", "-5m"},
		{"idle_timeout", "-1h"},
		{"retention", "-1ns"}, // the smallest negative: the check is on the SIGN, not a magnitude
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			cResp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
				`{"template_id":"bare","`+tc.field+`":"`+tc.value+`"}`)
			cResp.Body.Close()
			pResp := doBody(t, http.MethodPatch, srv.URL+"/v1/sandboxes/"+cr.SandboxID,
				`{"`+tc.field+`":"`+tc.value+`"}`)
			pResp.Body.Close()

			if cResp.StatusCode != pResp.StatusCode {
				t.Errorf("create = %d but PATCH = %d for %s=%q — the same field must not be legal at "+
					"one write point and a 400 at the other (Binding effective points: create and PATCH are the two "+
					"write points for the same Binding)", cResp.StatusCode, pResp.StatusCode, tc.field, tc.value)
			}
			if cResp.StatusCode != http.StatusBadRequest {
				t.Errorf("both write points answered %d for %s=%q; want 400", cResp.StatusCode, tc.field, tc.value)
			}
		})
	}

	// The refused PATCHes changed nothing: the seeded 24h is intact.
	rec, err := mem.GetSandbox(context.Background(), model.SandboxID(cr.SandboxID))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Retention != 24*time.Hour {
		t.Errorf("retention = %s after refused PATCHes; want 24h unchanged", rec.Retention)
	}
}

// TestCreateRefusesNegativeTTL: a negative TTL is a 400 on CREATE, and nothing lands.
//
// Asserted rather than left to the parse because `time.ParseDuration` ACCEPTS "-5m", so the sign
// check is the only thing between the caller and a stored negative — and a stored negative is not
// a smaller deadline: every consumer reads a non-positive TTL as an OPT-OUT (reaper:
// `Retention <= 0` returns early; classification: `IdleTimeout > 0`), so it silently means NEVER
// EVICT / NEVER IDLE-PAUSE with no error anywhere.
//
// `0s` is asserted LEGAL in the same test, being the documented opt-out spelling (the control-plane API) — a guard
// that also refused zero would break the contract while looking like the same fix.
func TestCreateRefusesNegativeTTL(t *testing.T) {
	srv, mem := bindingFixture(t)

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"negative retention", `{"template_id":"bare","retention":"-5m"}`, http.StatusBadRequest},
		{"negative idle_timeout", `{"template_id":"bare","idle_timeout":"-1h"}`, http.StatusBadRequest},
		{"both negative", `{"template_id":"bare","retention":"-5m","idle_timeout":"-1h"}`, http.StatusBadRequest},
		{"zero is the documented opt-out, still legal", `{"template_id":"bare","retention":"0s"}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes", tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("create = %d, want %d — a negative TTL reads as never-evict / "+
					"never-idle-pause at every consumer, so accepting it hands the caller a "+
					"sandbox with no deadline while they believe they set one",
					resp.StatusCode, tc.want)
			}
		})
	}

	// No refused create left a record, and no ACCEPTED one holds a negative.
	recs, err := mem.ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Retention < 0 || rec.IdleTimeout < 0 {
			t.Errorf("sandbox %s stored a negative TTL (retention=%s idle=%s) — the reaper and the "+
				"idle classifier both treat that as an opt-out, so it is unobservable after the fact",
				rec.ID, rec.Retention, rec.IdleTimeout)
		}
	}
}
