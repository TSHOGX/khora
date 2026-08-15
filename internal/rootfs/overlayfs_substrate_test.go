//go:build substrate

// L2 subsystem integration: real host overlayfs. Requires root (mount
// is privileged) and a native overlayfs-capable FS. Excluded from the default
// `go test ./...`; run explicitly on the machine:
//
//	sudo -E go test -tags substrate ./internal/rootfs/...
package rootfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/TSHOGX/khora/test/substratereq"
)

// mustRoot FAILS rather than skipping: `ok` from a substrate run must mean the substrate
// ran, and a skip made the two indistinguishable.
//
// Per-test rather than a TestMain, unlike internal/btrfs: this package's OTHER four test
// files carry no build tag, so they compile into the substrate binary too and legitimately
// need no root (MaxLowerDepth is arithmetic, PromoteToTemplate is a plain host copy). A
// package-level precondition would fail them for needing nothing.
func mustRoot(t *testing.T) {
	t.Helper()
	substratereq.Root(t)
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

	p, err := NewHostOverlayFS(stateDir, tmplDir, filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}

	o, err := p.CreateOverlay(ctx, "ov-test", []string{"bare"}, 0)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}

	// --- mount 1: merged sees Template content (CoW read-through) ------------
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

	// --- resume: OpenOverlay + re-mount SAME upper → byte-identical ----------
	o2, err := p.OpenOverlay(ctx, "ov-test", []string{"bare"}, 0)
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

// Mount must hand overlayfs an EMPTY workdir on every mount — the kernel rejects or
// corrupts a reused one (rootfs-overlay the documented invariant). Mount satisfies that by EMPTYING the
// directory rather than deleting and recreating it, because on a unit at its capacity cap
// the `mkdir` needs a metadata extent the quota has no room for while emptying needs none
// (the documented invariant). That is a real distinction on the substrate, so the invariant needs a test of its
// own rather than riding on the remount above: an implementation that stopped clearing work
// altogether would still pass the byte-identity assertions until a leftover made a mount
// fail.
//
// Asserted through TWO successive mounts with debris planted in between, which is exactly
// the resume shape (pause leaves a populated work behind).
func TestMountClearsStaleWorkWithoutRecreatingIt(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()

	base := t.TempDir()
	tmplDir := filepath.Join(base, "templates")
	makeTemplate(t, filepath.Join(tmplDir, "bare"))
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), tmplDir, filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	o, err := p.CreateOverlay(ctx, "ov-work", []string{"bare"}, 0)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}
	m, err := p.Mount(ctx, o)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := p.Unmount(ctx, m); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	// Record the work dir's identity, then plant leftovers of the shape a previous mount
	// leaves (the kernel's own `work/work`, plus a stray file).
	work := p.workPath("ov-work")
	var before syscall.Stat_t
	if err := syscall.Stat(work, &before); err != nil {
		t.Fatalf("stat work: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "leftover"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The mount must SUCCEED — which is the property: a stale work was cleared.
	m2, err := p.Mount(ctx, o)
	if err != nil {
		t.Fatalf("re-Mount with a stale work dir = %v; Mount must clear it (overlayfs rejects a reused workdir, the documented invariant)", err)
	}
	t.Cleanup(func() { _ = p.Unmount(ctx, m2) })
	if _, err := os.Stat(filepath.Join(work, "leftover")); !os.IsNotExist(err) {
		t.Fatalf("the stale file survived Mount (%v); workdir must be emptied", err)
	}

	// ...and the directory was EMPTIED, not recreated: same inode. This is the half that
	// keeps a full unit resumable (the documented invariant) — a recreate would need a metadata extent the
	// quota cannot grant, and it is invisible to any assertion about mount success on a
	// unit that is not full.
	var after syscall.Stat_t
	if err := syscall.Stat(work, &after); err != nil {
		t.Fatalf("stat work after: %v", err)
	}
	if after.Ino != before.Ino {
		t.Fatalf("work dir inode changed %d → %d — it was RECREATED, not emptied. On a unit at its capacity cap the mkdir returns ENOSPC (measured, tens of KiB of headroom), which makes a sandbox that hit the ordinary the documented invariant ENOSPC permanently unresumable and breaks the documented invariant's reversible-gate promise (rootfs-overlay the documented invariant)", before.Ino, after.Ino)
	}
}

// A stack past the options cap is REFUSED with a diagnosable error, not handed to the
// kernel. This is the one that needs a real mount: the failure being guarded against is
// that the classic mount path TRUNCATES options past a page instead of rejecting them
// (rootfs-overlay the documented invariant ⑩), so without the check the kernel either mounts a stack missing
// its tail — silent, and indistinguishable from mass file loss — or fails naming nothing.
//
// MaxLowerDepth's number cannot be what this test drives, because a builtin's TemplateID is
// a deployment-chosen directory name that no constant bounds; long names are exactly how a
// stack under the reported depth still overruns the cap. So this drives the real path.
func TestMountRefusesALowerStackOverTheOptionsCap(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()

	base := t.TempDir()
	tmplDir := filepath.Join(base, "templates")
	stateDir := filepath.Join(base, "state")

	// Long builtin names, so few layers overrun the cap: 40 × ~120 bytes > 4096.
	const nLayers = 40
	longName := strings.Repeat("t", 100)
	lower := make([]string, 0, nLayers)
	for i := 0; i < nLayers; i++ {
		id := fmt.Sprintf("%s%02d", longName, i)
		makeTemplate(t, filepath.Join(tmplDir, id))
		lower = append(lower, id)
	}

	p, err := NewHostOverlayFS(stateDir, tmplDir, filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	o, err := p.CreateOverlay(ctx, "ov-deep", lower, 0)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}

	m, err := p.Mount(ctx, o)
	if err == nil {
		_ = p.Unmount(ctx, m)
		t.Fatal("Mount of an over-cap lower stack SUCCEEDED; want a refusal — the kernel truncates the options instead of failing, so this mounts a stack missing its tail (the documented invariant ⑩)")
	}
	// The error must name the cause, since the whole point is that the kernel's own
	// failure names nothing.
	for _, want := range []string{"lower stack", "mount options"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v; want it to mention %q so the cause is diagnosable", err, want)
		}
	}
}
