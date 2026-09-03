package volume

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/TSHOGX/khora/internal/btrfs"
)

// ProviderName is the stable name recorded with a Volume.  Names are part of
// durable state, so a caller must select a registered provider rather than
// silently falling back to whichever implementation happens to be assembled.
type ProviderName string

const (
	// LocalBtrfsProvider is the resident, btrfs-backed implementation shipped by
	// the standard assembly.
	LocalBtrfsProvider ProviderName = "local_btrfs"
	// PlainDirProvider is an uncapped directory implementation.  It is useful for
	// development and contract tests; callers that require a capacity cap must
	// select a provider advertising the Quota capability.
	PlainDirProvider ProviderName = "plain_dir"
)

// BackendName is retained as a vocabulary alias for code that describes the
// value as a backend name rather than a provider name.  There is one durable
// name, not two independent identifiers.
type BackendName = ProviderName

var (
	// ErrUnknownProvider means that durable state or configuration names no
	// registered backend.  It is intentionally distinct from a provider that is
	// registered but unavailable.
	ErrUnknownProvider = errors.New("volume: unknown provider")
	// ErrProviderUnavailable means a registered provider failed its startup
	// readiness check.  The registry never substitutes another provider for it.
	ErrProviderUnavailable = errors.New("volume: provider unavailable")
	// ErrInvalidProvider is returned for a nil backend, an empty/malformed name,
	// or a duplicate registration.
	ErrInvalidProvider = errors.New("volume: invalid provider")
	// ErrRegistrySealed reports an attempted configuration mutation after the
	// assembly has been handed to a serving engine.
	ErrRegistrySealed = errors.New("volume: provider registry is sealed")
)

// ErrUnknownBackend and ErrBackendUnavailable are aliases that make errors.Is
// checks read naturally at call sites that use the Backend vocabulary.
var (
	ErrUnknownBackend     = ErrUnknownProvider
	ErrBackendUnavailable = ErrProviderUnavailable
)

// ContextValidator is an optional startup check implemented by a backend that
// can cheaply establish whether its configured substrate is usable.  The
// Backend seam itself deliberately does not require this method: a backend may
// remain usable for uncapped operations even when a capability-specific
// substrate is absent, and it reports that capability at the operation gate.
type ContextValidator interface {
	Validate(context.Context) error
}

// Validator is the context-free form accepted for small in-process providers.
// It is kept separate from ContextValidator so a provider can choose the check
// shape that matches its resources without widening Backend.
type Validator interface {
	Validate() error
}

// Registry is the immutable-at-use set of configured Volume backends.  Register
// and SetDefault are available while assembling an engine; callers should
// finish assembly and run Validate before serving requests.  Lookups are safe
// for concurrent lifecycle operations.
type Registry struct {
	mu          sync.RWMutex
	backends    map[ProviderName]Backend
	defaultName ProviderName
	sealed      bool
}

// NewRegistry builds a registry from backends.  The first backend is the
// default used for newly-created Volumes.  Existing Volumes are always routed
// by their recorded Backend field and never by this default.
func NewRegistry(backends ...Backend) (*Registry, error) {
	r := &Registry{backends: make(map[ProviderName]Backend)}
	for _, backend := range backends {
		if err := r.Register(backend); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// NewRegistryWithDefault is NewRegistry with an explicit default provider.
// The explicit name is useful when more than one provider is configured and
// avoids making map/slice order a policy decision.
func NewRegistryWithDefault(defaultName ProviderName, backends ...Backend) (*Registry, error) {
	r, err := NewRegistry(backends...)
	if err != nil {
		return nil, err
	}
	if err := r.SetDefault(defaultName); err != nil {
		return nil, err
	}
	return r, nil
}

// NewRegistryFromMap constructs a registry from explicitly named backends.
// The supplied name must agree with Backend.Name; accepting a second spelling
// would make a durable record impossible to route deterministically.
func NewRegistryFromMap(defaultName ProviderName, backends map[ProviderName]Backend) (*Registry, error) {
	r := &Registry{backends: make(map[ProviderName]Backend, len(backends))}
	for name, backend := range backends {
		if err := r.registerNamed(name, backend); err != nil {
			return nil, err
		}
	}
	if defaultName != "" {
		if err := r.SetDefault(defaultName); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one backend under its own durable Name.  Registration is an
// assembly operation; a duplicate name is rejected rather than replacing a
// provider that durable records may already reference.
func (r *Registry) Register(backend Backend) error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidProvider)
	}
	if backend == nil {
		return fmt.Errorf("%w: nil backend", ErrInvalidProvider)
	}
	return r.registerNamed(ProviderName(backend.Name()), backend)
}

// RegisterNamed is the explicit-name form used by assembly code that keeps
// provider names in a typed configuration object.  The backend's Name must
// match the supplied name.
func (r *Registry) RegisterNamed(name ProviderName, backend Backend) error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidProvider)
	}
	return r.registerNamed(name, backend)
}

