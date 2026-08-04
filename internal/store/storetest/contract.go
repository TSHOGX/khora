// Package storetest holds the store.Store CONTRACT test suite — one set of
// assertions every backend must pass (§1.3 Behavior seam / §4.4-C). Memory and
// SQLite both call RunContract, so "the two backends run the SAME assertions"
// is literally true, and the §4.4-C promise ("state change + outbox append =
// single atomic commit") is proven identically on both — no per-backend fork
// of the correctness checks (§4.4-C).
//
// It lives in its own package (not store_test) so the SQLite backend's test
// file — in package store_test — can import and run it too without an import
// cycle.
package storetest

import (
	"context"
	"fmt"
	"sync"
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

	// Death cause rides the CAS (§4.3 / §4.4-C): the demotion and the cause are
	// ONE commit, so the cause write is part of the SAME contract both backends
	// must honor — not a SQLite detail.
	t.Run("CASPersistsDeathCause", func(t *testing.T) { testCASPersistsDeathCause(t, mk) })
	t.Run("CASNilCauseLeavesDeathCause", func(t *testing.T) { testCASNilCauseLeavesDeathCause(t, mk) })
	t.Run("CASCauseNoneClearsDeathCause", func(t *testing.T) { testCASCauseNoneClears(t, mk) })

	// UpdateBinding (§3.4 / §6.1 PATCH): the create-time write point's only
	// sibling. Its semantics are contract, not backend detail — omitted-means-
	// unchanged, conditional on state, and it must NOT move updated_at.
	t.Run("UpdateBindingPartialAndConditional", func(t *testing.T) { testUpdateBinding(t, mk) })

	t.Run("OutboxNilPublishesNothing", func(t *testing.T) { testOutboxNil(t, mk) })
	t.Run("MarkPublishedIdempotent", func(t *testing.T) { testMarkPublished(t, mk) })
	t.Run("OutboxRowOutlivesRecord", func(t *testing.T) { testOutboxOutlivesRecord(t, mk) })
	t.Run("Durability", func(t *testing.T) { testDurability(t, mk) })

	// Template object model (§4.3): same assertions both backends.
	t.Run("TemplateCRUD", func(t *testing.T) { testTemplateCRUD(t, mk) })
	t.Run("TemplateReferenceProtection", func(t *testing.T) { testTemplateRefProtection(t, mk) })
	t.Run("TemplateLineageProtection", func(t *testing.T) { testTemplateLineage(t, mk) })
	t.Run("SandboxRefMissingTemplate", func(t *testing.T) { testSandboxRefMissingTemplate(t, mk) })
	t.Run("TemplateDurability", func(t *testing.T) { testTemplateDurability(t, mk) })
	t.Run("ReferenceReclaimRace", func(t *testing.T) { testReferenceReclaimRace(t, mk) })

	// §4.4-C under CONCURRENT writers. Every other subtest here is
	// single-threaded, so a backend whose CAS is not genuinely atomic — read,
	// then write, without holding anything across the two — would pass all of
	// them. Both backends must deliver this identically (§1.3 seam).
	t.Run("ConcurrentCASSingleWinner", func(t *testing.T) { testConcurrentCAS(t, mk) })
}

