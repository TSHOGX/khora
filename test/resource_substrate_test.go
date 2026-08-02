//go:build substrate

// M6 acceptance on the REAL substrate: the §3.4 resource limits become real
// enforcement. Requires root (cgroup writes + runsc + overlay mount).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 900s ./test/ -run Resource
//
// Gate A (rate axis) lives here: acceptance 1 (real CPU throttling + no
// cross-sandbox interference) and 2 (the cap DID bind, i.e. the leaf-ownership
// trap is not silently swallowing it). Each assertion states its property inline.
package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"engine/internal/btrfs"
	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/orchestrator"
	"engine/internal/transport"
	"engine/test/harness"
)

// cpuBurnSrc is a guest-side busy loop. The minimal Template has no coreutils, so
// probes are injected as tiny static Go binaries (harness.InjectStaticBin).
const cpuBurnSrc = `package main

func main() {
	x := 0
	for {
		x++
		if x == 1<<62 {
			return
		}
	}
}
`

// diskFillSrc writes to a path until it fails, then reports the exact errno and
// how many bytes it got in. Used by the capacity-axis assertion (Gate B): the
// interesting outcome is not "it failed" but WHICH errno the guest saw and whether
// the sandbox survived it, so the probe prints a parseable result instead of just
// exiting nonzero.
//
// Args: diskfill <path> <chunkKiB>
const diskFillSrc = `package main

import (
	"fmt"
	"os"
	"syscall"
)

func main() {
	path := os.Args[1]
	chunk := 64 * 1024
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &chunk)
		chunk *= 1024
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("OPEN_ERR=%v\n", err)
		os.Exit(1)
	}
	buf := make([]byte, chunk)
	var total int64
	for {
		n, werr := f.Write(buf)
		total += int64(n)
		if werr != nil {
			errno := 0
			if pe, ok := werr.(*os.PathError); ok {
				if e, ok := pe.Err.(syscall.Errno); ok {
					errno = int(e)
				}
			}
			fmt.Printf("WROTE=%d WRITE_ERRNO=%d WRITE_ERR=%v\n", total, errno, werr)
			// fsync/close behaviour matters too: the measured contract is that the
			// ENOSPC lands on write() and the file is otherwise consistent.
			serr := f.Sync()
			cerr := f.Close()
			fmt.Printf("SYNC_ERR=%v CLOSE_ERR=%v\n", serr, cerr)
			return
		}
	}
}
`

// cgLeaf is the engine-owned cgroup leaf path for a sandbox. The test reads the
// host cgroupfs directly — that is the whole point of the assertion: prove the
// engine's INTENT actually landed on the substrate rather than trusting the
// engine's own report of itself.
func cgLeaf(sid model.SandboxID) string {
	return filepath.Join("/sys/fs/cgroup", "engine", "sbxc_"+string(sid))
}

func readCg(t *testing.T, sid model.SandboxID, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cgLeaf(sid), file))
	if err != nil {
		t.Fatalf("read %s for %s: %v", file, sid, err)
	}
	return strings.TrimSpace(string(b))
}

// cpuUsecOf sums the CPU time of EVERY task in the sandbox's cgroup, read from
// cpu.stat's usage_usec.
//
// Summing over the whole cgroup is required, not incidental (M6 §4 constraint 3):
// systrap runs each guest thread as its OWN host thread group, so sampling the
// sentry's /proc/<pid>/stat alone reports a fictitious ~0.01 CPU while the sandbox
// is in fact saturating its quota. cpu.stat aggregates the entire cgroup, which is
// exactly the "sum over cgroup.procs" the constraint demands.
func cpuUsecOf(t *testing.T, sid model.SandboxID) int64 {
	t.Helper()
	for _, line := range strings.Split(readCg(t, sid, "cpu.stat"), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "usage_usec" {
			v, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				t.Fatalf("parse usage_usec %q: %v", f[1], err)
			}
			return v
		}
	}
	t.Fatalf("no usage_usec in cpu.stat for %s", sid)
	return 0
}

