//go:build substrate

package volume

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/test/substratereq"
)

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

func TestReclaimTimeoutRetainsRecordedRows(t *testing.T) {
	b := &LocalBtrfs{VolumesDir: reclaimScratch(t), deletedRows: map[uint64]bool{42: true}}
	b.Btrfs = btrfs.Client{Run: func(ctx context.Context, args []string) ([]byte, error) {
		if args[1] == "subvolume" && args[2] == "sync" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := b.ReclaimQuotaRows(ctx, []uint64{42}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reclaim: %v", err)
	}
	if len(b.DeletedRowIDs(context.Background())) != 1 {
		t.Fatal("timeout forgot pending row")
	}
}

func TestReclaimHolderIsPendingAndUnreadableIsFailure(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		b := &LocalBtrfs{VolumesDir: reclaimScratch(t), deletedRows: map[uint64]bool{42: true}}
		b.Btrfs = btrfs.Client{Run: func(_ context.Context, args []string) ([]byte, error) {
			if args[1] == "qgroup" && args[2] == "show" {
				if unreadable {
					return nil, errors.New("unreadable")
				}
				return []byte("qgroupid referenced exclusive max_referenced max_exclusive path\n0/42 1 1 none none <squota space holder>\n"), nil
			}
			if strings.Contains(strings.Join(args, " "), "qgroup destroy") {
				t.Fatal("destroyed a held row")
			}
			return nil, nil
		}}
		_, err := b.ReclaimQuotaRows(context.Background(), []uint64{42})
		if (err != nil) != unreadable {
			t.Fatalf("unreadable=%t: %v", unreadable, err)
		}
		if len(b.DeletedRowIDs(context.Background())) != 1 {
			t.Fatal("forgot unreclaimed row")
		}
	}
}