// testConcurrentCAS: N goroutines race the SAME transition. Exactly ONE may win,
// and exactly ONE outbox row may land.
//
// This is the §4.4-B CAS promise ("当前 lifecycle-state ≠ 期望源态则拒绝") and the
// §4.4-C atomic-commit promise met at the same moment: a torn implementation
// admits several winners, and each winner appends an event — so a single
// transition would be billed N times, which is precisely the double-count
// subscriber dedup CANNOT absorb (the events carry DIFFERENT transition IDs).
//
// The mediator's own transition lock would normally serialize these, which is
// exactly why the check belongs here: the store must not depend on that lock for
// its own contract, or a second caller of the seam inherits a race.
func testConcurrentCAS(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")

	const trials = 50
	const racers = 8
	for i := 0; i < trials; i++ {
		id := fmt.Sprintf("cas_%d", i)
		if err := s.CreateSandbox(ctx, mkRec(id), nil); err != nil {
			t.Fatalf("trial %d: create: %v", i, err)
		}

		var (
			wg    sync.WaitGroup
			start = make(chan struct{}) // release together, for maximum contention
			mu    sync.Mutex
			wins  int
			errs  []error
		)
		for j := 0; j < racers; j++ {
			wg.Add(1)
			evID := fmt.Sprintf("evt_%d_%d", i, j)
			go func() {
				defer wg.Done()
				<-start
				// All racers attempt the IDENTICAL transition creating→running, each
				// carrying its OWN event id.
				err := s.CompareAndSetState(ctx, model.SandboxID(id),
					model.Creating, model.Running,
					ev(id, evID, "created", model.Running), nil)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					wins++
				} else {
					errs = append(errs, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if wins != 1 {
			t.Fatalf("trial %d: %d of %d racers won the SAME creating→running CAS; want exactly 1 — a non-atomic CAS lets several callers each believe they performed the transition (§4.4-B)", i, wins, racers)
		}
		// Every loser must lose CLEANLY, with the contract's sentinel.
		for _, err := range errs {
			if err != store.ErrConflict {
				t.Fatalf("trial %d: loser got %v; want ErrConflict — a CAS that lost must say so, not fail opaquely", i, err)
			}
		}
		// Exactly ONE outbox row for this sandbox. N rows would bill one transition
		// N times, and their IDs differ so subscriber dedup could not collapse them.
		un, err := s.UnpublishedEvents(ctx)
		if err != nil {
			t.Fatalf("trial %d: unpublished: %v", i, err)
		}
		rows := 0
		for _, e := range un {
			if e.Sandbox == model.SandboxID(id) {
				rows++
			}
		}
		if rows != 1 {
			t.Fatalf("trial %d: %d outbox rows for one transition; want exactly 1 — the losing CAS must append NOTHING (§4.4-C: state change + outbox append are one commit, so a rejected transition writes neither)", i, rows)
		}
		if got, _ := s.GetSandbox(ctx, model.SandboxID(id)); got.State != model.Running {
			t.Fatalf("trial %d: state = %s; want running", i, got.State)
		}
	}
}

// seedTmpl inserts a base (parent-less) Template so a sandbox referencing it
// satisfies the §4.3 FK. Every sandbox-creating subtest calls it first: the
// sandboxes.template FK REQUIRES the row to pre-exist (§6 premise / boot seed).
func seedTmpl(t *testing.T, s store.Store, id string) {
	t.Helper()
	if err := s.CreateTemplate(context.Background(), model.Template{
		ID: model.TemplateID(id), Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template %q: %v", id, err)
	}
}

func mkRec(id string) model.Sandbox {
	now := time.Now()
	return model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Creating,
		// TTL Binding (§3.4 / §4.3): non-zero, distinct values so the round-trip
		// assertion catches a column swapped or dropped.
		IdleTimeout: 30 * time.Minute, Retention: 12 * time.Hour,
		// Limits Binding (§3.4): same discipline — non-zero and pairwise distinct
		// (and distinct from the TTL nanos above) so a cpu/memory/disk column
		// swapped or dropped is caught on read-back. The three axes are three
		// mechanisms with three different overrun consequences, never
		// interchangeable knobs (§3.4), so a silent swap is a real bug class.
		Limits:    model.Limits{CPUMillis: 1500, MemoryByte: 256 << 20, DiskByte: 512 << 20},
		CreatedAt: now, UpdatedAt: now,
	}
}

// wantLimits mirrors mkRec's Limits Binding — the single place the expected
// values live, so GetSandbox and ListSandboxes assert against one source.
var wantLimits = model.Limits{CPUMillis: 1500, MemoryByte: 256 << 20, DiskByte: 512 << 20}

// assertLimits checks the §3.4 Limits Binding survived a round-trip intact.
// Compared as a whole struct: a swap between two axes leaves each field
// individually "plausible", so only the joint value is a real guard.
func assertLimits(t *testing.T, where string, got model.Limits) {
	t.Helper()
	if got != wantLimits {
		t.Fatalf("%s: Limits Binding roundtrip mismatch: got %+v; want %+v", where, got, wantLimits)
	}
}

func ev(id, evid, kind string, state model.Lifecycle) *store.Event {
	return &store.Event{ID: evid, Sandbox: model.SandboxID(id), Kind: kind, State: state, OccurredAt: time.Now()}
}

func testCreateGetConflict(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
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
	// TTL Binding survives the round-trip (§3.4: stored as the concrete Binding).
	if got.IdleTimeout != 30*time.Minute || got.Retention != 12*time.Hour {
		t.Fatalf("TTL Binding roundtrip mismatch: idle=%s retention=%s; want 30m/12h", got.IdleTimeout, got.Retention)
	}
	// Limits Binding likewise (§3.4): re-applied at every placement FROM this
	// record, so a wrong read-back silently mis-caps the sandbox.
	assertLimits(t, "GetSandbox", got.Limits)
	// A fresh record has no death cause — CauseNone is the resting value, the
	// absence of a death rather than a death (§4.3).
	if got.DeathCause != model.CauseNone {
		t.Fatalf("fresh record DeathCause = %q; want CauseNone", got.DeathCause)
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
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	// wrong expected → ErrConflict, state unchanged.
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused, nil, nil); err != store.ErrConflict {
		t.Fatalf("CAS wrong-expected = %v; want ErrConflict", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.State != model.Creating {
		t.Fatalf("state changed on failed CAS: %s", got.State)
	}
	// right expected → succeeds.
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Running, nil, nil); err != nil {
		t.Fatalf("CAS right-expected: %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.State != model.Running {
		t.Fatalf("state = %s; want running", got.State)
	}
	// CAS on missing → ErrNotFound.
	if err := s.CompareAndSetState(ctx, "nope", model.Running, model.Paused, nil, nil); err != store.ErrNotFound {
		t.Fatalf("CAS missing = %v; want ErrNotFound", err)
	}
}

func testDelete(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
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
	seedTmpl(t, s, "bare")
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
		// The list read path carries the SAME Binding as the single-record read
		// (§3.4): reconcile lists records at boot and re-materializes caps from
		// them, so a column dropped only from the list SELECT would mis-cap every
		// sandbox after a restart.
		assertLimits(t, "ListSandboxes "+string(r.ID), r.Limits)
		if r.DeathCause != model.CauseNone {
			t.Fatalf("ListSandboxes %s: DeathCause = %q; want CauseNone", r.ID, r.DeathCause)
		}
		if r.IdleTimeout != 30*time.Minute || r.Retention != 12*time.Hour {
			t.Fatalf("ListSandboxes %s: TTL Binding mismatch: idle=%s retention=%s; want 30m/12h", r.ID, r.IdleTimeout, r.Retention)
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("list missing ids: %+v", l)
	}
}

// testCASPersistsDeathCause: a non-nil cause is written by the SAME
// CompareAndSetState call that changes state (§4.3 / §4.4-C) — see that method's
// contract for why no later window exists.
func testCASPersistsDeathCause(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, nil, nil)

	// the crash-paused demotion: running → paused carrying WHY (§4.1 / §4.3).
	cause := model.CauseGone
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused,
		ev("a", "evt_crash", "crash-paused", model.Paused), &cause); err != nil {
		t.Fatalf("CAS+cause: %v", err)
	}
	got, err := s.GetSandbox(ctx, "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// state AND cause both committed — one commit, never one without the other.
	if got.State != model.Paused {
		t.Fatalf("state = %s; want paused", got.State)
	}
	if got.DeathCause != model.CauseGone {
		t.Fatalf("DeathCause = %q; want %q", got.DeathCause, model.CauseGone)
	}
	// the event landed in the same commit too (§4.4-C).
	if un, _ := s.UnpublishedEvents(ctx); len(un) != 1 || un[0].ID != "evt_crash" {
		t.Fatalf("crash-paused event missing from outbox: %+v", un)
	}
	// resource-exceeded is the other disposition (§4.3: do NOT resume) and must
	// round-trip as its own distinct value, not collapse into "gone".
	rex := model.CauseResourceExceeded
	if err := s.CompareAndSetState(ctx, "a", model.Paused, model.Running, nil, &rex); err != nil {
		t.Fatalf("CAS+resource-exceeded: %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("DeathCause = %q; want %q", got.DeathCause, model.CauseResourceExceeded)
	}
	// the Limits Binding is untouched by a cause-carrying CAS — the UPDATE must
	// not clobber neighbouring columns (§3.4: caps are stable across resume).
	if got, _ := s.GetSandbox(ctx, "a"); got.Limits != wantLimits {
		t.Fatalf("CAS clobbered the Limits Binding: %+v; want %+v", got.Limits, wantLimits)
	}
}

// testCASNilCauseLeavesDeathCause: nil cause = leave death_cause UNTOUCHED
// (store.CompareAndSetState doc / §4.3). Every non-death transition passes nil,
// so a nil that silently cleared would erase recorded evidence: a user pause or
// an auto-pause landing on a sandbox already known dead would destroy the fact
// that separates dead-recoverable from resource-terminated.
func testCASNilCauseLeavesDeathCause(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	// record a cause first.
	rex := model.CauseResourceExceeded
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Paused, nil, &rex); err != nil {
		t.Fatalf("seed cause: %v", err)
	}
	// a nil-cause transition (user pause / auto-pause / resume attempt) must not
	// touch it, even across several hops.
	if err := s.CompareAndSetState(ctx, "a", model.Paused, model.Running, nil, nil); err != nil {
		t.Fatalf("CAS nil cause: %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("nil cause erased DeathCause: got %q; want %q", got.DeathCause, model.CauseResourceExceeded)
	}
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused, nil, nil); err != nil {
		t.Fatalf("CAS nil cause (2): %v", err)
	}
	got, _ := s.GetSandbox(ctx, "a")
	if got.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("nil cause erased DeathCause across hops: got %q; want %q", got.DeathCause, model.CauseResourceExceeded)
	}
	// state still tracks normally — "leave the cause alone" is not "ignore the CAS".
	if got.State != model.Paused {
		t.Fatalf("state = %s; want paused", got.State)
	}
}

// testCASCauseNoneClears: a non-nil CauseNone CLEARS death_cause — the
// successful-resume path (§4.3: a sandbox that came back up is no longer
// explained by its previous death). CauseNone is the empty string, so "nil
// leaves untouched" vs "&CauseNone clears" is a pointer-vs-value distinction the
// contract depends on and a backend could easily collapse.
func testCASCauseNoneClears(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	unknown := model.CauseUnknown
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Paused, nil, &unknown); err != nil {
		t.Fatalf("seed cause: %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.DeathCause != model.CauseUnknown {
		t.Fatalf("seeded DeathCause = %q; want %q", got.DeathCause, model.CauseUnknown)
	}
	// the resume: paused → running clearing the cause in the same commit.
	none := model.CauseNone
	if err := s.CompareAndSetState(ctx, "a", model.Paused, model.Running,
		ev("a", "evt_resumed", "resumed", model.Running), &none); err != nil {
		t.Fatalf("CAS+CauseNone: %v", err)
	}
	got, err := s.GetSandbox(ctx, "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeathCause != model.CauseNone {
		t.Fatalf("&CauseNone did not clear DeathCause: got %q", got.DeathCause)
	}
	if got.State != model.Running {
		t.Fatalf("state = %s; want running", got.State)
	}
}

// testOutboxAtomic is the §4.4-C heart: a state change carrying an Event lands
// BOTH the new state and the outbox row; the row is queryable via
// UnpublishedEvents with the exact fields the mediator wrote.
func testOutboxAtomic(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	e := ev("a", "evt_1", "resumed", model.Running)
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Running, e, nil); err != nil {
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
	// A FAILED CAS must write NOTHING — this is the §4.4-C property in its
	// strongest form: the call carries an event AND a death cause, and losing the
	// precondition must leave all three (state, cause, outbox) exactly as they
	// were. A backend that writes the cause before checking the precondition
	// would record a death that never happened.
	badCause := model.CauseResourceExceeded
	if err := s.CompareAndSetState(ctx, "a", model.Paused, model.Destroyed,
		ev("a", "evt_bad", "destroyed", model.Destroyed), &badCause); err != store.ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	un, _ = s.UnpublishedEvents(ctx)
	if len(un) != 1 {
		t.Fatalf("failed CAS leaked an outbox row: %d rows", len(un))
	}
	after, err := s.GetSandbox(ctx, "a")
	if err != nil {
		t.Fatalf("get after failed CAS: %v", err)
	}
	if after.State != model.Running {
		t.Fatalf("failed CAS changed state: %s; want running", after.State)
	}
	if after.DeathCause != model.CauseNone {
		t.Fatalf("failed CAS persisted a death cause: %q; want CauseNone", after.DeathCause)
	}

	// The mirror case: a failed CAS must also not ERASE an already-recorded
	// cause. Record one via a winning CAS, then lose a CAS carrying &CauseNone —
	// the clear must roll back with everything else.
	rex := model.CauseResourceExceeded
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused, nil, &rex); err != nil {
		t.Fatalf("seed cause: %v", err)
	}
	none := model.CauseNone
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Running, nil, &none); err != store.ErrConflict {
		t.Fatalf("expected ErrConflict on stale expected, got %v", err)
	}
	if got, _ := s.GetSandbox(ctx, "a"); got.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("failed CAS cleared the recorded cause: %q; want %q", got.DeathCause, model.CauseResourceExceeded)
	}
}

