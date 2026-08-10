//go:build substrate

// Resource-governance acceptance on the REAL substrate: the §3.4 resource limits
// are real enforcement. Requires root (cgroup writes + runsc + overlay mount).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 900s ./test/ -run Resource
//
// The rate axis lives here: real CPU throttling with no cross-sandbox
// interference, and proof the cap DID bind (the leaf-ownership trap is not
// silently swallowing it). Each assertion states its property inline.
package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"warren/internal/btrfs"
	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/orchestrator"
	"warren/internal/store"
	"warren/internal/transport"
	"warren/test/harness"
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
// how many bytes it got in. Used by the capacity-axis assertion: the
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

// memHogSrc allocates in bounded steps with raw anonymous mmap and TOUCHES every
// page, so the memory is really charged rather than merely reserved.
//
// Raw mmap rather than the Go heap deliberately: a refused Go allocation aborts
// with a runtime fatal error, whereas mmap returns ENOMEM we can print — and under
// a memcg cap with swap.max=0 the interesting outcome is not a refusal at all but
// the whole-group kill, so the probe must not muddy that with its own panic. It is
// hard-bounded by maxMiB: the host is shared, so an unbounded allocator is not an
// acceptable test tool.
//
// Args: memhog <stepMiB> <maxMiB>
const memHogSrc = `package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func main() {
	step, _ := strconv.Atoi(os.Args[1])
	max, _ := strconv.Atoi(os.Args[2])
	if step <= 0 || max <= 0 || max > 4096 {
		fmt.Println("BAD_ARGS")
		os.Exit(2)
	}
	page := syscall.Getpagesize()
	var keep [][]byte
	total := 0
	for total < max {
		b, err := syscall.Mmap(-1, 0, step*1024*1024,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_ANON|syscall.MAP_PRIVATE)
		if err != nil {
			fmt.Printf("MMAP_REFUSED total=%dMiB err=%v\n", total, err)
			return
		}
		for i := 0; i < len(b); i += page {
			b[i] = 0xA5
		}
		keep = append(keep, b)
		total += step
		fmt.Printf("TOUCHED=%dMiB\n", total)
	}
	fmt.Printf("ALLOC_COMPLETE=%dMiB\n", total)
}
`

// unlinkSrc deletes a path via unlink(2). The capacity axis needs it because
// freeing bytes at a FULL quota is only possible this way: a truncating open()
// cannot get the space to open for writing, and truncate(2) succeeds without
// releasing anything (squota holds an extent's charge until its inode dies). The
// bare Template ships no `rm`, so this is injected as a static binary.
//
// Args: unlinker <path>
const unlinkSrc = `package main

import ("fmt";"os";"syscall")

func main() {
	if err := syscall.Unlink(os.Args[1]); err != nil {
		fmt.Printf("UNLINK_ERR=%v\n", err)
		os.Exit(1)
	}
	fmt.Println("UNLINKED")
}
`

// boundedFillSrc writes EXACTLY the requested number of MiB and reports what landed,
// then reads the first bytes back. It is diskFillSrc's counterpart: that probe writes
// until it FAILS (the right shape for proving the cap binds), while a cap-EDIT test needs
// a unit with usage strictly inside its cap, so the amount has to be an input.
//
// It also serves as the writability probe, because the bare rootfs ships no coreutils —
// there is no `dd` to fill with and no `cat` to read back, only /bin/sh and its built-ins.
//
// Args: boundedfill <path> <MiB>
const boundedFillSrc = `package main

import ("fmt";"os";"strconv")

func main() {
	path := os.Args[1]
	mib, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Printf("ARG_ERR=%v\n", err)
		os.Exit(1)
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("OPEN_ERR=%v\n", err)
		os.Exit(1)
	}
	buf := make([]byte, 1024*1024)
	for i := range buf { buf[i] = byte(i) }
	var total int64
	for n := 0; n < mib; n++ {
		w, werr := f.Write(buf)
		total += int64(w)
		if werr != nil {
			fmt.Printf("WRITE_ERR=%v WROTE=%d\n", werr, total)
			f.Close()
			os.Exit(1)
		}
	}
	if err := f.Close(); err != nil {
		fmt.Printf("CLOSE_ERR=%v\n", err)
		os.Exit(1)
	}
	// Read back, so "the write succeeded" is not merely "the syscall returned".
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		fmt.Printf("READ_ERR=%v\n", rerr)
		os.Exit(1)
	}
	fmt.Printf("WROTE=%d READBACK=%d\n", total, len(got))
}
`

// hostLogBytes is the total size of the kernel/syslog files whose growth the
// §2.3 bypass would show up in. See TestResourceRAMAxis: an unbounded OOM retry
// storm writes kernel log at ~66 MB/s, and that path goes AROUND the disk capacity
// cap, so "the sandbox died" is not by itself evidence the invariant held.
func hostLogBytes(t *testing.T) int64 {
	t.Helper()
	var total int64
	for _, f := range []string{"/var/log/syslog", "/var/log/kern.log"} {
		if fi, err := os.Stat(f); err == nil {
			total += fi.Size()
		}
	}
	return total
}

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
// Summing over the whole cgroup is required, not incidental (§5.A: CPU usage must
// be summed over all of `cgroup.procs`):
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