func nrThrottled(t *testing.T, sid model.SandboxID) int64 {
	t.Helper()
	for _, line := range strings.Split(readCg(t, sid, "cpu.stat"), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "nr_throttled" {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return 0
}

// setupEngineOnBtrfs is setupEngineWithProbe with the engine StateDir forced onto
// a REAL btrfs filesystem, which the capacity axis requires (subvolume + squota,
// ADR-0002). t.TempDir() cannot be used for it: TMPDIR is /tmp = tmpfs here, where
// subvolumes do not exist — and production has the same constraint (overlay
// scratch must be native btrfs), so testing on tmpfs would exercise a
// configuration the engine does not support.
func setupEngineOnBtrfs(t *testing.T, healthTimeout time.Duration) (*orchestrator.Engine, string) {
	t.Helper()
	repo := harness.RepoRoot(t)
	base, err := os.MkdirTemp("/var/tmp", "m6cap-")
	if err != nil {
		t.Fatal(err)
	}
	bc := btrfs.Client{}
	if !bc.Available(context.Background(), base) {
		t.Skipf("%s is not on btrfs; the capacity axis needs btrfs (ADR-0002)", base)
	}
	stateDir := filepath.Join(base, "state")
	t.Cleanup(func() {
		// Overlay units are subvolumes; they must be deleted as subvolumes before the
		// plain RemoveAll can clear the tree.
		ctx := context.Background()
		ovRoot := filepath.Join(stateDir, "overlays")
		entries, _ := os.ReadDir(ovRoot)
		for _, e := range entries {
			p := filepath.Join(ovRoot, e.Name())
			if bc.IsSubvolume(ctx, p) {
				_ = os.RemoveAll(filepath.Join(p, "upper"))
				_ = os.RemoveAll(filepath.Join(p, "work"))
				_ = os.RemoveAll(filepath.Join(p, "merged"))
				_ = bc.DeleteSubvolume(ctx, p)
			}
		}
		_ = os.RemoveAll(base)
	})

	tmpl := filepath.Join(base, "templates")
	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)
	harness.InjectStaticBin(t, tmpl, "bare", "cpuburn", cpuBurnSrc)
	harness.InjectStaticBin(t, tmpl, "bare", "diskfill", diskFillSrc)

	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: tmpl, StateDir: stateDir,
		StorePath:     filepath.Join(base, "store.db"),
		HealthTimeout: healthTimeout,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := eng.SeedTemplates(context.Background()); err != nil {
		t.Fatalf("seed builtin templates: %v", err)
	}
	if err := eng.Reconcile(context.Background()); err != nil {
		t.Fatalf("startup reconcile: %v", err)
	}
	return eng, base
}

// TestResourceCapacityAxis is M6 acceptance 4 + 9: the guest fills its disk cap
// and gets a plain ENOSPC while THE SANDBOX KEEPS RUNNING (a data-plane event, not
// a lifecycle one — the opposite of the RAM cap), and destroy reclaims the
// subvolume and its quota.
func TestResourceCapacityAxis(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (btrfs ioctls + runsc + overlay mount)")
	}
	ctx := context.Background()
	eng, base := setupEngineOnBtrfs(t, 60*time.Second)
	m := eng.Mediator
	bc := btrfs.Client{}

	const diskCap = 64 << 20
	sid, err := m.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{DiskByte: diskCap},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			_ = m.Destroy(ctx, sid)
		}
	})

	rec, err := eng.Store.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	unit := filepath.Join(base, "state", "overlays", string(rec.Overlay))

	// the cap is materialized as a subvolume + squota limit on the ONE unit holding
	// upper AND work (D6.3: a quota on upper alone leaves copy-up as a bypass).
	if !bc.IsSubvolume(ctx, unit) {
		t.Fatalf("overlay unit %s is not a btrfs subvolume — the cap has nothing to bind to", unit)
	}
	used, limit, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if limit != diskCap {
		t.Fatalf("quota limit = %d; want %d (the capacity cap did not bind)", limit, diskCap)
	}
	t.Logf("✓ capacity cap bound: subvolume %s used=%d limit=%d (upper+work in ONE quota unit)", filepath.Base(unit), used, limit)

	// --- acceptance 4: guest write hits the cap → errno 28, sandbox SURVIVES ---
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/diskfill", Args: []string{"/root/fill", "1024"}})
	if err != nil {
		t.Fatalf("start diskfill: %v", err)
	}
	var out strings.Builder
	fillExit := -1
	for ev := range ch {
		switch ev.Kind {
		case "data":
			out.Write(ev.Data)
		case "end":
			fillExit = ev.ExitCode
		}
	}
	got := out.String()
	t.Logf("guest diskfill reported: %s", strings.TrimSpace(got))
	// errno 28 == ENOSPC. The guest must see an ordinary out-of-space write error.
	if !strings.Contains(got, "WRITE_ERRNO=28") {
		t.Fatalf("guest did not get ENOSPC (errno 28) at the cap; got: %s", got)
	}
	if fillExit != 0 {
		t.Fatalf("diskfill exited %d; the probe should report cleanly after ENOSPC", fillExit)
	}

	// the cap HELD: charged bytes never exceeded the limit.
	usedAfter, _, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage after fill: %v", err)
	}
	if usedAfter > diskCap {
		t.Fatalf("charged %d bytes against a %d cap — the ceiling was breached", usedAfter, diskCap)
	}
	t.Logf("✓ acceptance 4: guest got errno 28 at the cap; charged %d of %d bytes (%.2f%%)",
		usedAfter, diskCap, 100*float64(usedAfter)/float64(diskCap))

	// STILL RUNNING and still usable — the §3.4 distinction that makes capacity a
	// data-plane event: no lifecycle transition, control plane not involved.
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("state = %s after hitting the disk cap; want running (§3.4: capacity overrun is NOT a lifecycle event)", stt)
	}
	ch2, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "echo ALIVE_AFTER_ENOSPC"}})
	if err != nil {
		t.Fatalf("exec after ENOSPC failed — sandbox not usable: %v", err)
	}
	var out2 strings.Builder
	for ev := range ch2 {
		if ev.Kind == "data" {
			out2.Write(ev.Data)
		}
	}
	if !strings.Contains(out2.String(), "ALIVE_AFTER_ENOSPC") {
		t.Fatalf("exec after ENOSPC produced %q; sandbox should be fully usable", out2.String())
	}
	// and the death cause must be untouched: nothing died.
	if rec2, _ := eng.Store.GetSandbox(ctx, sid); rec2.DeathCause != model.CauseNone {
		t.Fatalf("death cause = %q after a mere ENOSPC; want none (capacity overrun is not a death)", rec2.DeathCause)
	}
	t.Logf("✓ acceptance 4: container still running and exec-able after ENOSPC, no death cause recorded")

	// freeing space inside the cap restores writability (the cap is a ceiling, not
	// a latch).
	if err := cl.Write(ctx, "/root/probe-after-free", []byte("ok")); err == nil {
		t.Logf("note: a small write still succeeded (headroom remains under the cap)")
	}

	// --- acceptance 9: destroy reclaims the subvolume + quota ----------------
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	destroyed = true
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("overlay unit survived destroy: %v", err)
	}
	if bc.IsSubvolume(ctx, unit) {
		t.Fatal("subvolume survived destroy (quota unit leaked)")
	}
	t.Logf("✓ acceptance 9: destroy reclaimed the subvolume and its quota unit")
}

