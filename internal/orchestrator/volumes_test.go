package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
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

// --- §6.2: no host path reaches an ERROR body ---------------------------------

// pathLeakingBackend wraps a real backend and fails the three write entry points with
// an error carrying the storage unit's ABSOLUTE HOST PATH.
//
// The message is copied from internal/btrfs verbatim in SHAPE — `btrfs: <verb> <path>:
// <cause>` — because that is what the production arm actually produces: every substrate
// call there is wrapped that way, and the path is filepath.Join(VolumesDir, volumeID)
// (internal/volume/local_btrfs.go). So this fake is not inventing a hypothetical leak;
// it reproduces the one the real backend hands the control plane on any ENOSPC, EPERM
// or missing-subvolume failure.
//
// Faked rather than driven on the real btrfs arm because THE ARRIVING ERROR is the
// whole input under test, and making the real substrate fail on demand needs root and
// a substrate tag. What must be real is the ENGINE side of the boundary — the mediator,
// the router and the funnel — and all three are.
type pathLeakingBackend struct {
	volume.Backend
	unit string // the absolute host path the errors interpolate
}

func (b *pathLeakingBackend) CreateVolume(_ context.Context, volumeID string, _ int64) error {
	return fmt.Errorf("btrfs: create subvolume %s: %w", b.unit+"/"+volumeID, errInjectedSubstrate)
}

func (b *pathLeakingBackend) SetCapacity(_ context.Context, volumeID string, _ int64) error {
	return fmt.Errorf("btrfs: set quota limit 4096 on %s: %w", b.unit+"/"+volumeID, errInjectedSubstrate)
}

// OpenRoot SUCCEEDS and hands back a root whose IO fails with a host-path error.
// Failing OpenRoot itself would be the weaker injection: the mediator has not touched
// the caller's `path` at that point, so the resulting message could not carry both a
// host path AND the caller's parameter — and "both, told apart correctly" is the
// property under test.
func (b *pathLeakingBackend) OpenRoot(_ context.Context, volumeID string) (volume.Root, error) {
	return &pathLeakingRoot{unit: b.unit + "/" + volumeID}, nil
}

type pathLeakingRoot struct {
	volume.Root
	unit string
}

func (r *pathLeakingRoot) Stat(name string) (volume.FileInfo, error) {
	return volume.FileInfo{}, fmt.Errorf("btrfs: stat %s: %w", r.unit+"/"+name, errInjectedSubstrate)
}

func (r *pathLeakingRoot) Mkdir(name string, _ os.FileMode) error {
	return fmt.Errorf("btrfs: mkdir %s: %w", r.unit+"/"+name, errInjectedSubstrate)
}

func (r *pathLeakingRoot) Remove(name string) error {
	return fmt.Errorf("btrfs: unlink %s: %w", r.unit+"/"+name, errInjectedSubstrate)
}

func (r *pathLeakingRoot) WriteAt(name string, _ []byte, _ int64) (int, error) {
	return 0, fmt.Errorf("btrfs: write %s: %w", r.unit+"/"+name, errInjectedSubstrate)
}

func (r *pathLeakingRoot) ReadAt(name string, _ []byte, _ int64) (int, error) {
	return 0, fmt.Errorf("btrfs: read %s: %w", r.unit+"/"+name, errInjectedSubstrate)
}

func (r *pathLeakingRoot) Close() error { return nil }

var errInjectedSubstrate = errors.New("operation not permitted")

// leakFixture serves a real router over a mediator whose backend leaks host paths, and
// returns the two host strings the response must never contain: the volumes dir the
// engine was configured with, and the temp root it lives under.
func leakFixture(t *testing.T) (*httptest.Server, *store.Memory, string) {
	t.Helper()
	dir := t.TempDir()
	mem := store.NewMemory()
	real, err := volume.NewPlainDir(dir)
	if err != nil {
		t.Fatalf("new plain_dir backend: %v", err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:  mem,
		Volume: &pathLeakingBackend{Backend: real, unit: dir},
		Logf:   func(string, ...any) {},
	})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, mem, dir
}

