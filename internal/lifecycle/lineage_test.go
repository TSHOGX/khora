package lifecycle

import (
	"context"
	"errors"
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

// --- lineage DEPTH (rootfs-overlay D5.1 ⑩) ---------------------------------
//
// The lower stack is passed to mount(2) as one colon-separated `lowerdir=` string, and the
// classic mount path TRUNCATES options past a page instead of refusing. So an over-deep
// lineage does not fail cleanly at the substrate: it mounts a stack missing its tail, or
// fails naming nothing. The engine refuses it at the gate instead, and the provider owns
// the ceiling because it derives from host path lengths (§3.1.1).

// A create over a lineage deeper than the provider can mount is REFUSED, and refused
// before any Overlay is allocated (§4.4-A leaves nothing half-built).
func TestCreateRefusesLineageDeeperThanProviderCanMount(t *testing.T) {
	ctx := context.Background()
	// The seeded lineage is 3 deep (leaf→mid→base); a ceiling of 2 puts it over.
	rp := &fakeRootfs{maxLowerDepth: 2}
	m, st := newLineageMediator(t, rp)

	_, err := m.Create(ctx, CreateSpec{Template: "leaf"})
	if !errors.Is(err, ErrLineageTooDeep) {
		t.Fatalf("Create over a too-deep lineage: err = %v; want ErrLineageTooDeep — an unrefused one mounts a silently truncated lower stack (rootfs-overlay D5.1 ⑩)", err)
	}
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "CreateOverlay") {
			t.Errorf("an Overlay was allocated for a too-deep lineage: %v", rp.Calls())
		}
	}
	if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
		t.Errorf("%d record(s) left after a refused create; want 0 (§4.4-A)", len(recs))
	}
}

// A lineage AT the ceiling still works — the bound must refuse what does not fit and
// nothing more, or it is a regression dressed as a fix.
func TestCreateAcceptsLineageAtTheCeiling(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{maxLowerDepth: 3} // exactly the seeded depth
	m, _ := newLineageMediator(t, rp)

	if _, err := m.Create(ctx, CreateSpec{Template: "leaf"}); err != nil {
		t.Fatalf("Create at exactly the ceiling: %v; want success", err)
	}
}

// 0 means "no limit", which is what a provider with no ceiling reports and what every
// other test in the suite relies on.
func TestZeroMaxLowerDepthMeansNoLimit(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{maxLowerDepth: 0}
	m, _ := newLineageMediator(t, rp)

	if _, err := m.Create(ctx, CreateSpec{Template: "leaf"}); err != nil {
		t.Fatalf("Create with an unlimited provider: %v; want success", err)
	}
}

// Promote is the path where an unbounded depth does real damage, and the reason the check
// sits at a gate rather than at mount time: the layer bytes are frozen and the templates
// row inserted BEFORE anything mounts the result, so an unrefused Promote reports SUCCESS
// and leaves a Template that can never start a sandbox. The failure would surface one
// create later, with nothing pointing back at the Promote that caused it.
//
// This is also the shape of the downstream "archived nightly, silently stops starting after
// ~2.5 months" report: each archive round stacks one more layer, since a Promote's parent is
// the source's Template and is not selectable.
func TestPromoteRefusesWhenTheNewLayerWouldExceedTheCeiling(t *testing.T) {
	ctx := context.Background()
	// Seeded lineage is 3 deep; a Promote of a sandbox on "leaf" would produce a 4-deep
	// stack. A ceiling of 3 admits the create and refuses exactly the Promote.
	rp := &fakeRootfs{maxLowerDepth: 3}
	m, _ := newLineageMediator(t, rp)

	sid, err := m.Create(ctx, CreateSpec{Template: "leaf"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = m.Promote(ctx, sid)
	if !errors.Is(err, ErrLineageTooDeep) {
		t.Fatalf("Promote producing a stack one past the ceiling: err = %v; want ErrLineageTooDeep", err)
	}
	// Refused BEFORE the quiesce and before any bytes: a refused Promote is not an
	// observable transition (§3.1 / rootfs-overlay D5, the same discipline as the
	// capacity gate). PromoteToTemplate must never have been called.
	for _, c := range rp.Calls() {
		if strings.HasPrefix(c, "PromoteToTemplate") {
			t.Errorf("layer bytes were frozen for a refused Promote: %v", rp.Calls())
		}
	}
	// And the sandbox is untouched — still running, not left quiesced by a refusal.
	rec, err := m.deps.Store.GetSandbox(ctx, sid)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if rec.State != model.Running {
		t.Errorf("state after a refused Promote = %s; want running — the gate comes before the quiesce", rec.State)
	}
}

// A failing depth QUERY refuses too, rather than admitting blind: the same fail-closed
// choice the capacity gate makes when it cannot forecast (§2.3).
func TestUnreadableMaxLowerDepthRefusesRatherThanAdmitting(t *testing.T) {
	ctx := context.Background()
	rp := &fakeRootfs{failMaxLowerDepth: true}
	m, _ := newLineageMediator(t, rp)

	if _, err := m.Create(ctx, CreateSpec{Template: "leaf"}); err == nil {
		t.Fatal("Create with an unreadable depth ceiling SUCCEEDED; want a refusal (fail-closed, §2.3)")
	}
}
