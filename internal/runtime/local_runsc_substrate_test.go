//go:build substrate

// L2 subsystem integration: local_runsc against REAL runsc. Requires
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

// Task E / B6 mechanism: Adopt an already-running container after a "crash"
// (simulated by a fresh provider instance with an empty live-map). Adopt must
// rebuild the handle from the SandboxID, judge it alive, and rediscover the SAME
// IP (not a new epoch); the re-dialed endpoint is reachable; then Destroy via the
// adopted handle cleanly tears down the live container (no leak).
func TestAdoptRunningContainerSameIP(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()
	tmplRoot := filepath.Join(base, "templates")
	buildFixtureRootfs(t, filepath.Join(tmplRoot, "bare"))
	runscRoot := filepath.Join(base, "runsc-root")
	stateDir := filepath.Join(base, "state")

	p1, err := NewLocalRunsc(runscRoot, stateDir)
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}
	spec := LaunchSpec{Sandbox: "sbxADOPT", Rootfs: RootfsSource{MergedPath: filepath.Join(tmplRoot, "bare")}}
	pl, err := p1.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	ep := pl.TransportEndpoint
	url := "http://" + ep + "/health"
	if !pollHealth(t, url, 15*time.Second) {
		t.Fatalf("health never reachable pre-adopt at %s", url)
	}

	// simulate control-plane crash+restart: a NEW provider over the SAME roots,
	// with no in-memory handle to the running container.
	p2, err := NewLocalRunsc(runscRoot, stateDir)
	if err != nil {
		t.Fatalf("NewLocalRunsc (restart): %v", err)
	}
	adopted, alive, err := p2.Adopt(ctx, "sbxADOPT")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !alive {
		_ = p1.Destroy(ctx, pl)
		t.Fatal("Adopt judged a running container dead")
	}
	if adopted.TransportEndpoint != ep {
		_ = p1.Destroy(ctx, pl)
		t.Fatalf("Adopt got endpoint %s; want SAME %s (§4.4-A 同一 IP)", adopted.TransportEndpoint, ep)
	}
	// adopted endpoint is reachable.
	if !pollHealth(t, url, 5*time.Second) {
		_ = p1.Destroy(ctx, pl)
		t.Fatalf("adopted endpoint unreachable at %s", url)
	}
	t.Logf("Adopt ✓ same IP %s, reachable", adopted.TransportEndpoint)

	// Destroy via the ADOPTED handle must cleanly tear down the live container.
	if err := p2.Destroy(ctx, adopted); err != nil {
		t.Fatalf("Destroy adopted: %v", err)
	}
	if pollHealth(t, url, 3*time.Second) {
		t.Fatal("container still reachable after Destroy(adopted) — leak")
	}
	t.Logf("Adopt+Destroy ✓ no leak")
}

// Adopt of a sandbox with NO live container returns alive=false (B6 dead case).
func TestAdoptDeadContainer(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}
	_, alive, err := p.Adopt(ctx, "sbxGHOST")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if alive {
		t.Fatal("Adopt of a nonexistent container returned alive=true")
	}
}

// ReclaimOrphans tears down placements not in keep, leaves kept ones running
// (B5/B7 mechanism). Launch two, keep one, reclaim → the kept container stays
// reachable, the orphan's netns is gone.
func TestReclaimOrphans(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()
	tmplRoot := filepath.Join(base, "templates")
	buildFixtureRootfs(t, filepath.Join(tmplRoot, "bare"))
	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}
	mk := func(id string) *Placement {
		pl, err := p.Launch(ctx, LaunchSpec{Sandbox: id, Rootfs: RootfsSource{MergedPath: filepath.Join(tmplRoot, "bare")}})
		if err != nil {
			t.Fatalf("Launch %s: %v", id, err)
		}
		return pl
	}
	keep := mk("sbxKEEP")
	orphan := mk("sbxORPHAN")
	t.Cleanup(func() { _ = p.Destroy(ctx, keep); _ = p.Destroy(ctx, orphan) })

	keepURL := "http://" + keep.TransportEndpoint + "/health"
	orphanURL := "http://" + orphan.TransportEndpoint + "/health"
	if !pollHealth(t, keepURL, 15*time.Second) || !pollHealth(t, orphanURL, 15*time.Second) {
		t.Fatal("both should be reachable before reclaim")
	}

	if err := p.ReclaimOrphans(ctx, []string{"sbxKEEP"}); err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}
	// orphan gone, keep alive.
	if pollHealth(t, orphanURL, 3*time.Second) {
		t.Fatal("orphan still reachable after ReclaimOrphans")
	}
	if !pollHealth(t, keepURL, 3*time.Second) {
		t.Fatal("kept container was wrongly reclaimed")
	}
	// orphan netns removed; keep netns present.
	if _, err := os.Stat(filepath.Join("/var/run/netns", nsPrefix+"sbxORPHAN")); !os.IsNotExist(err) {
		t.Fatalf("orphan netns survived reclaim: %v", err)
	}
	if _, err := os.Stat(filepath.Join("/var/run/netns", nsPrefix+"sbxKEEP")); err != nil {
		t.Fatalf("kept netns gone: %v", err)
	}
	t.Logf("ReclaimOrphans ✓ orphan torn down, keep intact")
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