// assertNoHostPath is the §6.2 property, asserted on a response BODY: the caller sees
// opaque IDs and no host location.
//
// It checks the configured dir (the specific leak), then the generic absolute-path
// prefixes — `/home`, `/tmp`, `/var`, `/srv` — because a sanitizer tuned to one
// configured root would pass this test while leaking every other path the engine
// composes. Nothing legitimately in an error body starts with a slash: §6.2 addresses
// sandboxes, templates and volumes by opaque ID, and a volume's own content paths are
// root-RELATIVE.
func assertNoHostPath(t *testing.T, what string, body []byte, hostDir string) {
	t.Helper()
	for _, leak := range append([]string{hostDir}, "/home", "/tmp", "/var", "/srv", "/proc") {
		if leak != "" && bytes.Contains(body, []byte(leak)) {
			t.Errorf("%s leaks a host path (%q) into the response body — §6.2 admits opaque IDs only: %s",
				what, leak, bytes.TrimSpace(body))
		}
	}
}

// TestErrorBodiesCarryTheVerdictWithoutTheHostPath is the test the leak survived the
// absence of: it drives REAL failures whose underlying error carries an absolute host
// path, and asserts BOTH halves at once — the body names no host location, and it still
// says which object relationship failed.
//
// Both halves are required, and asserting only the first is the trap: flattening every
// arm to "internal error" satisfies "no host path" perfectly and destroys the §6.2
// requirement that the four Volume 409s stay distinguishable. The whole reason each has
// its own sentinel is that a 409 naming the wrong object sends the caller to the wrong
// remedy — so each case below names the token that must SURVIVE the redaction.
func TestErrorBodiesCarryTheVerdictWithoutTheHostPath(t *testing.T) {
	t.Run("volume create: a leaking backend error becomes a 500 without the volumes dir", func(t *testing.T) {
		srv, _, dir := leakFixture(t)
		resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// The status code mapping is untouched by the sanitizer: an unclassified substrate
		// failure is still a 500.
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("create with a failing backend = %d, want 500: %s", resp.StatusCode, body)
		}
		assertNoHostPath(t, "POST /v1/volumes", body, dir)
		// And the caller is not left with nothing: the verdict prose survives, so they can
		// tell an allocation failure from a rejected request.
		for _, want := range []string{"create volume", "allocate"} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("body does not say %q — the redaction took the meaning with the path: %s", want, bytes.TrimSpace(body))
			}
		}
		// The redaction is VISIBLE rather than silent, so the caller can tell the engine
		// withheld a detail and quote the message to an operator who has the log.
		if !bytes.Contains(body, []byte(redacted)) {
			t.Errorf("nothing marked redacted, so the path was either passed through or silently elided: %s", bytes.TrimSpace(body))
		}
	})

	t.Run("volume patch: the 500 keeps its verdict and loses the unit path", func(t *testing.T) {
		srv, mem, dir := leakFixture(t)
		// The record exists (inserted directly — create is broken in this fixture by
		// design), so PATCH reaches the backend rather than 404ing first.
		if err := mem.CreateVolume(context.Background(), model.Volume{
			ID: "vol_leaky", Backend: "plain_dir", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		resp := doBody(t, http.MethodPatch, srv.URL+"/v1/volumes/vol_leaky", `{"size":0}`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertNoHostPath(t, "PATCH /v1/volumes/{id}", body, dir)
		// The volume ID is an opaque engine ID, so it MUST survive — it is how the caller
		// knows which object the verdict is about.
		if resp.StatusCode != http.StatusOK && !bytes.Contains(body, []byte("vol_leaky")) {
			t.Errorf("the refusal does not name the volume it refused: %s", bytes.TrimSpace(body))
		}
	})

	t.Run("volume import: the caller's own path is echoed, the host path is not", func(t *testing.T) {
		srv, mem, dir := leakFixture(t)
		if err := mem.CreateVolume(context.Background(), model.Volume{
			ID: "vol_leaky", Backend: "plain_dir", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes/vol_leaky/import?path=data/blob", "payload")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertNoHostPath(t, "POST /v1/volumes/{id}/import", body, dir)
		// `data/blob` is the caller's own parameter, so echoing it tells them nothing about
		// the host — and it is what identifies WHICH import failed.
		if !bytes.Contains(body, []byte("data/blob")) {
			t.Errorf("the failure does not name the path the caller asked for, so they cannot tell which import failed: %s", bytes.TrimSpace(body))
		}
	})

	t.Run("volume export: same funnel on the read side", func(t *testing.T) {
		srv, mem, dir := leakFixture(t)
		if err := mem.CreateVolume(context.Background(), model.Volume{
			ID: "vol_leaky", Backend: "plain_dir", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodGet, srv.URL+"/v1/volumes/vol_leaky/export?path=data/blob")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertNoHostPath(t, "GET /v1/volumes/{id}/export", body, dir)
	})

	t.Run("volume inspect: a failing usage read degrades rather than leaking", func(t *testing.T) {
		srv, mem, dir := leakFixture(t)
		if err := mem.CreateVolume(context.Background(), model.Volume{
			ID: "vol_leaky", Backend: "plain_dir", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodGet, srv.URL+"/v1/volumes/vol_leaky")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertNoHostPath(t, "GET /v1/volumes/{id}", body, dir)
	})

	t.Run("sandbox create: a leaking volume-mount failure keeps the sandbox id out of a path", func(t *testing.T) {
		srv, mem, dir := leakFixture(t)
		seedTemplate(t, mem, "bare", "", true)
		// An unknown volume in the binding: the refusal travels the same funnel.
		resp := doBody(t, http.MethodPost, srv.URL+"/v1/sandboxes",
			`{"template_id":"bare","volumes":[{"volume_id":"vol_ghost","path":"/data","access":"shared-rw"}]}`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertNoHostPath(t, "POST /v1/sandboxes", body, dir)
	})
}

// TestSanitizerKeepsEverySentinelDistinguishable is the other side of the same fence,
// asserted on the SENTINEL TEXTS directly rather than through a fixture per verdict.
//
// §6.2 requires the four Volume 409s and the two 400s to stay distinguishable, and the
// sanitizer is a text transform standing between them and the caller — so the property
// to pin is that it is a no-op on every one of them. If a future tightening starts
// redacting prose, this fails on the exact sentinel that lost its meaning rather than
// on whichever endpoint happened to be covered elsewhere.
func TestSanitizerKeepsEverySentinelDistinguishable(t *testing.T) {
	for _, sentinel := range []error{
		store.ErrVolumeInUse,
		store.ErrVolumeHasDescendants,
		store.ErrSandboxHasMounts,
		store.ErrMountLeaseConflict,
		store.ErrMountBindingInvalid,
		store.ErrVolumeNotFound,
		store.ErrNotFound,
		store.ErrTemplateNotFound,
		store.ErrInUse,
		lifecycle.ErrVolumeCapBelowUsage,
		lifecycle.ErrVolumeCapUnsupported,
		lifecycle.ErrVolumeLiveWriter,
		lifecycle.ErrCapRequired,
		lifecycle.ErrInsufficientCapacity,
		lifecycle.ErrResourceTerminated,
	} {
		if got := sanitizeErr(sentinel); got != sentinel.Error() {
			t.Errorf("the sanitizer altered a sentinel's text:\n  before: %s\n  after:  %s\n"+
				"every §6.2 verdict must survive verbatim — a 409 naming the wrong object sends the caller to the wrong remedy",
				sentinel.Error(), got)
		}
	}
}

// TestSanitizerRedactsPathsAndPlacement pins the transform itself at the token level:
// what goes, what stays, and the punctuation cases error prose actually produces.
//
// Unit-level rather than only end-to-end because the shapes below arrive from parts of
// the engine the orchestrator's own tests cannot make fail without root — a btrfs
// quota error, a netns dial error — and the funnel has to be right for those too.
func TestSanitizerRedactsPathsAndPlacement(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		echo     []string // tokens the caller supplied, so they come back
		gone     []string // must NOT appear in the output
		kept     []string // must appear (the verdict, and the opaque IDs)
	}{
		{
			name: "btrfs create wraps an absolute unit path",
			in:   "create volume: allocate: btrfs: create subvolume /srv/state/volumes/vol_abc: exit status 1",
			gone: []string{"/srv/state/volumes/vol_abc", "/srv"},
			kept: []string{"create volume", "allocate", "exit status 1"},
		},
		{
			name: "a trailing colon must not save the path",
			in:   "btrfs: qgroup show /srv/volumes/vol_x: no such file",
			gone: []string{"/srv/volumes/vol_x"},
			kept: []string{"qgroup show", "no such file"},
		},
		{
			name: "quoted paths are redacted too",
			in:   `store: cannot open "/var/lib/engine/store.db" for writing`,
			gone: []string{"/var/lib/engine/store.db", "/var"},
			kept: []string{"cannot open", "for writing"},
		},
		{
			name: "the netns endpoint is placement and goes as well",
			in:   "dial tcp 10.88.0.7:9000: connect: connection refused",
			gone: []string{"10.88.0.7", "10.88.0.7:9000"},
			kept: []string{"connection refused"},
		},
		{
			// net/http reports a failed request as `Get "<url>": dial tcp <host>: …`, so the
			// endpoint appears TWICE and only the second copy looks like a bare address.
			// Matching the bare form alone would redact the visible half and leave the URL —
			// worse than not redacting, because it READS as sanitized.
			name: "an endpoint hidden inside a URL goes too",
			in:   `download /etc/x: Get "http://10.88.0.7:9000/files?path=%2Fetc%2Fx": dial tcp 10.88.0.7:9000: connection refused`,
			// /etc/x is a GUEST path, indistinguishable from a host path here — which is why
			// files.go states it itself and passes it as echo, rather than hoping the funnel
			// can tell. This case pins that arrangement: with the echo, the caller learns
			// which download failed and nothing about where the sandbox is.
			echo: []string{"/etc/x"},
			gone: []string{"10.88.0.7", "9000"},
			kept: []string{"download", "/etc/x", "connection refused"},
		},
		{
			name: "opaque IDs and root-relative paths are NOT paths and stay",
			in:   "direct write to vol_abc: lifecycle: volume has a live read-write mount: sandbox sbx_9 holds it read-write at data/blob",
			gone: []string{},
			kept: []string{"vol_abc", "sbx_9", "live read-write mount", "data/blob"},
		},
		{
			name: "a bare slash is not a path (it is prose or a root reference)",
			in:   "mount binding is self-contradictory: / is not a legal destination",
			gone: []string{},
			kept: []string{"self-contradictory", "/ is not"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeErr(errors.New(tc.in), tc.echo...)
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("%q survived sanitization: %s", g, got)
				}
			}
			for _, k := range tc.kept {
				if !strings.Contains(got, k) {
					t.Errorf("%q was lost, so the caller cannot act on the verdict: %s", k, got)
				}
			}
		})
	}

	// The echo allowlist: a token the CALLER supplied comes back, because handing back
	// their own parameter tells them nothing about the host. It is an allowlist of exact
	// tokens, so a sibling path under the same directory is still redacted.
	got := sanitizeErr(errors.New("download /home/user/notes.txt: open /srv/state/overlays/ov_1/merged/home/user/notes.txt: no such file"), "/home/user/notes.txt")
	if !strings.Contains(got, "/home/user/notes.txt") {
		t.Errorf("the caller's own ?path= was redacted, leaving them unable to tell which download failed: %s", got)
	}
	if strings.Contains(got, "/srv/state/overlays") {
		t.Errorf("the ENGINE's path survived because a similar-looking one was echoed: %s", got)
	}
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
