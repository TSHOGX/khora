package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
)

func ev(id, evid, kind string, state model.Lifecycle) *store.Event {
	return &store.Event{ID: evid, Sandbox: model.SandboxID(id), Kind: kind, State: state, OccurredAt: time.Now()}
}

// seedPubTmpl inserts the "bare" Template these publisher tests reference, so
// the sandbox insert satisfies the §4.3 template FK (the publisher behavior
// under test is unchanged; the seed just makes the referenced row present).
func seedPubTmpl(t *testing.T, st store.Store) {
	t.Helper()
	if err := st.CreateTemplate(context.Background(), model.Template{ID: "bare", Builtin: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
}

// countingSink records every Deliver call (including redeliveries).
type countingSink struct {
	mu       sync.Mutex
	delivers []string // event IDs, in order, once per Deliver
	failNext error    // if set, the next Deliver fails and clears it
}

func (s *countingSink) Deliver(_ context.Context, e store.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	s.delivers = append(s.delivers, e.ID)
	return nil
}
func (s *countingSink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivers...)
}

// happy path: every unpublished event is delivered once and marked.
func TestPublisherDeliversAndMarks(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedPubTmpl(t, st)
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running))
	_ = st.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "e2", "paused", model.Paused))

	sink := &countingSink{}
	p := store.NewPublisher(st, sink)
	n, err := p.PublishOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("PublishOnce n=%d err=%v; want 2,nil", n, err)
	}
	// nothing left unpublished.
	un, _ := st.UnpublishedEvents(ctx)
	if len(un) != 0 {
		t.Fatalf("%d rows still unpublished", len(un))
	}
	// a second pass delivers nothing new.
	n, _ = p.PublishOnce(ctx)
	if n != 0 {
		t.Fatalf("second pass delivered %d; want 0", n)
	}
	if got := sink.ids(); len(got) != 2 {
		t.Fatalf("delivered %v; want 2 distinct", got)
	}
}

// a failing sink leaves the row unpublished; a later pass retries it (the flaky
// sink cannot drop an event — at-least-once).
func TestPublisherRetriesOnSinkFailure(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedPubTmpl(t, st)
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running))

	sink := &countingSink{failNext: errors.New("sink down")}
	p := store.NewPublisher(st, sink)
	if _, err := p.PublishOnce(ctx); err == nil {
		t.Fatal("want delivery error on first pass")
	}
	if un, _ := st.UnpublishedEvents(ctx); len(un) != 1 {
		t.Fatalf("failed delivery should stay unpublished; got %d", len(un))
	}
	// sink recovered → retry delivers it.
	n, err := p.PublishOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry n=%d err=%v; want 1,nil", n, err)
	}
}

// inject "delivered but crashed BEFORE MarkPublished" on a durable store,
// then restart → the same transition ID is re-delivered → the DedupSink forwards
// it only once, so a billing-style handler never double-counts. Redeliver, not
// drop, and dedup absorbs the redelivery.
func TestPublisherAtLeastOnceDedupOnRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	st, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// two committed transitions with durable outbox rows.
	if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running))
	_ = st.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "e2", "paused", model.Paused))

	// count how many times the dedup handler actually fires (billing semantics).
	var mu sync.Mutex
	counts := map[string]int{}
	dedup := store.NewDedupSink(func(e store.Event) {
		mu.Lock()
		counts[e.ID]++
		mu.Unlock()
	})

	// --- pre-crash: deliver e1 (dedup counts it), then CRASH before marking it.
	// Simulate by delivering through the dedup sink directly, WITHOUT calling
	// MarkPublished — exactly the "delivered, not yet marked" window.
	un, _ := st.UnpublishedEvents(ctx)
	if len(un) != 2 {
		t.Fatalf("want 2 unpublished pre-crash; got %d", len(un))
	}
	if err := dedup.Deliver(ctx, un[0]); err != nil { // e1 delivered...
		t.Fatal(err)
	}
	// ...crash here: e1 NOT marked. Simulate restart by reopening the store.
	_ = st.Close()
	st2, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	// after restart both e1 and e2 are STILL unpublished (durable, not dropped).
	un2, _ := st2.UnpublishedEvents(ctx)
	if len(un2) != 2 {
		t.Fatalf("want 2 unpublished after restart (nothing dropped); got %d", len(un2))
	}
	// the SAME dedup sink (a subscriber survives the engine restart) re-drains.
	p := store.NewPublisher(st2, dedup)
	if _, err := p.PublishOnce(ctx); err != nil {
		t.Fatalf("post-restart publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// e1 was delivered twice (pre-crash + post-restart) but counted ONCE.
	if counts["e1"] != 1 {
		t.Fatalf("e1 counted %d times; want exactly 1 (dedup absorbs redelivery)", counts["e1"])
	}
	if counts["e2"] != 1 {
		t.Fatalf("e2 counted %d times; want exactly 1", counts["e2"])
	}
}

// DedupSink absorbs a redelivery of the same ID within one process too.
func TestDedupSinkIdempotent(t *testing.T) {
	ctx := context.Background()
	var n int
	d := store.NewDedupSink(func(store.Event) { n++ })
	e := store.Event{ID: "x"}
	_ = d.Deliver(ctx, e)
	_ = d.Deliver(ctx, e)
	_ = d.Deliver(ctx, e)
	if n != 1 {
		t.Fatalf("handler fired %d times; want 1", n)
	}
}
