package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

func TestNewSelectsTypedVolumeProvider(t *testing.T) {
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		VolumeProvider: VolumeProviderPlainDir,
		VolumesDir:     filepath.Join(base, "volumes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.VolumeRegistry.DefaultName(); got != volume.PlainDirProvider {
		t.Fatalf("default provider = %q, want %q", got, volume.PlainDirProvider)
	}
	if closer, ok := eng.Store.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
}

func TestNewRejectsUnknownVolumeProviderBeforeServing(t *testing.T) {
	base := t.TempDir()
	_, err := New(Config{
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		VolumeProvider: VolumeProvider("not-shipped"),
		VolumesDir:     filepath.Join(base, "volumes"),
	})
	if !errors.Is(err, volume.ErrUnknownProvider) {
		t.Fatalf("New = %v; want ErrUnknownProvider", err)
	}
}

func TestNewRejectsDurableVolumeWithUnavailableProvider(t *testing.T) {
	base := t.TempDir()
	storePath := filepath.Join(base, "store.db")
	st, err := store.OpenSQLite(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVolume(context.Background(), model.Volume{
		ID: "vol_durable", Backend: "provider-not-configured", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = New(Config{
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		StorePath:      storePath,
		VolumeProvider: VolumeProviderLocalBtrfs,
		VolumesDir:     filepath.Join(base, "volumes"),
	})
	if !errors.Is(err, volume.ErrUnknownProvider) {
		t.Fatalf("New with unknown durable provider = %v; want ErrUnknownProvider", err)
	}
}

func TestNewRejectsDurableCapUnsupportedBySelectedProvider(t *testing.T) {
	base := t.TempDir()
	storePath := filepath.Join(base, "store.db")
	st, err := store.OpenSQLite(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVolume(context.Background(), model.Volume{
		ID: "vol_capped", Backend: string(volume.PlainDirProvider), CapByte: 64, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = New(Config{
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		StorePath:      storePath,
		VolumeProvider: VolumeProviderPlainDir,
		VolumesDir:     filepath.Join(base, "volumes"),
	})
	if !errors.Is(err, lifecycle.ErrVolumeCapUnsupported) {
		t.Fatalf("New with unsupported durable cap = %v; want ErrVolumeCapUnsupported", err)
	}
}