func (r *Registry) registerNamed(name ProviderName, backend Backend) error {
	if backend == nil {
		return fmt.Errorf("%w: provider %q has nil backend", ErrInvalidProvider, name)
	}
	if !validProviderName(name) {
		return fmt.Errorf("%w: provider name %q", ErrInvalidProvider, name)
	}
	backendName := ProviderName(backend.Name())
	if backendName != name {
		return fmt.Errorf("%w: provider name %q does not match backend name %q", ErrInvalidProvider, name, backend.Name())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrRegistrySealed
	}
	if r.backends == nil {
		r.backends = make(map[ProviderName]Backend)
	}
	if _, exists := r.backends[name]; exists {
		return fmt.Errorf("%w: provider %q is already registered", ErrInvalidProvider, name)
	}
	r.backends[name] = backend
	if r.defaultName == "" {
		r.defaultName = name
	}
	return nil
}

// SetDefault selects the provider used for new Volumes.  It does not affect
// routing of existing records.
func (r *Registry) SetDefault(name ProviderName) error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidProvider)
	}
	if !validProviderName(name) {
		return fmt.Errorf("%w: provider name %q", ErrInvalidProvider, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrRegistrySealed
	}
	if _, ok := r.backends[name]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	r.defaultName = name
	return nil
}

// Seal freezes the configured provider set.  Assembly calls this immediately
// before publishing the registry to lifecycle code; lookups remain concurrent
// and safe, while later mutation cannot invalidate durable routing decisions.
func (r *Registry) Seal() error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidProvider)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.backends) == 0 || r.defaultName == "" {
		return fmt.Errorf("%w: registry is not configured", ErrInvalidProvider)
	}
	r.sealed = true
	return nil
}

// Sealed reports whether configuration mutation is closed.
func (r *Registry) Sealed() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sealed
}

// Lookup resolves a durable provider name.  Unknown names fail closed.
func (r *Registry) Lookup(name string) (Backend, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrUnknownProvider)
	}
	r.mu.RLock()
	backend, ok := r.backends[ProviderName(name)]
	r.mu.RUnlock()
	if !ok || backend == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	return backend, nil
}

// Backend is an alias for Lookup for callers that use the seam's vocabulary.
func (r *Registry) Backend(name string) (Backend, error) { return r.Lookup(name) }

// Resolve is an alias for Lookup used by routing code.
func (r *Registry) Resolve(name string) (Backend, error) { return r.Lookup(name) }

// Default returns the configured provider for new Volumes.
func (r *Registry) Default() (Backend, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrUnknownProvider)
	}
	r.mu.RLock()
	name := r.defaultName
	backend := r.backends[name]
	r.mu.RUnlock()
	if name == "" || backend == nil {
		return nil, fmt.Errorf("%w: no default provider configured", ErrUnknownProvider)
	}
	return backend, nil
}

// DefaultName returns the durable name selected for new Volumes.
func (r *Registry) DefaultName() ProviderName {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultName
}

