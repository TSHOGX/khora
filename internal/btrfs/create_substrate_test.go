//go:build substrate

package btrfs

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentMissingSubvolumeOnBtrfs(t *testing.T) {
	path := filepath.Join(btrfsScratch(t), "unit")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	c := Client{Run: func(ctx context.Context, args []string) ([]byte, error) {
		if args[1] == "subvolume" && args[2] == "create" {
			mu.Lock()
			arrived++
			if arrived == 2 {
				close(gate)
			}
			mu.Unlock()
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	}}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- c.CreateSubvolume(ctx, path) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent create: %v", err)
		}
	}
	if !c.IsSubvolume(ctx, path) {
		t.Fatal("target is not a subvolume")
	}
}
