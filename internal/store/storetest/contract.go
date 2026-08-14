// Package storetest holds the store.Store CONTRACT test suite — one set of
// assertions every backend must pass (§1.3 Behavior seam / §4.4-C). Memory and
// SQLite both call RunContract, so no correctness check is ever forked per backend.
//
// It lives in its own package (not store_test) so the SQLite backend's test
// file — in package store_test — can import and run it too without an import
// cycle.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
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

	// The §5.D declaration is a Binding column like the caps, so its round-trip is
	// CONTRACT rather than backend detail. It had no coverage here at all, which is how the
	// read-shape divergence testNetworkNotAliased now pins survived.
	t.Run("NetworkDeclarationRoundTrip", func(t *testing.T) { testNetworkRoundTrip(t, mk) })
	t.Run("NetworkDeclarationReadIsNotAliased", func(t *testing.T) { testNetworkNotAliased(t, mk) })

	// §4.1's event kinds are a CLOSED set of six, and that closure is a contract both
	// backends must hold: a subscriber reading a seventh kind has no way to interpret it.
	t.Run("EventKindsAreAClosedSet", func(t *testing.T) { testEventKindClosed(t, mk) })
	t.Run("EventCauseSurvivesTheRecordItDescribes", func(t *testing.T) { testEventCauseOutlivesRecord(t, mk) })
	t.Run("EventCausesAreAClosedSet", func(t *testing.T) { testEventCauseClosed(t, mk) })

	t.Run("OutboxNilPublishesNothing", func(t *testing.T) { testOutboxNil(t, mk) })
	t.Run("MarkPublishedIdempotent", func(t *testing.T) { testMarkPublished(t, mk) })

	// The drain read's bound and the gauge's independence from it are ONE contract: the
	// publisher runs one algorithm against this interface, so a backend that ignored the
	// limit — or whose count saturated at it — would break §4.4-C ③ on that backend alone,
	// and only in production, where the backlog is large enough to notice.
	t.Run("UnpublishedEventsHonorsItsLimit", func(t *testing.T) { testUnpublishedLimit(t, mk) })
	t.Run("CountUnpublishedIsIndependentOfTheDrainBound", func(t *testing.T) { testCountUnpublished(t, mk) })

	// §4.4-C's retention MECHANISM. In the shared suite because every clause of the
	// predicate protects a subscriber-visible promise, so a backend that got one wrong
	// would break replay on that backend alone, in production, silently.
	t.Run("TrimOutboxDeliveredOnly", func(t *testing.T) { testTrimDeliveredOnly(t, mk) })
	t.Run("TrimOutboxAgesByDeliveryNotOccurrence", func(t *testing.T) { testTrimAgesByDelivery(t, mk) })
	t.Run("TrimOutboxNeverEmptiesTheTable", func(t *testing.T) { testTrimKeepsNewest(t, mk) })
	t.Run("TrimOutboxAdvancesTheWindowAndPreservesSeq", func(t *testing.T) { testTrimWindowAndSeq(t, mk) })
	t.Run("OutboxRowOutlivesRecord", func(t *testing.T) { testOutboxOutlivesRecord(t, mk) })
	t.Run("Durability", func(t *testing.T) { testDurability(t, mk) })

	// Template object model (§4.3): same assertions both backends.
	t.Run("TemplateCRUD", func(t *testing.T) { testTemplateCRUD(t, mk) })
	t.Run("TemplateReferenceProtection", func(t *testing.T) { testTemplateRefProtection(t, mk) })
	t.Run("TemplateLineageProtection", func(t *testing.T) { testTemplateLineage(t, mk) })
	t.Run("SandboxRefMissingTemplate", func(t *testing.T) { testSandboxRefMissingTemplate(t, mk) })
	t.Run("TemplateDurability", func(t *testing.T) { testTemplateDurability(t, mk) })
	t.Run("ReferenceReclaimRace", func(t *testing.T) { testReferenceReclaimRace(t, mk) })

	// Volume object model (§5.C / §4.3): same assertions both backends.
	t.Run("VolumeCRUD", func(t *testing.T) { testVolumeCRUD(t, mk) })
	t.Run("VolumeCapacityWritePoint", func(t *testing.T) { testVolumeCapacity(t, mk) })
	t.Run("VolumeDeleteReportsNotFound", func(t *testing.T) { testVolumeDeleteNotFound(t, mk) })
	t.Run("VolumeDurability", func(t *testing.T) { testVolumeDurability(t, mk) })

	// Mount table (§5.C / §4.3). These matter equally on both sides (§1.3 seam):
	// SQLite gets reference protection from constraints, Memory from explicit scans,
	// and only a shared suite proves the two agree.
	t.Run("MountCRUD", func(t *testing.T) { testMountCRUD(t, mk) })
	t.Run("MountRestrictsVolumeDelete", func(t *testing.T) { testMountRestrictsVolume(t, mk) })
	t.Run("MountRestrictsSandboxDelete", func(t *testing.T) { testMountRestrictsSandbox(t, mk) })
	t.Run("ExclusiveLeaseInvariant", func(t *testing.T) { testExclusiveLease(t, mk) })
	t.Run("ExclusiveDoesNotCoexistWithShared", func(t *testing.T) { testExclusiveVsShared(t, mk) })
	t.Run("MountBindingSelfContradiction", func(t *testing.T) { testMountBindingInvalid(t, mk) })
	t.Run("MountBindingIsAllOrNothing", func(t *testing.T) { testMountAllOrNothing(t, mk) })

	// ReplaceMounts (§5.C): the whole-binding swap is ONE atomic commit. Two cases, both
	// required by the spec bullet.
	t.Run("ReplaceMountsKeepsOldBindingOnRefusal", func(t *testing.T) { testReplaceMountsKeepsOldBindingOnRefusal(t, mk) })
	t.Run("ReplaceMountsNeverExposesZeroRows", func(t *testing.T) { testReplaceMountsNoZeroRowWindow(t, mk) })
	t.Run("MountReferenceSideMissingEndpoint", func(t *testing.T) { testMountMissingEndpoint(t, mk) })
	t.Run("MountVerdictsAreDistinguishable", func(t *testing.T) { testMountVerdictsDistinct(t, mk) })
	t.Run("ConcurrentExclusiveAttachSingleWinner", func(t *testing.T) { testConcurrentLease(t, mk) })
	t.Run("ForeignKeyEnforcementItself", func(t *testing.T) { testFKEnforcement(t, mk) })
	t.Run("MountDurability", func(t *testing.T) { testMountDurability(t, mk) })

	// Volume lineage (§5.C / volume.md D6.1a): the FOURTH FK path, and the fourth
	// distinct verdict.
	t.Run("VolumeLineageProtection", func(t *testing.T) { testVolumeLineage(t, mk) })

	// §4.4-C under CONCURRENT writers. Every other subtest here is
	// single-threaded, so a backend whose CAS is not genuinely atomic — read,
	// then write, without holding anything across the two — would pass all of
	// them. Both backends must deliver this identically (§1.3 seam).
	t.Run("ConcurrentCASSingleWinner", func(t *testing.T) { testConcurrentCAS(t, mk) })

	// The outbox sequence contract (see Store): total order, replay paging, and
	// the adversarial no-visibility-gap case.
	t.Run("SequenceTotalOrder", func(t *testing.T) { testSequenceTotalOrder(t, mk) })
	t.Run("SequenceSurvivesReopen", func(t *testing.T) { testSequenceSurvivesReopen(t, mk) })
	t.Run("EventsReplayPaging", func(t *testing.T) { testEventsReplayPaging(t, mk) })
	t.Run("SequenceNoVisibilityGap", func(t *testing.T) { testSequenceNoVisibilityGap(t, mk) })
}

