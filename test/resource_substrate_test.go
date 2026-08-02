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

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/transport"
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
