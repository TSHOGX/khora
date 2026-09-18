package btrfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestSyncScopesWaitAndSharesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls [][]string
	c := Client{Run: func(got context.Context, args []string) ([]byte, error) {
		if got != ctx {
			t.Fatal("command received a different budget")
		}
		calls = append(calls, args)
		return nil, nil
	}}
	if err := c.Sync(ctx, "/mount", nil); err != nil || len(calls) != 0 {
		t.Fatal("empty sync executed commands")
	}
	if err := c.Sync(ctx, "/mount", []uint64{42, 43}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"btrfs", "filesystem", "sync", "/mount"}, {"btrfs", "subvolume", "sync", "/mount", "42", "43"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls: %v", calls)
	}
}

func TestCommandCancellationPreservesDeadline(t *testing.T) {
	if os.Getenv("KHORA_BTRFS_BLOCK_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	t.Setenv("KHORA_BTRFS_BLOCK_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// A real child exercises CommandContext, rather than an injected cancellation result.
	c := Client{Bin: os.Args[0]}
	_, err := c.run(ctx, "-test.run=^TestCommandCancellationPreservesDeadline$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("missing child exit: %v", err)
	}
}