// testSequenceTotalOrder: every appended event gets a unique, strictly
// increasing Seq, and both read paths return events in that order.
//
// This is the property occurred_at could not provide: these events are appended
// in a tight loop, so several of them land in the SAME nanosecond on a fast
// clock, and an occurred_at ordering has no tie-break to offer there.
func testSequenceTotalOrder(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	if err := s.CreateSandbox(ctx, mkRec("a"), nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 20
	state := model.Creating
	for i := 0; i < n; i++ {
		next := model.Running
		if state == model.Running {
			next = model.Paused
		}
		if err := s.CompareAndSetState(ctx, "a", state, next,
			ev("a", fmt.Sprintf("evt_%02d", i), kindFor(next), next), nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		state = next
	}

	un, err := s.UnpublishedEvents(ctx, allUnpublished)
	if err != nil {
		t.Fatalf("unpublished: %v", err)
	}
	if len(un) != n {
		t.Fatalf("got %d events; want %d", len(un), n)
	}
	seen := map[int64]bool{}
	var prev int64
	for i, e := range un {
		if e.Seq <= 0 {
			t.Fatalf("event %d (%s) has Seq=%d; a read-back event must carry a positive sequence", i, e.ID, e.Seq)
		}
		if e.Seq <= prev {
			t.Fatalf("event %d (%s) Seq=%d not strictly increasing after %d", i, e.ID, e.Seq, prev)
		}
		if seen[e.Seq] {
			t.Fatalf("Seq %d issued twice", e.Seq)
		}
		seen[e.Seq] = true
		prev = e.Seq
		// append order must match sequence order: evt_00 first, evt_19 last.
		if want := fmt.Sprintf("evt_%02d", i); e.ID != want {
			t.Fatalf("position %d holds %s; want %s (read is not in append order)", i, e.ID, want)
		}
	}
}

// testSequenceSurvivesReopen: for a durable backend the sequence is monotonic
// ACROSS a restart — a reopened store never re-issues a number a subscriber's
// cursor has already passed. Memory (reopen == nil) skips.
func testSequenceSurvivesReopen(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	if err := s.CompareAndSetState(ctx, "a", model.Creating, model.Running,
		ev("a", "evt_before", "created", model.Running), nil); err != nil {
		t.Fatalf("pre-restart append: %v", err)
	}
	before, _, _, err := s.Events(ctx, 0, 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("want 1 event pre-restart; got %d", len(before))
	}
	high := before[0].Seq

	s2 := reopen(t)
	if err := s2.CompareAndSetState(ctx, "a", model.Running, model.Paused,
		ev("a", "evt_after", "paused", model.Paused), nil); err != nil {
		t.Fatalf("post-restart append: %v", err)
	}
	after, _, _, err := s2.Events(ctx, high, 10)
	if err != nil {
		t.Fatalf("events after cursor: %v", err)
	}
	if len(after) != 1 || after[0].ID != "evt_after" {
		t.Fatalf("resuming from cursor %d returned %+v; want just evt_after", high, after)
	}
	if after[0].Seq <= high {
		t.Fatalf("post-restart Seq %d did not advance past %d — a cursor at %d would MISS this event",
			after[0].Seq, high, high)
	}
}

// testEventsReplayPaging: Store.Events pages by cursor, honors limit, and
// reports the window bounds. Paging through in small pages must yield every
// event exactly once, in order — no duplicate at the page boundary and no gap.
func testEventsReplayPaging(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")

	// empty outbox: no events, and bounds are 0/0 rather than an error.
	evs, earliest, latest, err := s.Events(ctx, 0, 10)
	if err != nil {
		t.Fatalf("events on empty outbox: %v", err)
	}
	if len(evs) != 0 || earliest != 0 || latest != 0 {
		t.Fatalf("empty outbox: got %d events, bounds %d/%d; want 0 events, 0/0", len(evs), earliest, latest)
	}

	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	const n = 7
	state := model.Creating
	for i := 0; i < n; i++ {
		next := model.Running
		if state == model.Running {
			next = model.Paused
		}
		if err := s.CompareAndSetState(ctx, "a", state, next,
			ev("a", fmt.Sprintf("e%d", i), kindFor(next), next), nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		state = next
	}

	// limit is honored.
	page, earliest, latest, err := s.Events(ctx, 0, 3)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("limit=3 returned %d events", len(page))
	}
	if earliest <= 0 || latest <= 0 || earliest > latest {
		t.Fatalf("bounds %d/%d are not a sane window", earliest, latest)
	}
	if latest != page[0].Seq+n-1 {
		t.Fatalf("latest=%d does not match the last appended sequence (%d)", latest, page[0].Seq+n-1)
	}

	// page through with limit=2: every event exactly once, in order.
	var ids []string
	cursor := int64(0)
	for i := 0; i < 100; i++ { // bounded: a non-advancing cursor must fail, not spin
		pg, _, _, err := s.Events(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("page at cursor %d: %v", cursor, err)
		}
		if len(pg) == 0 {
			break
		}
		for _, e := range pg {
			if e.Seq <= cursor {
				t.Fatalf("page at cursor %d returned Seq %d — a served event was returned again", cursor, e.Seq)
			}
			ids = append(ids, e.ID)
			cursor = e.Seq
		}
	}
	if len(ids) != n {
		t.Fatalf("paging yielded %d events (%v); want %d exactly once", len(ids), ids, n)
	}
	for i, got := range ids {
		if want := fmt.Sprintf("e%d", i); got != want {
			t.Fatalf("paged position %d = %s; want %s", i, got, want)
		}
	}

	// Replay is over the retained window, NOT over delivery state: marking an
	// event published must not remove it from replay.
	if err := s.MarkPublished(ctx, "e0"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	all, _, _, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events after mark: %v", err)
	}
	if len(all) != n {
		t.Fatalf("replay lost a published event: %d of %d remain", len(all), n)
	}
}

// testSequenceNoVisibilityGap is the ADVERSARIAL case for promise 2 of the
// sequence contract (see Store): once Seq = N is visible, nothing below N may
// appear afterwards.
//
// The failure it hunts is a real, audited defect and not a hypothetical — the
// assign-at-INSERT / visible-at-COMMIT interleaving Store's promise 2 describes. Note what
// does NOT save you: serializing the readers. The ordering breaks on the producer side, so
// a lock around the drain sees a perfectly ordered stream that is already missing a row.
//
// The check: writers append concurrently while a reader repeatedly polls with a
// forward-only cursor, exactly as a subscriber would. Any row that appears below
// the highest sequence the reader has already passed is the defect. A backend with a
// BIGSERIAL default and genuinely concurrent write transactions FAILS here until it adds a
// watermark gate — that failure is the entire point, because the alternative is
// discovering it during a billing reconciliation.
func testSequenceNoVisibilityGap(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")

	const writers = 8
	const perWriter = 12
	for w := 0; w < writers; w++ {
		if err := s.CreateSandbox(ctx, mkRec(fmt.Sprintf("w%d", w)), nil); err != nil {
			t.Fatalf("create w%d: %v", w, err)
		}
	}

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		done  = make(chan struct{})
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			id := model.SandboxID(fmt.Sprintf("w%d", w))
			state := model.Creating
			for i := 0; i < perWriter; i++ {
				next := model.Running
				if state == model.Running {
					next = model.Paused
				}
				// Retries are irrelevant to what is being measured: an append that
				// fails simply produced no row. What matters is that every row that
				// DID land is observed in sequence order.
				if err := s.CompareAndSetState(ctx, id, state, next,
					ev(string(id), fmt.Sprintf("w%d_%d", w, i), kindFor(next), next), nil); err == nil {
					state = next
				}
			}
		}(w)
	}

	// The reader: a forward-only cursor, the way a real subscriber persists one.
	var (
		bad     []seqViolation
		seenSeq = map[int64]bool{}
		cursor  int64
	)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				// one final sweep after the writers finish
				drainCheck(ctx, t, s, &cursor, seenSeq, &bad)
				return
			default:
				drainCheck(ctx, t, s, &cursor, seenSeq, &bad)
			}
		}
	}()

	close(start)
	wg.Wait()
	close(done)
	<-readerDone

	if len(bad) > 0 {
		t.Fatalf("no-visibility-gap VIOLATED: %d row(s) appeared below an already-served cursor, e.g. Seq=%d id=%s appeared when the cursor was already at %d. A subscriber resuming from its cursor would never see them.",
			len(bad), bad[0].seq, bad[0].id, bad[0].cursor)
	}

	// Sanity: the reader must actually have observed the rows that landed, or the
	// assertion above passed vacuously.
	final, _, _, err := s.Events(ctx, 0, 10000)
	if err != nil {
		t.Fatalf("final events read: %v", err)
	}
	if len(final) == 0 {
		t.Fatal("no events landed at all — the test proved nothing")
	}
	for _, e := range final {
		if !seenSeq[e.Seq] {
			t.Fatalf("event Seq=%d id=%s was never visible to the forward-only reader (it is only reachable by rewinding the cursor)", e.Seq, e.ID)
		}
	}
}

// seqViolation is one observed no-visibility-gap breach: row `seq` (event `id`)
// became visible when the forward-only reader had already served up to `cursor`.
type seqViolation struct {
	seq, cursor int64
	id          string
}

// drainCheck reads the outbox once, recording any row that appears BELOW the
// cursor already served — i.e. a visibility gap. Split out so the reader
// goroutine runs the identical check on its final post-writer sweep.
func drainCheck(ctx context.Context, t *testing.T, s store.Store, cursor *int64, seen map[int64]bool, bad *[]seqViolation) {
	t.Helper()
	// Read from the START each pass, not from the cursor: reading from the cursor
	// could not detect a row that landed BELOW it — which is the whole defect.
	evs, _, _, err := s.Events(ctx, 0, 10000)
	if err != nil {
		return // transient backend error is not what this test measures
	}
	for _, e := range evs {
		if seen[e.Seq] {
			continue
		}
		if e.Seq < *cursor {
			*bad = append(*bad, seqViolation{seq: e.Seq, cursor: *cursor, id: e.ID})
		}
		seen[e.Seq] = true
		if e.Seq > *cursor {
			*cursor = e.Seq
		}
	}
}

// testConcurrentCAS: N goroutines race the SAME transition. Exactly ONE may win,
// and exactly ONE outbox row may land.
//
// The §4.4-B CAS promise and the §4.4-C atomic-commit promise met at the same moment: a
// torn implementation admits several winners, and each winner appends an event — so a
// single transition is billed N times, the double-count subscriber dedup CANNOT absorb
// (the events carry DIFFERENT transition IDs).
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
		un, err := s.UnpublishedEvents(ctx, allUnpublished)
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
		// swapped or dropped is caught on read-back. The three axes are never
		// interchangeable knobs (model.Limits), so a silent swap is a real bug class.
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

// kindFor is the §4.1 kind matching a transition INTO state. Tests that only need
// "some valid event" must still name a real kind: the outbox schema asserts §4.1's
// closed set, so a placeholder like "flip" is rejected — which is the point of the
// constraint, not an obstacle to it.
func kindFor(state model.Lifecycle) model.EventKind {
	if state == model.Running {
		return model.EventResumed
	}
	return model.EventPaused
}

