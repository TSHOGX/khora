package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWaitForLoopsHonorsContextDeadline(t *testing.T) {
	var loops sync.WaitGroup
	loops.Add(1)
	t.Cleanup(loops.Done)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := waitForLoops(ctx, &loops); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForLoops error = %v, want deadline exceeded", err)
	}
}

func TestWaitForLoopsReturnsWhenComplete(t *testing.T) {
	var loops sync.WaitGroup
	if err := waitForLoops(context.Background(), &loops); err != nil {
		t.Fatalf("waitForLoops: %v", err)
	}
}