// Names returns registered provider names in deterministic order.
func (r *Registry) Names() []ProviderName {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]ProviderName, 0, len(r.backends))
	for name := range r.backends {
		out = append(out, name)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Backends returns registered providers in name order.  The returned slice is
// independent; backend implementations remain owned by the registry.
func (r *Registry) Backends() []Backend {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	names := make([]ProviderName, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	backends := make([]Backend, 0, len(names))
	r.mu.RUnlock()
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, name := range names {
		backend, err := r.Lookup(string(name))
		if err == nil {
			backends = append(backends, backend)
		}
	}
	return backends
}

// Validate checks the configured set and runs optional provider readiness
// checks.  It is intended to run before the control plane starts serving.  A
// missing provider is never replaced by the default: durable references must
// remain addressable by their recorded name.
func (r *Registry) Validate(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrProviderUnavailable)
	}
	r.mu.RLock()
	backends := make(map[ProviderName]Backend, len(r.backends))
	for name, backend := range r.backends {
		backends[name] = backend
	}
	defaultName := r.defaultName
	r.mu.RUnlock()
	if len(backends) == 0 {
		return fmt.Errorf("%w: no providers configured", ErrProviderUnavailable)
	}
	if defaultName == "" {
		return fmt.Errorf("%w: no default provider configured", ErrProviderUnavailable)
	}
	if _, ok := backends[defaultName]; !ok {
		return fmt.Errorf("%w: default provider %q is not registered", ErrUnknownProvider, defaultName)
	}
	for name, backend := range backends {
		if backend == nil || !validProviderName(name) {
			return fmt.Errorf("%w: provider %q", ErrInvalidProvider, name)
		}
		if ProviderName(backend.Name()) != name {
			return fmt.Errorf("%w: provider %q changed its backend name to %q", ErrInvalidProvider, name, backend.Name())
		}
		var err error
		switch validator := backend.(type) {
		case ContextValidator:
			err = validator.Validate(ctx)
		case Validator:
			err = validator.Validate()
		}
		if err != nil {
			return fmt.Errorf("%w: %q: %v", ErrProviderUnavailable, name, err)
		}
	}
	return nil
}

func validProviderName(name ProviderName) bool {
	if name == "" || strings.TrimSpace(string(name)) != string(name) {
		return false
	}
	for _, r := range string(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// LocalBtrfsConfig contains settings owned by the local Volume provider.  The
// orchestrator supplies the resolved directory; Btrfs is injectable for
// contract tests and otherwise uses its zero-value command client.
type LocalBtrfsConfig struct {
	VolumesDir string
	Btrfs      btrfs.Client
}

// PlainDirConfig contains settings owned by the uncapped directory provider.
type PlainDirConfig struct {
	Dir string
}

// ProviderConfig is the typed selection accepted by Assemble.  Only the
// selected provider's settings are used; unknown names fail before a backend
// or store is opened.
type ProviderConfig struct {
	Provider   ProviderName
	LocalBtrfs LocalBtrfsConfig
	PlainDir   PlainDirConfig
}

// Selection is a shorter alias for ProviderConfig.
type Selection = ProviderConfig

// Assemble constructs the built-in registry selected by cfg.  The standard
// assembly may register additional internal implementations by extending this
// switch; no dynamic plugin or public provider API is implied.
func Assemble(cfg ProviderConfig) (*Registry, error) {
	name := cfg.Provider
	if name == "" {
		name = LocalBtrfsProvider
	}
	switch name {
	case LocalBtrfsProvider:
		backend, err := NewLocalBtrfs(cfg.LocalBtrfs.VolumesDir)
		if err != nil {
			return nil, fmt.Errorf("assemble volume provider %q: %w", name, err)
		}
		backend.Btrfs = cfg.LocalBtrfs.Btrfs
		return NewRegistryWithDefault(name, backend)
	case PlainDirProvider:
		backend, err := NewPlainDir(cfg.PlainDir.Dir)
		if err != nil {
			return nil, fmt.Errorf("assemble volume provider %q: %w", name, err)
		}
		return NewRegistryWithDefault(name, backend)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
}

// Build is an alias for Assemble for assembly callers that prefer a verb.
func Build(cfg ProviderConfig) (*Registry, error) { return Assemble(cfg) }

// compile-time checks keep the registry's built-in selection set honest.
var _ Backend = (*LocalBtrfs)(nil)
var _ Backend = (*PlainDir)(nil)
