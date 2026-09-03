package lifecycle

import (
	"context"
	"fmt"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/volume"
)

// namedVolumeBackend is the lifecycle's private routing pair. The name is
// useful for diagnostics and for keeping an orphan unit tied to the backend
// that enumerated it; neither field crosses a subsystem seam.
type namedVolumeBackend struct {
	name    string
	backend volume.Backend
}

// defaultVolumeBackend resolves the configured provider for a NEW Volume. A
// registry is authoritative when supplied. The legacy single-backend field is
// intentionally only a fallback for direct in-process assembly and tests.
func (m *Mediator) defaultVolumeBackend() (namedVolumeBackend, error) {
	if m.deps.VolumeRegistry != nil {
		name, backend, err := m.deps.VolumeRegistry.DefaultWithName()
		if err != nil {
			return namedVolumeBackend{}, fmt.Errorf("no default volume provider: %w", err)
		}
		return namedVolumeBackend{name: string(name), backend: backend}, nil
	}
	if m.deps.Volume == nil {
		return namedVolumeBackend{}, fmt.Errorf("no volume backend registered")
	}
	name := volume.ProviderName(m.deps.Volume.Name())
	if err := name.Validate(); err != nil {
		return namedVolumeBackend{}, err
	}
	return namedVolumeBackend{name: string(name), backend: m.deps.Volume}, nil
}

// backendForRecord resolves the provider named by a durable Volume row. It
// never falls back to the configured default: doing so could send an operation
// for bytes owned by one provider to another provider after a restart.
func (m *Mediator) backendForRecord(rec model.Volume) (volume.Backend, error) {
	if rec.Backend == "" {
		return nil, fmt.Errorf("volume %s has no recorded backend: %w", rec.ID, volume.ErrUnknownProvider)
	}
	if m.deps.VolumeRegistry != nil {
		backend, err := m.deps.VolumeRegistry.Lookup(rec.Backend)
		if err != nil {
			return nil, fmt.Errorf("volume %s references provider %q: %w", rec.ID, rec.Backend, err)
		}
		return backend, nil
	}
	if m.deps.Volume == nil {
		return nil, fmt.Errorf("volume %s references provider %q but no provider is configured: %w", rec.ID, rec.Backend, volume.ErrProviderUnavailable)
	}
	// A single-backend direct assembly is still required to honor the durable
	// name. Refusing a mismatch is safer than treating the old field as a global
	// router.
	if got := m.deps.Volume.Name(); got != rec.Backend {
		return nil, fmt.Errorf("volume %s references provider %q, but only %q is configured: %w", rec.ID, rec.Backend, got, volume.ErrUnknownProvider)
	}
	return m.deps.Volume, nil
}

// backendForVolume reads the authoritative row and resolves its provider.
func (m *Mediator) backendForVolume(ctx context.Context, id model.VolumeID) (model.Volume, volume.Backend, error) {
	rec, err := m.deps.Store.GetVolume(ctx, id)
	if err != nil {
		return model.Volume{}, nil, err
	}
	backend, err := m.backendForRecord(rec)
	if err != nil {
		return model.Volume{}, nil, err
	}
	return rec, backend, nil
}

func (m *Mediator) validateVolumeRecord(rec model.Volume) error {
	backend, err := m.backendForRecord(rec)
	if err != nil {
		return err
	}
	if rec.CapByte > 0 && !backend.Capabilities().Quota {
		return fmt.Errorf("volume %s records a %d-byte cap but provider %q cannot enforce capacity caps: %w: %w", rec.ID, rec.CapByte, rec.Backend, ErrVolumeCapUnsupported, volume.ErrUnsupported)
	}
	return nil
}

// configuredVolumeBackends returns every provider in deterministic registry
// order. It is used only for backend-owned enumeration and aggregate
// measurements; operations on a specific Volume use backendForVolume instead.
func (m *Mediator) configuredVolumeBackends() ([]namedVolumeBackend, error) {
	if m.deps.VolumeRegistry != nil {
		names := m.deps.VolumeRegistry.Names()
		out := make([]namedVolumeBackend, 0, len(names))
		for _, name := range names {
			backend, err := m.deps.VolumeRegistry.Lookup(string(name))
			if err != nil {
				return nil, fmt.Errorf("resolve volume provider %q: %w", name, err)
			}
			out = append(out, namedVolumeBackend{name: string(name), backend: backend})
		}
		return out, nil
	}
	if m.deps.Volume == nil {
		return nil, nil
	}
	name := volume.ProviderName(m.deps.Volume.Name())
	if err := name.Validate(); err != nil {
		return nil, err
	}
	return []namedVolumeBackend{{name: string(name), backend: m.deps.Volume}}, nil
}

// validateDurableVolumeProviders checks all persisted Volume references before
// the control plane serves dependent operations. Store errors and unknown
// names are returned, never interpreted as an empty volume set.
func (m *Mediator) validateDurableVolumeProviders(ctx context.Context) error {
	providers, err := m.configuredVolumeBackends()
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		volumes, listErr := m.deps.Store.ListVolumes(ctx)
		if listErr != nil {
			return fmt.Errorf("list durable volumes: %w", listErr)
		}
		if len(volumes) > 0 {
			return fmt.Errorf("%d durable volume(s) exist but no provider is configured: %w", len(volumes), volume.ErrProviderUnavailable)
		}
		return nil
	}
	volumes, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list durable volumes: %w", err)
	}
	for _, rec := range volumes {
		if err := m.validateVolumeRecord(rec); err != nil {
			return err
		}
	}
	return nil
}

// ValidateVolumeProviders is the startup gate for the Volume registry and all
// durable Volume references. Call it before serving the control plane; callers
// that construct a Mediator directly can use the same gate as the orchestrator.
func (m *Mediator) ValidateVolumeProviders(ctx context.Context) error {
	if m.deps.VolumeRegistry != nil {
		if err := m.deps.VolumeRegistry.Validate(ctx); err != nil {
			return err
		}
	} else if providers, err := m.configuredVolumeBackends(); err != nil {
		return err
	} else {
		for _, selected := range providers {
			if validator, ok := selected.backend.(volume.ContextValidator); ok {
				if err := validator.Validate(ctx); err != nil {
					return fmt.Errorf("%w: %q: %w", volume.ErrProviderUnavailable, selected.name, err)
				}
			} else if validator, ok := selected.backend.(volume.Validator); ok {
				if err := validator.Validate(); err != nil {
					return fmt.Errorf("%w: %q: %w", volume.ErrProviderUnavailable, selected.name, err)
				}
			}
		}
	}
	return m.validateDurableVolumeProviders(ctx)
}
