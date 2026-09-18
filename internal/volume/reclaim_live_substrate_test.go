//go:build substrate

package volume

import (
	"context"
	"fmt"
	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/test/substratereq"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimFailedDeleteLeavesLiveUnitUntouched(t *testing.T) {
	b, err := NewLocalBtrfs(reclaimScratch(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := b.CreateVolume(ctx, "vol_review", 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.DestroyVolume(ctx, "vol_review"); err != nil {
			t.Error(err)
		}
	}()
	b.Btrfs = btrfs.Client{Run: func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 2 && args[1] == "subvolume" && args[2] == "delete" {
			return nil, fmt.Errorf("injected delete failure")
		}
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%w: %s", err, out)
		}
		return out, nil
	}}
	if err := b.DestroyVolume(ctx, "vol_review"); err == nil {
		t.Fatal("fault did not fire")
	}
	b.Btrfs = btrfs.Client{}
	ids := recordedIDs(b)
	if len(ids) != 1 {
		t.Fatalf("pending rows: %v", ids)
	}
	mnt, err := b.Btrfs.MountPoint(ctx, b.VolumesDir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := b.Btrfs.QuotaRows(ctx, mnt, ids)
	if err != nil || rows[ids[0]] != btrfs.RowLive {
		t.Fatalf("expected live row: %v %v", rows, err)
	}
	limited, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = b.Reclaim(limited)
	if err != nil {
		t.Errorf("live row prevented reclamation pass: %v", err)
	}
	if !b.Btrfs.IsSubvolume(ctx, filepath.Join(b.VolumesDir, "vol_review")) {
		t.Fatal("reclaim destroyed live unit")
	}
	if pending := recordedIDs(b); len(pending) != 0 {
		t.Errorf("live-row branch not reached; pending=%v", pending)
	}
}

func reclaimScratch(t *testing.T) string {
	t.Helper()
	substratereq.Root(t)
	dir, err := os.MkdirTemp("/var/tmp", "khora-reclaim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Error(err)
		}
	})
	substratereq.Btrfs(t, dir)
	return dir
}

func TestDeletionRegistersAfterAttemptAndRetainsConcurrentWork(t *testing.T) {
	b, err := NewLocalBtrfs(reclaimScratch(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.CreateVolume(ctx, "vol_interleaved", 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		b.Btrfs = btrfs.Client{}
		if err := b.DestroyVolume(context.Background(), "vol_interleaved"); err != nil {
			t.Error(err)
		}
	}()
	id, err := b.Btrfs.SubvolumeID(ctx, filepath.Join(b.VolumesDir, "vol_interleaved"))
	if err != nil {
		t.Fatal(err)
	}
	b.recordDeletedRow(id)
	entered := make(chan struct{})
	release := make(chan struct{})
	b.Btrfs.Run = func(ctx context.Context, args []string) ([]byte, error) {
		if args[1] == "subvolume" && args[2] == "delete" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("injected deletion failure")
		}
		return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	}
	done := make(chan error, 1)
	go func() { done <- b.DestroyVolume(ctx, "vol_interleaved") }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	n, reclaimErr := b.Reclaim(ctx)
	close(release)
	deletionErr := <-done
	if reclaimErr != nil || n != 1 {
		t.Fatalf("old live record: %d %v", n, reclaimErr)
	}
	if deletionErr == nil || len(recordedIDs(b)) != 1 {
		t.Fatalf("new deletion lost: %v pending=%v", deletionErr, recordedIDs(b))
	}
}