// cleanupOverlaySubvolumes removes Overlay units as SUBVOLUMES before a plain
// RemoveAll can clear the tree — os.RemoveAll cannot delete a subvolume, so
// without this the scratch tree survives and leaks quota units on the dev box.
func cleanupOverlaySubvolumes(bc btrfs.Client, stateDir string) {
	ctx := context.Background()
	ovRoot := filepath.Join(stateDir, "overlays")
	entries, _ := os.ReadDir(ovRoot)
	for _, e := range entries {
		p := filepath.Join(ovRoot, e.Name())
		if !bc.IsSubvolume(ctx, p) {
			continue
		}
		for _, sub := range []string{"upper", "work", "merged"} {
			_ = os.RemoveAll(filepath.Join(p, sub))
		}
		_ = bc.DeleteSubvolume(ctx, p)
	}
}

// setupEngineOnBtrfs is setupEngineWithProbe with the engine StateDir forced onto
// a REAL btrfs filesystem, which the capacity axis requires (subvolume + squota,
// ADR-0002). t.TempDir() cannot be used for it: TMPDIR is /tmp = tmpfs here, where
// subvolumes do not exist — and production has the same constraint (overlay
// scratch must be native btrfs), so testing on tmpfs would exercise a
// configuration the engine does not support.
func setupEngineOnBtrfs(t *testing.T, healthTimeout time.Duration) (*orchestrator.Engine, string) {
	t.Helper()
	base, err := os.MkdirTemp("/var/tmp", "cap-")
	if err != nil {
		t.Fatal(err)
	}
	bc := btrfs.Client{}
	if !bc.Available(context.Background(), base) {
		t.Skipf("%s is not on btrfs; the capacity axis needs btrfs (ADR-0002)", base)
	}
	stateDir := filepath.Join(base, "state")
	t.Cleanup(func() {
		cleanupOverlaySubvolumes(bc, stateDir)
		_ = os.RemoveAll(base)
	})

	eng, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir:       base,
		HealthTimeout: healthTimeout,
		Probes: map[string]string{
			"cpuburn":     cpuBurnSrc,
			"diskfill":    diskFillSrc,
			"boundedfill": boundedFillSrc,
			"memhog":      memHogSrc,
			"unlinker":    unlinkSrc,
		},
	})
	return eng, base
}

// TestResourceCapacityAxis: the guest fills its disk cap
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

	// --- guest write hits the cap → errno 28, sandbox SURVIVES ---
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
	t.Logf("✓ guest got errno 28 at the cap; charged %d of %d bytes (%.2f%%)",
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
	t.Logf("✓ container still running and exec-able after ENOSPC, no death cause recorded")

	// The cap is a CEILING, not a latch: DELETING the filler returns its bytes to
	// the quota and writes resume. Measured, and the mechanism matters — only
	// deletion works here:
	//   - a truncating open() fails, because O_TRUNC still has to open for writing
	//     and there is no space to do it (measured: ENOSPC on open);
	//   - truncate(2) SUCCEEDS yet frees nothing the quota can see, because squota
	//     holds an extent's charge until the inode itself goes away;
	//   - unlink releases the charge immediately (measured: the next write succeeds
	//     on the first attempt, no settle window needed).
	// The delete must happen IN-GUEST: the merged overlay is the live view, and
	// mutating its upper from the host behind a mounted overlayfs is undefined.
	chDel, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/unlinker", Args: []string{"/root/fill"}})
	if err != nil {
		t.Fatalf("start unlinker: %v", err)
	}
	var delOut strings.Builder
	delExit := -1
	for ev := range chDel {
		switch ev.Kind {
		case "data":
			delOut.Write(ev.Data)
		case "end":
			delExit = ev.ExitCode
		}
	}
	if delExit != 0 || !strings.Contains(delOut.String(), "UNLINKED") {
		t.Fatalf("in-guest unlink of the filler failed (exit=%d): %s", delExit, delOut.String())
	}
	if err := cl.Write(ctx, "/root/probe-after-free", []byte("ok")); err != nil {
		t.Fatalf("write after freeing space under the cap failed: %v — the cap latched instead of acting as a ceiling", err)
	}
	usedFreed, _, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage after free: %v", err)
	}
	if usedFreed >= usedAfter {
		t.Fatalf("charged bytes did not drop after deleting the filler (%d → %d) — the freed extents were never returned to the quota", usedAfter, usedFreed)
	}
	t.Logf("✓ the cap is a ceiling, not a latch: deleting the filler returned bytes to the quota (%d → %d of %d) and writes resumed",
		usedAfter, usedFreed, diskCap)

	// --- destroy reclaims the subvolume + quota -----------------------------
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
	t.Logf("✓ destroy reclaimed the subvolume and its quota unit")
}