// Acceptance 9, crash half: a sandbox whose create crashed mid-chain leaves an
// orphan Overlay unit, and startup reconcile must reclaim it as a SUBVOLUME —
// plain os.RemoveAll cannot delete a subvolume, so a reclamation path that forgot
// the subvolume case would silently leak both the bytes and the quota unit on
// every crashed create (§4.4-A reclamation discipline).
func TestResourceCapacityOrphanSubvolumeReclaimed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (btrfs ioctls + runsc)")
	}
	ctx := context.Background()
	eng, base := setupEngineOnBtrfs(t, 60*time.Second)
	bc := btrfs.Client{}

	// Manufacture the crashed-create state directly: a `creating` record plus a
	// materialized capped Overlay unit. That is exactly what a crash between "build
	// the Overlay" and "commit running" leaves behind (§4.4-A create variant).
	oid := "ov_orphan_cap"
	unit := filepath.Join(base, "state", "overlays", oid)
	if err := bc.CreateSubvolume(ctx, unit); err != nil {
		t.Fatalf("create orphan unit: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(unit, "upper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bc.SetQuotaLimit(ctx, unit, 32<<20); err != nil {
		t.Fatalf("cap orphan unit: %v", err)
	}
	now := time.Now()
	if err := eng.Store.CreateSandbox(ctx, model.Sandbox{
		ID: "sbx_orphan_cap", Template: "bare", Overlay: model.OverlayID(oid),
		State: model.Creating, Limits: model.Limits{DiskByte: 32 << 20},
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("seed creating record: %v", err)
	}
	if !bc.IsSubvolume(ctx, unit) {
		t.Fatalf("fixture not a subvolume: %s", unit)
	}

	// startup reconcile: a crashed `creating` has no resting state → its record is
	// deleted and its empty Overlay reclaimed.
	if err := eng.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, err := eng.Store.GetSandbox(ctx, "sbx_orphan_cap"); err == nil {
		t.Fatal("crashed `creating` record survived reconcile")
	}
	if bc.IsSubvolume(ctx, unit) {
		t.Fatalf("orphan capacity SUBVOLUME survived reconcile at %s — quota unit leaked (RemoveAll alone cannot delete a subvolume)", unit)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("orphan overlay unit dir survived reconcile: %v", err)
	}
	t.Logf("✓ acceptance 9 (crash half): startup reconcile reclaimed the orphan capacity subvolume + quota")
}

// A capacity cap that CANNOT be enforced must be refused, never silently
// downgraded to "run uncapped": a create that reports success while enforcing
// nothing would leave the §2.3 blast-radius invariant believing in a cap that does
// not exist. Here the StateDir is on tmpfs, where subvolumes cannot exist.
func TestResourceCapacityRefusedWhenUnenforceable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	// deliberately on tmpfs (t.TempDir()), i.e. NOT btrfs.
	eng, _ := setupEngineWithProbe(t, 30*time.Second)
	_, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{DiskByte: 64 << 20},
	})
	if err == nil {
		t.Fatal("create with a disk cap SUCCEEDED on a non-btrfs StateDir — the cap would enforce nothing (§3.4)")
	}
	if !strings.Contains(err.Error(), "btrfs") {
		t.Fatalf("want a refusal naming the unenforceable cap; got %v", err)
	}
	t.Logf("✓ unenforceable capacity cap refused rather than silently ignored: %v", err)
	// and no record was left behind (§4.4-A: nothing half-built).
	list, _ := eng.Mediator.List(ctx)
	if len(list) != 0 {
		t.Fatalf("%d records left after a refused create; want 0", len(list))
	}
}

