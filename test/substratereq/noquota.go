package substratereq

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// NoQuotaBtrfs mounts an isolated filesystem without changing host quota policy.
func NoQuotaBtrfs(t *testing.T) string {
	t.Helper()
	Root(t)
	Bin(t, "mkfs.btrfs", "mount", "umount", "btrfs")
	dir := t.TempDir()
	image := filepath.Join(dir, "disk.img")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	mount := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mount, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("fixture %s: %v: %s", args[0], err, out)
		}
	}
	run("mkfs.btrfs", "-f", image)
	run("mount", "-o", "loop", image, mount)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "umount", mount).CombinedOutput(); err != nil {
			t.Errorf("unmount: %v: %s", err, out)
		}
	})
	return mount
}