func testOutboxNil(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	// nil Event on every mutation → nothing to publish.
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, nil, nil)
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
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "evt_1", "created", model.Running), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "evt_2", "paused", model.Paused), nil)

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
	seedTmpl(t, s, "bare")
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
// and redelivery. Memory (reopen == nil) skips.
func testDurability(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	_ = s.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "evt_1", "created", model.Running), nil)

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

// testTemplateCRUD: round-trip a Template + its fields, duplicate → ErrConflict,
// missing → ErrTemplateNotFound, delete an unreferenced row succeeds and is then
// gone (§4.3 existence-is-state).
func testTemplateCRUD(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	tm := model.Template{
		ID: "tmpl_base", Builtin: true,
		CreatedAt: time.Now(),
	}
	if err := s.CreateTemplate(ctx, tm); err != nil {
		t.Fatalf("create template: %v", err)
	}
	got, err := s.GetTemplate(ctx, "tmpl_base")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got.ID != "tmpl_base" || got.Parent != "" || !got.Builtin {
		t.Fatalf("template round-trip mismatch: %+v", got)
	}
	// duplicate → ErrConflict.
	if err := s.CreateTemplate(ctx, tm); err != store.ErrConflict {
		t.Fatalf("duplicate template = %v; want ErrConflict", err)
	}
	// missing → ErrTemplateNotFound.
	if _, err := s.GetTemplate(ctx, "nope"); err != store.ErrTemplateNotFound {
		t.Fatalf("get missing template = %v; want ErrTemplateNotFound", err)
	}
	// unreferenced delete succeeds, then the row is gone.
	if err := s.DeleteTemplate(ctx, "tmpl_base"); err != nil {
		t.Fatalf("delete unreferenced template: %v", err)
	}
	if _, err := s.GetTemplate(ctx, "tmpl_base"); err != store.ErrTemplateNotFound {
		t.Fatalf("get after delete = %v; want ErrTemplateNotFound", err)
	}
	// deleting a missing template → ErrTemplateNotFound.
	if err := s.DeleteTemplate(ctx, "tmpl_base"); err != store.ErrTemplateNotFound {
		t.Fatalf("delete missing template = %v; want ErrTemplateNotFound", err)
	}
}

