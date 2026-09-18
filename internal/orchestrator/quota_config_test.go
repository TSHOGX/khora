package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/volume"
)

func TestQuotaDisabledConfiguration(t *testing.T) {
	for _, budget := range []int64{0, 1 << 30} {
		base := t.TempDir()
		e, err := New(Config{TemplatesDir: filepath.Join(base, "templates"), StateDir: filepath.Join(base, "state"), NodeDiskByte: budget, VolumeLocalBtrfs: volume.LocalBtrfsConfig{Btrfs: btrfs.Client{ProbeQuota: func(context.Context, string) (btrfs.QuotaMode, error) { return btrfs.QuotaDisabled, nil }}}})
		if budget > 0 {
			if !errors.Is(err, volume.ErrUsageUnavailable) {
				t.Fatalf("budget config: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.VolumeRegistry.DefaultName() != volume.LocalBtrfsProvider {
			t.Fatal("provider silently substituted")
		}
		if closer, ok := e.Store.(interface{ Close() error }); ok {
			closer.Close()
		}
	}
}
