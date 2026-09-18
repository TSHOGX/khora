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

type capacityRootfs struct {
	*fakeRootfs
	required bool
}

func (p *capacityRootfs) ValidateCapacity(_ context.Context, required bool) error {
	p.required = required
	if required {
		return model.ErrUnsupported
	}
	return nil
}

type capacityRecords struct {
	store.Store
	records []model.Sandbox
}

func (s capacityRecords) ListSandboxes(context.Context) ([]model.Sandbox, error) {
	return s.records, nil
}

func TestRootfsCapacityIncludesDurableSandboxLimits(t *testing.T) {
	for _, tc := range []struct {
		disk, node int64
		required   bool
	}{
		{0, 0, false}, {1024, 0, true}, {0, 1024, true},
	} {
		root := &capacityRootfs{fakeRootfs: &fakeRootfs{}}
		st := capacityRecords{Store: store.NewMemory(), records: []model.Sandbox{{Limits: model.Limits{DiskByte: tc.disk}}}}
		m := New(Deps{Store: st, Rootfs: root, NodeCapacity: NodeCapacity{DiskByte: tc.node}})
		err := m.ValidateRootfsCapacity(context.Background())
		if root.required != tc.required || errors.Is(err, model.ErrUnsupported) != tc.required {
			t.Fatalf("%+v: required=%v err=%v", tc, root.required, err)
		}
	}
}
