//go:build substrate

package volume_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/volume"
	"github.com/TSHOGX/khora/internal/volume/volumetest"
	"github.com/TSHOGX/khora/test/substratereq"
)

// The isolated filesystem is a negative fixture; it never changes host quota policy.
func TestNoQuotaBtrfsContract(t *testing.T) {
	substratereq.Root(t)
	substratereq.Bin(t, "mkfs.btrfs", "mount", "umount", "btrfs")
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
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "umount", mount).CombinedOutput(); err != nil {
			t.Errorf("unmount: %v: %s", err, out)
		}
	}()
	mode, err := (btrfs.Client{}).QuotaStatus(context.Background(), mount)
	if err != nil || mode != btrfs.QuotaDisabled {
		t.Fatalf("fixture quota mode: %v %v", mode, err)
	}
	volumetest.RunContract(t, func(t *testing.T) volume.Backend {
		root, err := os.MkdirTemp(mount, "volumes-")
		if err != nil {
			t.Fatal(err)
		}
		b, err := volume.NewLocalBtrfs(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ids, err := b.ListUnitIDs(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			for _, id := range ids {
				if err := b.DestroyVolume(context.Background(), id); err != nil {
					t.Error(err)
				}
			}
			if err := os.Remove(root); err != nil {
				t.Error(err)
			}
		})
		return b
	})
	t.Run("Reopen", func(t *testing.T) {
		root := filepath.Join(mount, "reopen")
		first, err := volume.NewLocalBtrfs(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.CreateVolume(context.Background(), "vol_saved", 0); err != nil {
			t.Fatal(err)
		}
		next, err := volume.NewLocalBtrfs(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := next.AdoptMaterialized(context.Background(), "vol_saved"); err != nil {
			t.Fatal(err)
		}
		if err := next.DestroyVolume(context.Background(), "vol_saved"); err != nil {
			t.Fatal(err)
		}
	})
}
