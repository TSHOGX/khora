//go:build substrate

// L2 for the internal/btrfs leaf package against REAL btrfs. Requires root
// (subvolume + qgroup ioctls). The engine scratch fs is btrfs in squota mode
// (the root-required deployment / btrfs quota materialization).
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

	"github.com/TSHOGX/khora/test/substratereq"
)

// TestMain states this package's substrate preconditions ONCE, and FAILS rather than
// skipping when they are absent: `ok` from a substrate package must mean the substrate
// ran, and it used to be indistinguishable from "every test skipped" (measured: this
// package printed `ok` in 0.007s having run nothing). One dev box, root available —
// a missing precondition is a misconfigured machine, so it is a finding.
func TestMain(m *testing.M) {
	substratereq.Root(substratereq.Main)
	substratereq.Bin(substratereq.Main, "btrfs")
	os.Exit(m.Run())
}

// btrfsScratch makes a scratch dir on a real btrfs filesystem. t.TempDir() is
// NOT usable: TMPDIR is /tmp = tmpfs here, where subvolumes cannot exist. The
// engine has the same constraint in production (the root-required deployment: overlay scratch must be
// on native btrfs), so testing on tmpfs would test a configuration the engine
// does not support.
func btrfsScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "btrfs-l2-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup REPORTS its own failures. Reclamation is atomic transition rollback's own subject here, so
	// a scratch dir that silently survives the run is not a cosmetic annoyance: it
	// is this suite losing the ability to tell "the engine leaks" from "my fixtures
	// leak" on the dev box. Cleanup runs after the real assertions, so t.Errorf here
	// can only add information.
	t.Cleanup(func() {
		c := Client{}
		ctx := context.Background()
		// Remove any subvolumes we made before the plain RemoveAll — os.RemoveAll
		// CANNOT delete a subvolume (rmdir(2) refuses one), which test/
		// resource_substrate_test.go asserts about the engine's own reclaim path.
		//
		// ONE level of walking is enough TODAY, and the reason is that nothing in this
		// package's tests nests: every subvolume is created directly under the scratch
		// root (root/unit, root/churn, root/occupied), and btrfs.Client itself creates
		// exactly the one path it is handed. Should that change, the RemoveAll
		// verification below is the backstop — a nested subvolume makes RemoveAll(dir)
		// fail, and the check reports the surviving path instead of swallowing it.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("cleanup: cannot list scratch dir %s, subvolumes may leak: %v", dir, err)
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if !c.IsSubvolume(ctx, p) {
				continue
			}
			// contents first; the error is EXPECTED (this removes what is inside the
			// subvolume but cannot rmdir the subvolume root), so it stays ignored — the
			// DeleteSubvolume below is the call whose failure means a real leak.
			_ = os.RemoveAll(p)
			if err := c.DeleteSubvolume(ctx, p); err != nil {
				t.Errorf("cleanup: DeleteSubvolume(%s) failed, quota unit leaked: %v", p, err)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("cleanup: RemoveAll(%s): %v", dir, err)
		}
		// Verify, do not assume: a subvolume RemoveAll could not delete leaves the dir
		// in place with no error worth trusting. Name the path so it can be cleaned by
		// hand (`btrfs subvolume delete`).
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("cleanup: scratch dir SURVIVED at %s (stat err=%v) — leaked on the dev box; "+
				"a leftover subvolume cannot be removed by RemoveAll, delete it with `btrfs subvolume delete`", dir, err)
		}
	})
	c := Client{}
	mode, err := c.QuotaStatus(context.Background(), dir)
	substratereq.Squota(t, dir, mode == QuotaSimple, err)
	return dir
}

func TestSubvolumeLifecycleAndQuota(t *testing.T) {
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
	// must be a no-op rather than an error (atomic transition rollback).
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
	// gofer/sentry chain is asserted in test/resource_substrate_test.go.
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

// A plain directory sitting where a subvolume belongs must be REPORTED, not
// silently accepted: accepting it would leave the sandbox running uncapped while
// the record claims a cap.
func TestCreateSubvolumeRefusesPlainDirectory(t *testing.T) {
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
