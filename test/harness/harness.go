//go:build substrate

// Package harness builds the real substrate fixtures shared by the L2/L3
// substrate tests (Task D transport round-trip, Task F e2e). It compiles the
// real sandboxd, builds a guest rootfs Template with it (reusing the R1
// preflight builder), and constructs the two real providers. Requires root.
package harness

import (
	"os"
	"os/exec"
	"path/filepath"
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
// templatesDir, using the R1 builder + the given sandboxd binary.
func BuildTemplate(t *testing.T, repoRoot, templatesDir, name, sandboxdBin string) {
	t.Helper()
	dst := filepath.Join(templatesDir, name)
	build := exec.Command(filepath.Join(repoRoot, "test/preflight/build-rootfs.sh"), dst, sandboxdBin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs: %v: %s", err, out)
	}
}

// Providers builds the two real providers rooted under base.
func Providers(t *testing.T, base, templatesDir string) (*rootfs.HostOverlayFS, *runtime.LocalRunsc) {
	t.Helper()
	rp, err := rootfs.NewHostOverlayFS(filepath.Join(base, "overlays"), templatesDir)
	if err != nil {
		t.Fatalf("rootfs provider: %v", err)
	}
	rt, err := runtime.NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "runsc-state"))
	if err != nil {
		t.Fatalf("runtime provider: %v", err)
	}
	return rp, rt
}