func ev(id, evid string, kind model.EventKind, state model.Lifecycle) *store.Event {
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
	if un, _ := s.UnpublishedEvents(ctx, allUnpublished); len(un) != 1 || un[0].ID != "evt_crash" {
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
	un, err := s.UnpublishedEvents(ctx, allUnpublished)
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
	un, _ = s.UnpublishedEvents(ctx, allUnpublished)
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
	un, err := s.UnpublishedEvents(ctx, allUnpublished)
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

	if un, _ := s.UnpublishedEvents(ctx, allUnpublished); len(un) != 2 {
		t.Fatalf("want 2 unpublished, got %d", len(un))
	}
	// mark one → it drops out, the other remains.
	if err := s.MarkPublished(ctx, "evt_1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
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
	if un, _ := s.UnpublishedEvents(ctx, allUnpublished); len(un) != 1 {
		t.Fatalf("idempotent mark changed count: %d", len(un))
	}
}

// testTrimDeliveredOnly: an UNDELIVERED row is never trimmed, at any cutoff.
//
// This is the floor the at-least-once contract rests on: trimming an undelivered row
// drops an event silently, which is the one failure the whole outbox design exists to
// prevent. Note it is also structurally guaranteed — the publisher drains in Seq order
// and stops at the first failure, so the delivered set is a prefix — but a backend that
// dropped the predicate would break it, hence the assertion.
func testTrimDeliveredOnly(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	seedUnpublished(t, s, "a", 3)

	// Nothing delivered yet: a cutoff far in the future must still trim nothing.
	n, err := s.TrimOutbox(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("TrimOutbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("trimmed %d UNDELIVERED rows — an undelivered row is an undelivered event, and deleting it loses it silently (the at-least-once contract's one prohibition)", n)
	}
	if un, _ := s.UnpublishedEvents(ctx, allUnpublished); len(un) != 3 {
		t.Fatalf("%d rows remain unpublished, want 3", len(un))
	}
}

// testTrimAgesByDelivery: the cutoff applies to DELIVERY time, not occurrence time (the
// halted-stream counterexample is on store.TrimOutbox).
//
// THE FIXTURE HAS TO SEPARATE THE TWO TIMESTAMPS OR IT ASSERTS NOTHING. Rows occurring
// and delivered at the same moment are excluded by a past cutoff on either field, so
// such a test passes against both implementations (found by falsifying it: swapping
// published_at for occurred_at left it green). So: occurrence is backdated two hours,
// delivery is now, and the cutoff sits BETWEEN them — where the two readings disagree.
func testTrimAgesByDelivery(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	// Backdated OCCURRENCE, the way a long-halted stream's backlog looks.
	old := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		from, to := model.Paused, model.Running
		kind := model.EventKind("resumed")
		switch {
		case i == 0:
			from, to, kind = model.Creating, model.Running, model.EventKind("created")
		case i%2 == 1:
			from, to, kind = model.Running, model.Paused, model.EventKind("paused")
		}
		e := ev("a", fmt.Sprintf("evt_%d", i), kind, to)
		e.OccurredAt = old
		if err := s.CompareAndSetState(ctx, "a", from, to, e, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Delivered NOW.
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	for _, e := range un {
		if err := s.MarkPublished(ctx, e.ID); err != nil {
			t.Fatalf("mark %s: %v", e.ID, err)
		}
	}

	// A cutoff BETWEEN occurrence and delivery. Correct (published_at): nothing is old
	// enough, so nothing is trimmed. Wrong (occurred_at): everything looks ancient.
	cutoff := time.Now().Add(-time.Hour)
	n, err := s.TrimOutbox(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("TrimOutbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("trimmed %d rows that OCCURRED before the cutoff but were DELIVERED after it — the age is being measured from occurrence, so a backlog delivered late (a resumed poison halt is the real case) becomes instantly trimmable and subscribers lose events they never had the chance to read", n)
	}

	// And the positive control: once delivery itself is old enough, they do go — or the
	// assertion above would also pass on an implementation that trims nothing at all.
	if n, err = s.TrimOutbox(ctx, time.Now().Add(time.Hour), 100); err != nil {
		t.Fatalf("TrimOutbox (past-delivery cutoff): %v", err)
	}
	if n == 0 {
		t.Fatalf("a cutoff after delivery trimmed nothing — the window would never advance, so §6.1's 410 stays unreachable and the retention flag does nothing")
	}
}

// testTrimKeepsNewest: trimming never empties the table, even when every row qualifies —
// the 0/0 window a stale cursor cannot be refused from (store.TrimOutbox / §6.1's 410).
func testTrimKeepsNewest(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	seedUnpublished(t, s, "a", 4)
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	for _, ev := range un {
		_ = s.MarkPublished(ctx, ev.ID)
	}

	// Every row is delivered and older than this cutoff, so all four "qualify".
	cutoff := time.Now().Add(time.Hour)
	if _, err := s.TrimOutbox(ctx, cutoff, 100); err != nil {
		t.Fatalf("TrimOutbox: %v", err)
	}
	// Trim again to be sure a second pass cannot finish the job either.
	if _, err := s.TrimOutbox(ctx, cutoff, 100); err != nil {
		t.Fatalf("TrimOutbox (second pass): %v", err)
	}

	_, earliest, latest, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if earliest == 0 || latest == 0 {
		t.Fatalf("after trimming everything the window reads earliest=%d latest=%d — 0/0 is what an outbox that never held anything reports, so a stale cursor gets a silent empty page instead of a 410 (§6.1)", earliest, latest)
	}
	if earliest != latest {
		t.Errorf("window is %d..%d; want exactly one row retained", earliest, latest)
	}
}

// testTrimWindowAndSeq: trimming advances the window's lower bound (which is what makes
// §6.1's 410 reachable at all) and does NOT roll the sequence back.
//
// The sequence half is the subtler one. If the counter were derived from the rows still
// present, emptying or shrinking the table would re-issue numbers a subscriber has
// already consumed, pointing its cursor past fresh events — a silent gap with no error
// anywhere. The SQLite arm defends this with AUTOINCREMENT (whose high-water mark lives
// outside the table); this asserts the PROPERTY, so any backend must deliver it however
// it likes.
func testTrimWindowAndSeq(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	seedUnpublished(t, s, "a", 5)
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	for _, ev := range un {
		_ = s.MarkPublished(ctx, ev.ID)
	}
	_, before, highest, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	n, err := s.TrimOutbox(ctx, time.Now().Add(time.Hour), 2)
	if err != nil {
		t.Fatalf("TrimOutbox: %v", err)
	}
	if n != 2 {
		t.Fatalf("trimmed %d with limit=2 — the bound is ignored, so a large backlog is deleted in one statement and every transition waits behind it", n)
	}
	_, after, _, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("Events after trim: %v", err)
	}
	if after <= before {
		t.Fatalf("earliest did not advance (%d → %d) — §6.1's 410 is reachable only once the window's lower bound moves, so without this trimming activates nothing", before, after)
	}

	// A new event must take a sequence ABOVE everything ever issued, not reuse a
	// trimmed one.
	if err := s.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "evt_after_trim", model.EventKind("paused"), model.Paused), nil); err != nil {
		t.Fatalf("post-trim transition: %v", err)
	}
	evs, _, latest, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if latest <= highest {
		t.Fatalf("a post-trim event took seq %d, at or below the pre-trim high-water mark %d — a subscriber's cursor is now past freshly-issued sequences and will silently skip them", latest, highest)
	}
	for _, e := range evs {
		if e.ID == "evt_after_trim" && e.Seq <= highest {
			t.Errorf("evt_after_trim reused seq %d", e.Seq)
		}
	}
}

// seedUnpublished writes n unpublished outbox rows for one sandbox by walking the §4.1
// machine, so every row comes from a LEGAL transition (the store enforces the
// precondition, so a shortcut would just fail the CAS). Rows are named evt_0..evt_n-1 in
// sequence order, which is what lets a caller assert a bounded read returned the OLDEST.
func seedUnpublished(t *testing.T, s store.Store, id string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		from, to := model.Paused, model.Running
		kind := model.EventKind("resumed")
		switch {
		case i == 0:
			from, to, kind = model.Creating, model.Running, model.EventKind("created")
		case i%2 == 1:
			from, to, kind = model.Running, model.Paused, model.EventKind("paused")
		}
		if err := s.CompareAndSetState(ctx, model.SandboxID(id), from, to, ev(id, fmt.Sprintf("evt_%d", i), kind, to), nil); err != nil {
			t.Fatalf("seed transition %d (%s→%s): %v", i, from, to, err)
		}
	}
}

// testUnpublishedLimit: the drain read must HONOR its bound, and must refuse a
// non-positive one rather than reading it as "everything".
//
// Both halves guard the same REGRESSION, which was real: UnpublishedEvents used to take no
// limit and select the entire unpublished backlog (cost on store.UnpublishedEvents). A
// backend that accepted the parameter and ignored it would restore that silently, and a "0
// means unbounded" reading would restore it on any caller that omitted the argument.
func testUnpublishedLimit(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)
	// Five unpublished rows, alternating so each transition is legal.
	seedUnpublished(t, s, "a", 5)

	un, err := s.UnpublishedEvents(ctx, 2)
	if err != nil {
		t.Fatalf("UnpublishedEvents(limit=2): %v", err)
	}
	if len(un) != 2 {
		t.Fatalf("limit=2 returned %d rows — the bound is ignored, so the per-second drain read materializes the whole backlog on a single-connection pool and every transition waits behind it", len(un))
	}
	// And it must be the OLDEST rows: delivery is in Seq order and stops at the first
	// failure, so a bound that returned an arbitrary window would skip ahead.
	if un[0].ID != "evt_0" || un[1].ID != "evt_1" {
		t.Errorf("limit=2 returned %s,%s; want the two OLDEST (evt_0,evt_1) — a bounded read must still be a prefix, or the drain skips events", un[0].ID, un[1].ID)
	}

	for _, bad := range []int{0, -1} {
		if _, err := s.UnpublishedEvents(ctx, bad); err == nil {
			t.Errorf("UnpublishedEvents(limit=%d) succeeded — a non-positive bound must be refused, not read as unbounded, which is how the whole-backlog scan comes back on a caller that forgot the argument", bad)
		}
	}
}

// testCountUnpublished: the backlog gauge's source must be independent of the drain
// bound (§4.4-C ③). This is the half that makes the bound above safe to add — a gauge of
// len(UnpublishedEvents(ctx, DrainBatch)) saturates at that constant (store.CountUnpublished).
func testCountUnpublished(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	n, err := s.CountUnpublished(ctx)
	if err != nil {
		t.Fatalf("CountUnpublished on an empty outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty outbox counts %d, want 0", n)
	}

	seedUnpublished(t, s, "a", 5)

	// The count sees all five even though a drain read of 2 sees two.
	if _, err := s.UnpublishedEvents(ctx, 2); err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	n, err = s.CountUnpublished(ctx)
	if err != nil {
		t.Fatalf("CountUnpublished: %v", err)
	}
	if n != 5 {
		t.Fatalf("CountUnpublished = %d with 5 unpublished rows and a drain bound of 2 — the gauge is saturating at the drain bound, so a large backlog reports as a small constant and a wedged drain looks idle (§4.4-C ③)", n)
	}

	// Marking one delivered decrements it: the count tracks delivery, not arrival.
	if err := s.MarkPublished(ctx, "evt_0"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if n, _ := s.CountUnpublished(ctx); n != 4 {
		t.Errorf("after marking one delivered the count is %d, want 4", n)
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
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	if len(un) != 1 || un[0].ID != "evt_d" {
		t.Fatalf("destroyed event missing from outbox: %+v", un)
	}
}

// testEventCauseOutlivesRecord: the §4.4-C cause survives the record it describes, and
// reads back unchanged. This is the property the field exists for (model.EventCause), and
// it is testable only in this order — destroy deletes the row, so if the cause did not
// round-trip through the OUTBOX there would be nowhere left to read it from.
//
// Both backends run it: the SQLite arm has a CHECK plus a nullable column, Memory copies
// the struct, and a silent divergence there would mean the tests prove the behaviour on
// the backend production does not use.
func testEventCauseOutlivesRecord(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("a"), nil)

	e := ev("a", "evt_d", "destroyed", model.Destroyed)
	e.Cause = model.CauseRetentionExpired
	if err := s.DeleteSandbox(ctx, "a", e); err != nil {
		t.Fatalf("delete+event: %v", err)
	}
	if _, err := s.GetSandbox(ctx, "a"); err != store.ErrNotFound {
		t.Fatalf("record should be gone: %v", err)
	}

	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	if len(un) != 1 {
		t.Fatalf("want 1 outbox row, got %d: %+v", len(un), un)
	}
	if un[0].Cause != model.CauseRetentionExpired {
		t.Errorf("cause read back as %q; want %q — the record is deleted, so this row is the "+
			"ONLY place the route is readable and a lost cause here is lost permanently",
			un[0].Cause, model.CauseRetentionExpired)
	}

	// The replay read (§6.1 GET /v1/events) must carry it too, not only the drain read:
	// the pull surface is what an external subscriber actually uses.
	evs, _, _, err := s.Events(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 1 || evs[0].Cause != model.CauseRetentionExpired {
		t.Errorf("replay read lost the cause: %+v", evs)
	}
}

// testEventCauseClosed: the cause vocabulary is closed on BOTH backends, the same
// obligation model.EventKinds carries. The SQLite arm asserts it with a CHECK, so
// without this Memory would accept a misspelling — i.e. it would be refused in
// production and pass in the tests meant to prove the behaviour.
//
// Absent is legitimate and must stay so: most kinds have one route and carry no cause,
// so "" is not a rejected value, it is the common one.
func testEventCauseClosed(t *testing.T, mk Factory) {
	ctx := context.Background()

	for i, c := range model.EventCauses {
		s, _ := mk(t)
		seedTmpl(t, s, "bare")
		_ = s.CreateSandbox(ctx, mkRec("c1"), nil)
		e := ev("c1", fmt.Sprintf("evt_ok_%d", i), "destroyed", model.Destroyed)
		e.Cause = c
		if err := s.DeleteSandbox(ctx, "c1", e); err != nil {
			t.Errorf("the declared cause %q was refused: %v — model.EventCauses and the store "+
				"must agree", c, err)
		}
	}

	// Absent → accepted, and reads back as absent rather than as some placeholder.
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	_ = s.CreateSandbox(ctx, mkRec("c2"), nil)
	if err := s.DeleteSandbox(ctx, "c2", ev("c2", "evt_none", "destroyed", model.Destroyed)); err != nil {
		t.Fatalf("an event with no cause was refused: %v — a cause is populated only where a "+
			"kind has more than one route, so absent is the COMMON case", err)
	}
	un, _ := s.UnpublishedEvents(ctx, allUnpublished)
	if len(un) != 1 || un[0].Cause != model.CauseNotApplicable {
		t.Errorf("absent cause read back as %+v; want empty", un)
	}

	// …and an undeclared one is refused. A hyphen-for-underscore typo, the realistic
	// mistake and invisible to review — same failure shape testEventKindClosed pins.
	s2, _ := mk(t)
	seedTmpl(t, s2, "bare")
	_ = s2.CreateSandbox(ctx, mkRec("c3"), nil)
	bad := ev("c3", "evt_bad", "destroyed", model.Destroyed)
	bad.Cause = model.EventCause("retention_expired")
	if err := s2.DeleteSandbox(ctx, "c3", bad); err == nil {
		t.Error("an undeclared event cause was PERSISTED — a value the schema cannot write is a " +
			"value no reader has to handle, and a subscriber matching on the closed set would " +
			"silently drop this one")
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
	un, _ := s2.UnpublishedEvents(ctx, allUnpublished)
	if len(un) != 1 || un[0].ID != "evt_1" {
		t.Fatalf("unpublished event lost across reopen: %+v", un)
	}
	// marking published also survives.
	_ = s2.MarkPublished(ctx, "evt_1")
	s3 := reopen(t)
	if un, _ := s3.UnpublishedEvents(ctx, allUnpublished); len(un) != 0 {
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

// --- Volume (§5.C / §4.3) ----------------------------------------------------

// testVolumeCRUD: a Volume round-trips intact and is addressed by its own
// sentinel. Existence IS the state (§4.3) — there is no state column to check, so
// what the assertions cover instead is that presence/absence is the whole answer.
func testVolumeCRUD(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)

	// Absent → the VOLUME sentinel, not the sandbox one. A 404 naming the wrong
	// object sends the caller looking in the wrong place (§6.2's reasoning about the
	// Volume verdicts, applied to not-found).
	if _, err := s.GetVolume(ctx, "nope"); err != store.ErrVolumeNotFound {
		t.Fatalf("GetVolume(absent) = %v; want ErrVolumeNotFound", err)
	}

	v := model.Volume{ID: "vol_a", Backend: "local_btrfs", CapByte: 32 << 20, CreatedAt: time.Now()}
	if err := s.CreateVolume(ctx, v); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetVolume(ctx, "vol_a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != v.ID || got.Backend != v.Backend || got.CapByte != v.CapByte {
		t.Errorf("round trip = %+v, want ID/Backend/CapByte of %+v", got, v)
	}

	// Duplicate → ErrConflict, matching every other insert in this interface.
	if err := s.CreateVolume(ctx, v); err != store.ErrConflict {
		t.Errorf("duplicate create = %v; want ErrConflict", err)
	}

	// List is what the §3.4 admission gate sums Σ(volume caps) from, so it must
	// return the cap, not just the identity.
	list, err := s.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].CapByte != v.CapByte {
		t.Errorf("list = %+v; want one row carrying CapByte %d — the admission gate sums this", list, v.CapByte)
	}

	if err := s.DeleteVolume(ctx, "vol_a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetVolume(ctx, "vol_a"); err != store.ErrVolumeNotFound {
		t.Errorf("get after delete = %v; want ErrVolumeNotFound", err)
	}
}

// testVolumeCapacity: the cap write point, and the thing about it that is NOT
// symmetric with UpdateBinding — it takes no lifecycle-state precondition, for the
// mechanical reason on store.SetVolumeCapacity.
func testVolumeCapacity(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	if err := s.CreateVolume(ctx, model.Volume{ID: "vol_c", Backend: "local_btrfs", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetVolumeCapacity(ctx, "vol_c", 64<<20); err != nil {
		t.Fatalf("set capacity: %v", err)
	}
	got, err := s.GetVolume(ctx, "vol_c")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CapByte != 64<<20 {
		t.Errorf("cap after write = %d, want %d", got.CapByte, 64<<20)
	}
	// 0 is a MEANINGFUL value here (uncapped), not "unset": the record is the
	// authority for the cap, so writing 0 must actually record 0 (§3.4 / volume.md
	// D7.3 — the backend cannot be consulted to disambiguate, since it maps "no cap"
	// and 0 onto each other).
	if err := s.SetVolumeCapacity(ctx, "vol_c", 0); err != nil {
		t.Fatalf("clear capacity: %v", err)
	}
	if got, _ = s.GetVolume(ctx, "vol_c"); got.CapByte != 0 {
		t.Errorf("cap after clearing = %d, want 0", got.CapByte)
	}
	// Absent row → the volume sentinel.
	if err := s.SetVolumeCapacity(ctx, "nope", 1); err != store.ErrVolumeNotFound {
		t.Errorf("SetVolumeCapacity(absent) = %v; want ErrVolumeNotFound", err)
	}
}

// testVolumeDeleteReportsNotFound pins §6.2's "idempotent delete must not swallow the
// verdicts", on the cheapest verdict — it shares a path with the reference-protection
// refusals, so a backend that folded not-found into success would fold those too
// (store.DeleteVolume carries the audited external counter-example).
func testVolumeDeleteNotFound(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	if err := s.DeleteVolume(ctx, "never"); err != store.ErrVolumeNotFound {
		t.Fatalf("DeleteVolume(absent) = %v; want ErrVolumeNotFound — a delete that folds this into success cannot report a REFUSED delete either (§6.2)", err)
	}
	if err := s.CreateVolume(ctx, model.Volume{ID: "vol_d", Backend: "local_btrfs", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteVolume(ctx, "vol_d"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := s.DeleteVolume(ctx, "vol_d"); err != store.ErrVolumeNotFound {
		t.Errorf("second delete = %v; want ErrVolumeNotFound", err)
	}
}

// testVolumeDurability: a volume outlives a process restart, and so does its cap.
// This is the record-layer half of "a volume's lifecycle is decoupled from any
// sandbox" (§5.C) — the byte-layer half is the backend's, asserted in volumetest.
func testVolumeDurability(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	if err := s.CreateVolume(ctx, model.Volume{
		ID: "vol_p", Backend: "local_btrfs", CapByte: 16 << 20, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	s2 := reopen(t)
	got, err := s2.GetVolume(ctx, "vol_p")
	if err != nil {
		t.Fatalf("volume lost across reopen: %v", err)
	}
	if got.CapByte != 16<<20 {
		t.Errorf("cap lost across reopen: %d, want %d — the record is the cap's only authority (volume.md D7.3)", got.CapByte, 16<<20)
	}
	if got.Backend != "local_btrfs" {
		t.Errorf("backend lost across reopen: %q — a later operation must route to the SAME backend that allocated the bytes", got.Backend)
	}
}

// testReferenceReclaimRace is the DYNAMIC half of §4.3 (model.md reference-reclaim
// atomicity): the static "referenced → cannot delete" judgment is not enough; the spec adds
// an invariant closing the reclaim-vs-reference TOCTOU window. This races a DeleteTemplate
// against several CreateSandbox referrers AND a child CreateTemplate, all referencing the
// SAME Template, launched simultaneously.
//
// The invariant (per §4.3): reference and reclaim NEVER interleave — either
//   - the DELETE commits first → the template is gone and EVERY referencing
//     create loses with ErrTemplateNotFound, OR
//   - a reference commits first → the DELETE loses with ErrInUse and the template
//     survives.
//
// Never both, never a torn state. Runs on BOTH backends, so the SQLite FK-RESTRICT path and
// the Memory Go-check path prove the SAME contract. Run under -race.
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

// testNetworkRoundTrip pins the §5.D declaration's column-level contract: it survives
// a write/read cycle intact, and UpdateBinding replaces it WHOLE (§6.2 — no field-level
// merge) while leaving the other Binding fields alone.
//
// The empty-array shape is part of it: §6.1 requires the three arrays to read back as `[]`
// and never as null (model.NetworkDecl.Normalize).
func testNetworkRoundTrip(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	rec := mkRec("n1")
	rec.Network = model.NetworkDecl{
		Egress: model.NetEgress{
			Default: model.NetDeny,
			Allow:   []model.NetTuple{{CIDR: netip.MustParsePrefix("1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Via: []model.NetVia{{
				Match:    []model.NetTuple{{CIDR: netip.MustParsePrefix("9.9.9.9/32"), Proto: model.NetTCP}},
				Endpoint: "127.0.0.9:15443",
			}},
		},
		Ingress: model.NetIngress{Publish: []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}},
	}
	if err := s.CreateSandbox(ctx, rec, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandbox(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Network.Egress.Default != model.NetDeny {
		t.Errorf("egress.default = %q; want %q", got.Network.Egress.Default, model.NetDeny)
	}
	if len(got.Network.Egress.Allow) != 1 || got.Network.Egress.Allow[0].Port != 443 ||
		got.Network.Egress.Allow[0].CIDR.String() != "1.1.1.1/32" {
		t.Errorf("allow[] did not round-trip: %+v", got.Network.Egress.Allow)
	}
	if len(got.Network.Egress.Via) != 1 || got.Network.Egress.Via[0].Endpoint != "127.0.0.9:15443" ||
		len(got.Network.Egress.Via[0].Match) != 1 {
		t.Errorf("via[] did not round-trip: %+v", got.Network.Egress.Via)
	}
	if len(got.Network.Ingress.Publish) != 1 || got.Network.Ingress.Publish[0].GuestPort != 8080 {
		t.Errorf("publish[] did not round-trip: %+v", got.Network.Ingress.Publish)
	}

	// WHOLE-FOR-WHOLE replacement, and the other Binding fields untouched by it.
	next := model.NetworkDecl{Egress: model.NetEgress{Default: model.NetAllow}}.Normalize()
	if err := s.UpdateBinding(ctx, "n1", rec.State, store.Binding{Network: &next}, nil); err != nil {
		t.Fatalf("UpdateBinding(network): %v", err)
	}
	after, err := s.GetSandbox(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Network.Egress.Default != model.NetAllow {
		t.Errorf("egress.default = %q after replacement; want %q", after.Network.Egress.Default, model.NetAllow)
	}
	if len(after.Network.Egress.Allow) != 0 || len(after.Network.Egress.Via) != 0 ||
		len(after.Network.Ingress.Publish) != 0 {
		t.Errorf("the declaration was MERGED rather than replaced whole (§6.2): %+v", after.Network)
	}
	// …and the arrays must be `[]`, not nil: §6.1's read face never emits null.
	if after.Network.Egress.Allow == nil || after.Network.Egress.Via == nil ||
		after.Network.Ingress.Publish == nil {
		t.Errorf("an array read back as nil; §6.1 requires `[]` so the read face's output stays a "+
			"legal write body: %+v", after.Network)
	}
	if after.Limits != got.Limits || after.IdleTimeout != got.IdleTimeout {
		t.Errorf("a network-only update touched another Binding field: %+v → %+v",
			got.Limits, after.Limits)
	}
}

// testNetworkNotAliased is the read-SHAPE half, and it is the one that FAILED: a caller
// must not be able to mutate the store by writing through a value it read.
//
// SQLite cannot alias — it re-decodes JSON on every read — so an in-memory backend that
// hands out its live backing arrays makes the same mediator bug destructive against one
// backend and invisible against the other (model.NetworkDecl.Clone).
func testNetworkNotAliased(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	rec := mkRec("n2")
	rec.Network = model.NetworkDecl{
		Egress: model.NetEgress{
			Default: model.NetDeny,
			Allow:   []model.NetTuple{{CIDR: netip.MustParsePrefix("1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Via: []model.NetVia{{
				Match:    []model.NetTuple{{CIDR: netip.MustParsePrefix("9.9.9.9/32"), Proto: model.NetTCP}},
				Endpoint: "127.0.0.9:15443",
			}},
		},
		Ingress: model.NetIngress{Publish: []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}},
	}
	if err := s.CreateSandbox(ctx, rec, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandbox(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	// A caller mutating what it read, at every nesting depth that carries a slice.
	got.Network.Ingress.Publish[0].GuestPort = 9999
	got.Network.Egress.Allow[0].Port = 1
	got.Network.Egress.Via[0].Match[0].Proto = model.NetUDP

	again, err := s.GetSandbox(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	if again.Network.Ingress.Publish[0].GuestPort != 8080 {
		t.Errorf("a caller's in-place edit reached the store: publish[0].guest_port = %d, want 8080 "+
			"— the backend handed out its live backing array, which makes a mediator bug corrupt "+
			"state on THIS backend and be invisible on the other",
			again.Network.Ingress.Publish[0].GuestPort)
	}
	if again.Network.Egress.Allow[0].Port != 443 {
		t.Errorf("a caller's in-place edit reached the store: allow[0].port = %d, want 443",
			again.Network.Egress.Allow[0].Port)
	}
	if again.Network.Egress.Via[0].Match[0].Proto != model.NetTCP {
		t.Errorf("a caller's in-place edit reached a NESTED slice: via[0].match[0].proto = %q, want "+
			"tcp — the outer slice was copied but each Via carries its own match list",
			again.Network.Egress.Via[0].Match[0].Proto)
	}
}

// testEventKindClosed pins §4.1's closed set: each of the six is writable, and a kind
// outside it is REFUSED rather than persisted.
//
// The refusal direction is the one that matters and the one nothing asserted before
// (model.EventKind carries the typo counterexample).
func testEventKindClosed(t *testing.T, mk Factory) {
	ctx := context.Background()

	// Every declared kind is writable. Iterating model.EventKinds rather than a hand
	// list keeps this in step with the type: adding a kind without teaching the store
	// about it fails here.
	for i, k := range model.EventKinds {
		s, _ := mk(t)
		seedTmpl(t, s, "bare")
		if err := s.CreateSandbox(ctx, mkRec("k1"), nil); err != nil {
			t.Fatal(err)
		}
		if err := s.CompareAndSetState(ctx, "k1", model.Creating, model.Running,
			ev("k1", fmt.Sprintf("evt_ok_%d", i), k, model.Running), nil); err != nil {
			t.Errorf("the declared kind %q was refused: %v — model.EventKinds and the store must "+
				"agree on §4.1's set", k, err)
		}
	}

	// …and one OUTSIDE the set is refused. `crash_paused` deliberately: an underscore
	// for a hyphen is the realistic typo, and it is invisible to review.
	s, _ := mk(t)
	seedTmpl(t, s, "bare")
	if err := s.CreateSandbox(ctx, mkRec("k2"), nil); err != nil {
		t.Fatal(err)
	}
	err := s.CompareAndSetState(ctx, "k2", model.Creating, model.Running,
		ev("k2", "evt_bad", model.EventKind("crash_paused"), model.Running), nil)
	if err == nil {
		t.Error("an undeclared event kind was PERSISTED — §4.1 states a closed set of six, and a " +
			"seventh reaches subscribers as something no reader handles. The whole point of naming " +
			"the set is that the store cannot write outside it")
	}
	// …and the refusal must not have committed the state change either: the event and
	// the transition are ONE commit (§4.4-C), so a rejected event means a rejected
	// transition.
	if rec, gerr := s.GetSandbox(ctx, "k2"); gerr == nil && rec.State != model.Creating {
		t.Errorf("the state moved to %q despite the event being refused — the transition and its "+
			"event are one atomic commit (§4.4-C)", rec.State)
	}
}

// --- Mount table (§5.C / §4.3) -----------------------------------------------

// seedVol inserts a Volume row so a mount referencing it satisfies the FK.
func seedVol(t *testing.T, s store.Store, id string) {
	t.Helper()
	if err := s.CreateVolume(context.Background(), model.Volume{
		ID: model.VolumeID(id), Backend: "test", CreatedAt: time.Now(),
	}); err != nil && err != store.ErrConflict {
		t.Fatalf("seed volume %q: %v", id, err)
	}
}

// seedSbx inserts a sandbox row (template pre-seeded) so a mount referencing it
// satisfies the FK.
func seedSbx(t *testing.T, s store.Store, id string) {
	t.Helper()
	// Tolerates being called for several sandboxes in one subtest, unlike seedTmpl
	// (which fatals on a duplicate — correct for its own callers, which seed once).
	if err := s.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil && err != store.ErrConflict {
		t.Fatalf("seed template: %v", err)
	}
	if err := s.CreateSandbox(context.Background(), model.Sandbox{
		ID: model.SandboxID(id), Template: "bare", Overlay: model.OverlayID("ov_" + id),
		State: model.Running, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil && err != store.ErrConflict {
		t.Fatalf("seed sandbox %q: %v", id, err)
	}
}

func mount(sbx, vol, path string, access model.AccessMode) model.Mount {
	return model.Mount{
		Sandbox: model.SandboxID(sbx), Volume: model.VolumeID(vol),
		Path: path, Access: access,
	}
}

// testMountCRUD: rows round-trip, both list views work, and delete is idempotent.
// No event is published for any of it — a mount row is not a §4.1 transition (§4.3).
func testMountCRUD(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")
	seedVol(t, s, "vol_2")

	binding := []model.Mount{
		mount("sbx_a", "vol_1", "/data", model.SharedReadWrite),
		mount("sbx_a", "vol_2", "/cache", model.SharedReadOnly),
	}
	if err := s.CreateMounts(ctx, binding); err != nil {
		t.Fatalf("create mounts: %v", err)
	}
	got, err := s.ListMounts(ctx, "sbx_a")
	if err != nil {
		t.Fatalf("list mounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list returned %d rows, want 2: %+v", len(got), got)
	}
	// Ordered by path, so a caller re-materializing a binding gets a stable topology
	// rather than map order.
	if got[0].Path != "/cache" || got[1].Path != "/data" {
		t.Errorf("rows are not path-ordered: %+v", got)
	}
	if got[1].Access != model.SharedReadWrite {
		t.Errorf("access did not round-trip: %+v", got[1])
	}

	byVol, err := s.ListMountsByVolume(ctx, "vol_1")
	if err != nil {
		t.Fatalf("list by volume: %v", err)
	}
	if len(byVol) != 1 || byVol[0].Sandbox != "sbx_a" {
		t.Errorf("list by volume = %+v, want one row for sbx_a", byVol)
	}

	// NO event was published by any of this (§4.3: mount rows are not transitions).
	evs, _, _, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Kind), "mount") {
			t.Errorf("a mount row published an event (%q) — mount rows are not §4.1 transitions", ev.Kind)
		}
	}

	if err := s.DeleteMounts(ctx, "sbx_a"); err != nil {
		t.Fatalf("delete mounts: %v", err)
	}
	if got, _ = s.ListMounts(ctx, "sbx_a"); len(got) != 0 {
		t.Errorf("rows survived delete: %+v", got)
	}
	// Idempotent — destroy, unwind and evict all reach it.
	if err := s.DeleteMounts(ctx, "sbx_a"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// testMountRestrictsVolume is §6.2 verdict ①: a volume held by a live mount row
// cannot be deleted, and the verdict is ErrVolumeInUse — NOT ErrInUse, whose text
// names a Template and would misdirect the caller.
func testMountRestrictsVolume(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.SharedReadWrite)}); err != nil {
		t.Fatalf("create mount: %v", err)
	}
	err := s.DeleteVolume(ctx, "vol_1")
	if !errors.Is(err, store.ErrVolumeInUse) {
		t.Fatalf("delete a mounted volume = %v; want ErrVolumeInUse", err)
	}
	if errors.Is(err, store.ErrInUse) {
		t.Errorf("the verdict reuses ErrInUse, whose text says \"template in use\" — a 409 naming the wrong object sends the caller to look at something unrelated (§6.2)")
	}
	// Released once the row is gone.
	if err := s.DeleteMounts(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume(ctx, "vol_1"); err != nil {
		t.Errorf("delete after the mount row was removed: %v", err)
	}
}

// testMountRestrictsSandbox is §6.2 verdict ②: a sandbox held by a live mount row
// cannot be deleted.
//
// The point of the case is the TRANSLATION: without a sentinel, DeleteSandbox returns
// the raw driver error and the API's default arm makes it a 500. The FK-violation
// predicate is reusable across tables; the translation is not.
func testMountRestrictsSandbox(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.SharedReadWrite)}); err != nil {
		t.Fatalf("create mount: %v", err)
	}
	err := s.DeleteSandbox(ctx, "sbx_a", nil)
	if !errors.Is(err, store.ErrSandboxHasMounts) {
		t.Fatalf("delete a sandbox with live mounts = %v; want ErrSandboxHasMounts (an untranslated driver error reaches the API's default arm as a 500)", err)
	}
	// And the ORDER is what resolves it: mounts first, then the sandbox. Measured,
	// RESTRICT fires immediately, so no transaction wrapping buys order-independence.
	if err := s.DeleteMounts(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSandbox(ctx, "sbx_a", nil); err != nil {
		t.Errorf("delete after mounts were removed: %v", err)
	}
}

// testExclusiveLease is the ONE new store invariant Volume introduces (§5.C): a
// volume has at most one `exclusive` mount row.
func testExclusiveLease(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedSbx(t, s, "sbx_b")
	seedVol(t, s, "vol_1")

	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Fatalf("first exclusive attach: %v", err)
	}
	err := s.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/data", model.Exclusive)})
	if !errors.Is(err, store.ErrMountLeaseConflict) {
		t.Fatalf("second exclusive attach = %v; want ErrMountLeaseConflict", err)
	}
	// The lease is RELEASED by the row going away — there is no lease object to release
	// separately (§5.C).
	if err := s.DeleteMounts(ctx, "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Errorf("attach after the holder released: %v — the lease IS the row's existence", err)
	}
}

// testExclusiveVsShared is the HALF THE PARTIAL INDEX CANNOT COVER, and the reason
// the invariant needs the atomic statement rather than the index alone (§5.C, and the
// mounts_exclusive_lease comment).
//
// Both directions are checked, because a predicate that only looks at the row being
// inserted covers just one of them.
func testExclusiveVsShared(t *testing.T, mk Factory) {
	ctx := context.Background()

	t.Run("shared cannot join an exclusive holder", func(t *testing.T) {
		s, _ := mk(t)
		seedSbx(t, s, "sbx_a")
		seedSbx(t, s, "sbx_b")
		seedVol(t, s, "vol_1")
		if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
			t.Fatalf("exclusive attach: %v", err)
		}
		if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/data", model.SharedReadWrite)}); !errors.Is(err, store.ErrMountLeaseConflict) {
			t.Errorf("a SHARED attach onto an exclusively-held volume = %v; want ErrMountLeaseConflict — the partial index alone permits this, which is why it is only half the invariant", err)
		}
	})

	t.Run("exclusive cannot join a shared holder", func(t *testing.T) {
		s, _ := mk(t)
		seedSbx(t, s, "sbx_a")
		seedSbx(t, s, "sbx_b")
		seedVol(t, s, "vol_1")
		if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.SharedReadWrite)}); err != nil {
			t.Fatalf("shared attach: %v", err)
		}
		if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/data", model.Exclusive)}); !errors.Is(err, store.ErrMountLeaseConflict) {
			t.Errorf("an EXCLUSIVE attach onto a shared-held volume = %v; want ErrMountLeaseConflict", err)
		}
	})

	t.Run("two SHARED mounts coexist", func(t *testing.T) {
		s, _ := mk(t)
		seedSbx(t, s, "sbx_a")
		seedSbx(t, s, "sbx_b")
		seedVol(t, s, "vol_1")
		for _, sbx := range []string{"sbx_a", "sbx_b"} {
			if err := s.CreateMounts(ctx, []model.Mount{mount(sbx, "vol_1", "/data", model.SharedReadWrite)}); err != nil {
				t.Fatalf("shared attach for %s: %v — \"at most one rw row\" would contradict SharedReadWrite's whole fanout (§5.C)", sbx, err)
			}
		}
		rows, err := s.ListMountsByVolume(ctx, "vol_1")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Errorf("two shared mounts of one volume produced %d rows, want 2", len(rows))
		}
	})
}

// testMountBindingInvalid is the §6.2 400 set: a binding that CONTRADICTS ITSELF.
//
// 400 rather than 409 because it is decidable without reading any other row and no retry can
// make it legal. A sandbox holds one COMPLETE binding and PATCH swaps whole for whole, so
// there is no "add one mount to an existing set" operation a duplicate could be relative to.
func testMountBindingInvalid(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	for _, v := range []string{"vol_1", "vol_2"} {
		seedVol(t, s, v)
	}

	for _, tc := range []struct {
		name    string
		binding []model.Mount
		why     string
	}{{
		name: "same destination twice",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data", model.SharedReadWrite),
			mount("sbx_a", "vol_2", "/data", model.SharedReadWrite),
		},
		why: "the second bind SHADOWS the first, so one volume is invisible in the guest while still counting in §3.4 and still holding its lease",
	}, {
		name: "same volume twice",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data", model.SharedReadWrite),
			mount("sbx_a", "vol_1", "/other", model.SharedReadWrite),
		},
		why: "the rule stays single instead of forking by access mode",
	}, {
		name: "one volume nested inside another",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data", model.SharedReadWrite),
			mount("sbx_a", "vol_2", "/data/sub", model.SharedReadWrite),
		},
		why: "UNIQUE(sandbox,path) CANNOT catch this — /data and /data/sub are different strings, so nesting needs a prefix scan",
	}, {
		name: "un-normalized path",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data/", model.SharedReadWrite),
		},
		why: "the uniqueness assertions are literal string comparisons, so an un-normalized path makes them silently not apply",
	}, {
		name: "the guest root as destination",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/", model.SharedReadWrite),
		},
		why: `"/" is the one illegal destination that is ALREADY NORMALIZED — path.Clean's fixed point — so the normalization check cannot catch it, and a bind there shadows the WHOLE guest rootfs (§5.C: absolute, cleaned, NOT "/")`,
	}, {
		// The spellings that normalize TO "/". They are refused by the normalization check
		// rather than the root check, and both verdicts are the same 400 — asserted so the
		// two checks together cover the family, not just the one spelling each was written
		// for.
		name: "a path that normalizes to the guest root",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data/..", model.SharedReadWrite),
		},
		why: `"/data/.." IS the guest root spelled indirectly; a normalize-then-accept would bind over the whole rootfs`,
	}, {
		name: "the guest root spelled with a trailing dot",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/.", model.SharedReadWrite),
		},
		why: `same mount point as "/", a different string — literal UNIQUE comparisons make the difference silent`,
	}, {
		name: "unknown access mode",
		binding: []model.Mount{
			mount("sbx_a", "vol_1", "/data", model.AccessMode("Exclusive")),
		},
		why: "a mis-cased value makes the lease partial index NOT APPLY, letting two exclusive rows both insert — a silent failure of the one new invariant",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.CreateMounts(ctx, tc.binding)
			if !errors.Is(err, store.ErrMountBindingInvalid) {
				t.Fatalf("CreateMounts = %v; want ErrMountBindingInvalid (%s)", err, tc.why)
			}
			// A rejected binding inserted NOTHING.
			rows, _ := s.ListMounts(ctx, "sbx_a")
			if len(rows) != 0 {
				t.Errorf("a rejected binding left %d row(s) behind: %+v", len(rows), rows)
			}
		})
	}

	// The guest root survives the MEDIATOR'S PATH too, which is the shape a real request
	// takes: the mediator normalizes every path before insert (§5.C), so a check that only
	// caught the un-normalized spellings would pass every case above and still let
	// NormalizeMountPath("/data/..") through as a legal "/". Asserted as a property over
	// the whole family rather than one string, because a bind over the guest rootfs is
	// exactly the kind of failure nothing downstream reports.
	for _, spelling := range []string{"/", "//", "/.", "/data/..", " / ", "/./", "///."} {
		normalized := store.NormalizeMountPath(spelling)
		if normalized != "/" {
			t.Errorf("NormalizeMountPath(%q) = %q; this case is meant to normalize to the guest root", spelling, normalized)
			continue
		}
		err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", normalized, model.SharedReadWrite)})
		if !errors.Is(err, store.ErrMountBindingInvalid) {
			t.Errorf("CreateMounts with %q normalized to %q = %v; want ErrMountBindingInvalid — a bind at the guest root shadows the entire sandbox rootfs",
				spelling, normalized, err)
		}
		if rows, _ := s.ListMounts(ctx, "sbx_a"); len(rows) != 0 {
			t.Fatalf("a mount at the guest root (from %q) was INSERTED: %+v", spelling, rows)
		}
	}
}

