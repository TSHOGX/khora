//go:build substrate

// L2 for the internal/btrfs leaf package against REAL btrfs. Requires root
// (subvolume + qgroup ioctls). The engine scratch fs is btrfs in squota mode
// (ADR-0002 / rootfs-overlay D6.2).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 ./internal/btrfs/
package btrfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// btrfsScratch makes a scratch dir on a real btrfs filesystem. t.TempDir() is
// NOT usable: TMPDIR is /tmp = tmpfs here, where subvolumes cannot exist. The
// engine has the same constraint in production (ADR-0002: overlay scratch must be
// on native btrfs), so testing on tmpfs would test a configuration the engine
// does not support.
func btrfsScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "btrfs-l2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := Client{}
		ctx := context.Background()
		// remove any subvolumes we made before the plain RemoveAll (a subvolume
		// cannot be rmdir'd).
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if c.IsSubvolume(ctx, p) {
				_ = os.RemoveAll(p) // contents first
				_ = c.DeleteSubvolume(ctx, p)
			}
		}
		_ = os.RemoveAll(dir)
	})
	c := Client{}
	if !c.Available(context.Background(), dir) {
		t.Skipf("%s is not on btrfs; the capacity axis needs btrfs (ADR-0002)", dir)
	}
	return dir
}

func TestSubvolumeLifecycleAndQuota(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (btrfs subvolume + qgroup ioctls)")
	}
	ctx := context.Background()
	c := Client{}
	root := btrfsScratch(t)
	unit := filepath.Join(root, "unit")

	if err := c.CreateSubvolume(ctx, unit); err != nil {
		t.Fatalf("CreateSubvolume: %v", err)
	}
	if !c.IsSubvolume(ctx, unit) {
		t.Fatal("IsSubvolume=false right after CreateSubvolume")
	}
	// idempotent: reclamation and placement paths both retry, so a second create
	// must be a no-op rather than an error (§4.4-A).
	if err := c.CreateSubvolume(ctx, unit); err != nil {
		t.Fatalf("CreateSubvolume not idempotent: %v", err)
	}

	const cap = 64 << 20
	if err := c.SetQuotaLimit(ctx, unit, cap); err != nil {
		t.Fatalf("SetQuotaLimit: %v", err)
	}
	used, limit, err := c.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if limit != cap {
		t.Fatalf("limit = %d; want %d (the cap did not bind)", limit, cap)
	}
	t.Logf("✓ cap set: used=%d limit=%d", used, limit)

	// --- the cap is a HARD ceiling: writing past it fails with ENOSPC ---------
	// dd is the host-side check; the guest-side ENOSPC through the whole
	// gofer/sentry chain is asserted in the M6 capacity acceptance.
	out, derr := exec.Command("dd", "if=/dev/zero", "of="+filepath.Join(unit, "big"),
		"bs=1M", "count=100", "status=none").CombinedOutput()
	if derr == nil {
		t.Fatal("wrote 100 MiB into a 64 MiB-capped subvolume without error — the cap is not enforced")
	}
	if !strings.Contains(string(out), "No space left") {
		t.Fatalf("write failed but not with ENOSPC: %v: %s", derr, out)
	}
	used, _, err = c.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("Usage after fill: %v", err)
	}
	if used > cap {
		t.Fatalf("charged %d bytes against a %d cap — the ceiling was exceeded", used, cap)
	}
	// precision: it should have filled nearly to the cap, not stopped early.
	if used < cap*90/100 {
		t.Fatalf("only %d of %d bytes charged; the cap is far too conservative", used, cap)
	}
	t.Logf("✓ hard ENOSPC at the cap: charged %d of %d bytes (%.2f%%)", used, cap, 100*float64(used)/float64(cap))

	// --- removing the cap ----------------------------------------------------
	if err := c.SetQuotaLimit(ctx, unit, 0); err != nil {
		t.Fatalf("clear limit: %v", err)
	}
	if _, limit, _ := c.Usage(ctx, unit); limit != 0 {
		t.Fatalf("limit = %d after clearing; want 0", limit)
	}
	t.Logf("✓ cap cleared (record stays authoritative)")

	// --- delete reclaims the subvolume ---------------------------------------
	id, err := c.subvolumeID(ctx, unit)
	if err != nil {
		t.Fatalf("subvolumeID: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(unit, "big")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSubvolume(ctx, unit); err != nil {
		t.Fatalf("DeleteSubvolume: %v", err)
	}
	if c.IsSubvolume(ctx, unit) {
		t.Fatal("subvolume still present after DeleteSubvolume")
	}
	t.Logf("✓ delete reclaimed the subvolume (id 0/%d)", id)

	// idempotent delete: absent path, and a plain directory, are both no-ops.
	if err := c.DeleteSubvolume(ctx, unit); err != nil {
		t.Fatalf("DeleteSubvolume on an absent path should be a no-op: %v", err)
	}
	plain := filepath.Join(root, "plaindir")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSubvolume(ctx, plain); err != nil {
		t.Fatalf("DeleteSubvolume on a plain dir should be a no-op: %v", err)
	}
	if _, err := os.Stat(plain); err != nil {
		t.Fatal("DeleteSubvolume removed a plain directory it does not own")
	}
	t.Logf("✓ delete is idempotent and leaves non-subvolumes alone")
}