// testTemplateRefProtection: a Template referenced by a sandbox cannot be
// deleted (§4.3 referenced-means-undeletable → ErrInUse); once the sandbox is
// gone, the delete succeeds. The reference is a persistent row, seen by RESTRICT
// — the E2B-bug immunity (§4.3).
func testTemplateRefProtection(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	if err := s.CreateSandbox(ctx, mkRec("a"), nil); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// referenced → ErrInUse.
	if err := s.DeleteTemplate(ctx, "bare"); err != store.ErrInUse {
		t.Fatalf("delete referenced template = %v; want ErrInUse", err)
	}
	// row still there (the failed delete changed nothing).
	if _, err := s.GetTemplate(ctx, "bare"); err != nil {
		t.Fatalf("template vanished on failed delete: %v", err)
	}
	// remove the referrer → now deletable.
	if err := s.DeleteSandbox(ctx, "a", nil); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if err := s.DeleteTemplate(ctx, "bare"); err != nil {
		t.Fatalf("delete after referrer gone: %v", err)
	}
}

// testTemplateLineage: a parent Template referenced by a child (parent = id)
// cannot be deleted (§4.3, references point upward → ErrInUse); delete the child
// first,
// then the parent. Also: a child with a MISSING parent cannot be created
// (reference-side atomicity).
func testTemplateLineage(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	now := time.Now()
	if err := s.CreateTemplate(ctx, model.Template{ID: "t1", Builtin: true, CreatedAt: now}); err != nil {
		t.Fatalf("create t1: %v", err)
	}
	// child with a missing parent → ErrTemplateNotFound (reference-side).
	if err := s.CreateTemplate(ctx, model.Template{ID: "t2", Parent: "ghost", CreatedAt: now}); err != store.ErrTemplateNotFound {
		t.Fatalf("child of missing parent = %v; want ErrTemplateNotFound", err)
	}
	// valid child.
	if err := s.CreateTemplate(ctx, model.Template{ID: "t2", Parent: "t1", CreatedAt: now}); err != nil {
		t.Fatalf("create t2(parent=t1): %v", err)
	}
	// parent referenced by child → ErrInUse.
	if err := s.DeleteTemplate(ctx, "t1"); err != store.ErrInUse {
		t.Fatalf("delete referenced parent = %v; want ErrInUse", err)
	}
	// delete child, then parent.
	if err := s.DeleteTemplate(ctx, "t2"); err != nil {
		t.Fatalf("delete child t2: %v", err)
	}
	if err := s.DeleteTemplate(ctx, "t1"); err != nil {
		t.Fatalf("delete parent t1 after child gone: %v", err)
	}
}

