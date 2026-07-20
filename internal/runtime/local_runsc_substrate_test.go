//go:build substrate

// L2 subsystem integration (M1 §8): local_runsc against REAL runsc. Requires
// root (--network=sandbox rejects --rootless). Excluded from default test run:
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod \
//	  go test -tags substrate ./internal/runtime/...
package runtime

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func mustRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("substrate test needs root (runsc --network=sandbox); run under sudo")
	}
}

// buildFixtureRootfs builds a Template dir whose /sbin/sandboxd is the fakeguest
// stand-in (isolates runtime from the real wire protocol; see fakeguest).
func buildFixtureRootfs(t *testing.T, dst string) {
	t.Helper()
	// compile fakeguest (static).
	bin := filepath.Join(t.TempDir(), "fakeguest")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeguest")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeguest: %v: %s", err, out)
	}
	// reuse the R1 rootfs builder.
	build := exec.Command("../../test/preflight/build-rootfs.sh", dst, bin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs: %v: %s", err, out)
	}
}

func TestLaunchStopDestroyAgainstRealRunsc(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()

	base := t.TempDir()
	tmplRoot := filepath.Join(base, "templates")
	buildFixtureRootfs(t, filepath.Join(tmplRoot, "bare"))

	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}

	// Runtime provider consumes an already-merged rootfs (opaque RootfsSource).
	// For an L2 runtime test we point it straight at the Template dir (read-only
	// base is enough; the fakeguest writes nothing). Overlay assembly is Task B.
	spec := LaunchSpec{
		Sandbox: "s1",
		Rootfs:  RootfsSource{MergedPath: filepath.Join(tmplRoot, "bare")},
	}

	pl, err := p.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(ctx, pl) })

	if pl.TransportEndpoint == "" {
		t.Fatal("Launch returned empty TransportEndpoint")
	}
	t.Logf("placement endpoint = %s", pl.TransportEndpoint)

	// dial the endpoint's /health from the host — proves netns/veth/IP-discovery
	// + runsc-adopt + netstack transport all wired (⑥/⑦).
	url := "http://" + pl.TransportEndpoint + "/health"
	if !pollHealth(t, url, 15*time.Second) {
		t.Fatalf("Health never reachable at %s", url)
	}

	// Stop releases placement; the endpoint must go unreachable and the netns gone.
	if err := p.Stop(ctx, pl); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pollHealth(t, url, 2*time.Second) {
		t.Fatalf("endpoint still reachable after Stop (placement not released)")
	}

	// Destroy is idempotent after Stop.
	if err := p.Destroy(ctx, pl); err != nil {
		t.Fatalf("Destroy after Stop: %v", err)
	}
}

func TestLaunchResumeGetsNewIP(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()
	tmplRoot := filepath.Join(base, "templates")
	buildFixtureRootfs(t, filepath.Join(tmplRoot, "bare"))
	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}
	spec := LaunchSpec{Sandbox: "s1", Rootfs: RootfsSource{MergedPath: filepath.Join(tmplRoot, "bare")}}

	pl1, err := p.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	ep1 := pl1.TransportEndpoint
	_ = p.Stop(ctx, pl1)

	pl2, err := p.Launch(ctx, spec) // simulate resume: fresh placement
	if err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(ctx, pl2) })
	if pl2.TransportEndpoint == ep1 {
		t.Fatalf("resume reused IP %s; want new (§3.3 IP-as-epoch)", ep1)
	}
	t.Logf("ep1=%s ep2=%s (new IP on resume)", ep1, pl2.TransportEndpoint)
}

func pollHealth(t *testing.T, url string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	c := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) == "OK\n" {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