// Quota rows must not ACCUMULATE across sandbox churn — one leaked row per
// destroyed sandbox would grow without bound, and a reused subvolume id could
// inherit a stale limit.
//
// The property is "no accumulation", NOT "the row vanishes the instant delete
// returns", and the difference is a measured substrate fact rather than a
// technicality: under squota (this substrate, ADR-0002) btrfs owns the row and
// reclaims it ASYNCHRONOUSLY — immediately after delete it is still listed as
// `<under deletion>` / `<squota space holder>`, and an explicit `qgroup destroy`
// returns ENOENT because the row is not the caller's to remove. Asserting instant
// removal would therefore encode a false expectation and fail on a correct
// system. So this drives several full cycles and asserts the filesystem's qgroup
// count returns to where it started.
func TestQuotaRowsDoNotAccumulateAcrossChurn(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	c := Client{}
	root := btrfsScratch(t)
	mnt, err := c.mountPoint(ctx, root)
	if err != nil {
		t.Fatalf("mountPoint: %v", err)
	}
	countRows := func() int {
		out, _ := exec.Command("btrfs", "qgroup", "show", "-re", "--raw", "--sync", mnt).CombinedOutput()
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "0/") {
				n++
			}
		}
		return n
	}

	before := countRows()
	const cycles = 5
	ids := make(map[uint64]bool, cycles)
	for i := 0; i < cycles; i++ {
		unit := filepath.Join(root, "churn")
		if err := c.CreateSubvolume(ctx, unit); err != nil {
			t.Fatalf("cycle %d create: %v", i, err)
		}
		id, err := c.subvolumeID(ctx, unit)
		if err != nil {
			t.Fatalf("cycle %d id: %v", i, err)
		}
		if ids[id] {
			t.Fatalf("cycle %d reused subvolume id %d — a stale limit could be inherited", i, id)
		}
		ids[id] = true
		if err := c.SetQuotaLimit(ctx, unit, 32<<20); err != nil {
			t.Fatalf("cycle %d limit: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(unit, "f"), make([]byte, 4<<20), 0o644); err != nil {
			t.Fatalf("cycle %d write: %v", i, err)
		}
		if err := os.RemoveAll(filepath.Join(unit, "f")); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteSubvolume(ctx, unit); err != nil {
			t.Fatalf("cycle %d delete: %v", i, err)
		}
	}
	// let the asynchronous squota reclamation settle before counting.
	_, _ = exec.Command("btrfs", "subvolume", "sync", mnt).CombinedOutput()
	after := countRows()
	if after > before {
		out, _ := exec.Command("btrfs", "qgroup", "show", "-re", "--raw", "--sync", mnt).CombinedOutput()
		t.Fatalf("qgroup rows grew from %d to %d across %d cycles (quota residue accumulates):\n%s",
			before, after, cycles, out)
	}
	t.Logf("✓ %d create/fill/delete cycles left the qgroup row count at %d (was %d) — no quota residue", cycles, after, before)
}

// A plain directory sitting where a subvolume belongs must be REPORTED, not
// silently accepted: accepting it would leave the sandbox running uncapped while
// the record claims a cap.
func TestCreateSubvolumeRefusesPlainDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	c := Client{}
	root := btrfsScratch(t)
	p := filepath.Join(root, "occupied")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateSubvolume(ctx, p); err == nil {
		t.Fatal("CreateSubvolume accepted a pre-existing plain directory — a cap set on it would enforce nothing")
	}
}
