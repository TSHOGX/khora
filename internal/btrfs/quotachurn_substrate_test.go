//go:build substrate && quotachurn

// L2 for quota-row reclamation across churn, against REAL btrfs. Requires root.
//
// SEPARATELY TAGGED, for cost, following the `drainrace` precedent — and the cost here
// is not this test's own work but the substrate's. Seven of its steps end in a `--sync`
// (`qgroup show --sync`, `subvolume sync`), each of which blocks until the kernel has
// settled the qgroup tree for the WHOLE filesystem. On a shared dev box that tree is
// not ours: measured on `sbx`, /var/tmp carries ~8800 qgroup rows belonging to docker,
// k8s and scon, and this one test swings between 3.5 s and 26.5 s depending only on how
// much those tenants are churning concurrently. It was 87% of internal/btrfs's runtime
// and the single largest source of variance in the routine gate.
//
// What it pins does not change per commit — squota's asynchronous reclamation is a
// substrate property, not engine behaviour — so it belongs on a deliberate run:
//
//	sudo env "PATH=$PATH" "HOME=$HOME" \
//	  go test -tags 'substrate quotachurn' -count=1 ./internal/btrfs/
//
// It keeps `substrate` as well as its own tag because it borrows btrfsScratch from
// btrfs_substrate_test.go, which is behind that tag (the documentation: keep both).
package btrfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Quota rows must not ACCUMULATE across sandbox churn — one leaked row per
// destroyed sandbox would grow without bound, and a reused subvolume id could
// inherit a stale limit.
//
// The property is "no accumulation", NOT "the row vanishes the instant delete
// returns", and the difference is a measured substrate fact rather than a
// technicality: under squota (this substrate, the recorded design) btrfs owns the row and
// reclaims it ASYNCHRONOUSLY — immediately after delete it is still listed as
// `<under deletion>` / `<squota space holder>`, and an explicit `qgroup destroy`
// returns ENOENT because the row is not the caller's to remove. Asserting instant
// removal would therefore encode a false expectation and fail on a correct
// system. So this drives several full cycles and asserts the filesystem's qgroup
// count returns to where it started.
func TestQuotaRowsDoNotAccumulateAcrossChurn(t *testing.T) {
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