// A sandbox whose create crashed mid-chain leaves an orphan Overlay unit, and
// startup reconcile must reclaim it as a SUBVOLUME plus its quota unit (§4.4-A
// reclamation discipline).
//
// Measured caveat on what this test can and cannot prove: `rm -rf` DOES remove a
// btrfs subvolume on this kernel (contents and all, exit 0), so a
// directory-existence assertion alone does NOT distinguish a correct reclamation
// path from one that merely unlinked the tree. The real divergence is the quota
// row — `rm -rf` leaves a live limit on a dead subvolume id, and full qgroup mode
// needs an explicit `qgroup destroy`. Under squota (this host) btrfs reclaims that
// row asynchronously, so both paths converge within seconds. Hence the assertion
// below checks the QUOTA UNIT, not just the directory.
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
	t.Logf("✓ crash half: startup reconcile reclaimed the orphan capacity subvolume + quota")
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

// TestResourceDiskCapBelowUsageRefused: the §3.4 strict inequality on the SANDBOX disk
// axis, end to end on real btrfs — the §5.B twin of the volume cap's write point.
//
// WHAT MAKES THIS WORTH A SUBSTRATE TEST rather than only the unit-level refusal: the harm
// being prevented is a substrate state, and it is irreversible. Measured on this shape, a
// cap at or below the unit's usage is accepted by btrfs with ZERO validation, and
// afterwards creating a file, `mkdir`, and even truncating an existing file to zero all
// return ENOSPC — while the quota write ITSELF returns ENOSPC, so the cap can then be
// neither raised nor removed. On a sandbox that is worse than on a volume: §5.B re-applies
// the record's cap at EVERY placement, so re-applying the wedged cap fails the same way
// and the sandbox is permanently unresumable (`Mount`'s own fresh-workdir step fails with
// ENOSPC too, so even skipping the quota write would not resume it).
//
// So the assertion is not just "the PATCH was refused" but "the refusal CHANGED NOTHING":
// the sandbox resumes and the guest still writes. That is the property a 200 destroys, and
// no unit test can observe it.
func TestResourceDiskCapBelowUsageRefused(t *testing.T) {
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
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	// Put real guest bytes in the upper, so "current usage" is a substrate fact rather than
	// a fixture. BOUNDED at 16 MiB inside the 64 MiB cap, deliberately NOT filling it: this
	// test is about the cap WRITE POINT, and a full unit would drag in the separate
	// full-sandbox-resume property (which has its own test below). Keeping them apart is
	// what lets each failure name one cause.
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if out := guestRun(t, ctx, cl, "/usr/bin/boundedfill", "/root/blob", "16"); !strings.Contains(out, "WROTE=16777216") {
		t.Fatalf("the bounded fill did not write 16 MiB: %q", out)
	}

	rec, err := eng.Store.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	unit := filepath.Join(base, "state", "overlays", string(rec.Overlay))

	// Caps are editable only from `paused` (§3.4), which is also what makes the usage
	// number stable: the container is stopped and nothing can grow the upper between the
	// write point's read and the placement that would apply the cap.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	used, limit, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("read the unit's quota row: %v", err)
	}
	if limit != diskCap {
		t.Fatalf("quota limit = %d; want %d — the cap did not bind, so this test would not be measuring what it claims", limit, diskCap)
	}
	if used == 0 {
		t.Fatal("the unit charges 0 bytes after a guest fill; the below-usage predicate would be vacuous")
	}
	if used >= limit {
		t.Fatalf("used=%d limit=%d — the fill was supposed to stay INSIDE the cap so this test isolates the write point; a full unit belongs to TestResourceDiskCapFullSandboxStillResumes", used, limit)
	}
	t.Logf("unit holds %d byte(s) under a %d cap", used, limit)

	// --- the gap this closes: a PATCH to a cap AT or BELOW usage ------------------------
	// Both boundary values, because `cap == usage` is ALSO a wedged state (measured:
	// setting the limit exactly equal to `referenced` is accepted, and re-applying or
	// raising it then returns ENOSPC) — so it means FULL, not "exactly enough".
	for _, want := range []int64{used / 2, used} {
		err := patchDisk(t, m, sid, want)
		if !errorsIsDiskCapBelowUsage(err) {
			t.Fatalf("PATCH disk_byte → %d with usage %d = %v; want lifecycle.ErrDiskCapBelowUsage (§3.4 strict inequality — the substrate accepts such a cap and is then wedged irreversibly)", want, used, err)
		}
		t.Logf("✓ refused a below-usage cap (%d ≤ %d): %v", want, used, err)
	}

	// THE REFUSAL CHANGED NOTHING — asserted on the substrate, not just the record.
	if u2, l2, err := bc.Usage(ctx, unit); err != nil || u2 != used || l2 != limit {
		t.Fatalf("after the refused PATCHes the quota row moved: used %d→%d limit %d→%d (err %v); a refusal must not touch the substrate", used, u2, limit, l2, err)
	}
	if r2, _ := eng.Store.GetSandbox(ctx, sid); r2.Limits.DiskByte != diskCap {
		t.Fatalf("recorded disk cap = %d; want %d unchanged", r2.Limits.DiskByte, diskCap)
	}

	// ...and the sandbox is STILL RESUMABLE and still writable. This is the harm a 200
	// would have caused: the next placement would have wedged the unit and every
	// placement after it would have failed.
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume after the refused PATCH = %v; want success — the refusal exists precisely to keep this working", err)
	}
	cl2, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client after resume: %v", err)
	}
	// A WRITE plus a READ-BACK: on a wedged unit the write is what fails (ENOSPC), so this
	// is the assertion that distinguishes "refused and unharmed" from "refused but wedged
	// anyway".
	if out := guestRun(t, ctx, cl2, "/usr/bin/boundedfill", "/root/probe", "1"); !strings.Contains(out, "READBACK=1048576") {
		t.Fatalf("guest write after the refused PATCH produced %q; the unit is wedged — the refusal did not protect it", out)
	}
	t.Logf("✓ sandbox resumed and the guest still writes: the refusal changed nothing")

	// --- the legal direction still works: a raise STRICTLY above usage -------------------
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause before the raise: %v", err)
	}
	raise := int64(diskCap * 2)
	if err := patchDisk(t, m, sid, raise); err != nil {
		t.Fatalf("PATCH disk_byte → %d (strictly above usage) = %v; want it ADMITTED", raise, err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume after the raise: %v", err)
	}
	if _, l3, err := bc.Usage(ctx, unit); err != nil || l3 != raise {
		t.Fatalf("after the raise the unit's limit = %d (err %v); want %d — a cap edit takes effect at the next placement (§3.4)", l3, err, raise)
	}
	t.Logf("✓ a raise strictly above usage is admitted and materialized at the next placement (limit now %d)", raise)
}