// testMountAllOrNothing: if any one mount in a binding loses the lease, NONE is
// inserted. A sandbox holds one complete binding, so a partially-applied one is not a
// state the model has.
func testMountAllOrNothing(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_holder")
	seedSbx(t, s, "sbx_b")
	seedVol(t, s, "vol_free")
	seedVol(t, s, "vol_taken")

	// vol_taken is exclusively held by someone else.
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_holder", "vol_taken", "/x", model.Exclusive)}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	// A two-mount binding where the FIRST would succeed and the second loses.
	err := s.CreateMounts(ctx, []model.Mount{
		mount("sbx_b", "vol_free", "/a", model.SharedReadWrite),
		mount("sbx_b", "vol_taken", "/b", model.Exclusive),
	})
	if !errors.Is(err, store.ErrMountLeaseConflict) {
		t.Fatalf("binding with one contended lease = %v; want ErrMountLeaseConflict", err)
	}
	rows, err := s.ListMounts(ctx, "sbx_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a lost binding left %d row(s) behind: %+v — all-or-nothing means the first mount must not survive the second's failure", len(rows), rows)
	}
}

// testReplaceMountsKeepsOldBindingOnRefusal is the first of §5.C's two required
// ReplaceMounts cases: a replacement that LOSES leaves the OLD binding intact and
// observable (§5.C consequence ①).
//
// Under the two-commit shape this could not hold — the delete had already committed, so the
// old binding's survival depended on a best-effort re-insert that could itself fail, and the
// failure mode was "the new binding was refused AND the old one is gone". The atomic promise
// removes that state, which is why the compensation code went with it.
//
// It also pins the RE-SUBMITTED IDENTICAL BINDING, the case that forces delete-before-insert.
func testReplaceMountsKeepsOldBindingOnRefusal(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedSbx(t, s, "sbx_holder")
	seedVol(t, s, "vol_1")
	seedVol(t, s, "vol_taken")

	// sbx_a's binding: one exclusive mount. This is what must survive every refusal.
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	// Someone else holds vol_taken exclusively, so any replacement naming it loses.
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_holder", "vol_taken", "/x", model.Exclusive)}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}

	assertOriginal := func(what string) {
		t.Helper()
		rows, err := s.ListMounts(ctx, "sbx_a")
		if err != nil {
			t.Fatalf("%s: list mounts: %v", what, err)
		}
		if len(rows) != 1 || rows[0].Volume != "vol_1" || rows[0].Path != "/data" || rows[0].Access != model.Exclusive {
			t.Fatalf("%s left sbx_a with %+v; want the ORIGINAL exclusive vol_1@/data binding — a refused replacement must not detach anything (§5.C consequence ①)", what, rows)
		}
		// And the lease was never released: the row that IS the lease is still there.
		byVol, err := s.ListMountsByVolume(ctx, "vol_1")
		if err != nil {
			t.Fatalf("%s: list by volume: %v", what, err)
		}
		if len(byVol) != 1 || byVol[0].Sandbox != "sbx_a" {
			t.Fatalf("%s: vol_1 is held by %+v; want sbx_a's single exclusive row (consequence ②: the lease is never transiently released)", what, byVol)
		}
	}

	// ① The replacement loses a lease. Note it KEEPS vol_1 and adds a contended volume,
	// which is the realistic PATCH: "keep what I have, add this one".
	err := s.ReplaceMounts(ctx, "sbx_a", []model.Mount{
		mount("sbx_a", "vol_1", "/data", model.Exclusive),
		mount("sbx_a", "vol_taken", "/other", model.Exclusive),
	})
	if !errors.Is(err, store.ErrMountLeaseConflict) {
		t.Fatalf("replacement onto a contended lease = %v; want ErrMountLeaseConflict (the API maps it to 409)", err)
	}
	assertOriginal("a lease-contended replacement")

	// ② The replacement contradicts ITSELF (two volumes at one destination). A 400, and
	// the old binding must be just as intact — the verdict differs, the guarantee does not.
	err = s.ReplaceMounts(ctx, "sbx_a", []model.Mount{
		mount("sbx_a", "vol_1", "/data", model.SharedReadWrite),
		mount("sbx_a", "vol_taken", "/data", model.SharedReadWrite),
	})
	if !errors.Is(err, store.ErrMountBindingInvalid) {
		t.Fatalf("self-contradictory replacement = %v; want ErrMountBindingInvalid (the API maps it to 400)", err)
	}
	assertOriginal("a self-contradictory replacement")

	// ③ A row naming a DIFFERENT sandbox is refused too, and this one matters because the
	// alternative is destructive: the delete targets sbx_a, so accepting it would detach
	// sbx_a and attach sbx_holder in a commit the caller thinks touched one sandbox.
	err = s.ReplaceMounts(ctx, "sbx_a", []model.Mount{mount("sbx_holder", "vol_1", "/data", model.Exclusive)})
	if !errors.Is(err, store.ErrMountBindingInvalid) {
		t.Fatalf("replacement carrying another sandbox's row = %v; want ErrMountBindingInvalid", err)
	}
	assertOriginal("a replacement naming another sandbox")

	// ④ The RE-SUBMITTED IDENTICAL binding SUCCEEDS. This is the case that forces
	// delete-before-insert: against the un-filtered rows it would self-contend on both the
	// lease and UNIQUE(sandbox, path).
	if err := s.ReplaceMounts(ctx, "sbx_a", []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Fatalf("re-submitting the IDENTICAL binding = %v; want success — a replacement must not contend with the rows it is replacing", err)
	}
	assertOriginal("an identical re-submission")

	// ⑤ And a successful replacement really does replace: empty means detach everything
	// (§6.2 — PATCH swaps whole bindings, so this is not a special case).
	if err := s.ReplaceMounts(ctx, "sbx_a", nil); err != nil {
		t.Fatalf("replacement with an EMPTY binding = %v; want success (detach everything)", err)
	}
	if rows, _ := s.ListMounts(ctx, "sbx_a"); len(rows) != 0 {
		t.Errorf("an empty replacement left %+v; want no rows", rows)
	}
	// The lease is released by that, so the previously-blocked volume is now takeable.
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_holder", "vol_1", "/data2", model.Exclusive)}); err != nil {
		t.Errorf("vol_1 after a detaching replacement = %v; want takeable (the lease IS the row, so removing it IS the release)", err)
	}
}

