//go:build substrate

// L2 transport round-trip against the REAL sandboxd running under real runsc:
// Start streams stdout, Write+Read round-trips, and PID 1 restarts the transport
// child in place (D5.3). Requires root.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 180s ./test/ -run Transport
package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"warren/internal/runtime"
	"warren/internal/transport"
	"warren/test/harness"
)

func launchOne(t *testing.T) (transport.Client, func()) {
	return launchWith(t, nil)
}

// launchWith builds the bare Template + one running sandbox, optionally calling
// inject(templatesDir, "bare") after the Template is built (to drop test probes
// into the rootfs) and before it is launched. Returns a ready transport client.
func launchWith(t *testing.T, inject func(templatesDir, template string)) (transport.Client, func()) {
	t.Helper()
	ctx := context.Background()
	repo := harness.RepoRoot(t)
	base := t.TempDir()
	tmpl := base + "/templates"

	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)
	if inject != nil {
		inject(tmpl, "bare")
	}
	rp, rt := harness.Providers(t, base, tmpl)

	ov, err := rp.CreateOverlay(ctx, "ovD", []string{"bare"}, 0)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}
	merged, err := rp.Mount(ctx, ov)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	pl, err := rt.Launch(ctx, runtime.LaunchSpec{
		Sandbox: "sD", Rootfs: runtime.RootfsSource{MergedPath: merged.MergedPath},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cleanup := func() {
		_ = rt.Destroy(ctx, pl)
		_ = rp.Unmount(ctx, merged)
		_ = rp.DestroyOverlay(ctx, ov)
	}

	cl := transport.Dial(transport.Endpoint(pl.TransportEndpoint))
	// readiness gate: poll Health to first success (⑦).
	if !waitHealth(cl, 20*time.Second) {
		cleanup()
		t.Fatalf("Health never succeeded at %s", pl.TransportEndpoint)
	}
	return cl, cleanup
}

func waitHealth(cl transport.Client, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := cl.Health(ctx)
		cancel()
		if err == nil {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func TestTransportStartStreamsStdout(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "echo SKELETON_OK"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var out strings.Builder
	gotStart, exit := false, -1
	for ev := range ch {
		switch ev.Kind {
		case "start":
			gotStart = ev.PID > 0
		case "data":
			out.Write(ev.Data)
		case "end":
			exit = ev.ExitCode
		}
	}
	if !gotStart {
		t.Error("no start event with pid")
	}
	if !strings.Contains(out.String(), "SKELETON_OK") {
		t.Errorf("stdout = %q; want contains SKELETON_OK", out.String())
	}
	if exit != 0 {
		t.Errorf("exit_code = %d; want 0", exit)
	}
}

func TestTransportWriteReadRoundTrip(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	want := []byte("RANDOM_MARKER_" + time.Now().Format("150405.000"))
	if err := cl.Write(ctx, "/home/marker.txt", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := cl.Read(ctx, "/home/marker.txt", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("read back %q; want %q", got, want)
	}
}

// PID 1 restarts the transport child in place (D5.3): kill the transport child
// (found via the pidfile — its pid is not fixed), then Health must recover on
// the SAME endpoint (not a resume, no new IP).
func TestTransportPID1RestartsChild(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	// discover the transport child's real pid (published to a tmpfs pidfile —
	// it is NOT pid 2; the Go runtime consumes low pids).
	pidBytes, err := cl.Read(ctx, "/tmp/.sandboxd-transport.pid", 0, 0)
	if err != nil {
		t.Fatalf("read transport pidfile: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("empty transport pidfile")
	}
	t.Logf("transport child pid = %s", pid)

	// kill it from inside the guest (dash has a `kill` builtin).
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "kill -TERM " + pid}})
	if err != nil {
		t.Fatalf("Start kill: %v", err)
	}
	for range ch { // drain (stream breaks when transport dies)
	}

	// transport should be briefly down, then restarted in place by PID 1.
	time.Sleep(500 * time.Millisecond)
	if !waitHealth(cl, 15*time.Second) {
		t.Fatalf("Health did not recover after transport child killed (no in-place restart)")
	}
	// prove it actually serves again post-restart.
	if err := cl.Write(ctx, "/tmp/post-restart", []byte("ok")); err != nil {
		t.Fatalf("Write after restart: %v", err)
	}
}
