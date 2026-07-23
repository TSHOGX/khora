package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// PromoteToTemplate is a plain host-side copy (no mount, no root), so it runs in
// the default unit suite. It freezes a sandbox's at-rest upper into a new
// read-only layer dir, EXCLUDING the engine-injected runtime paths (§7).

func newProvider(t *testing.T) (*HostOverlayFS, string) {
	t.Helper()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	return p, base
}

// writeUpper plants files into a sandbox's upper dir (the at-rest tree Promote
// reads). Returns the overlay id.
func writeUpper(t *testing.T, p *HostOverlayFS, id string, files map[string]string) {
	t.Helper()
	upper := p.upperPath(id)
	for rel, content := range files {
		full := filepath.Join(upper, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// §7 / §5.B: Promote copies the upper into a new layer dir byte-for-byte, but
// EXCLUDES the sandboxd portal/work paths (they must not enter the frozen layer,
// acceptance §11.7). bytes-first: the layer exists on disk after Promote, ready
// for the row insert.
func TestPromoteToTemplateExcludesRuntimePaths(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)

	writeUpper(t, p, "ov_src", map[string]string{
		"home/work/app.py":            "print('hello')",
		"etc/installed-pkg":           "PKG",
		"tmp/.sandboxd-transport.pid": "12345", // §7 excluded
		"tmp/.sandboxd-crashloop":     "x",     // §7 excluded
		"tmp/user-scratch":            "keepme", // NOT excluded (only the sandboxd paths are)
	})

	ov := &Overlay{ID: "ov_src", Template: "bare", Lower: []string{"bare"}}
	if err := p.PromoteToTemplate(ctx, ov, "tmpl_new"); err != nil {
		t.Fatalf("PromoteToTemplate: %v", err)
	}

	layer := filepath.Join(p.LayersDir, "tmpl_new")
	// captured content present + byte-identical.
	if b, err := os.ReadFile(filepath.Join(layer, "home/work/app.py")); err != nil || string(b) != "print('hello')" {
		t.Fatalf("app.py not captured byte-identical: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(layer, "etc/installed-pkg")); err != nil || string(b) != "PKG" {
		t.Fatalf("installed-pkg not captured: %q %v", b, err)
	}
	// non-excluded tmp file survives.
	if _, err := os.Stat(filepath.Join(layer, "tmp/user-scratch")); err != nil {
		t.Errorf("non-excluded tmp file was dropped: %v", err)
	}
	// §7 excluded sandboxd paths must NOT be in the layer.
	if _, err := os.Stat(filepath.Join(layer, "tmp/.sandboxd-transport.pid")); !os.IsNotExist(err) {
		t.Errorf("sandboxd pidfile leaked into layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer, "tmp/.sandboxd-crashloop")); !os.IsNotExist(err) {
		t.Errorf("sandboxd crashloop sentinel leaked into layer: %v", err)
	}
	// no leftover staging dir.
	if _, err := os.Stat(layer + ".staging"); !os.IsNotExist(err) {
		t.Errorf("staging dir not cleaned up: %v", err)
	}
}

// A promoted layer is then usable as a lower: a sandbox created over
// [tmpl_new] mounts it (multi-lowerdir needs a real dir). We assert the layer
// dir resolves via layerPath (the mount itself is substrate-only).
func TestPromotedLayerResolvesAsLower(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	writeUpper(t, p, "ov_src", map[string]string{"etc/x": "1"})
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_new"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	lp, err := p.layerPath("tmpl_new")
	if err != nil {
		t.Fatalf("promoted layer not resolvable: %v", err)
	}
	if lp != filepath.Join(p.LayersDir, "tmpl_new") {
		t.Fatalf("layerPath = %s; want under LayersDir", lp)
	}
}

// Promote refuses to overwrite an existing layer id (bytes-first + immutability:
// a layer dir is written once).
func TestPromoteRefusesExistingLayer(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	writeUpper(t, p, "ov_src", map[string]string{"etc/x": "1"})
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_dup"); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_dup"); err == nil {
		t.Fatal("second promote to same id should fail (layer already exists)")
	}
}