// testReplaceMountsNoZeroRowWindow is the ADVERSARIAL case for ReplaceMounts, in the shape
// of testSequenceNoVisibilityGap: a concurrent ListMountsByVolume must NEVER observe zero
// rows while a binding is being replaced.
//
// WHY THIS IS THE ONE THAT FAILED SILENTLY. ListMountsByVolume is the write-regime
// predicate (§5.C): "no mount rows for this volume" means "nobody has it mounted, so a
// direct host-side write is safe". A replacement implemented as DeleteMounts followed by
// CreateMounts opens exactly that window, and nothing about it is visible to a
// single-threaded test. Measured against the two-commit shape: 299/31,653 and 9,103/196,029
// zero-row observations across two runs — thousands, not a rare race.
//
// Rebinding to the IDENTICAL binding is deliberate: it is the case that forces
// delete-before-insert, so it is the one an implementation is most tempted to serve with
// two calls.
//
// A read cannot be caught INSIDE a correct replacement — both take the same exclusive lock,
// so it serializes before or after. What a torn implementation does differently is DROP that
// lock mid-way, which is why the reader only has to be live across the replacements rather
// than land at some instant. The writer therefore waits for one read per replacement (see
// the handshake below), instead of the earlier `reads >= replacements` asserted after the
// fact: that number measures sync.Mutex handoff and the host's CPU count, not the store —
// 115,700,882 observations at GOMAXPROCS=1 against 41,321 at GOMAXPROCS=8 and 86 on a 2-vCPU
// hosted runner, all on one unchanged tree.
func testReplaceMountsNoZeroRowWindow(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_watched")

	binding := []model.Mount{mount("sbx_a", "vol_watched", "/data", model.Exclusive)}
	if err := s.CreateMounts(ctx, binding); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	const replacements = 300
	var (
		reads     atomic.Int64 // total observations the reader made
		zeroReads atomic.Int64 // observations of ZERO rows — every one is a defect
		wrongRows atomic.Int64 // observations of a row count that is neither 0 nor 1
		readErrs  atomic.Int64 // failed reads: they observe nothing, so they cannot gate a turn
		writeErrs int
	)
	// The reader must be RUNNING before the first replacement. Without this barrier the
	// test is vacuous on a fast backend rather than merely weak: measured on Memory, all
	// 300 replacements completed in microseconds and the reader goroutine had been
	// scheduled exactly ONCE — after the writer was done. The barrier gets it started; the
	// per-replacement handshake below is what keeps it running.
	ready := make(chan struct{})
	done := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		first := true
		for {
			rows, err := s.ListMountsByVolume(ctx, "vol_watched")
			if err == nil {
				switch len(rows) {
				case 0:
					zeroReads.Add(1)
				case 1:
				default:
					wrongRows.Add(1)
				}
				// Incremented LAST, after the observation is recorded. The writer treats a
				// bump as "a read completed", so bumping first would let it proceed on a
				// read whose verdict is not yet counted.
				reads.Add(1)
			} else {
				readErrs.Add(1)
			}
			if first {
				first = false
				close(ready)
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	<-ready

	for i := 0; i < replacements; i++ {
		// Sampled BEFORE the replacement, so the read the writer waits for below is one
		// that could not have completed before this replacement started.
		before := reads.Load()
		if err := s.ReplaceMounts(ctx, "sbx_a", binding); err != nil {
			writeErrs++
			t.Errorf("replacement %d = %v; want success (identical binding)", i, err)
		}
		// THE HANDSHAKE, and the reason this test is not scheduler-dependent. Waiting for
		// the reader to complete one read per replacement makes `reads >= replacements`
		// true by CONSTRUCTION on any host, where asserting it afterwards only expressed a
		// hope the scheduler was free to refuse.
		//
		// It does not merely preserve detection, it SHARPENS it, and the reason is worth
		// keeping: a torn replacement commits its DELETE and drops the lock, so the read
		// this loop waits for is one that lands in the zero-row window. Measured against
		// the two-commit shape on SQLite, 6 runs: exactly 300 zero-row observations every
		// time, one per replacement. The old free-running hunt got 290-294 of 300 under the
		// same starvation, i.e. it MISSED windows this cannot miss.
		//
		// It also makes the test cheaper on SQLite rather than dearer (476ms -> 38ms):
		// the old unconditional Gosched handed the connection lock away every iteration
		// and waited ~1.5ms to get it back. Total reads there fall (38k -> ~500) because
		// the run is shorter, not because the reader is starved — and the floor is what
		// this assertion needs, not the volume.
		//
		// Gosched rather than a sleep or a spin: the writer holds no lock here, and at
		// GOMAXPROCS=1 yielding is the only way the reader runs at all.
		for reads.Load() == before {
			if readErrs.Load() > 0 {
				break // a reader that cannot read will never bump the counter — reported below
			}
			runtime.Gosched()
		}
	}
	close(done)
	<-readerDone

	if zeroReads.Load() > 0 {
		t.Fatalf("the write-regime predicate observed ZERO mount rows %d time(s) out of %d reads across %d replacements — the volume looked UNMOUNTED mid-replacement, which would wrongly admit a direct host-side write against a volume a live sandbox holds (§5.C consequence ③). A replacement must be ONE commit.",
			zeroReads.Load(), reads.Load(), replacements)
	}
	if wrongRows.Load() > 0 {
		t.Errorf("%d observation(s) saw a row count other than 0 or 1 — the exclusive lease was doubled mid-replacement", wrongRows.Load())
	}
	if writeErrs > 0 {
		t.Fatalf("%d replacement(s) failed; the concurrency assertion above is not meaningful", writeErrs)
	}
	// A failing read observes nothing, so it can neither catch a zero-row window nor gate a
	// turn — and the handshake above stops waiting on one, which would otherwise hang. It is
	// reported rather than tolerated: ListMountsByVolume is not permitted to fail here, and
	// a run where it did is a run that hunted with fewer observations than it claims.
	if readErrs.Load() > 0 {
		t.Errorf("%d concurrent ListMountsByVolume call(s) returned an error; the reader is the whole instrument here, so those replacements went unobserved", readErrs.Load())
	}
	// NON-VACUITY, now an invariant rather than a hope: the handshake waits for a read per
	// replacement, so this can only trip if that loop broke out on a read error. It stays
	// because it states what the handshake is FOR — a reader that never ran would report
	// zero violations no matter how torn the implementation is (measured on Memory before
	// the barrier existed: 1 observation for 300 writes).
	if reads.Load() < replacements {
		t.Errorf("the reader made %d observation(s) across %d replacements — too few to have been interleaved, so the zero-row assertion passed vacuously", reads.Load(), replacements)
	}
	// And the binding is intact at the end.
	if rows, _ := s.ListMounts(ctx, "sbx_a"); len(rows) != 1 {
		t.Errorf("after %d replacements the binding is %+v; want the single row", replacements, rows)
	}
	t.Logf("%d replacements, %d concurrent ListMountsByVolume observations, %d saw zero rows", replacements, reads.Load(), zeroReads.Load())
}

// testMountMissingEndpoint is the reference side of §4.3 atomicity: a mount whose
// sandbox or volume a concurrent delete already removed loses cleanly, with the
// verdict naming WHICH endpoint is missing.
func testMountMissingEndpoint(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")

	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_gone", "/data", model.SharedReadWrite)}); !errors.Is(err, store.ErrVolumeNotFound) {
		t.Errorf("mount referencing an absent volume = %v; want ErrVolumeNotFound", err)
	}
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_gone", "vol_1", "/data", model.SharedReadWrite)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mount referencing an absent sandbox = %v; want ErrNotFound", err)
	}
}

