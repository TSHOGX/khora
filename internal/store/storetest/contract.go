// Package storetest holds the store.Store CONTRACT test suite — one set of
// assertions every backend must pass (§1.3 Behavior seam / §4.4-C). Memory and
// SQLite both call RunContract, so "the two backends run the SAME assertions"
// is literally true, and the §4.4-C promise ("state change + outbox append =
// single atomic commit") is proven identically on both — no per-backend fork
// of the correctness checks (§4.4-C 铁律).
//
// It lives in its own package (not store_test) so the SQLite backend's test
// file — in package store_test — can import and run it too without an import
// cycle.
package storetest

import (
	"context"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"
)

// Factory builds a fresh, empty Store for one subtest. The bool return says
// whether the backend is durable across a "reopen" (SQLite yes, Memory no); a
// nil reopen func means "not durable". reopen returns a NEW Store handle over
// the SAME backing state (e.g. same SQLite file) to simulate a process restart.
type Factory func(t *testing.T) (s store.Store, reopen func(t *testing.T) store.Store)

// RunContract runs the full shared suite against the backend built by mk.
func RunContract(t *testing.T, mk Factory) {
	t.Run("CreateGetConflict", func(t *testing.T) { testCreateGetConflict(t, mk) })
	t.Run("CASPreconditions", func(t *testing.T) { testCASPreconditions(t, mk) })
	t.Run("DeleteIdempotentShape", func(t *testing.T) { testDelete(t, mk) })
	t.Run("List", func(t *testing.T) { testList(t, mk) })
	t.Run("OutboxAtomicWithState", func(t *testing.T) { testOutboxAtomic(t, mk) })
	t.Run("OutboxNilPublishesNothing", func(t *testing.T) { testOutboxNil(t, mk) })
	t.Run("MarkPublishedIdempotent", func(t *testing.T) { testMarkPublished(t, mk) })
	t.Run("OutboxRowOutlivesRecord", func(t *testing.T) { testOutboxOutlivesRecord(t, mk) })
	t.Run("Durability", func(t *testing.T) { testDurability(t, mk) })
}

func mkRec(id string) model.Sandbox {
	now := time.Now()
	return model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Creating, CreatedAt: now, UpdatedAt: now,
	}
}

func ev(id, evid, kind string, state model.Lifecycle) *store.Event {
	return &store.Event{ID: evid, Sandbox: model.SandboxID(id), Kind: kind, State: state, OccurredAt: time.Now()}
}

