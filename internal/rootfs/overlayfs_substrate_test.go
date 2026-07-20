//go:build substrate

// L2 subsystem integration (M1 §8): real host overlayfs. Requires root (mount
// is privileged) and a native overlayfs-capable FS. Excluded from the default
// `go test ./...`; run explicitly on the machine:
//
//	sudo -E go test -tags substrate ./internal/rootfs/...
package rootfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mustRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("substrate test needs root (overlayfs mount); run under sudo -E")
	}
}

// makeTemplate builds a minimal lower dir with a marker file.
func makeTemplate(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc", "marker"), []byte("TEMPLATE_MARKER"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayCoWAndRemountByteIdentical(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()

	base := t.TempDir()
	tmplDir := filepath.Join(base, "templates")
	makeTemplate(t, filepath.Join(tmplDir, "bare"))
	stateDir := filepath.Join(base, "state")

	p, err := NewHostOverlayFS(stateDir, tmplDir)
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}

	o, err := p.CreateOverlay(ctx, "ov-test", "bare")
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}

	// --- mount 1: merged sees Template content (A2 CoW read-through) ---------
	m, err := p.Mount(ctx, o)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.MergedPath, "etc", "marker"))
	if err != nil || !bytes.Equal(got, []byte("TEMPLATE_MARKER")) {
		t.Fatalf("merged marker = %q, %v; want TEMPLATE_MARKER", got, err)
	}

	// write into merged → lands in upper only (CoW), lower stays clean.
	rnd := []byte("PERSIST_ME_42")
	if err := os.WriteFile(filepath.Join(m.MergedPath, "etc", "written"), rnd, 0o644); err != nil {
		t.Fatalf("write into merged: %v", err)
	}
	if err := p.Unmount(ctx, m); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	// --- upper is a plain host dir tree after stop; lower untouched ----------
	upperWritten := filepath.Join(stateDir, "ov-test", "upper", "etc", "written")
	if b, err := os.ReadFile(upperWritten); err != nil || !bytes.Equal(b, rnd) {
		t.Fatalf("upper file = %q, %v; want %q (upper must be a plain dir tree)", b, err, rnd)
	}
	if _, err := os.Stat(filepath.Join(tmplDir, "bare", "etc", "written")); !os.IsNotExist(err) {
		t.Fatalf("lower Template was polluted by copy-up (want clean): %v", err)
	}

	// --- resume: OpenOverlay + re-mount SAME upper → byte-identical (A7) -----
	o2, err := p.OpenOverlay(ctx, "ov-test", "bare")
	if err != nil {
		t.Fatalf("OpenOverlay: %v", err)
	}
	m2, err := p.Mount(ctx, o2)
	if err != nil {
		t.Fatalf("re-Mount: %v", err)
	}
	got2, err := os.ReadFile(filepath.Join(m2.MergedPath, "etc", "written"))
	if err != nil || !bytes.Equal(got2, rnd) {
		t.Fatalf("re-mounted written = %q, %v; want %q (persistence across remount)", got2, err, rnd)
	}
	if err := p.Unmount(ctx, m2); err != nil {
		t.Fatalf("Unmount 2: %v", err)
	}

	// --- destroy reclaims the bytes ------------------------------------------
	if err := p.DestroyOverlay(ctx, o2); err != nil {
		t.Fatalf("DestroyOverlay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ov-test")); !os.IsNotExist(err) {
		t.Fatalf("overlay dir survived destroy")
	}
}
