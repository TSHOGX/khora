package volume

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRegistrySelectsAndLooksUpByDurableName(t *testing.T) {
	plain, err := NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Give the second test backend a distinct durable name without creating a
	// second implementation: the registry's explicit-name form must reject this
	// mismatch rather than silently creating an alias.
	if _, err := NewRegistryWithDefault(PlainDirProvider, plain, local); err == nil {
		t.Fatal("registered two backends with the same Name; want duplicate refusal")
	}

	r, err := NewRegistry(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.DefaultName(); got != PlainDirProvider {
		t.Fatalf("default name = %q, want %q", got, PlainDirProvider)
	}
	got, err := r.Lookup(string(PlainDirProvider))
	if err != nil || got != plain {
		t.Fatalf("lookup = %v, %v; want the registered backend", got, err)
	}
	if _, err := r.Lookup("missing"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown lookup = %v; want ErrUnknownProvider", err)
	}
	if err := r.Validate(context.Background()); err != nil {
		t.Fatalf("validate = %v", err)
	}
}

type registryBackend struct {
	name string
	err  error
	Backend
}

func (b *registryBackend) Name() string                   { return b.name }
func (b *registryBackend) Validate(context.Context) error { return b.err }

func TestRegistryValidationFailsClosed(t *testing.T) {
	base, err := NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{name: "provider unavailable", err: errors.New("device offline"), want: ErrProviderUnavailable},
		{name: "provider invalid", err: nil, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := string(PlainDirProvider)
			if tc.name == "provider invalid" {
				name = "bad/name"
			}
			b := &registryBackend{name: name, err: tc.err, Backend: base}
			r, err := NewRegistry(b)
			if tc.want == nil {
				if err == nil || !errors.Is(err, ErrInvalidProvider) {
					t.Fatalf("construct = %v; want ErrInvalidProvider", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = r.Validate(context.Background())
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "device offline") {
				t.Fatalf("validate = %v; want %v with provider detail", err, tc.want)
			}
		})
	}
}

func TestAssembleTypedProviderSelection(t *testing.T) {
	t.Run("plain directory", func(t *testing.T) {
		r, err := Assemble(ProviderConfig{Provider: PlainDirProvider, PlainDir: PlainDirConfig{Dir: t.TempDir()}})
		if err != nil {
			t.Fatal(err)
		}
		if r.DefaultName() != PlainDirProvider {
			t.Fatalf("default = %q", r.DefaultName())
		}
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := Assemble(ProviderConfig{Provider: ProviderName("not-shipped")})
		if !errors.Is(err, ErrUnknownProvider) {
			t.Fatalf("assemble = %v; want ErrUnknownProvider", err)
		}
	})
}

func TestRegistrySealPreventsServingTimeMutation(t *testing.T) {
	b, err := NewPlainDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Seal(); err != nil {
		t.Fatal(err)
	}
	if !r.Sealed() {
		t.Fatal("registry did not report sealed")
	}
	if err := r.SetDefault(PlainDirProvider); !errors.Is(err, ErrRegistrySealed) {
		t.Fatalf("SetDefault after Seal = %v; want ErrRegistrySealed", err)
	}
}

func TestRegistryRejectsTypedNilBackend(t *testing.T) {
	var backend *PlainDir
	if _, err := NewRegistry(backend); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("typed nil registration = %v; want ErrInvalidProvider", err)
	}
}
