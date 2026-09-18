package rootfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/model"
)

func TestOverlayQuotaDisabledRejectsCapAndClearsOnlyWhenEnabled(t *testing.T) {
	for _, mode := range []btrfs.QuotaMode{btrfs.QuotaDisabled, btrfs.QuotaSimple} {
		p, err := NewHostOverlayFS(filepath.Join(t.TempDir(), "state"), t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p.unitPath("ov"), 0755); err != nil {
			t.Fatal(err)
		}
		cleared := false
		p.Btrfs = btrfs.Client{ProbeQuota: func(context.Context, string) (btrfs.QuotaMode, error) { return mode, nil }, Run: func(_ context.Context, args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "qgroup limit none") {
				cleared = true
			}
			return nil, nil
		}}
		if err := p.applyCap(context.Background(), "ov", 0); err != nil {
			t.Fatal(err)
		}
		if cleared != (mode == btrfs.QuotaSimple) {
			t.Fatal("wrong zero-cap behavior")
		}
		if mode == btrfs.QuotaDisabled {
			if err := p.applyCap(context.Background(), "ov", 4096); !errors.Is(err, model.ErrUnsupported) {
				t.Fatalf("positive cap: %v", err)
			}
			mode = btrfs.QuotaSimple
			if err := p.ValidateCapacity(context.Background(), true); !errors.Is(err, model.ErrUnavailable) {
				t.Fatalf("mode change: %v", err)
			}
		}
	}
}