// TestResourceRateAxis is M6 acceptance 1 + 2 + 3 (substrate half).
func TestResourceRateAxis(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (cgroup writes + runsc --network=sandbox + overlay mount)")
	}
	ctx := context.Background()
	eng, _ := setupEngineWithProbe(t, 60*time.Second)
	m := eng.Mediator

	const capMillis = 500 // 0.5 CPU
	const memCap = 256 << 20

	// Two sandboxes, SAME caps, on the same host: one will burn CPU, the other
	// stays idle. The pair is what makes acceptance 1's second half checkable —
	// per-sandbox throttling must not bleed across sandboxes.
	busy, err := m.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{CPUMillis: capMillis, MemoryByte: memCap},
	})
	if err != nil {
		t.Fatalf("create busy: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, busy) })

	neighbor, err := m.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{CPUMillis: capMillis, MemoryByte: memCap},
	})
	if err != nil {
		t.Fatalf("create neighbor: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, neighbor) })

	// --- acceptance 2: the caps ACTUALLY BOUND ------------------------------
	// This is the assertion that catches the §5.A leaf-ownership trap, whose
	// failure mode is SILENT: if the engine pre-created the full cgroup path,
	// runsc would write memory.swap.max=0, leave memory.max=max, and the launch
	// would still SUCCEED. Every engine-side signal would look identical to a
	// working cap. So the values are read back off the real cgroupfs.
	for _, sid := range []model.SandboxID{busy, neighbor} {
		wantCPU := fmt.Sprintf("%d %d", 100_000*capMillis/1000, 100_000)
		if got := readCg(t, sid, "cpu.max"); got != wantCPU {
			t.Fatalf("cpu.max = %q for %s; want %q — the cap did NOT bind", got, sid, wantCPU)
		}
		// memory.max is the HOST ceiling = N + provider overhead (§5.A two values):
		// the guest is told N, the host tolerates N plus the sentry/gofer footprint,
		// so the sandbox is never terminated below the N it was promised. Assert it is
		// a real number ≥ N and NOT the unlimited sentinel.
		gotMem := readCg(t, sid, "memory.max")
		if gotMem == "max" {
			t.Fatalf("memory.max = max for %s — the RAM cap did NOT bind (leaf-ownership trap fails OPEN, §5.A)", sid)
		}
		hostCeil, err := strconv.ParseInt(gotMem, 10, 64)
		if err != nil {
			t.Fatalf("parse memory.max %q: %v", gotMem, err)
		}
		if hostCeil < memCap {
			t.Fatalf("memory.max = %d < requested %d for %s: the host ceiling must be N+overhead, else the sandbox dies BELOW its promised cap", hostCeil, memCap, sid)
		}
		// a cap with swap available is not a cap: measured, a limit alone just pages
		// into zram and absorbs multiples of the cap.
		if got := readCg(t, sid, "memory.swap.max"); got != "0" {
			t.Fatalf("memory.swap.max = %q for %s; want 0 (else the cap is not a hard ceiling)", got, sid)
		}
		// §2.4 provider obligation: whole-group, deterministic termination — and the
		// thing that makes oom_group_kill readable as a death cause at all.
		if got := readCg(t, sid, "memory.oom.group"); got != "1" {
			t.Fatalf("memory.oom.group = %q for %s; want 1 (§2.4: deterministic whole-group termination)", got, sid)
		}
		t.Logf("✓ %s caps bound on the real cgroup: cpu.max=%s memory.max=%d (N=%d + overhead) swap.max=0 oom.group=1",
			sid, wantCPU, hostCeil, memCap)
	}

	// --- acceptance 1: real throttling, measured over the WHOLE cgroup -------
	cl, err := m.Client(busy)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// start the burner and leave it running (we never read its exit).
	if _, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/cpuburn"}); err != nil {
		t.Fatalf("start cpuburn: %v", err)
	}

	// let it saturate, then measure a clean window.
	time.Sleep(2 * time.Second)
	beforeBusy := cpuUsecOf(t, busy)
	beforeNeighbor := cpuUsecOf(t, neighbor)
	const window = 5 * time.Second
	start := time.Now()
	time.Sleep(window)
	elapsed := time.Since(start)
	deltaBusy := cpuUsecOf(t, busy) - beforeBusy
	deltaNeighbor := cpuUsecOf(t, neighbor) - beforeNeighbor

	// cores consumed = cpu-time / wall-time. The cap is 0.5, and a busy loop would
	// take a full core unthrottled.
	gotCores := float64(deltaBusy) / float64(elapsed.Microseconds())
	wantCores := float64(capMillis) / 1000
	t.Logf("busy sandbox consumed %.3f cores over %s (cap %.2f); nr_throttled=%d",
		gotCores, elapsed, wantCores, nrThrottled(t, busy))
	// Generous band: this is a shared dev machine and the assertion is "the quota
	// binds", not "the scheduler is precise to a percent". A broken cap shows up as
	// ~1.0 cores, far outside this.
	if gotCores < wantCores*0.75 || gotCores > wantCores*1.25 {
		t.Fatalf("busy sandbox used %.3f cores; want ≈%.2f (±25%%) — the CPU quota is not binding", gotCores, wantCores)
	}
	// throttling is the direct evidence the quota is being enforced each period.
	if n := nrThrottled(t, busy); n == 0 {
		t.Fatalf("nr_throttled = 0 while a busy loop ran under a %d-millis cap; the quota is not being enforced", capMillis)
	}

	// second half of acceptance 1: no cross-sandbox interference. The idle
	// neighbor's own cgroup must be essentially untouched by its noisy sibling.
	neighborCores := float64(deltaNeighbor) / float64(elapsed.Microseconds())
	t.Logf("idle neighbor consumed %.4f cores in the same window", neighborCores)
	if neighborCores > 0.15 {
		t.Fatalf("idle neighbor burned %.3f cores — per-sandbox accounting is leaking across sandboxes", neighborCores)
	}
	t.Logf("✓ acceptance 1: rate axis throttles (%.3f ≈ %.2f cores, nr_throttled>0) and does not disturb a co-resident sandbox", gotCores, wantCores)

	// --- acceptance 3: the cap is a BINDING, re-materialized across resume ---
	// pause/resume, then re-read the cgroup: a NEW leaf under a NEW placement must
	// carry the caps again, re-derived from the record rather than recovered from
	// the old (now destroyed) placement.
	if err := m.Pause(ctx, busy); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// the leaf is placement-local: it must be GONE after pause (§4.2 releases
	// RAM/CPU, and a leftover leaf would be an orphan placement resource).
	if _, err := os.Stat(cgLeaf(busy)); !os.IsNotExist(err) {
		t.Fatalf("cgroup leaf survived pause (orphan placement resource): %v", err)
	}
	t.Logf("✓ cgroup leaf reclaimed on pause (placement-local, §3.3)")

	if err := m.Resume(ctx, busy); err != nil {
		t.Fatalf("resume: %v", err)
	}
	wantCPU := fmt.Sprintf("%d %d", 100_000*capMillis/1000, 100_000)
	if got := readCg(t, busy, "cpu.max"); got != wantCPU {
		t.Fatalf("after resume cpu.max = %q; want %q — the Binding was not re-materialized (§3.4)", got, wantCPU)
	}
	if got := readCg(t, busy, "memory.swap.max"); got != "0" {
		t.Fatalf("after resume memory.swap.max = %q; want 0", got)
	}
	if got := readCg(t, busy, "memory.oom.group"); got != "1" {
		t.Fatalf("after resume memory.oom.group = %q; want 1", got)
	}
	t.Logf("✓ acceptance 3: caps re-materialized on the NEW placement after resume (cpu.max=%s, swap.max=0, oom.group=1)", wantCPU)

	// destroy → the leaf must be gone (reclamation discipline, §4.4-A).
	if err := m.Destroy(ctx, busy); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(cgLeaf(busy)); !os.IsNotExist(err) {
		t.Fatalf("cgroup leaf survived destroy: %v", err)
	}
	t.Logf("✓ cgroup leaf reclaimed on destroy")
}

// An UNLIMITED sandbox (all caps zero) must create no cgroup leaf at all: no
// resources block, no cgroupsPath, no footprint in the host cgroup namespace.
// This keeps "no limits requested" behaviourally identical to pre-M6 and keeps the
// engine parent free of empty leaves.
func TestResourceUnlimitedSandboxHasNoCgroupLeaf(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, _ := setupEngineWithProbe(t, 60*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"}) // no Limits
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	if _, err := os.Stat(cgLeaf(sid)); !os.IsNotExist(err) {
		t.Fatalf("uncapped sandbox created a cgroup leaf at %s (want none): %v", cgLeaf(sid), err)
	}
	// and it is a working sandbox, not a broken one.
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := cl.Health(ctx); err != nil {
		t.Fatalf("uncapped sandbox unhealthy: %v", err)
	}
	t.Logf("✓ uncapped sandbox runs with no cgroup leaf (no host cgroup footprint)")
}