// testSandboxRefMissingTemplate: creating a sandbox that references a
// nonexistent Template fails (§4.3 reference-side atomicity: create loses when
// the parent Template is absent). The boot seed (§4.3 existence-as-state /
// §4.4-A idempotent reconcile) exists precisely to make the builtin rows present
// before any create.
func testSandboxRefMissingTemplate(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	// no seed → "bare" absent.
	if err := s.CreateSandbox(ctx, mkRec("a"), nil); err != store.ErrTemplateNotFound {
		t.Fatalf("create referencing missing template = %v; want ErrTemplateNotFound", err)
	}
}

// testTemplateDurability: Template rows survive a reopen (durable backends);
// existence-is-state means the row IS the persistent object (§4.3).
func testTemplateDurability(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	_ = s.CreateTemplate(ctx, model.Template{ID: "t1", Builtin: true, CreatedAt: time.Now()})
	_ = s.CreateTemplate(ctx, model.Template{ID: "t2", Parent: "t1", CreatedAt: time.Now()})
	s2 := reopen(t)
	got, err := s2.GetTemplate(ctx, "t2")
	if err != nil {
		t.Fatalf("template lost across reopen: %v", err)
	}
	if got.Parent != "t1" {
		t.Fatalf("lineage lost across reopen: parent=%q want t1", got.Parent)
	}
	// lineage RESTRICT still holds after reopen.
	if err := s2.DeleteTemplate(ctx, "t1"); err != store.ErrInUse {
		t.Fatalf("post-reopen delete referenced parent = %v; want ErrInUse", err)
	}
}

