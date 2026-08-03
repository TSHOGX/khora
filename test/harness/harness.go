//go:build substrate

// Package harness builds the real substrate fixtures shared by the L2/L3
// substrate tests (transport round-trip, e2e). It compiles the real sandboxd,
// builds a guest rootfs Template with it (via the preflight builder), and
// constructs the two real providers. Requires root.
package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"engine/internal/rootfs"
	"engine/internal/runtime"
)

// RepoRoot resolves the engine repo root from this file's location.
func RepoRoot(t *testing.T) string {
	t.Helper()
	// test/harness/harness.go → ../.. is the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// tests run with CWD = the test package dir; walk up to the module root.
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("could not find repo root from %s", wd)
	return ""
}

// BuildSandboxd compiles the real cmd/sandboxd as a static binary.
func BuildSandboxd(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandboxd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sandboxd")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sandboxd: %v: %s", err, out)
	}
	return bin
}

// BuildTemplate builds a guest rootfs Template dir named `name` under
// templatesDir, using the preflight builder + the given sandboxd binary.
func BuildTemplate(t *testing.T, repoRoot, templatesDir, name, sandboxdBin string) {
	t.Helper()
	dst := filepath.Join(templatesDir, name)
	build := exec.Command(filepath.Join(repoRoot, "test/preflight/build-rootfs.sh"), dst, sandboxdBin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs: %v: %s", err, out)
	}
}

// BuildBrokenTemplate builds a Template whose /sbin/sandboxd exits immediately
// (Health never comes up) — used to inject a ⑦ cold-start failure on real runsc
// (L4). The entrypoint is a tiny static binary that just exits 0.
func BuildBrokenTemplate(t *testing.T, repoRoot, templatesDir, name string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "exiter")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	main := "package main\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "exiter-bin")
	cmd := exec.Command("go", "build", "-o", bin, filepath.Join(src, "main.go"))
	cmd.Dir = src
	// single-file build outside any module (no go.mod in the temp dir).
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build exiter: %v: %s", err, out)
	}
	dst := filepath.Join(templatesDir, name)
	build := exec.Command(filepath.Join(repoRoot, "test/preflight/build-rootfs.sh"), dst, bin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs(broken): %v: %s", err, out)
	}
}

// InjectStaticBin compiles a single-file static Go program (src) into the
// already-built Template dir at /usr/bin/<name>. Used to drop tiny test probes
// into the bare rootfs (which has no coreutils) — e.g. a winsize reader for the
// PTY resize acceptance (C1). CGO off so it runs in the guest with no loader deps.
func InjectStaticBin(t *testing.T, templatesDir, template, name, mainSrc string) {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(templatesDir, template, "usr", "bin", name)
	cmd := exec.Command("go", "build", "-o", dst, filepath.Join(srcDir, "main.go"))
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("InjectStaticBin %s: %v: %s", name, err, out)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatal(err)
	}
}

// CountEngineNetns counts engine-owned netns on this host (the placement marker
// local_runsc enumerates, §4.4-A). Tests use it to assert no orphan leaked.
func CountEngineNetns(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sbxns_") {
			n++
		}
	}
	return n
}

// Providers builds the two real providers rooted under base.
func Providers(t *testing.T, base, templatesDir string) (*rootfs.HostOverlayFS, *runtime.LocalRunsc) {
	t.Helper()
	rp, err := rootfs.NewHostOverlayFS(filepath.Join(base, "overlays"), templatesDir, filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("rootfs provider: %v", err)
	}
	rt, err := runtime.NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "runsc-state"))
	if err != nil {
		t.Fatalf("runtime provider: %v", err)
	}
	return rp, rt
}