// testMountVerdictsDistinct pins §6.2's requirement directly: the four Volume
// verdicts must stay MUTUALLY DISTINGUISHABLE.
//
// It exists because of HOW this degrades — someone later collapses two sentinels that
// happen to map to the same status code, and the 409 then names the wrong object.
func testMountVerdictsDistinct(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedSbx(t, s, "sbx_b")
	seedVol(t, s, "vol_1")
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Fatal(err)
	}

	// Collect one instance of each reachable verdict.
	verdicts := map[string]error{
		"volume held by a mount row":  s.DeleteVolume(ctx, "vol_1"),
		"sandbox held by a mount row": s.DeleteSandbox(ctx, "sbx_a", nil),
		"lease contended":             s.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/d", model.Exclusive)}),
		"binding self-contradictory": s.CreateMounts(ctx, []model.Mount{
			mount("sbx_b", "vol_1", "/d", model.SharedReadWrite),
			mount("sbx_b", "vol_1", "/e", model.SharedReadWrite),
		}),
	}
	want := map[string]error{
		"volume held by a mount row":  store.ErrVolumeInUse,
		"sandbox held by a mount row": store.ErrSandboxHasMounts,
		"lease contended":             store.ErrMountLeaseConflict,
		"binding self-contradictory":  store.ErrMountBindingInvalid,
	}
	for name, got := range verdicts {
		if !errors.Is(got, want[name]) {
			t.Errorf("%s → %v; want %v", name, got, want[name])
		}
	}
	// And each must NOT match any of the others, which is the property that actually
	// degrades. Note ErrInUse is checked too: it is the one whose text names a Template.
	all := []error{
		store.ErrVolumeInUse, store.ErrSandboxHasMounts,
		store.ErrMountLeaseConflict, store.ErrMountBindingInvalid, store.ErrInUse,
	}
	for name, got := range verdicts {
		matches := 0
		for _, sentinel := range all {
			if errors.Is(got, sentinel) {
				matches++
			}
		}
		if matches != 1 {
			t.Errorf("%s matches %d sentinels, want exactly 1 — the four §6.2 verdicts must be mutually distinguishable, and a 409 naming the wrong object misdirects the caller", name, matches)
		}
	}
}

