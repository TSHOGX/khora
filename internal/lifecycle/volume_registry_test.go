package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

type namedFakeVolume struct {
	*fakeVolume
	name string
}

func (b *namedFakeVolume) Name() string { return b.name }

func TestVolumeOperationsUseTheRecordedBackend(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	a := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_a"}
	b := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_b"}
	registry, err := volume.NewRegistryWithDefault(volume.ProviderName(a.name), a, b)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st, VolumeRegistry: registry, NodeCapacity: NodeCapacity{}, Logf: func(string, ...any) {}})

	// New creation uses the configured default and records that name.
	idA, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	recB := model.Volume{ID: "vol_b", Backend: b.name, CapByte: 0, CreatedAt: time.Now()}
	if err := b.CreateVolume(ctx, string(recB.ID), 0); err != nil {
		t.Fatalf("seed B bytes: %v", err)
	}
	if err := st.CreateVolume(ctx, recB); err != nil {
		t.Fatalf("seed B record: %v", err)
	}

	// Every operation on B must reach B, even though A is still the default.
	if _, err := m.GetVolume(ctx, recB.ID); err != nil {
		t.Fatalf("inspect B: %v", err)
	}
	if err := m.SetVolumeCapacity(ctx, recB.ID, 0); err != nil {
		t.Fatalf("patch B: %v", err)
	}
	if _, err := m.ImportVolume(ctx, recB.ID, "seed.txt", strings.NewReader("payload")); err != nil {
		t.Fatalf("import B: %v", err)
	}
	var out bytes.Buffer
	if _, err := m.ExportVolume(ctx, recB.ID, "seed.txt", &out); err != nil {
		t.Fatalf("export B: %v", err)
	}
	if out.String() != "payload" {
		t.Fatalf("export B = %q", out.String())
	}

	if err := m.DeleteVolume(ctx, recB.ID); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	if _, err := st.GetVolume(ctx, recB.ID); !errors.Is(err, store.ErrVolumeNotFound) {
		t.Fatalf("B record after delete = %v", err)
	}

	for _, call := range a.Calls() {
		// Match the complete provider call token, not the shared `vol_` prefix:
		// the default backend legitimately handled the newly-created A volume.
		parts := strings.SplitN(call, ":", 2)
		if len(parts) == 2 && (parts[1] == string(recB.ID) || strings.HasPrefix(parts[1], string(recB.ID)+"->")) {
			t.Errorf("default backend handled operation for recorded B volume: %s", call)
		}
	}
	if calls := b.Calls(); !containsCall(calls, "Usage:"+string(recB.ID)) || !containsCall(calls, "DestroyVolume:"+string(recB.ID)) {
		t.Errorf("recorded backend calls = %v; want usage and destroy", calls)
	}
	_ = idA
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func TestVolumeProviderValidationRejectsUnknownDurableReference(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.CreateVolume(ctx, model.Volume{ID: "vol_unknown", Backend: "not_configured", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "configured"}
	r, err := volume.NewRegistry(b)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st, VolumeRegistry: r, Logf: func(string, ...any) {}})
	if err := m.ValidateVolumeProviders(ctx); !errors.Is(err, volume.ErrUnknownProvider) {
		t.Fatalf("validation = %v; want ErrUnknownProvider", err)
	}
	if _, err := m.GetVolume(ctx, "vol_unknown"); !errors.Is(err, volume.ErrUnknownProvider) {
		t.Fatalf("operation = %v; want ErrUnknownProvider", err)
	}
}

func TestSweepOrphanVolumesEnumeratesEveryConfiguredBackend(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	a := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_a"}
	b := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_b"}
	r, err := volume.NewRegistryWithDefault(volume.ProviderName(a.name), a, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		backend *namedFakeVolume
		id      string
	}{
		{a, "orphan_a"},
		{b, "orphan_b"},
	} {
		if err := item.backend.CreateVolume(ctx, item.id, 0); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Deps{Store: st, VolumeRegistry: r, Logf: func(string, ...any) {}})
	if err := m.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep = %v", err)
	}
	if !containsCall(a.Calls(), "DestroyVolume:orphan_a") || !containsCall(b.Calls(), "DestroyVolume:orphan_b") {
		t.Fatalf("backend calls: A=%v B=%v; want each orphan destroyed by its enumerating backend", a.Calls(), b.Calls())
	}
}

func TestDerivedVolumeKeepsSourceBackend(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	a := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_a"}
	b := &namedFakeVolume{fakeVolume: newFakeVolume(), name: "volume_b"}
	r, err := volume.NewRegistryWithDefault(volume.ProviderName(a.name), a, b)
	if err != nil {
		t.Fatal(err)
	}
	src := model.Volume{ID: "vol_source_b", Backend: b.name, CreatedAt: time.Now()}
	if err := b.CreateVolume(ctx, string(src.ID), 0); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVolume(ctx, src); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st, VolumeRegistry: r, Logf: func(string, ...any) {}})
	child, err := m.CloneVolume(ctx, src.ID, 0)
	if err != nil {
		t.Fatalf("clone = %v", err)
	}
	rec, err := st.GetVolume(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Backend != b.name {
		t.Fatalf("derived backend = %q, want %q", rec.Backend, b.name)
	}
	if !containsCall(b.Calls(), "Clone:"+string(src.ID)+"->"+string(child)+":cap=0") {
		t.Fatalf("source backend calls = %v; want clone on B", b.Calls())
	}
	for _, call := range a.Calls() {
		if strings.Contains(call, string(src.ID)) || strings.Contains(call, string(child)) {
			t.Errorf("default backend handled derived operation: %s", call)
		}
	}
}
