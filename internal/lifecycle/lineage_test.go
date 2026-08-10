package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"warren/internal/model"
	"warren/internal/store"
	"warren/internal/transport"
)

// resolveLower's ORDER (§5.B multi-lowerdir). Every other test in the suite uses a
// single-element lower stack, where order is unobservable — so a reversed walk
// would pass the whole suite.
//
// Why order is correctness and not cosmetics: overlayfs resolves lowerdir
// LEFT-TO-FIRST-MATCH, so the stack must be leaf-first ([leaf, …ancestors, base]).
// Reversed, a base-layer file SHADOWS the promoted layer that was meant to replace
// it — a Promote that appears to do nothing, or worse, silently serves stale
// content. Nothing else in the engine would report an error.

// seedLineage inserts a base←mid←leaf Template chain.
func seedLineage(t *testing.T, st *store.Memory) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	for _, tm := range []model.Template{
		{ID: "base", Builtin: true, CreatedAt: now},
		{ID: "mid", Parent: "base", CreatedAt: now},
		{ID: "leaf", Parent: "mid", CreatedAt: now},
	} {
		if err := st.CreateTemplate(ctx, tm); err != nil {
			t.Fatalf("seed template %s: %v", tm.ID, err)
		}
	}
}

func newLineageMediator(t *testing.T, rp *fakeRootfs) (*Mediator, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	seedLineage(t, st)
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
	})
	return m, st
}

// A create over a 3-deep lineage hands the provider the FULL stack, leaf-first.
func TestCreateResolvesFullLineageLeafFirst(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{}
	m, st := newLineageMediator(t, rp)

	sid, err := m.Create(ctx, CreateSpec{Template: "leaf"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, _ := st.GetSandbox(ctx, sid)

	want := "CreateOverlay:" + string(rec.Overlay) + ";lower=leaf|mid|base;cap=0"
	if !contains(rp.Calls(), want) {
		t.Fatalf("lower stack = %v;\nwant a call %q — the stack must be the FULL lineage, LEAF-FIRST (§5.B multi-lowerdir: overlayfs takes the first match left-to-right, so a reversed stack lets a base-layer file shadow the promoted layer meant to replace it)",
			rp.Calls(), want)
	}
}

// A resume re-resolves the SAME full stack in the same order. Resume rebuilds the
// lower stack from the store rather than reusing a cached one, so the ordering
// guarantee has to hold on this path independently.
func TestResumeReResolvesFullLineageLeafFirst(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{}
	m, st := newLineageMediator(t, rp)

	sid, err := m.Create(ctx, CreateSpec{Template: "leaf"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	rec, _ := st.GetSandbox(ctx, sid)

	want := "OpenOverlay:" + string(rec.Overlay) + ";lower=leaf|mid|base;cap=0"
	if !contains(rp.Calls(), want) {
		t.Fatalf("resume lower stack = %v;\nwant a call %q — a resume re-derives the lineage from the store, so the leaf-first order must hold here too (§5.B)",
			rp.Calls(), want)
	}
}

// brokenLineageStore makes ONE ancestor unreadable mid-walk. The store's own FK +
// RESTRICT make a broken chain unreachable through its API (that is the §4.3
// guarantee), so the mediator's walk error is a DEFENSIVE path against on-disk
// corruption — and this stub is the only way to reach it.
type brokenLineageStore struct {
	store.Store
	missing model.TemplateID
}

func (s *brokenLineageStore) GetTemplate(ctx context.Context, id model.TemplateID) (model.Template, error) {
	if id == s.missing {
		return model.Template{}, store.ErrTemplateNotFound
	}
	return s.Store.GetTemplate(ctx, id)
}

// A create whose lineage walk cannot read an ANCESTOR must fail cleanly and build
// nothing. The walk must not silently truncate the stack: a short stack mounts a
// sandbox missing its lower layers, which presents as mass unexplained file loss
// rather than as an error.
func TestCreateRefusesBrokenLineage(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	seedLineage(t, mem)
	rp := &fakeRootfs{}
	m := New(Deps{
		// "mid" becomes unreadable, so resolving leaf→mid→base breaks partway.
		Store:   &brokenLineageStore{Store: mem, missing: "mid"},
		Runtime: &fakeRuntime{}, Rootfs: rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: 500 * time.Millisecond,
	})

	_, err := m.Create(ctx, CreateSpec{Template: "leaf"})
	if err == nil {
		t.Fatal("Create over a broken lineage SUCCEEDED; want a refusal — a silently truncated lower stack mounts a sandbox missing its layers, which looks like mass file loss rather than an error (§5.B)")
	}
	if !strings.Contains(err.Error(), "lineage") {
		t.Errorf("error = %v; want it to name the lineage resolution as the cause", err)
	}
	// Nothing half-built (§4.4-A): no overlay allocated, no record left.
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "CreateOverlay") {
			t.Errorf("an Overlay was allocated for a broken lineage: %v", rp.Calls())
		}
	}
	if recs, _ := mem.ListSandboxes(ctx); len(recs) != 0 {
		t.Errorf("%d record(s) left after a refused create; want 0 (§4.4-A)", len(recs))
	}
}
