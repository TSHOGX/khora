//go:build substrate

package test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/test/harness"
	"github.com/TSHOGX/khora/test/substratereq"
)

func TestQuotaDisabledVolumeGuestLifecycle(t *testing.T) {
	base := substratereq.NoQuotaBtrfs(t)
	mode, err := (btrfs.Client{}).QuotaStatus(context.Background(), base)
	if err != nil || mode != btrfs.QuotaDisabled {
		t.Fatalf("fixture mode: %v %v", mode, err)
	}
	eng, _ := harness.Engine(t, harness.EngineOpts{BaseDir: base})
	t.Cleanup(func() {
		if err := eng.Store.(*store.SQLite).Close(); err != nil {
			t.Error(err)
		}
		cleanupUnitsAndOverlays(filepath.Join(base, "state"))
	})
	ctx := context.Background()
	if _, err := eng.Mediator.CreateVolume(ctx, 1024); !errors.Is(err, lifecycle.ErrVolumeCapUnsupported) {
		t.Fatalf("positive cap: %v", err)
	}
	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "seed", strings.NewReader("seeded\n")); err != nil {
		t.Fatal(err)
	}
	info, err := eng.Mediator.GetVolume(ctx, vid)
	if err != nil || info.UsedByte != nil {
		t.Fatalf("unmeasured info: %+v %v", info, err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare", Volumes: []model.Mount{{Volume: vid, Path: "/shared", Access: model.SharedReadWrite}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eng.Mediator.Destroy(ctx, sid); err != nil {
			t.Error(err)
		}
	}()
	if out := guestSh(t, eng, sid, "read x < /shared/seed; echo $x; echo guest > /shared/written"); !strings.Contains(out, "seeded") {
		t.Fatalf("guest: %q", out)
	}
	if err := eng.Mediator.Pause(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := eng.Mediator.Resume(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if out := guestSh(t, eng, sid, "read x < /shared/written; echo $x"); !strings.Contains(out, "guest") {
		t.Fatalf("resume: %q", out)
	}
	if err := eng.Mediator.Pause(ctx, sid); err != nil {
		t.Fatal(err)
	}
	for _, derive := range []func(context.Context, model.VolumeID, int64) (model.VolumeID, error){eng.Mediator.SnapshotVolume, eng.Mediator.CloneVolume} {
		child, err := derive(ctx, vid, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := eng.Mediator.ReadVolume(ctx, child, "written", 0, 32)
		if err != nil || string(got) != "guest\n" {
			t.Fatalf("derived bytes: %q %v", got, err)
		}
		if err := eng.Mediator.DeleteVolume(ctx, child); err != nil {
			t.Fatal(err)
		}
	}
}
