package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
)

// §4.4-C at the MEDIATOR layer. The store contract suite (storetest) proves each
// individual call commits state+event atomically; what it cannot prove is that
// the mediator asks for the right events in the first place. That is a separate
// failure mode with the same consequence: §4.4-C exists because billing-style
// subscribers keep their ledger from this stream, so a dropped event is a dropped
// charge — and an event the mediator never constructed is just as missing as one a
// backend lost, while an extra one double-counts.
//
// So these pin the mediator's event SEQUENCE across a full lifecycle, and the
// deliberate silences around it.

// kindsFor returns the outbox event Kinds for one sandbox, in commit order.
func kindsFor(t *testing.T, st *store.Memory, id model.SandboxID) []string {
	t.Helper()
	evs, err := st.UnpublishedEvents(context.Background())
	if err != nil {
		t.Fatalf("UnpublishedEvents: %v", err)
	}
	var out []string
	for _, e := range evs {
		if e.Sandbox == id {
			out = append(out, e.Kind)
		}
	}
	return out
}

// A full create → pause → resume → destroy emits EXACTLY four events, in order,
// with four distinct transition IDs.
//
// Three ways this can break, all invisible without this test:
//   - the transient `creating` insert publishes an event (it must not — a
//     subscriber would see a sandbox announced twice, and `Create` passes nil
//     precisely to prevent it);
//   - a transition forgets its event (silent under-billing);
//   - two events share an ID, which collapses to one after subscriber dedup
//     (§4.4-C dedups on the transition-unique ID, so a duplicated ID silently
//     DROPS the second transition).
func TestLifecycleEmitsExactlyOneEventPerTransition(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// After create, exactly ONE event — not two. A "created" for the `creating`
	// insert plus another for the commit would double-announce the sandbox.
	if got := kindsFor(t, st, sid); len(got) != 1 || got[0] != "created" {
		t.Fatalf("after create, events = %v; want exactly [created] — the transient `creating` insert must publish nothing", got)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []string{"created", "paused", "resumed", "destroyed"}
	got := kindsFor(t, st, sid)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v; want %v (§4.4-C: one event per §4.1 transition, none extra, none missing)", got, want)
	}

	// Distinct transition IDs. Subscribers dedup on this ID (§4.4-C), so two
	// transitions sharing one would silently collapse into a single delivery.
	evs, _ := st.UnpublishedEvents(ctx)
	seen := map[string]string{}
	for _, e := range evs {
		if e.Sandbox != sid {
			continue
		}
		if prev, dup := seen[e.ID]; dup {
			t.Fatalf("events %q and %q share transition ID %q — subscriber dedup would DROP the second (§4.4-C)", prev, e.Kind, e.ID)
		}
		seen[e.ID] = e.Kind
	}

	// The destroyed event OUTLIVES the deleted record (§4.4-C): the record is gone
	// but the outbox row must remain for the publisher to drain.
	if _, err := st.GetSandbox(ctx, sid); err == nil {
		t.Fatal("record still present after destroy")
	}
	if got := kindsFor(t, st, sid); len(got) != 4 {
		t.Fatalf("after the record was deleted, %d event(s) remain; want all 4 (the outbox row outlives the record, §4.4-C)", len(got))
	}
}

// A FAILED create publishes nothing at all. The unwind path deletes the record
// with a nil Event on purpose: a subscriber must never see a "destroyed" for a
// sandbox it never saw "created" (an unpaired teardown in a billing ledger).
func TestFailedCreatePublishesNothing(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{failLaunch: true}
	m, st := newTestMediator(t, rp, rt, true)

	if _, err := m.Create(ctx, CreateSpec{Template: "bare"}); err == nil {
		t.Fatal("Create should have failed at launch")
	}
	evs, _ := st.UnpublishedEvents(ctx)
	if len(evs) != 0 {
		var kinds []string
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		t.Fatalf("a failed create published %v; want nothing — an unpaired event for a sandbox that never became visible", kinds)
	}
}

// Destroying a `creating` record publishes NOTHING — the same rule reconcile
// applies when it cleans up a crashed create, now on the operator-driven path.
//
// The record is reachable: a crash mid-create leaves `creating`, and if reconcile's
// cleanup delete then fails it survives for an operator to destroy later. Such a
// record never emitted `created` (Create publishes only once its chain commits), so
// a `destroyed` here would tell a billing subscriber a sandbox ended that it never
// saw begin. Two paths, one invariant — they must not disagree.
func TestDestroyFromCreatingPublishesNothing(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	// A wedged `creating` row, as a failed post-crash cleanup would leave behind.
	const sid = model.SandboxID("sbx_wedged")
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: sid, Template: "bare", Overlay: "ov_wedged", State: model.Creating,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed creating record: %v", err)
	}
	if err := m.Destroy(ctx, sid); err != nil {
		t.Fatalf("Destroy of a wedged creating record: %v — an operator must be able "+
			"to clean it up", err)
	}
	// Gone from the store...
	if _, err := st.GetSandbox(ctx, sid); err == nil {
		t.Error("record survived Destroy")
	}
	// ...and it published nothing.
	evs, _ := st.UnpublishedEvents(ctx)
	for _, e := range evs {
		if e.Sandbox == sid {
			t.Errorf("destroying a `creating` record published %q — unpaired terminal: "+
				"this sandbox never emitted `created`", e.Kind)
		}
	}
}

// UpdateBinding is NOT a §4.1 transition (§3.4 / §6.1 PATCH): the state does not
// change, so it must publish nothing. A PATCH that emitted an event would make
// every cap edit look like a lifecycle transition to a subscriber counting them.
func TestUpdateBindingPublishesNothing(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(ctx, CreateSpec{Template: "bare", Limits: model.Limits{MemoryByte: 128 << 20}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	before := kindsFor(t, st, sid)

	bigger := int64(512 << 20)
	if err := m.UpdateBinding(ctx, sid, store.Binding{MemoryByte: &bigger}); err != nil {
		t.Fatalf("UpdateBinding: %v", err)
	}
	after := kindsFor(t, st, sid)
	if len(after) != len(before) {
		t.Fatalf("PATCH emitted an event (%v → %v); a Binding rewrite is not a §4.1 transition", before, after)
	}
}

// Promote is not a §4.1 transition either, but it is not silent: quiescing a
// `running` source IS a real pause, and it is published as an ordinary "paused"
// (lifecycle.go's Promote: "from the event stream's view this is an ordinary
// pause"). So the event count grows by exactly one, and that one says "paused" —
// not a bespoke "promoted" Kind the §4.1 state machine does not name.
func TestPromoteFromRunningPublishesOnlyItsQuiescePause(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)

	sid, err := m.Create(ctx, CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Promote(ctx, sid); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	want := []string{"created", "paused"}
	if got := kindsFor(t, st, sid); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("promote-from-running events = %v; want %v (the quiesce is an ordinary pause; Promote itself is not a transition)", got, want)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Paused {
		t.Fatalf("source state = %s; want paused (Promote leaves the source at rest)", stt)
	}
}

// Promoting an ALREADY-paused source publishes nothing: there is no transition to
// report, since the sandbox was already at rest.
func TestPromoteFromPausedPublishesNothing(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	rp := &fakeRootfs{}
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: rp,
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
	})
	seedPaused(t, st, "sbx_pp", "ov_pp", "bare")

	if _, err := m.Promote(ctx, "sbx_pp"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := kindsFor(t, st, "sbx_pp"); len(got) != 0 {
		t.Fatalf("promote of an already-paused sandbox published %v; want nothing (no state changed)", got)
	}
}
