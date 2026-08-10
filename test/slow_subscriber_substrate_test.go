//go:build substrate

// C4 at L4: a never-reading subscriber must NOT freeze the real
// guest process on runsc (D4.3 invariant 1 — source never blocks on a slow
// subscriber; the slow one is dropped). The mechanism is also proven as pure
// logic under -race in cmd/sandboxd/process_test.go; this pins it end-to-end on
// the real substrate. Requires root.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 180s ./test/ -run C4
package test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"warren/internal/transport"
)

func TestC4SlowSubscriberDoesNotFreezeSource(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	ctx := context.Background()

	// A fast flood using ONLY dash builtins (no coreutils): an incrementing
	// counter echoed in a tight loop. Output far exceeds the ring + any window,
	// so a stuck subscriber's window is guaranteed to overflow.
	ch, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", "i=0; while :; do i=$((i+1)); echo LINE$i; done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var pid int
	for ev := range ch {
		if ev.Kind == "start" {
			pid = ev.PID
			break
		}
	}
	if pid <= 0 {
		t.Fatal("no start pid")
	}
	// drain the Start stream in the background (it is just another subscriber).
	go func() {
		for range ch {
		}
	}()
	// kill the infinite loop at the end (group signal, SIGKILL).
	defer func() { _ = cl.SendSignal(context.Background(), pid, 9) }()

	// The STUCK subscriber: Connect, then never read. Its per-subscriber window
	// will overflow and it will be dropped — the source must not care.
	stuckCtx, stuckCancel := context.WithCancel(ctx)
	defer stuckCancel()
	stuck, err := cl.Connect(stuckCtx, pid)
	if err != nil {
		t.Fatalf("Connect stuck: %v", err)
	}
	_ = stuck // deliberately never drained

	// Sample the SOURCE's progress via fresh Connects: each replays the ring's
	// recent tail (a brief read-then-close never overflows), so the max LINE it
	// sees tracks how far the guest process has advanced. If the stuck
	// subscriber had frozen the source (backpressure to the guest write()), the
	// ring tail would stall. It must keep advancing.
	t1 := peekMaxLine(t, cl, pid)
	time.Sleep(1500 * time.Millisecond)
	t2 := peekMaxLine(t, cl, pid)
	t.Logf("source progress: t1=%d t2=%d (never-reading subscriber attached)", t1, t2)
	if t1 <= 0 {
		t.Fatal("no output at all — source may be frozen")
	}
	if t2 <= t1 {
		t.Fatalf("ring tail did not advance (t1=%d t2=%d) — slow subscriber froze the source", t1, t2)
	}

	// The process is still live (source advanced while the never-reading
	// subscriber was attached and being dropped).
	pids, err := cl.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsPid(pids, pid) {
		t.Fatalf("flood process pid %d not live after fault-inject; pids=%v", pid, pids)
	}
}

// peekMaxLine does a fresh Connect, reads frames for a short window (ring replay
// + a little live), returns the highest LINE<n> seen, and closes. Short enough
// that this peeker never overflows its own window.
func peekMaxLine(t *testing.T, cl transport.Client, pid int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	ch, err := cl.Connect(ctx, pid)
	if err != nil {
		t.Fatalf("peek Connect: %v", err)
	}
	max := 0
	for ev := range ch {
		if ev.Kind == "data" {
			if n := maxLineIn(ev.Data); n > max {
				max = n
			}
		}
	}
	return max
}

// maxLineIn scans a data chunk for LINE<n> tokens and returns the largest n.
func maxLineIn(b []byte) int {
	max := 0
	for _, tok := range strings.Fields(string(b)) {
		if strings.HasPrefix(tok, "LINE") {
			if n, err := strconv.Atoi(strings.TrimPrefix(tok, "LINE")); err == nil && n > max {
				max = n
			}
		}
	}
	return max
}

func containsPid(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}
