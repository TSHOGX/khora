package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

func TestUnmeasuredProviderRejectsNodeBudget(t *testing.T) {
	b := newFakeVolume()
	b.caps = volume.Capabilities{Snapshot: true, Clone: true}
	m := New(Deps{Store: store.NewMemory(), Volume: b, NodeCapacity: NodeCapacity{DiskByte: 1024}})
	if err := m.ValidateVolumeProviders(context.Background()); !errors.Is(err, volume.ErrUsageUnavailable) {
		t.Fatal(err)
	}
}

func TestDurableCapRequiresCurrentCapability(t *testing.T) {
	b := newFakeVolume()
	b.caps = volume.Capabilities{Snapshot: true, Clone: true}
	st := store.NewMemory()
	if err := st.CreateVolume(context.Background(), model.Volume{ID: "vol_old", Backend: b.Name(), CapByte: 1024, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st, Volume: b})
	if err := m.ValidateVolumeProviders(context.Background()); !errors.Is(err, ErrVolumeCapUnsupported) {
		t.Fatal(err)
	}
}
