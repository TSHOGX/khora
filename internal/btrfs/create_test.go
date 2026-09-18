package btrfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentCreateSubvolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
			return nil, os.Mkdir(path, 0755)
		}
		return nil, nil
	}}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- c.CreateSubvolume(ctx, path) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Errorf("create: %v", err)
		}
	}
}

func TestCreateRechecksDirectoryAfterInitialProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := Client{Run: func(context.Context, []string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("initial probe raced another creator")
		}
		return nil, nil
	}}
	if err := c.CreateSubvolume(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestCreateDoesNotAcceptPlainDirectoryOrCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("not a subvolume")
	c := Client{Run: func(context.Context, []string) ([]byte, error) { return nil, denied }}
	if err := c.CreateSubvolume(context.Background(), path); err == nil {
		t.Fatal("accepted ordinary directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.Run = func(context.Context, []string) ([]byte, error) { cancel(); return nil, nil }
	if err := c.CreateSubvolume(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe: %v", err)
	}
}
