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
	t.Run("OutboxNilPublishesNothing", func(t *testing.T) { testOutboxNil(t, mk) })
	t.Run("MarkPublishedIdempotent", func(t *testing.T) { testMarkPublished(t, mk) })
	t.Run("OutboxRowOutlivesRecord", func(t *testing.T) { testOutboxOutlivesRecord(t, mk) })
	t.Run("Durability", func(t *testing.T) { testDurability(t, mk) })

	// Template object model (§4.3 / M5): same assertions both backends.
	t.Run("TemplateCRUD", func(t *testing.T) { testTemplateCRUD(t, mk) })
	t.Run("TemplateReferenceProtection", func(t *testing.T) { testTemplateRefProtection(t, mk) })
	t.Run("TemplateLineageProtection", func(t *testing.T) { testTemplateLineage(t, mk) })
	t.Run("SandboxRefMissingTemplate", func(t *testing.T) { testSandboxRefMissingTemplate(t, mk) })
	t.Run("TemplateDurability", func(t *testing.T) { testTemplateDurability(t, mk) })
	t.Run("ReferenceReclaimRace", func(t *testing.T) { testReferenceReclaimRace(t, mk) })
}

// seedTmpl inserts a base (parent-less) Template so a sandbox referencing it
// satisfies the §4.3 FK. Every sandbox-creating subtest calls it first: the
// sandboxes.template FK now REQUIRES the row to pre-exist (§6 前提 / boot seed).
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
		CreatedAt: now, UpdatedAt: now,
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
	seedTmpl(t, s, "bare")
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
	seedTmpl(t, s, "bare")
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
	seedTmpl(t, s, "bare")
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
// deleted (§4.3 INV-1 被引用即拒删 → ErrInUse); once the sandbox is gone, the
// delete succeeds. This is the E2B-bug immunity (M5 §4): the reference is a
// persistent row, seen by RESTRICT.
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
// cannot be deleted (§4.3 INV-2 引用向上 → ErrInUse); delete the child first,
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

// testReferenceReclaimRace is the DYNAMIC half of §4.3 (model.md 引用-回收原子性):
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