// testReferenceReclaimRace is the DYNAMIC half of §4.3 (model.md
// reference-reclaim atomicity):
// the static "referenced → cannot delete" judgment is not enough; the spec adds
// an invariant that closes the reclaim-vs-reference TOCTOU window. This test
// exercises that window directly — for many trials it races a DeleteTemplate
// against several CreateSandbox referrers AND a child CreateTemplate, all
// referencing the SAME Template, launched simultaneously.
//
// The invariant (per §4.3): the authoritative single store serializes reference
// and reclaim so they NEVER interleave — either
//   - the DELETE commits first → the template is gone and EVERY referencing
//     create loses with ErrTemplateNotFound (no sandbox row is left pointing at a
//     deleted template), OR
//   - a reference commits first → the DELETE loses with ErrInUse and the template
//     survives.
//
// Never both, never a torn state. Runs on BOTH backends via RunContract, so the
// SQLite FK-RESTRICT path and the Memory Go-check path prove the SAME contract
// (§1.3 seam). Run under -race.
func testReferenceReclaimRace(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)

	const (
		trials = 300
		refs   = 4 // concurrent sandbox referrers per trial
	)
	for i := 0; i < trials; i++ {
		tmplID := model.TemplateID(fmt.Sprintf("race_tmpl_%d", i))
		childID := model.TemplateID(fmt.Sprintf("race_child_%d", i))
		if err := s.CreateTemplate(ctx, model.Template{ID: tmplID, Builtin: true, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("trial %d: seed template: %v", i, err)
		}

		var (
			wg    sync.WaitGroup
			start = make(chan struct{}) // release all goroutines at once for max contention
			mu    sync.Mutex
			sbOK  []model.SandboxID // sandbox creates that returned nil
		)
		var delErr, childErr error // each written by exactly one goroutine, read after Wait

		launch := func(fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn()
			}()
		}

		// the reclaim
		launch(func() { delErr = s.DeleteTemplate(ctx, tmplID) })
		// a child Template referencing tmpl as parent (lineage reference-side)
		launch(func() {
			childErr = s.CreateTemplate(ctx, model.Template{ID: childID, Parent: tmplID, CreatedAt: time.Now()})
		})
		// sandbox referrers
		for j := 0; j < refs; j++ {
			sbID := model.SandboxID(fmt.Sprintf("race_sb_%d_%d", i, j))
			launch(func() {
				sb := mkRec(string(sbID))
				sb.Template = tmplID
				sb.Overlay = model.OverlayID("ov_" + string(sbID))
				err := s.CreateSandbox(ctx, sb, nil)
				switch {
				case err == nil:
					mu.Lock()
					sbOK = append(sbOK, sbID)
					mu.Unlock()
				case err == store.ErrTemplateNotFound:
					// clean loss: the reclaim won this reference (§4.3).
				default:
					t.Errorf("trial %d: sandbox create = %v; want nil or ErrTemplateNotFound", i, err)
				}
			})
		}

		close(start)
		wg.Wait()

		// --- outcome must be one of the two clean verdicts, never torn ---
		if delErr != nil && delErr != store.ErrInUse {
			t.Fatalf("trial %d: delete = %v; want nil or ErrInUse", i, delErr)
		}
		if childErr != nil && childErr != store.ErrTemplateNotFound {
			t.Fatalf("trial %d: child create = %v; want nil or ErrTemplateNotFound", i, childErr)
		}

		_, getErr := s.GetTemplate(ctx, tmplID)
		templateGone := getErr == store.ErrTemplateNotFound
		if !templateGone && getErr != nil {
			t.Fatalf("trial %d: get template = %v", i, getErr)
		}

		// delete succeeded IFF the template is now absent.
		if (delErr == nil) != templateGone {
			t.Fatalf("trial %d: delErr=%v but templateGone=%v (delete/existence disagree)", i, delErr, templateGone)
		}

		// If the reclaim won, NO reference may have committed: no sandbox referrer
		// succeeded and the child create must have lost. Both succeeding is the
		// TOCTOU bug the invariant forbids.
		if templateGone {
			if len(sbOK) != 0 {
				t.Fatalf("trial %d: TORN STATE: template reclaimed but %d sandbox create(s) succeeded referencing it", i, len(sbOK))
			}
			if childErr == nil {
				t.Fatalf("trial %d: TORN STATE: template reclaimed but child create succeeded referencing it", i)
			}
		}

		// No sandbox row may point at a reclaimed template, and exactly the
		// successful creates leave rows (a create that errored must leave none).
		list, err := s.ListSandboxes(ctx)
		if err != nil {
			t.Fatalf("trial %d: list sandboxes: %v", i, err)
		}
		refCount := 0
		for _, sb := range list {
			if sb.Template == tmplID {
				refCount++
			}
		}
		if templateGone && refCount != 0 {
			t.Fatalf("trial %d: TORN STATE: template deleted yet %d sandbox row(s) still reference it", i, refCount)
		}
		if refCount != len(sbOK) {
			t.Fatalf("trial %d: %d sandbox rows reference tmpl but %d creates returned nil", i, refCount, len(sbOK))
		}

		// clean up this trial's rows (referrers first, then the template).
		for _, id := range sbOK {
			if err := s.DeleteSandbox(ctx, id, nil); err != nil {
				t.Fatalf("trial %d: cleanup sandbox %s: %v", i, id, err)
			}
		}
		if childErr == nil {
			if err := s.DeleteTemplate(ctx, childID); err != nil {
				t.Fatalf("trial %d: cleanup child: %v", i, err)
			}
		}
		if !templateGone {
			if err := s.DeleteTemplate(ctx, tmplID); err != nil {
				t.Fatalf("trial %d: cleanup template: %v", i, err)
			}
		}
	}
}

