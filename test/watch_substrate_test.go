//go:build substrate

package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/transport"
)

func TestPlacementWatchDeliversRecursiveChange(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	watcher, ok := cl.(transport.WatchClient)
	if !ok {
		t.Fatal("transport client does not expose Watch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ch, err := watcher.Watch(ctx, "/tmp", true, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := cl.Write(ctx, "/tmp/watch-marker", []byte("watch")); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Stat(ctx, "/tmp/watch-marker"); err != nil {
		t.Fatalf("marker stat after write: %v", err)
	}
	for ev := range ch {
		t.Logf("watch event: %+v", ev)
		if ev.Operation == "create" && strings.HasSuffix(ev.Path, "/tmp/watch-marker") {
			return
		}
	}
	t.Fatal("watch stream ended without the recursive create event")
}

func TestPlacementWatchCursorSurvivesTransportRestart(t *testing.T) {
	cl, cleanup := launchOne(t)
	defer cleanup()
	watcher, ok := cl.(transport.WatchClient)
	if !ok {
		t.Fatal("transport client does not expose Watch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	ch, err := watcher.Watch(watchCtx, "/tmp", true, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Write(ctx, "/tmp/watch-before-restart", []byte("before")); err != nil {
		t.Fatal(err)
	}
	var before uint64
	for ev := range ch {
		if ev.Operation == "create" && strings.HasSuffix(ev.Path, "/tmp/watch-before-restart") {
			before = ev.Sequence
			break
		}
		if ev.Operation == "error" {
			t.Fatalf("watch ended before first event: %v", ev.Err)
		}
	}
	if before == 0 {
		t.Fatal("watch event had no sequence")
	}
	stopWatch()

	pidBytes, err := cl.Read(ctx, "/tmp/.sandboxd-transport.pid", 0, 0)
	if err != nil {
		t.Fatalf("read transport pidfile: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("empty transport pid")
	}
	kill, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "kill -TERM " + pid}})
	if err != nil {
		t.Fatalf("kill transport child: %v", err)
	}
	for range kill {
	}
	if !waitHealth(cl, 15*time.Second) {
		t.Fatal("transport child did not recover")
	}

	resumed, err := watcher.Watch(ctx, "/tmp", true, 1, before)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Write(ctx, "/tmp/watch-after-restart", []byte("after")); err != nil {
		t.Fatal(err)
	}
	var after uint64
	for ev := range resumed {
		if ev.Operation == "create" && strings.HasSuffix(ev.Path, "/tmp/watch-after-restart") {
			after = ev.Sequence
			break
		}
		if ev.Operation == "error" {
			t.Fatalf("watch ended before second event: %v", ev.Err)
		}
	}
	if after <= before {
		t.Fatalf("watch sequence after transport restart = %d; want > %d", after, before)
	}
	t.Logf("watch cursor survived transport restart: %d -> %d", before, after)
}
