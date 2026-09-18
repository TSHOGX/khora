package volume

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/btrfs"
)

func recordedIDs(b *LocalBtrfs) []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]uint64, 0, len(b.deletedRows))
	for id := range b.deletedRows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func reclaimBackend(t *testing.T, run btrfs.Runner, ids ...uint64) *LocalBtrfs {
	t.Helper()
	b, err := NewLocalBtrfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.Btrfs.Run = run
	for _, id := range ids {
		b.recordDeletedRow(id)
	}
	return b
}

const rowHeader = "qgroupid referenced exclusive max_referenced max_exclusive path\n"

func TestReclaimClassifiesBatchWithoutWaiting(t *testing.T) {
	reads := 0
	var destroyed []string
	b := reclaimBackend(t, func(_ context.Context, args []string) ([]byte, error) {
		if args[1] == "qgroup" && args[2] == "show" {
			reads++
			return []byte(rowHeader + "0/1 0 0 none none live\n0/3 0 0 none none <stale>\n0/4 0 0 none none <under deletion>\n0/5 0 0 none none <squota space holder>\n0/99 0 0 none none <stale>\n"), nil
		}
		if args[1] == "qgroup" && args[2] == "destroy" {
			destroyed = append(destroyed, args[3])
			return nil, nil
		}
		t.Errorf("unexpected command: %v", args)
		return nil, errors.New("unexpected command")
	}, 1, 2, 3, 4, 5)
	n, err := b.Reclaim(context.Background())
	if err != nil || n != 3 || reads != 1 || !reflect.DeepEqual(destroyed, []string{"0/3"}) || !reflect.DeepEqual(recordedIDs(b), []uint64{4, 5}) {
		t.Fatalf("n=%d err=%v reads=%d destroyed=%v pending=%v", n, err, reads, destroyed, recordedIDs(b))
	}
}

func TestReclaimPartialFailureKeepsClassAndOtherProgress(t *testing.T) {
	denied := errors.New("delete failed")
	b := reclaimBackend(t, func(_ context.Context, args []string) ([]byte, error) {
		if args[2] == "show" {
			return []byte(rowHeader + "0/1 0 0 none none <stale>\n0/2 0 0 none none <stale>\n0/3 0\n"), nil
		}
		if args[3] == "0/1" {
			return nil, denied
		}
		return nil, nil
	}, 1, 2, 3)
	n, err := b.Reclaim(context.Background())
	if n != 1 || !errors.Is(err, denied) || strings.Contains(err.Error(), "\n") || !reflect.DeepEqual(recordedIDs(b), []uint64{1, 3}) {
		t.Fatalf("n=%d err=%v pending=%v", n, err, recordedIDs(b))
	}
}

func TestReclaimReadFailureAndTimeoutRetainWork(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			readErr := errors.New("read failed")
			b := reclaimBackend(t, func(ctx context.Context, _ []string) ([]byte, error) {
				if timeout {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, readErr
			}, 42)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := b.Reclaim(ctx)
			want := readErr
			if timeout {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || len(recordedIDs(b)) != 1 {
				t.Fatalf("err=%v pending=%v", err, recordedIDs(b))
			}
			b.Btrfs.Run = func(context.Context, []string) ([]byte, error) { return []byte(rowHeader), nil }
			if n, err := b.Reclaim(context.Background()); err != nil || n != 1 {
				t.Fatalf("retry: %d %v", n, err)
			}
		})
	}
}

func TestReclaimSnapshotDoesNotForgetNewDeletion(t *testing.T) {
	var b *LocalBtrfs
	b = reclaimBackend(t, func(context.Context, []string) ([]byte, error) {
		b.recordDeletedRow(42)
		return []byte(rowHeader), nil
	}, 42)
	if n, err := b.Reclaim(context.Background()); err != nil || n != 0 || len(recordedIDs(b)) != 1 {
		t.Fatalf("n=%d err=%v pending=%v", n, err, recordedIDs(b))
	}
}

func TestReclaimGateIsCancelableAndReleased(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	b := reclaimBackend(t, func(ctx context.Context, _ []string) ([]byte, error) {
		if first {
			first = false
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(rowHeader), nil
	}, 42)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := b.Reclaim(ctx); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	other, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := b.Reclaim(other)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("gate: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reclaim(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimTimeoutPreservesPartialProgress(t *testing.T) {
	b := reclaimBackend(t, func(ctx context.Context, args []string) ([]byte, error) {
		if args[2] == "show" {
			return []byte(rowHeader + "0/1 0 0 none none <stale>\n0/2 0 0 none none <stale>\n"), nil
		}
		if args[3] == "0/2" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	n, err := b.Reclaim(ctx)
	if n != 1 || !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(recordedIDs(b), []uint64{2}) {
		t.Fatalf("n=%d err=%v pending=%v", n, err, recordedIDs(b))
	}
}

func TestEmptyReclaimDoesNoIOAndCanceledPassKeepsWork(t *testing.T) {
	b := reclaimBackend(t, func(context.Context, []string) ([]byte, error) { t.Fatal("unexpected I/O"); return nil, nil })
	if n, err := b.Reclaim(context.Background()); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}
	b.recordDeletedRow(42)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Reclaim(ctx); !errors.Is(err, context.Canceled) || len(recordedIDs(b)) != 1 {
		t.Fatalf("cancel: %v pending=%v", err, recordedIDs(b))
	}
}
