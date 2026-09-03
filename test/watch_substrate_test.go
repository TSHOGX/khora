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
	if !ok { t.Fatal("transport client does not expose Watch") }
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ch, err := watcher.Watch(ctx, "/tmp", true, 1, 0)
	if err != nil { t.Fatal(err) }
	time.Sleep(500 * time.Millisecond)
	if err := cl.Write(ctx, "/tmp/watch-marker", []byte("watch")); err != nil { t.Fatal(err) }
	if _, err := cl.Stat(ctx, "/tmp/watch-marker"); err != nil { t.Fatalf("marker stat after write: %v", err) }
	for ev := range ch {
		t.Logf("watch event: %+v", ev)
		if ev.Operation == "create" && strings.HasSuffix(ev.Path, "/tmp/watch-marker") { return }
	}
	t.Fatal("watch stream ended without the recursive create event")
}
