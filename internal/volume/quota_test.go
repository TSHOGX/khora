package volume_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/volume"
)

func TestQuotaDisabledAndModeChange(t *testing.T) {
	b, err := volume.NewLocalBtrfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mode := btrfs.QuotaDisabled
	var commands []string
	b.Btrfs = btrfs.Client{ProbeQuota: func(context.Context, string) (btrfs.QuotaMode, error) { return mode, nil }, Run: func(_ context.Context, args []string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		if args[1] == "subvolume" && args[2] == "create" {
			return nil, os.Mkdir(args[3], 0755)
		}
		return nil, nil
	}}
	ctx := context.Background()
	if err := b.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	caps := b.Capabilities()
	if caps.Quota || caps.Usage || !caps.Snapshot || !caps.Clone {
		t.Fatalf("caps: %+v", caps)
	}
	if err := b.CreateVolume(ctx, "vol_capped", 1024); !errors.Is(err, volume.ErrUnsupported) {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatal("refused cap had substrate side effects")
	}
	if err := b.CreateVolume(ctx, "vol_open", 0); err != nil {
		t.Fatal(err)
	}
	if err := b.SetCapacity(ctx, "vol_open", 0); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range commands {
		if strings.Contains(cmd, "qgroup") {
			t.Fatalf("quota write while disabled: %s", cmd)
		}
	}
	if _, _, err := b.Usage(ctx, "vol_open"); !errors.Is(err, volume.ErrUsageUnavailable) {
		t.Fatal(err)
	}
	if _, err := b.UncappedBytes(ctx); !errors.Is(err, volume.ErrUsageUnavailable) {
		t.Fatal(err)
	}
	mode = btrfs.QuotaSimple
	if err := b.CreateVolume(ctx, "vol_changed", 0); !errors.Is(err, volume.ErrProviderUnavailable) {
		t.Fatal("mode change accepted", err)
	}
	if _, err := os.Stat(filepath.Join(b.VolumesDir, "vol_changed")); !os.IsNotExist(err) {
		t.Fatal("mode change left bytes")
	}
}

func TestMaterializeNeverFallsBackAfterFailure(t *testing.T) {
	b, _ := volume.NewLocalBtrfs(t.TempDir())
	b.Btrfs = btrfs.Client{ProbeQuota: func(context.Context, string) (btrfs.QuotaMode, error) { return btrfs.QuotaDisabled, nil }, Run: func(context.Context, []string) ([]byte, error) { return nil, errors.New("denied") }}
	if _, err := b.Materialize(context.Background(), "vol_no"); err == nil {
		t.Fatal("failed substrate accepted")
	}
	if _, err := os.Stat(filepath.Join(b.VolumesDir, "vol_no")); !os.IsNotExist(err) {
		t.Fatal("ordinary directory substituted")
	}
}

func TestUncappedWriteClearsLimitWhenQuotasEnabled(t *testing.T) {
	b, _ := volume.NewLocalBtrfs(t.TempDir())
	if err := os.Mkdir(filepath.Join(b.VolumesDir, "vol_old"), 0755); err != nil {
		t.Fatal(err)
	}
	cleared := false
	b.Btrfs = btrfs.Client{ProbeQuota: func(context.Context, string) (btrfs.QuotaMode, error) { return btrfs.QuotaSimple, nil }, Run: func(_ context.Context, args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "qgroup limit none") {
			cleared = true
		}
		return nil, nil
	}}
	if err := b.SetCapacity(context.Background(), "vol_old", 0); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("old limit not cleared")
	}
}