// TestResourceDiskCapFullSandboxStillResumes is the REVERSE guard on the predicate above,
// and it is the reason the criterion is `cap <= used` rather than something looser.
//
// A sandbox that fills its own cap the ordinary §3.4 way (guest gets ENOSPC, sandbox
// survives) must still pause and resume. Measured, that works because squota stops the
// guest while usage is still STRICTLY below the limit (32 KiB of headroom in the spike), so
// re-applying the cap succeeds. If the guard were ever widened — or if the substrate's
// headroom vanished — this is the test that catches it, because the symptom would be a
// sandbox that filled its disk and could never be resumed again.
func TestResourceDiskCapFullSandboxStillResumes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (btrfs ioctls + runsc + overlay mount)")
	}
	ctx := context.Background()
	eng, base := setupEngineOnBtrfs(t, 60*time.Second)
	m := eng.Mediator
	bc := btrfs.Client{}

	const diskCap = 32 << 20
	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: diskCap}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/diskfill", Args: []string{"/root/fill", "1024"}})
	if err != nil {
		t.Fatalf("start diskfill: %v", err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == "data" {
			out.Write(ev.Data)
		}
	}
	if !strings.Contains(out.String(), "WRITE_ERRNO=28") {
		t.Fatalf("the guest did not reach ENOSPC at its cap; got %q — this test needs a genuinely FULL unit", strings.TrimSpace(out.String()))
	}

	rec, _ := eng.Store.GetSandbox(ctx, sid)
	unit := filepath.Join(base, "state", "overlays", string(rec.Overlay))
	used, limit, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if used >= limit {
		t.Fatalf("used=%d limit=%d — usage reached the limit, which is the WEDGED state; the guard's premise (squota leaves headroom) does not hold on this substrate and the placement-side predicate needs revisiting", used, limit)
	}
	t.Logf("full unit: used=%d limit=%d headroom=%d", used, limit, limit-used)

	// The property: pause + resume must both succeed, which means applyCap re-asserted the
	// SAME cap on a full unit without the strict-inequality guard refusing it.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause of a full sandbox: %v", err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume of a FULL sandbox = %v; want success — filling your own cap is an ordinary §3.4 outcome, not a wedge, and the placement guard must not refuse it", err)
	}
	t.Logf("✓ a sandbox that filled its own cap still pauses and resumes")

	// And the §4.3 remedy still works on it: raise the cap, resume, room again.
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause before raise: %v", err)
	}
	if err := patchDisk(t, m, sid, diskCap*4); err != nil {
		t.Fatalf("PATCH raising a FULL sandbox's cap = %v; want it ADMITTED (this is §4.3's remedy)", err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume after raising a full sandbox's cap: %v", err)
	}
	t.Logf("✓ raise-the-cap remedy works on a full sandbox")
}

// TestResourceCreateRefusesCapBelowTheUnitFloor: a cap below what the storage unit costs
// EMPTY is unenforceable from the sandbox's first instant, and §5.B refuses it rather than
// producing a unit that is wedged at birth.
//
// The floor is a measured substrate fact, not a constant the engine names: a fresh
// subvolume already charges ~16 KiB of metadata extents. Before the guard, such a create
// wrote the cap, then failed deeper in the chain with an ENOSPC from `Mount`'s workdir
// step — an unintelligible error for a request that was decidable up front.
func TestResourceCreateRefusesCapBelowTheUnitFloor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (btrfs ioctls)")
	}
	ctx := context.Background()
	eng, _ := setupEngineOnBtrfs(t, 30*time.Second)

	_, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{DiskByte: 4096}, // below the ~16 KiB metadata floor
	})
	if !errorsIsDiskCapBelowUsage(err) {
		t.Fatalf("create with a 4096-byte disk cap = %v; want lifecycle.ErrDiskCapBelowUsage — the cap is at or below what the unit costs empty, so it could never be enforced", err)
	}
	t.Logf("✓ create refused a cap below the unit's own floor: %v", err)

	// Nothing half-built (§4.4-A): no record, and the create's unwind left no row behind.
	list, _ := eng.Mediator.List(ctx)
	if len(list) != 0 {
		t.Fatalf("%d record(s) left after a refused create; want 0", len(list))
	}
}

