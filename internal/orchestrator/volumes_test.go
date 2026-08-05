package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"engine/internal/lifecycle"
	"engine/internal/store"
	"engine/internal/volume"
)

// volumeFixture assembles an Engine with a REAL volume backend — the test-only
// plain-directory arm over a temp dir.
//
// Real rather than faked deliberately: the endpoints under test move BYTES, and the
// property that matters most about the path they move them through (root
// confinement) is a property of the actual accessor. A fake would only prove the
// fake agrees with itself. The plain-directory arm shares its accessor with the
// production arm, so this exercises the real mechanism without needing root or a
// substrate.
//
// The arm reports NO quota capability, which also makes it the right fixture for the
// capability-refusal case below.
func volumeFixture(t *testing.T) (*httptest.Server, *store.Memory, *volume.PlainDir) {
	t.Helper()
	mem := store.NewMemory()
	vb, err := volume.NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatalf("new plain_dir backend: %v", err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:  mem,
		Volume: vb,
		Logf:   func(string, ...any) {},
	})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, mem, vb
}

func decodeVolume(t *testing.T, resp *http.Response) volumeResp {
	t.Helper()
	defer resp.Body.Close()
	var v volumeResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode volume response: %v", err)
	}
	return v
}

// TestVolumeCRUDEndpoints walks the §6.1 Volume block: create → list → inspect →
// patch → delete, plus the not-found shapes.
func TestVolumeCRUDEndpoints(t *testing.T) {
	srv, _, _ := volumeFixture(t)

	// create (uncapped — this arm cannot enforce a cap, and the node is unmetered so
	// no cap is required)
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	created := decodeVolume(t, resp)
	if !strings.HasPrefix(created.VolumeID, "vol_") {
		t.Errorf("volume_id %q does not look engine-minted", created.VolumeID)
	}

	// list
	resp = do(t, http.MethodGet, srv.URL+"/v1/volumes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200", resp.StatusCode)
	}
	var list []volumeResp
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0].VolumeID != created.VolumeID {
		t.Fatalf("list = %+v, want the one created volume", list)
	}
	// NOTHING in the response names a host path (§6.2): volumes are ID-addressed, and
	// the backend's storage location must not reach the wire.
	raw, _ := json.Marshal(list)
	for _, leak := range []string{"/tmp", "/var", "SourcePath", "source_path"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("list response leaks a host path (%q): %s", leak, raw)
		}
	}

	// inspect
	resp = do(t, http.MethodGet, srv.URL+"/v1/volumes/"+created.VolumeID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inspect = %d, want 200", resp.StatusCode)
	}
	got := decodeVolume(t, resp)
	if got.VolumeID != created.VolumeID {
		t.Errorf("inspect returned %q, want %q", got.VolumeID, created.VolumeID)
	}
	if got.Backend != "plain_dir" {
		t.Errorf("inspect backend = %q, want plain_dir — the record must name which backend holds the bytes", got.Backend)
	}

	// patch to 0 (uncapped) is accepted even on this arm: removing a cap is not an
	// unsupported operation, it is the only state this backend can be in.
	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/volumes/"+created.VolumeID, `{"size":0}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch size=0 = %d, want 200: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// PATCH with no size at all is a 400: size is the only editable field, so an empty
	// body asks for nothing and should say so rather than silently no-op.
	resp = doBody(t, http.MethodPatch, srv.URL+"/v1/volumes/"+created.VolumeID, `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("patch with no size = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// delete
	resp = do(t, http.MethodDelete, srv.URL+"/v1/volumes/"+created.VolumeID)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// A SECOND delete is 404, NOT 204 (§6.2): idempotent-by-swallowing would make a
	// delete the store REFUSED indistinguishable from one that succeeded, and the
	// reference-protection refusals land on this exact path. An external
	// implementation returns `200 {"deleted": true}` here.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/volumes/"+created.VolumeID)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete of an already-deleted volume = %d, want 404 (§6.2: not folded into success)", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown ID on the read paths.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/volumes/vol_nope"},
		{http.MethodPatch, "/v1/volumes/vol_nope"},
		{http.MethodDelete, "/v1/volumes/vol_nope"},
	} {
		resp := doBody(t, tc.method, srv.URL+tc.path, `{"size":1024}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestVolumeCapRefusalsAre400 pins the two §6.2 400 verdicts, and that neither is
// reported as something the caller could retry.
func TestVolumeCapRefusalsAre400(t *testing.T) {
	t.Run("a backend that cannot enforce a cap refuses with 400", func(t *testing.T) {
		srv, _, _ := volumeFixture(t) // plain_dir: no quota capability
		resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{"size":1048576}`)
		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("capped create on a quota-less backend = %d, want 400: %s", resp.StatusCode, body)
		}
		resp.Body.Close()
		// And it is NOT 507: nothing is full.
		if resp.StatusCode == http.StatusInsufficientStorage {
			t.Error("the refusal conflates 'this backend cannot' with 'the node is full'")
		}
	})

	t.Run("a metered disk axis makes size required (400, not 507)", func(t *testing.T) {
		mem := store.NewMemory()
		vb, err := volume.NewPlainDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		med := lifecycle.New(lifecycle.Deps{
			Store:        mem,
			Volume:       vb,
			NodeCapacity: lifecycle.NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		eng := &Engine{Mediator: med, Store: mem}
		srv := httptest.NewServer(eng.HTTPHandler())
		t.Cleanup(srv.Close)

		resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("uncapped create on a metered node = %d, want 400 (§6.2 conditionally required per axis): %s", resp.StatusCode, body)
		}
		resp.Body.Close()
	})

	t.Run("negative size is 400", func(t *testing.T) {
		srv, _, _ := volumeFixture(t)
		resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{"size":-1}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("negative size = %d, want 400", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

// TestVolumeImportExportRoundTrip is the endpoint half of M7 acceptance 21: bytes in
// and byte-for-byte back out, on a server that has NEVER had a sandbox.
//
// That is what makes "the volume's lifecycle is decoupled from any sandbox" true in
// substance rather than only in the record layer (§5.C): seeding from scratch and
// reading after the last sandbox is gone have no other route.
func TestVolumeImportExportRoundTrip(t *testing.T) {
	srv, mem, _ := volumeFixture(t)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	vid := decodeVolume(t, resp).VolumeID

	// No sandbox has ever existed on this server.
	if recs, _ := mem.ListSandboxes(nil); len(recs) != 0 {
		t.Fatalf("fixture unexpectedly has %d sandbox(es)", len(recs))
	}

	payload := bytes.Repeat([]byte("payload-"), 64<<10) // 512 KiB, several stream chunks
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/volumes/"+vid+"/import?path=data/blob", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("import = %d, want 200: %s", resp.StatusCode, body)
	}
	var imp importResp
	if err := json.NewDecoder(resp.Body).Decode(&imp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	resp.Body.Close()
	if imp.BytesWritten != int64(len(payload)) {
		t.Errorf("import wrote %d of %d bytes", imp.BytesWritten, len(payload))
	}

	resp = do(t, http.MethodGet, srv.URL+"/v1/volumes/"+vid+"/export?path=data/blob")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("export = %d, want 200: %s", resp.StatusCode, body)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("export returned %d bytes; want the %d imported byte-for-byte", len(got), len(payload))
	}

	// A missing path is 400 rather than an empty read: the path is required, and
	// defaulting it to the volume root would silently export a directory.
	for _, url := range []string{
		srv.URL + "/v1/volumes/" + vid + "/import",
		srv.URL + "/v1/volumes/" + vid + "/export",
	} {
		method := http.MethodGet
		if strings.HasSuffix(url, "/import") {
			method = http.MethodPost
		}
		resp := doBody(t, method, url, "x")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with no path = %d, want 400", url, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Exporting a directory is refused rather than streaming something undefined.
	resp = do(t, http.MethodGet, srv.URL+"/v1/volumes/"+vid+"/export?path=data")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("export of a directory = 200; want a refusal")
	}
	resp.Body.Close()

	// An unknown volume is 404 on both content endpoints.
	for _, url := range []string{
		srv.URL + "/v1/volumes/vol_nope/export?path=f",
		srv.URL + "/v1/volumes/vol_nope/import?path=f",
	} {
		method := http.MethodGet
		if strings.Contains(url, "/import") {
			method = http.MethodPost
		}
		resp := doBody(t, method, url, "x")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", url, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestVolumeImportIsRootConfinedThroughTheEndpoint is M7 acceptance 22 reached
// through the WIRE rather than only at the accessor: a traversal-shaped `path`
// lands inside the volume and the host file is untouched.
//
// It is asserted at this layer as well because confinement here is delegated rather
// than re-implemented — the endpoint passes the caller's string straight to the
// root-confined accessor and does NOT validate it. That is the stronger design (a
// validator has to be right; the accessor cannot be wrong), but it is only true as
// long as nothing between the two re-resolves the path, which is what this pins.
func TestVolumeImportIsRootConfinedThroughTheEndpoint(t *testing.T) {
	srv, _, vb := volumeFixture(t)

	resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	vid := decodeVolume(t, resp).VolumeID

	// A canary the traversal aims at, outside every volume.
	canaryDir := t.TempDir()
	canary := canaryDir + "/passwd"
	const original = "host-owned bytes"
	if err := writeFile(canary, original); err != nil {
		t.Fatalf("seed canary: %v", err)
	}

	// Mirror the canary's parent chain inside the volume so the clamped write can
	// actually land — otherwise this would only prove "the write failed", which a
	// uniformly broken endpoint also satisfies.
	root, err := vb.OpenRoot(nil, vid)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	var chain string
	for _, seg := range strings.Split(strings.Trim(canaryDir, "/"), "/") {
		if chain == "" {
			chain = seg
		} else {
			chain = chain + "/" + seg
		}
		if err := root.Mkdir(chain, 0o755); err != nil {
			t.Fatalf("mirror chain %s: %v", chain, err)
		}
	}
	root.Close()

	// An ABSOLUTE traversal-shaped path, straight through the endpoint.
	escaping := "../../../../../../.." + canary
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/volumes/%s/import?path=%s", srv.URL, vid, escaping),
		strings.NewReader("owned"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("escaping import → %d %s", resp.StatusCode, strings.TrimSpace(string(body)))

	// The property that matters: the host file did NOT change. Failure of the accessor
	// is SILENT — the write would simply succeed against the host file — so this is
	// asserted rather than inferred from a status code.
	got, err := readFile(canary)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if got != original {
		t.Fatalf("CONFINEMENT BREACH: the host file outside the volume changed to %q", got)
	}

	// And the bytes landed INSIDE, at the clamped path.
	resp = do(t, http.MethodGet,
		fmt.Sprintf("%s/v1/volumes/%s/export?path=%s", srv.URL, vid, strings.TrimPrefix(canary, "/")))
	if resp.StatusCode == http.StatusOK {
		inside, _ := io.ReadAll(resp.Body)
		if string(inside) != "owned" {
			t.Errorf("clamped write landed with content %q, want %q", inside, "owned")
		}
	} else {
		t.Logf("clamped path not readable back (%d) — the write may have been refused, which is also confined", resp.StatusCode)
	}
	resp.Body.Close()
}

// writeFile / readFile are tiny os wrappers kept local to this file so the canary
// checks read as "host truth", visibly separate from every path that goes through
// the volume accessor.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
