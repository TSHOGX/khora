//go:build substrate

package volume_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/volume"
	"github.com/TSHOGX/khora/internal/volume/volumetest"
	"github.com/TSHOGX/khora/test/substratereq"
)

// The isolated filesystem is a negative fixture; it never changes host quota policy.
func TestNoQuotaBtrfsContract(t *testing.T) {
	mount := substratereq.NoQuotaBtrfs(t)
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
