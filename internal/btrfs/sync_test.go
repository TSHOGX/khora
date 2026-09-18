package btrfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

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
