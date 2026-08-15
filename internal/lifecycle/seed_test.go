package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// newSeedMediator builds a mediator whose fakeRootfs reports the given builtin
// Template dirs (boot seed source), over a fresh empty store.
func newSeedMediator(t *testing.T, builtins ...string) (*Mediator, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	rp := &fakeRootfs{builtins: builtins}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
	})
	return m, st
}

// boot seed: every static Template dir gets a builtin row (builtin=1, no
// parent), and the seed is IDEMPOTENT — running it twice leaves the row count
// unchanged and adds no duplicates.
func TestSeedBuiltinTemplatesIdempotent(t *testing.T) {
	ctx := context.Background()
	m, st := newSeedMediator(t, "bare", "agent-stack")

	if err := m.SeedBuiltinTemplates(ctx); err != nil {
		t.Fatalf("seed (first): %v", err)
	}
	first, _ := st.ListTemplates(ctx)
	if len(first) != 2 {
		t.Fatalf("after seed: %d templates; want 2", len(first))
	}
	for _, tm := range first {
		if !tm.Builtin {
			t.Errorf("template %s not marked builtin", tm.ID)
		}
		if tm.Parent != "" {
			t.Errorf("builtin %s has a parent %q; want none (base layer)", tm.ID, tm.Parent)
		}
	}

	// second run: idempotent — same rows, no duplicates, no error.
	if err := m.SeedBuiltinTemplates(ctx); err != nil {
		t.Fatalf("seed (second): %v", err)
	}
	second, _ := st.ListTemplates(ctx)
	if len(second) != 2 {
		t.Fatalf("after re-seed: %d templates; want 2 (idempotent)", len(second))
	}
}

// The seed makes a create's sandboxes.template FK resolve: before seeding, a
// create referencing a builtin fails (the documented invariant reference-side); after seeding it
// succeeds. This is exactly why the seed MUST run before serving.
func TestSeedEnablesCreate(t *testing.T) {
	ctx := context.Background()
	m, _ := newSeedMediator(t, "bare")

	// no seed yet → create loses on the missing-template FK.
	if _, err := m.Create(ctx, CreateSpec{Template: "bare"}); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("pre-seed create = %v; want ErrTemplateNotFound", err)
	}

	if err := m.SeedBuiltinTemplates(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("post-seed create: %v", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("state=%s; want running", stt)
	}
}