// testUpdateBinding pins the §6.1 PATCH write point's contract (§3.4). Four
// properties, all of which a backend could plausibly get wrong on its own:
// partial application, the state precondition, the updated_at prohibition, and
// the death-cause convention it shares with CompareAndSetState.
func testUpdateBinding(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	rec := mkRec("u1")
	rec.DeathCause = model.CauseResourceExceeded
	if err := s.CreateSandbox(ctx, rec, nil); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSandbox(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}

	// (1) OMITTED MEANS UNCHANGED. Only memory is given, so the other four fields
	// must survive verbatim — a backend that writes zeros for absent fields would
	// silently un-cap the sandbox on every PATCH.
	want := int64(777 << 20)
	if err := s.UpdateBinding(ctx, "u1", model.Creating, store.Binding{MemoryByte: &want}, nil); err != nil {
		t.Fatalf("UpdateBinding: %v", err)
	}
	got, err := s.GetSandbox(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits.MemoryByte != want {
		t.Fatalf("memory = %d; want %d", got.Limits.MemoryByte, want)
	}
	if got.Limits.CPUMillis != before.Limits.CPUMillis || got.Limits.DiskByte != before.Limits.DiskByte {
		t.Fatalf("untouched axes changed: %+v → %+v", before.Limits, got.Limits)
	}
	if got.IdleTimeout != before.IdleTimeout || got.Retention != before.Retention {
		t.Fatalf("untouched TTLs changed: idle %s→%s, retention %s→%s",
			before.IdleTimeout, got.IdleTimeout, before.Retention, got.Retention)
	}
	// nil cause leaves the recorded cause alone, exactly as in CompareAndSetState.
	if got.DeathCause != model.CauseResourceExceeded {
		t.Fatalf("death cause = %q; want it untouched by a nil-cause update", got.DeathCause)
	}

	// (2) UPDATED_AT MUST NOT MOVE. `retention` is measured from it (§4.3), so a
	// backend that refreshes it here would silently extend eviction deadlines.
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("UpdatedAt moved (%s → %s); PATCH must not touch it (§4.3 retention clock)",
			before.UpdatedAt, got.UpdatedAt)
	}

	// (3) CONDITIONAL ON STATE (§4.4-B): a caller that decided against one state
	// must not have its write land after a concurrent transition moved it.
	other := int64(1)
	if err := s.UpdateBinding(ctx, "u1", model.Running, store.Binding{MemoryByte: &other}, nil); err != store.ErrConflict {
		t.Fatalf("wrong-state update = %v; want ErrConflict", err)
	}
	if again, _ := s.GetSandbox(ctx, "u1"); again.Limits.MemoryByte != want {
		t.Fatalf("a refused update still wrote: memory = %d", again.Limits.MemoryByte)
	}

	// (4) A non-nil CauseNone CLEARS the cause, in the SAME commit as the caps —
	// this is what makes §4.3's raise-the-cap remedy able to complete at all.
	cleared := model.CauseNone
	bigger := int64(999 << 20)
	if err := s.UpdateBinding(ctx, "u1", model.Creating, store.Binding{MemoryByte: &bigger}, &cleared); err != nil {
		t.Fatalf("clearing update: %v", err)
	}
	final, _ := s.GetSandbox(ctx, "u1")
	if final.DeathCause != model.CauseNone {
		t.Fatalf("death cause = %q; want cleared", final.DeathCause)
	}
	if final.Limits.MemoryByte != bigger {
		t.Fatalf("memory = %d; want %d (caps and the clear are one commit)", final.Limits.MemoryByte, bigger)
	}

	// missing record → ErrNotFound, not a silent no-op.
	if err := s.UpdateBinding(ctx, "nope", model.Creating, store.Binding{MemoryByte: &want}, nil); err != store.ErrNotFound {
		t.Fatalf("update of a missing record = %v; want ErrNotFound", err)
	}
}
