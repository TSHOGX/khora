//go:build substrate && quotachurn

// Quota churn acceptance on native btrfs. Synchronization is bounded and scoped
// to fixture IDs; unrelated filesystem activity does not determine the verdict.
package btrfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Deleted fixture units must not leave quota rows after their cleaner completes.
func TestQuotaRowsDoNotAccumulateAcrossChurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := Client{}
	root := btrfsScratch(t)
	mnt, err := c.mountPoint(ctx, root)
	if err != nil {
		t.Fatalf("mountPoint: %v", err)
	}
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
	if _, err := c.run(ctx, "filesystem", "sync", mnt); err != nil {
		t.Fatal(err)
	}
	args := []string{"subvolume", "sync", mnt}
	for id := range ids {
		args = append(args, fmt.Sprint(id))
	}
	if _, err := c.run(ctx, args...); err != nil {
		t.Fatal(err)
	}
	tracked := make([]uint64, 0, len(ids))
	for id := range ids {
		tracked = append(tracked, id)
	}
	rows, err := c.QuotaRows(ctx, mnt, tracked)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range tracked {
		if rows[id] != RowAbsent {
			t.Errorf("fixture quota row %d remains: %s", id, rows[id])
		}
	}
}