// testConcurrentLease is the ADVERSARIAL case for §5.C's lease, and the reason the
// judgment and the insert cannot be separated.
//
// A mediator-side pre-check plus an insert is a REAL cross-sandbox TOCTOU: two
// concurrent creates mint two fresh SandboxIDs and hold two DISJOINT transition
// locks, so nothing serializes them. Measured on the model: 8 racers with no
// scheduling hooks broke the lease 2 times in 4000 iterations, and it reproduces every
// time once an explicit scheduling point is added. So the STORE has to arbitrate.
//
// Run under -race. 24 racers, matching the measured configuration.
func testConcurrentLease(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedVol(t, s, "vol_contended")
	const racers = 24
	for i := 0; i < racers; i++ {
		seedSbx(t, s, fmt.Sprintf("sbx_%02d", i))
	}

	start := make(chan struct{})
	results := make(chan error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them all at once, so the window is as tight as possible
			results <- s.CreateMounts(ctx, []model.Mount{
				mount(fmt.Sprintf("sbx_%02d", i), "vol_contended", "/data", model.Exclusive),
			})
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var winners, conflicts, other int
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrMountLeaseConflict):
			conflicts++
		default:
			other++
			t.Errorf("unexpected error from a racer: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers won the exclusive lease, want exactly 1 (conflicts=%d other=%d) — the lease is the ONE new invariant Volume introduces (§5.C)", winners, conflicts, other)
	}
	if other != 0 {
		t.Errorf("%d racers got a driver error rather than a clean verdict; the measured run had ZERO", other)
	}
	// And the table agrees: exactly one row.
	rows, err := s.ListMountsByVolume(ctx, "vol_contended")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("the contended volume has %d mount rows, want 1: %+v", len(rows), rows)
	}
}

