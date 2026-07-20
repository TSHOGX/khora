//go:build substrate

// L3 end-to-end acceptance (M1 §9): the full walking-skeleton vertical slice
// through the assembled engine against REAL runsc on this machine. Requires
// root. Each assertion is tagged with the joint it proves (plan §9.1/§9.2).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 300s ./test/ -run E2E
package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/orchestrator"
	"engine/internal/transport"
	"engine/test/harness"
)

func setupEngine(t *testing.T, healthTimeout time.Duration) (*orchestrator.Engine, string) {
	t.Helper()
	repo := harness.RepoRoot(t)
	base := t.TempDir()
	tmpl := filepath.Join(base, "templates")
	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)
	// A2 fixture: a marker in the Template (lower) to prove overlay read-through.
	if err := os.WriteFile(filepath.Join(tmpl, "bare", "etc", "tmpl-marker"), []byte("FROM_TEMPLATE"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: tmpl, StateDir: filepath.Join(base, "state"),
		HealthTimeout: healthTimeout,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	return eng, base
}

// upperDir returns the host upper path for a sandbox's Overlay (test-only peek
// into placement-internal state to assert A5/A8 byte reclamation).
func upperDir(t *testing.T, eng *orchestrator.Engine, base string, id model.SandboxID) string {
	t.Helper()
	rec, err := eng.Store.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	return filepath.Join(base, "state", "overlays", string(rec.Overlay), "upper")
}

func TestE2EWalkingSkeleton(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("e2e needs root (runsc --network=sandbox + overlay mount)")
	}
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// --- A1 create → running -------------------------------------------------
	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("A1 create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("A1: state=%s want running", stt)
	}
	t.Logf("A1 ✓ create → %s state=running", sid)

	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// --- A2 overlay effective: merged reads Template content -----------------
	got, err := cl.Read(ctx, "/etc/tmpl-marker", 0, 0)
	if err != nil || string(got) != "FROM_TEMPLATE" {
		t.Fatalf("A2 overlay read-through: got %q err=%v; want FROM_TEMPLATE", got, err)
	}
	t.Logf("A2 ✓ merged rootfs reads Template content (lower read-through)")

	// --- A3 process.Start streams stdout + exit 0 ----------------------------
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "echo SKELETON_OK"}})
	if err != nil {
		t.Fatalf("A3 start: %v", err)
	}
	var out strings.Builder
	exit := -1
	for ev := range ch {
		if ev.Kind == "data" {
			out.Write(ev.Data)
		}
		if ev.Kind == "end" {
			exit = ev.ExitCode
		}
	}
	if !strings.Contains(out.String(), "SKELETON_OK") || exit != 0 {
		t.Fatalf("A3: out=%q exit=%d; want SKELETON_OK + exit 0", out.String(), exit)
	}
	t.Logf("A3 ✓ process.Start streamed SKELETON_OK, exit=0")

	// --- A4 fs.write /home/marker.txt = R ------------------------------------
	R := []byte("R_" + time.Now().Format("150405.000000"))
	if err := cl.Write(ctx, "/home/marker.txt", R); err != nil {
		t.Fatalf("A4 write: %v", err)
	}
	t.Logf("A4 ✓ fs.write marker (%s)", R)

	// --- A5 pause: state=paused, endpoint gone, upper still on backend -------
	epBefore, _ := m.Endpoint(sid)
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("A5 pause: %v", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Paused {
		t.Fatalf("A5: state=%s want paused", stt)
	}
	if _, err := m.Endpoint(sid); err == nil {
		t.Fatal("A5: endpoint still live after pause (placement not released)")
	}
	up := upperDir(t, eng, base, sid)
	if _, err := os.Stat(up); err != nil {
		t.Fatalf("A5: upper dir gone after pause (should persist): %v", err)
	}
	t.Logf("A5 ✓ pause → paused, placement released, upper persists")

	// --- A6 resume: new IP, running, same sandbox_id -------------------------
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("A6 resume: %v", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("A6: state=%s want running", stt)
	}
	epAfter, err := m.Endpoint(sid)
	if err != nil {
		t.Fatalf("A6 endpoint: %v", err)
	}
	if epAfter == epBefore {
		t.Fatalf("A6: resume reused endpoint %s; want NEW IP (§3.3 IP-as-epoch)", epBefore)
	}
	t.Logf("A6 ✓ resume → new IP (%s → %s), running, same id=%s", epBefore, epAfter, sid)

	// --- A7 persistence + identity: read back R, same sandbox_id -------------
	cl2, err := m.Client(sid)
	if err != nil {
		t.Fatalf("A7 client: %v", err)
	}
	got2, err := cl2.Read(ctx, "/home/marker.txt", 0, 0)
	if err != nil || string(got2) != string(R) {
		t.Fatalf("A7: read back %q err=%v; want %q", got2, err, R)
	}
	t.Logf("A7 ✓ resume persisted marker (%s), same sandbox id", got2)

	// --- A8 destroy: container stopped + upper reclaimed ---------------------
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("A8 destroy: %v", err)
	}
	if _, err := m.State(ctx, sid); err == nil {
		t.Fatal("A8: record still present after destroy")
	}
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatalf("A8: upper dir survived destroy (want reclaimed): %v", err)
	}
	t.Logf("A8 ✓ destroy → record gone, upper reclaimed")
}

// A9 (L4 real runsc): inject a ⑦ cold-start failure (sandboxd exits → Health
// never comes up → timeout). create must reclaim the half-built placement +
// empty Overlay, delete the record → destroyed, and leave NO orphan netns.
func TestE2EColdStartUnwindRealRunsc(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("e2e needs root")
	}
	ctx := context.Background()
	repo := harness.RepoRoot(t)
	base := t.TempDir()
	tmpl := filepath.Join(base, "templates")
	harness.BuildBrokenTemplate(t, repo, tmpl, "broken")
	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: tmpl, StateDir: filepath.Join(base, "state"),
		HealthTimeout: 4 * time.Second, // fast fail
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}

	nsBefore := countSbxNetns(t)
	_, err = eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "broken"})
	if err == nil {
		t.Fatal("A9: create should fail (Health never comes up)")
	}
	if !strings.Contains(err.Error(), "health") {
		t.Fatalf("A9: want health-gate failure; got %v", err)
	}
	t.Logf("A9 ✓ cold-start failed at health gate: %v", err)

	// no record left (deleted → destroyed, no `failed` state).
	list, _ := eng.Mediator.List(ctx)
	if len(list) != 0 {
		t.Fatalf("A9: %d records left after unwind; want 0", len(list))
	}
	// no orphan netns (half-built placement reclaimed).
	nsAfter := countSbxNetns(t)
	if nsAfter > nsBefore {
		t.Fatalf("A9: orphan netns leaked (before=%d after=%d)", nsBefore, nsAfter)
	}
	t.Logf("A9 ✓ record deleted, placement reclaimed, no orphan netns")
}

func countSbxNetns(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sbxns-") {
			n++
		}
	}
	return n
}