func testCreateGetConflict(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	if err := s.CreateSandbox(ctx, mkRec("a"), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetSandbox(ctx, "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "a" || got.State != model.Creating || got.Overlay != "ov_a" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// duplicate ID → ErrConflict.
	if err := s.CreateSandbox(ctx, mkRec("a"), nil); err != store.ErrConflict {
		t.Fatalf("duplicate create = %v; want ErrConflict", err)
	}
	// missing → ErrNotFound.
	if _, err := s.GetSandbox(ctx, "nope"); err != store.ErrNotFound {
		t.Fatalf("get missing = %v; want ErrNotFound", err)
	}
}

func testCASPreconditions(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	// wrong expected → ErrConflict, state unchanged.
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused, nil); err != store.ErrConflict {
		t.Fatalf("CAS wrong-expected = %v; want ErrConflict", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.State != model.Creating {
		t.Fatalf("state changed on failed CAS: %s", got.State)
	}
	// right expected → succeeds.
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Running, nil); err != nil {
		t.Fatalf("CAS right-expected: %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.State != model.Running {
		t.Fatalf("state = %s; want running", got.State)
	}
	// CAS on missing → ErrNotFound.
	if err := s.CompareAndSetState(ctx, "nope", model.Running, model.Paused, nil); err != store.ErrNotFound {
		t.Fatalf("CAS missing = %v; want ErrNotFound", err)
	}
}

func testDelete(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	if err := s.DeleteSandbox(ctx, "a", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSandbox(ctx, "a"); err != store.ErrNotFound {
		t.Fatalf("get after delete = %v; want ErrNotFound", err)
	}
	// deleting a missing record → ErrNotFound (mediator maps to idempotent nil).
	if err := s.DeleteSandbox(ctx, "a", nil); err != store.ErrNotFound {
		t.Fatalf("delete missing = %v; want ErrNotFound", err)
	}
}

func testList(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	if l, _ := s.ListSandboxes(ctx); len(l) != 0 {
		t.Fatalf("empty list len=%d", len(l))
	}
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CreateSandbox(ctx, mkRec("b"), nil)
	l, err := s.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(l) != 2 {
		t.Fatalf("list len=%d; want 2", len(l))
	}
	seen := map[model.SandboxID]bool{}
	for _, r := range l {
		seen[r.ID] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("list missing ids: %+v", l)
	}
}

// testOutboxAtomic is the §4.4-C heart: a state change carrying an Event lands
// BOTH the new state and the outbox row; the row is queryable via
// UnpublishedEvents with the exact fields the mediator wrote.
func testOutboxAtomic(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	e := ev("a", "evt_1", "resumed", model.Running)
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Running, e); err != nil {
		t.Fatalf("CAS+event: %v", err)
	}
	// state committed.
	if got, _ := s.GetSandbox(ctx, "a"); got.State != model.Running {
		t.Fatalf("state = %s; want running", got.State)
	}
	// event committed in the same call.
	un, err := s.UnpublishedEvents(ctx)
	if err != nil {
		t.Fatalf("unpublished: %v", err)
	}
	if len(un) != 1 {
		t.Fatalf("unpublished len=%d; want 1", len(un))
	}
	if un[0].ID != "evt_1" || un[0].Kind != "resumed" || un[0].Sandbox != "a" || un[0].State != model.Running {
		t.Fatalf("event fields mismatch: %+v", un[0])
	}
	// a FAILED CAS must NOT append its event (atomicity: no state change, no row).
	if err := s.CompareAndSetState(ctx, "a", model.Paused, model.Destroyed, ev("a", "evt_bad", "destroyed", model.Destroyed)); err != store.ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	un, _ = s.UnpublishedEvents(ctx)
	if len(un) != 1 {
		t.Fatalf("failed CAS leaked an outbox row: %d rows", len(un))
	}
}

func testOutboxNil(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	// nil Event on every mutation → nothing to publish.
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, nil)
	_ = s.DeleteSandbox(ctx, "a", nil)
	un, err := s.UnpublishedEvents(ctx)
	if err != nil {
		t.Fatalf("unpublished: %v", err)
	}
	if len(un) != 0 {
		t.Fatalf("nil events still published %d rows", len(un))
	}
}

func testMarkPublished(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "evt_1", "created", model.Running))
	_ = s.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "evt_2", "paused", model.Paused))

	if un, _ := s.UnpublishedEvents(ctx); len(un) != 2 {
		t.Fatalf("want 2 unpublished, got %d", len(un))
	}
	// mark one → it drops out, the other remains.
	if err := s.MarkPublished(ctx, "evt_1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	un, _ := s.UnpublishedEvents(ctx)
	if len(un) != 1 || un[0].ID != "evt_2" {
		t.Fatalf("after mark: %+v; want only evt_2", un)
	}
	// idempotent: re-marking the same ID, and marking an unknown ID, are no-ops.
	if err := s.MarkPublished(ctx, "evt_1"); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if err := s.MarkPublished(ctx, "does_not_exist"); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if un, _ := s.UnpublishedEvents(ctx); len(un) != 1 {
		t.Fatalf("idempotent mark changed count: %d", len(un))
	}
}

// testOutboxOutlivesRecord: destroy deletes the record but its "destroyed"
// event must remain in the outbox for the publisher (§4.4-C).
func testOutboxOutlivesRecord(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	if err := s.DeleteSandbox(ctx, "a", ev("a", "evt_d", "destroyed", model.Destroyed)); err != nil {
		t.Fatalf("delete+event: %v", err)
	}
	if _, err := s.GetSandbox(ctx, "a"); err != store.ErrNotFound {
		t.Fatalf("record should be gone: %v", err)
	}
	un, _ := s.UnpublishedEvents(ctx)
	if len(un) != 1 || un[0].ID != "evt_d" {
		t.Fatalf("destroyed event missing from outbox: %+v", un)
	}
}

// testDurability: for durable backends, records AND unpublished outbox rows
// survive a reopen (process restart) — the substrate for §4.4-A reconciliation
// and B2 redelivery. Memory (reopen == nil) skips.
func testDurability(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "evt_1", "created", model.Running))

	s2 := reopen(t)
	got, err := s2.GetSandbox(ctx, "a")
	if err != nil {
		t.Fatalf("record lost across reopen: %v", err)
	}
	if got.State != model.Running {
		t.Fatalf("state lost across reopen: %s", got.State)
	}
	un, _ := s2.UnpublishedEvents(ctx)
	if len(un) != 1 || un[0].ID != "evt_1" {
		t.Fatalf("unpublished event lost across reopen: %+v", un)
	}
	// marking published also survives.
	_ = s2.MarkPublished(ctx, "evt_1")
	s3 := reopen(t)
	if un, _ := s3.UnpublishedEvents(ctx); len(un) != 0 {
		t.Fatalf("published mark lost across reopen: %d rows", len(un))
	}
}