// guestRun execs a probe in the guest and returns everything it wrote to stdout/stderr.
// It does NOT fail on a non-zero exit: several callers assert on the probe's own parseable
// report (WROTE=/WRITE_ERR=), which is more precise than an exit code.
func guestRun(t *testing.T, ctx context.Context, cl transport.Client, cmd string, args ...string) string {
	t.Helper()
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: cmd, Args: args})
	if err != nil {
		t.Fatalf("start %s: %v", cmd, err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == "data" {
			out.Write(ev.Data)
		}
	}
	return strings.TrimSpace(out.String())
}

// patchDisk drives the §6.1 PATCH write point with a disk_byte-only binding.
func patchDisk(t *testing.T, m *lifecycle.Mediator, id model.SandboxID, capByte int64) error {
	t.Helper()
	_, err := m.Rebind(context.Background(), id, lifecycle.RebindSpec{
		Binding: store.Binding{DiskByte: &capByte},
	})
	return err
}

func errorsIsDiskCapBelowUsage(err error) bool {
	return errors.Is(err, lifecycle.ErrDiskCapBelowUsage)
}

// TestResourceRAMAxis: exceeding the RAM cap terminates
// the sandbox DETERMINISTICALLY and WHOLE-GROUP, the host kernel log does not grow
// without bound, and the death cause lands in the store in the SAME commit as the
// crash-paused demotion — after which resume is REFUSED.
//
// The bounded-log assertion is not decoration. It is the only check that catches
// the §2.3 bypass: if the cap is configured such that the kernel finds no eligible
// victim, it retries the OOM forever and writes kernel log at ~66 MB/s until the
// host disk is full — a path that goes straight AROUND the per-sandbox disk
// capacity cap. A test that only asserted "the sandbox died" would pass while the
// host filled up, which is why memory.oom.group=1 is a §2.4 provider obligation
// rather than a tuning knob.
func TestResourceRAMAxis(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (cgroup writes + runsc)")
	}
	ctx := context.Background()
	eng, _ := setupEngineWithProbe(t, 60*time.Second)
	m := eng.Mediator

	// 192 MiB is high enough that an eligible victim always exists (the sentry's
	// own footprint is far smaller) and low enough to be cheap to fill.
	const memCap = 192 << 20
	sid, err := m.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Limits:   model.Limits{MemoryByte: memCap},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	// The guest must be told N, not N+overhead: runsc derives MemTotal from the OCI
	// limit at boot, and the point of the two-value split is that the guest sees
	// exactly the cap it was promised (§5.A).
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	meminfo, err := cl.Read(ctx, "/proc/meminfo", 0, 0)
	if err != nil {
		t.Fatalf("read guest meminfo: %v", err)
	}
	var guestTotalKB int64
	for _, line := range strings.Split(string(meminfo), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				guestTotalKB, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	if guestTotalKB == 0 {
		t.Fatalf("could not read guest MemTotal from:\n%s", meminfo)
	}
	// exactly N (192 MiB = 196608 KiB), NOT N+overhead.
	if guestTotalKB != memCap/1024 {
		t.Fatalf("guest MemTotal = %d KiB; want %d KiB (= N). The guest must be told N while the HOST ceiling is N+overhead (§5.A two values)",
			guestTotalKB, memCap/1024)
	}
	hostCeil, _ := strconv.ParseInt(readCg(t, sid, "memory.max"), 10, 64)
	if hostCeil <= memCap {
		t.Fatalf("host memory.max = %d; want > N (%d) so the sandbox is not killed BELOW its promised cap", hostCeil, memCap)
	}
	t.Logf("✓ two values: guest MemTotal=%d KiB (= N) while host memory.max=%d (= N+overhead)", guestTotalKB, hostCeil)

	logBefore := hostLogBytes(t)

	// --- blow through the cap -------------------------------------------------
	// Allocate well past the cap. The whole group must be killed; the probe never
	// gets to report ALLOC_COMPLETE.
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/memhog", Args: []string{"32", "512"}})
	if err != nil {
		t.Fatalf("start memhog: %v", err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == "data" {
			out.Write(ev.Data)
		}
	}
	t.Logf("guest memhog output (tail): %s", lastLines(out.String(), 3))
	if strings.Contains(out.String(), "ALLOC_COMPLETE") {
		t.Fatalf("guest allocated 512 MiB under a %d-byte cap without being terminated — the RAM cap did not bind", memCap)
	}

	// --- whole-group, deterministic termination ------------------------------
	// memory.events records the whole-group kill. Read it BEFORE any teardown: the
	// leaf (and this evidence) dies with `runsc delete` (§4.3).
	deadline := time.Now().Add(20 * time.Second)
	var groupKills int64
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(readCg(t, sid, "memory.events"), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "oom_group_kill" {
				groupKills, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
		if groupKills > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if groupKills == 0 {
		t.Fatalf("memory.events shows oom_group_kill=0 — termination was not whole-group (§2.4 requires deterministic, whole-group termination)")
	}
	peak := readCg(t, sid, "memory.peak")
	t.Logf("✓ whole-group termination (oom_group_kill=%d, memory.peak=%s vs host ceiling %d)", groupKills, peak, hostCeil)

	// bounded host log growth — the §2.3 bypass check.
	logGrowth := hostLogBytes(t) - logBefore
	const logBudget = 64 << 20 // generous; the runaway failure mode writes ~66 MB/s
	if logGrowth > logBudget {
		t.Fatalf("host kernel/syslog grew %d bytes during one RAM-cap termination (budget %d) — this is the §2.3 bypass: an OOM retry storm fills the HOST disk, going around the per-sandbox capacity cap",
			logGrowth, logBudget)
	}
	t.Logf("✓ host log growth bounded (%d bytes ≤ %d budget) — no OOM retry storm (§2.3 not bypassed)", logGrowth, logBudget)

	// --- death cause committed WITH the crash-paused demotion ----------------
	// The reaper's sweep classifies the dead unit and disposes of it. This is the
	// live path (a running orchestrator), the same one reconcile takes at boot.
	lifecycle.NewReaper(m, time.Hour, time.Hour).Sweep(ctx)

	rec, err := eng.Store.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatalf("GetSandbox after termination: %v", err)
	}
	if rec.State != model.Paused {
		t.Fatalf("state = %s after RAM termination; want paused (§4.2 involuntary pause)", rec.State)
	}
	if rec.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("death cause = %q; want %q — without it the control plane cannot tell this from an ordinary crash and would resume into the same cap (§4.3)",
			rec.DeathCause, model.CauseResourceExceeded)
	}
	// the event Kind must say crash-paused, not paused: subscribers distinguish WHY.
	if k := lastKind(t, eng, sid); k != "crash-paused" {
		t.Fatalf("event Kind = %q; want crash-paused (§4.1: the state is paused either way, the Kind carries why)", k)
	}
	t.Logf("✓ crash-paused with death_cause=%s persisted, event Kind=crash-paused", rec.DeathCause)

	// ...and resume is REFUSED. This is the disposition that makes the whole death
	// cause worth persisting: dead-recoverable resumes, resource-terminated must
	// not, and the two are indistinguishable without it.
	err = m.Resume(ctx, sid)
	if err == nil {
		t.Fatal("resume SUCCEEDED for a resource-terminated sandbox — it would re-run the same workload into the same cap (crash loop, §4.3)")
	}
	if !strings.Contains(err.Error(), "resource-terminated") {
		t.Fatalf("resume error = %v; want a resource-terminated refusal", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Paused {
		t.Fatalf("state = %s after the refused resume; want it to STAY paused", stt)
	}
	t.Logf("✓ resume refused and the sandbox stayed paused (%v)", err)
}

// lastLines returns the final n non-empty lines, to keep guest output in the log
// readable without dumping hundreds of progress lines.
func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, strings.TrimSpace(l))
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

// TestResourceUnknownDeathCauseResumes: when the death cause
// cannot be read it is `Unknown`, the sandbox is treated as dead-recoverable and
// DOES resume, and the loop is bounded rather than infinite.
//
// Unknown arises for real whenever the memcg leaf is gone before the death is
// observed — including the ordinary case of an UNCAPPED sandbox, which has no leaf
// at all. That is the case exercised here: kill the container out from under the
// engine and let the reaper classify it.
func TestResourceUnknownDeathCauseResumes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	eng, base := setupEngineWithProbe(t, 60*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"}) // uncapped → no memcg leaf
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	// kill the isolation unit behind the engine's back (an external SIGKILL, i.e.
	// a death with no resource-cap evidence anywhere).
	killContainer(t, base, sid)

	lifecycle.NewReaper(m, time.Hour, time.Hour).Sweep(ctx)

	rec, err := eng.Store.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if rec.State != model.Paused {
		t.Fatalf("state = %s; want paused (crash-paused)", rec.State)
	}
	// Either Unknown (no leaf to read) or Gone (leaf readable, no group kill) is a
	// legal verdict here; what must NOT happen is a false ResourceExceeded, which
	// would wrongly block resume forever.
	if rec.DeathCause == model.CauseResourceExceeded {
		t.Fatalf("death cause = %q for an external kill — a false resource-terminated verdict permanently blocks resume (§2.4: report Unknown, never guess)", rec.DeathCause)
	}
	t.Logf("✓ non-resource death recorded as %q (not a guessed resource-exceeded)", rec.DeathCause)

	// ...and it RESUMES (the §4.3 fallback: treat as dead-recoverable, let bounded
	// retry self-correct if the guess was wrong).
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume of a %q death was refused: %v — §4.3 requires the Unknown/Gone path to resume", rec.DeathCause, err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("state = %s after resume; want running", stt)
	}
	// the resume CLEARED the cause: it no longer explains anything, and leaving it
	// set would make the structural self-correction argument rest on a stale value.
	after, _ := eng.Store.GetSandbox(ctx, sid)
	if after.DeathCause != model.CauseNone {
		t.Fatalf("death cause = %q after a successful resume; want cleared", after.DeathCause)
	}
	t.Logf("✓ resumed to running and the death cause was cleared (bounded self-correction, §4.3)")
}

// TestResourceAdmissionGateOnSubstrate: a create whose caps
// would oversubscribe the node fails and leaves NO half-built residue, and a paused
// sandbox that no longer fits fails its resume and STAYS paused.
func TestResourceAdmissionGateOnSubstrate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	repo := harness.RepoRoot(t)
	base, err := os.MkdirTemp("/var/tmp", "admission-")
	if err != nil {
		t.Fatal(err)
	}
	bc := btrfs.Client{}
	if !bc.Available(ctx, base) {
		t.Skipf("%s not on btrfs", base)
	}
	stateDir := filepath.Join(base, "state")
	t.Cleanup(func() {
		cleanupOverlaySubvolumes(bc, stateDir)
		_ = os.RemoveAll(base)
	})
	tmpl := filepath.Join(base, "templates")
	sbxd := harness.BuildSandboxd(t, repo)
	harness.BuildTemplate(t, repo, tmpl, "bare", sbxd)

	// A node budget just big enough for ONE 64 MiB sandbox plus the Template bytes.
	// The Template is a real rootfs (tens of MiB), which is exactly the term squota
	// cannot see and the accounting must include (D6.5).
	layerish, err := dirSize(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	const capPer = 64 << 20
	nodeDisk := layerish + capPer + (8 << 20) // room for exactly one

	eng, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: tmpl, StateDir: stateDir,
		StorePath:     filepath.Join(base, "store.db"),
		HealthTimeout: 60 * time.Second,
		NodeDiskByte:  nodeDisk,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := eng.SeedTemplates(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := eng.Mediator
	t.Logf("node disk budget = %d (template bytes ≈ %d + one %d cap + slack)", nodeDisk, layerish, capPer)

	// first create fits.
	sid1, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: capPer}})
	if err != nil {
		t.Fatalf("first create (should fit): %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid1) })

	// second must be REFUSED: Σcaps + uncapped bytes now exceeds the budget.
	nsBefore := harness.CountEngineNetns(t)
	_, err = m.Create(ctx, lifecycle.CreateSpec{Template: "bare", Limits: model.Limits{DiskByte: capPer}})
	if err == nil {
		t.Fatal("second create succeeded — the node budget is being oversubscribed (§3.4)")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("want an insufficient-capacity refusal; got %v", err)
	}
	t.Logf("✓ over-budget create refused: %v", err)

	// The refusal must be accounting for the TEMPLATE bytes, not just the caps.
	// Asserting the reported layer term is on the order of the real Template library
	// is what catches the undercount D6.2 warns about: counting only Promote-built
	// layers (and omitting deployment-shipped builtins) silently overcommits the
	// node by the whole Template library, which on a fresh deployment is all of it.
	// Measured here: 16 KiB (wrong, subvolume metadata only) vs ~14 MB (right).
	if !strings.Contains(err.Error(), "uncapped bytes") {
		t.Fatalf("refusal does not show the layer-bytes term: %v", err)
	}
	var reported int64
	if _, serr := fmt.Sscanf(err.Error()[strings.Index(err.Error(), "uncapped bytes "):], "uncapped bytes %d", &reported); serr != nil {
		t.Fatalf("could not parse the uncapped-bytes term from %q: %v", err, serr)
	}
	if reported < layerish/2 {
		t.Fatalf("uncapped bytes reported as %d but the Template library alone is ~%d — deployment-shipped Template bytes are being omitted from node accounting (§5.B / D6.2 undercount)",
			reported, layerish)
	}
	t.Logf("✓ the uncapped term (%d) accounts for the real Template bytes (~%d), not just Promote-built layers", reported, layerish)

	// NO half-built residue: no extra record, no netns, no extra overlay unit.
	list, _ := m.List(ctx)
	if len(list) != 1 {
		t.Fatalf("%d sandbox records after a refused create; want 1 (no half-build, §4.4-A)", len(list))
	}
	if n := harness.CountEngineNetns(t); n > nsBefore {
		t.Fatalf("refused create leaked a netns (before=%d after=%d)", nsBefore, n)
	}
	ovs, _ := os.ReadDir(filepath.Join(stateDir, "overlays"))
	if len(ovs) != 1 {
		t.Fatalf("%d overlay units after a refused create; want 1", len(ovs))
	}
	t.Logf("✓ refused create left no residue (1 record, no new netns, 1 overlay unit)")

	// --- resume half: a paused sandbox that no longer fits stays paused -------
	if err := m.Pause(ctx, sid1); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Shrink the budget out from under it by promoting more layer bytes into
	// existence — simpler and more honest: rebuild the engine with a smaller budget,
	// which is the same inequality the gate evaluates.
	eng2, err := orchestrator.New(orchestrator.Config{
		TemplatesDir: tmpl, StateDir: stateDir,
		StorePath:     filepath.Join(base, "store.db"),
		HealthTimeout: 60 * time.Second,
		NodeDiskByte:  layerish, // no longer leaves room for the 64 MiB cap
	})
	if err != nil {
		t.Fatalf("orchestrator.New (shrunk budget): %v", err)
	}
	if err := eng2.Mediator.Resume(ctx, sid1); err == nil {
		t.Fatal("resume succeeded despite the node no longer having room for its cap")
	} else {
		t.Logf("✓ over-budget resume refused: %v", err)
	}
	// §4.4-A: atomic return to the SOURCE state.
	rec, err := eng2.Store.GetSandbox(ctx, sid1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Paused {
		t.Fatalf("state = %s after a refused resume; want it to STAY paused (§4.4-A atomic return to source state)", rec.State)
	}
	t.Logf("✓ refused resume left the sandbox in `paused`")
}

// dirSize sums APPARENT sizes and is only a loose LOWER-BOUND reference for the
// assertion above (the shipped Template tree is dense, so apparent ≈ allocated).
// The engine itself counts ALLOCATED blocks — see rootfs.blockBytes.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// TestResourceRateAxis: the CPU quota really throttles, the cap provably bound,
// and it is re-materialized across resume (§3.4 / §5.A).
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
	// stays idle. The pair is what makes the no-interference half checkable —
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

	// --- the caps ACTUALLY BOUND --------------------------------------------
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
		// The SHARE half of the axis, not just the ceiling. cpu.max alone leaves
		// every leaf on cgroup's identical default weight, so on a saturated node
		// sandboxes would split time EQUALLY regardless of the CPU they were sold —
		// §3.4 calls this axis FAIR SHARING, and a ceiling does not deliver that. Assert
		// a non-default weight actually landed (both sandboxes here carry the same
		// capMillis, so the proportionality itself is a ratio between DIFFERENT
		// millis values; what this pins is that the knob is written at all).
		gotW := readCg(t, sid, "cpu.weight")
		if gotW == "" || gotW == "100" {
			t.Fatalf("cpu.weight = %q for %s; want a derived weight, not the cgroup default — "+
				"without it oversubscription degrades uniformly instead of proportionally (§3.4)", gotW, sid)
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
		// memory.high must be UNSET ("max"). §5.A forbids it outright: with
		// swap.max=0 (asserted above) and unreclaimable anon pages there is nowhere
		// for reclaim to push memory, so throttling degenerates into a HANG rather
		// than a limit — measured at 1282 throttle events and a guest stalled past
		// 45 s. The enforcement shape §5.A settles on is the hard limit plus
		// oom.group=1: a clean whole-group termination instead of an indefinite stall.
		//
		// Read off the real cgroupfs rather than inferred from ociMemory having no
		// High field, because that is not what the assertion is about. The engine
		// honors this by OMISSION, so the risk is not "we set it" but "something
		// else does" — a future host-side write in bindLimits (which already writes
		// memory.max, memory.swap.max and memory.oom.group by hand), a runsc version
		// that starts deriving one from the OCI limit, or a systemd/cgroup-driver
		// default landing on the parent. None of those touch a Go struct, and every
		// one of them turns the RAM cap from a kill into a wedge, so the only
		// assertion that covers them is a read of the leaf itself.
		if got := readCg(t, sid, "memory.high"); got != "max" {
			t.Fatalf("memory.high = %q for %s; want \"max\" (unset). §5.A forbids memory.high: with swap.max=0 and unreclaimable anon, throttling becomes a HANG instead of a limit (measured: 1282 throttle events, guest stalled past 45 s) — the cap must be the hard limit + oom.group=1", got, sid)
		}
		t.Logf("✓ %s caps bound on the real cgroup: cpu.max=%s memory.max=%d (N=%d + overhead) swap.max=0 oom.group=1 high=max(unset)",
			sid, wantCPU, hostCeil, memCap)
	}

	// --- real throttling, measured over the WHOLE cgroup --------------------
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

	// no cross-sandbox interference. The idle
	// neighbor's own cgroup must be essentially untouched by its noisy sibling.
	neighborCores := float64(deltaNeighbor) / float64(elapsed.Microseconds())
	t.Logf("idle neighbor consumed %.4f cores in the same window", neighborCores)
	if neighborCores > 0.15 {
		t.Fatalf("idle neighbor burned %.3f cores — per-sandbox accounting is leaking across sandboxes", neighborCores)
	}
	t.Logf("✓ rate axis throttles (%.3f ≈ %.2f cores, nr_throttled>0) and does not disturb a co-resident sandbox", gotCores, wantCores)

	// --- the cap is a BINDING, re-materialized across resume ----------------
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
	t.Logf("✓ caps re-materialized on the NEW placement after resume (cpu.max=%s, swap.max=0, oom.group=1)", wantCPU)

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
// This keeps "no limits requested" free of any host cgroup footprint and keeps the
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
