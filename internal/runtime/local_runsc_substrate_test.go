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

	"github.com/TSHOGX/khora/internal/waitfor"
	"github.com/TSHOGX/khora/test/substratereq"
)

// mustRoot FAILS rather than skipping: `ok` from a substrate run must mean the substrate
// ran. Also states the binaries, because runsc/nft/ip absent from PATH produces a failure
// deep inside a launch rather than at the precondition.
//
// Per-test rather than a TestMain: this package's provider_test.go and seam_test.go carry
// no build tag and need no root (the seam guard is an AST scan), so a package-level
// precondition would fail them for needing nothing.
func mustRoot(t *testing.T) {
	t.Helper()
	substratereq.Root(t)
	substratereq.Bin(t, "runsc", "nft", "ip")
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
	// reuse the shared rootfs builder.
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
	// base is enough; the fakeguest writes nothing).
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
	// Two releases of pl1, deliberately, and they are not redundant: the Stop below
	// is the test's ACT (pl1 must be down BEFORE pl2 launches, or "resume got a new
	// IP" proves nothing), while this Cleanup is the failure-path guarantee — a
	// t.Fatalf between here and that Stop would otherwise leak pl1's container,
	// netns and veth into the dev environment, exactly the reclamation hole the documented invariant
	// makes this suite's own subject. teardownPlacement is idempotent and
	// best-effort, so running after the explicit Stop is a no-op.
	t.Cleanup(func() { _ = p.Destroy(ctx, pl1) })
	ep1 := pl1.TransportEndpoint
	_ = p.Stop(ctx, pl1)

	pl2, err := p.Launch(ctx, spec) // simulate resume: fresh placement
	if err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(ctx, pl2) })
	if pl2.TransportEndpoint == ep1 {
		t.Fatalf("resume reused IP %s; want new (the documented invariant IP-as-epoch)", ep1)
	}
	t.Logf("ep1=%s ep2=%s (new IP on resume)", ep1, pl2.TransportEndpoint)
}

// Adopt an already-running container after a "crash"
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
	// Failure-path guarantee for the whole test, registered the moment there is
	// something to reclaim. The later Destroy(adopted) is the test's ACT and tears
	// the same container down by the same names; teardownPlacement is idempotent, so
	// this cleanup is then a no-op. Without it, every t.Fatalf below — starting with
	// the pre-adopt health poll — would leak the container, netns and veth.
	t.Cleanup(func() { _ = p1.Destroy(ctx, pl) })
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
	adopted, alive, _, err := p2.Adopt(ctx, "sbxADOPT")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !alive {
		t.Fatal("Adopt judged a running container dead")
	}
	if adopted.TransportEndpoint != ep {
		t.Fatalf("Adopt got endpoint %s; want the SAME %s — adopt is not a resume, so the epoch must not change (the documented invariant)", adopted.TransportEndpoint, ep)
	}
	// adopted endpoint is reachable.
	if !pollHealth(t, url, 5*time.Second) {
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

// Adopt of a sandbox with NO live container returns alive=false (dead case).
func TestAdoptDeadContainer(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()
	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}
	_, alive, _, err := p.Adopt(ctx, "sbxGHOST")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if alive {
		t.Fatal("Adopt of a nonexistent container returned alive=true")
	}
}

// ReclaimOrphans tears down placements not in keep, leaves kept ones running.
// Launch two, keep one, reclaim → the kept container stays
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
	// Each placement registers its OWN cleanup as soon as it exists, rather than one
	// combined cleanup after both launches: with the latter, a Launch failure on the
	// second sandbox leaked the first one (nothing had been registered yet).
	mk := func(id string) *Placement {
		t.Helper()
		pl, err := p.Launch(ctx, LaunchSpec{Sandbox: id, Rootfs: RootfsSource{MergedPath: filepath.Join(tmplRoot, "bare")}})
		if err != nil {
			t.Fatalf("Launch %s: %v", id, err)
		}
		t.Cleanup(func() { _ = p.Destroy(ctx, pl) })
		return pl
	}
	keep := mk("sbxKEEP")
	orphan := mk("sbxORPHAN")

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
	c := &http.Client{Timeout: time.Second}
	return waitfor.Until(d, 200*time.Millisecond, func() bool {
		resp, err := c.Get(url)
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b) == "OK\n"
	})
}