// testFKEnforcement guards the ENFORCEMENT ITSELF rather than the verdict each violation
// maps to, because losing it is SILENT: measured, with the foreign-keys pragma off,
// deleting a referenced row simply SUCCEEDS and returns nil. Asserted BEHAVIORALLY, not by
// checking that a DSN string contains something.
//
// It checks BOTH reference directions plus the rows' survival in one place, so the failure
// names the cause ("enforcement is off") rather than one arbitrary verdict that regressed.
func testFKEnforcement(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.SharedReadWrite)}); err != nil {
		t.Fatal(err)
	}
	// Both directions must REFUSE. A backend with enforcement disabled returns nil for both.
	if err := s.DeleteVolume(ctx, "vol_1"); err == nil {
		t.Error("deleting a REFERENCED volume returned nil — reference enforcement is off, which is a silent failure (the verdict tests cannot detect it)")
	}
	if err := s.DeleteSandbox(ctx, "sbx_a", nil); err == nil {
		t.Error("deleting a REFERENCED sandbox returned nil — reference enforcement is off")
	}
	// And the rows really are still there.
	if rows, _ := s.ListMounts(ctx, "sbx_a"); len(rows) != 1 {
		t.Errorf("mount rows after two refused deletes = %d, want 1", len(rows))
	}
	if _, err := s.GetVolume(ctx, "vol_1"); err != nil {
		t.Errorf("the volume was removed despite the refusal: %v", err)
	}
}

// testMountDurability: the mount topology survives a restart, which is what makes it
// a Binding rather than placement state — a resume re-mounts from the CURRENT table
// (§3.4 / §5.C).
func testMountDurability(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, reopen := mk(t)
	if reopen == nil {
		t.Skip("backend is not durable across reopen")
	}
	seedSbx(t, s, "sbx_a")
	seedVol(t, s, "vol_1")
	if err := s.CreateMounts(ctx, []model.Mount{mount("sbx_a", "vol_1", "/data", model.Exclusive)}); err != nil {
		t.Fatal(err)
	}
	s2 := reopen(t)
	rows, err := s2.ListMounts(ctx, "sbx_a")
	if err != nil {
		t.Fatalf("mount rows lost across reopen: %v", err)
	}
	if len(rows) != 1 || rows[0].Access != model.Exclusive || rows[0].Path != "/data" {
		t.Fatalf("mount row did not survive intact: %+v", rows)
	}
	// The LEASE survives too, and that is correct rather than incidental: a crash is
	// dead-recoverable (§4.3), the sandbox will resume onto the same volume, and
	// releasing on crash would make that resume collide. The lease releases only when
	// the row is torn down (§5.C).
	seedSbx(t, s2, "sbx_b")
	if err := s2.CreateMounts(ctx, []model.Mount{mount("sbx_b", "vol_1", "/data", model.Exclusive)}); !errors.Is(err, store.ErrMountLeaseConflict) {
		t.Errorf("the exclusive lease did not survive the restart (got %v) — a crash KEEPING the lease is correct behavior (§5.C)", err)
	}
}

// testVolumeLineage is the fourth FK path: a volume with a live snapshot or clone cannot
// be deleted, and the verdict is DISTINCT from "held by a mount row".
//
// The FK is a CORRECTNESS lever rather than tidiness — the measured quota-leak argument and
// the external counter-example are on model.Volume.Parent (volume.md D6.1a / D7.1).
func testVolumeLineage(t *testing.T, mk Factory) {
	ctx := context.Background()
	s, _ := mk(t)
	seedVol(t, s, "vol_src")
	if err := s.CreateVolume(ctx, model.Volume{
		ID: "vol_snap", Backend: "test", Parent: "vol_src", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create a snapshot with a parent: %v", err)
	}
	// Lineage round-trips.
	got, err := s.GetVolume(ctx, "vol_snap")
	if err != nil {
		t.Fatal(err)
	}
	if got.Parent != "vol_src" {
		t.Errorf("parent = %q, want vol_src", got.Parent)
	}

	// The SOURCE is protected, with its OWN verdict.
	err = s.DeleteVolume(ctx, "vol_src")
	if !errors.Is(err, store.ErrVolumeHasDescendants) {
		t.Fatalf("delete a volume with a live snapshot = %v; want ErrVolumeHasDescendants", err)
	}
	if errors.Is(err, store.ErrVolumeInUse) {
		t.Errorf("the lineage verdict is indistinguishable from the mount verdict — the caller's remedy differs (delete the descendant vs detach a mount), so they must not be collapsed (§6.1)")
	}

	// DELETE THE DESCENDANT FIRST, then the source: the order the FK forces, and the
	// order measured to leak nothing.
	if err := s.DeleteVolume(ctx, "vol_snap"); err != nil {
		t.Fatalf("delete the snapshot: %v", err)
	}
	if err := s.DeleteVolume(ctx, "vol_src"); err != nil {
		t.Errorf("delete the source after its descendant: %v", err)
	}

	// A snapshot naming a parent that does not exist loses cleanly (reference side of
	// §4.3 atomicity).
	if err := s.CreateVolume(ctx, model.Volume{
		ID: "vol_orphan", Backend: "test", Parent: "vol_gone", CreatedAt: time.Now(),
	}); !errors.Is(err, store.ErrVolumeNotFound) {
		t.Errorf("create with a missing parent = %v; want ErrVolumeNotFound", err)
	}
}

// allUnpublished is the bound the contract suite passes to UnpublishedEvents, which
// requires a positive one (a zero-means-everything reading is how the unbounded scan it
// replaced would return). Far above what any contract case writes.
const allUnpublished = 1000
