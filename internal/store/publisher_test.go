package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// kindFor is the §4.1 kind matching a transition INTO state. Tests needing only "a
// valid event" must still name a real kind: both store backends assert §4.1's closed
// set, so a placeholder is refused — which is what the closure is for.
func kindFor(state model.Lifecycle) model.EventKind {
	if state == model.Running {
		return model.EventResumed
	}
	return model.EventPaused
}

func ev(id, evid string, kind model.EventKind, state model.Lifecycle) *store.Event {
	return &store.Event{ID: evid, Sandbox: model.SandboxID(id), Kind: kind, State: state, OccurredAt: time.Now()}
}

// seedPubTmpl inserts the "bare" Template these publisher tests reference, so
// the sandbox insert satisfies the §4.3 template FK.
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
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running), nil)
	_ = st.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "e2", "paused", model.Paused), nil)

	sink := &countingSink{}
	p := store.NewPublisher(st, sink)
	n, err := p.PublishOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("PublishOnce n=%d err=%v; want 2,nil", n, err)
	}
	// nothing left unpublished.
	un, _ := st.UnpublishedEvents(ctx, allUnpublished)
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
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running), nil)

	sink := &countingSink{failNext: errors.New("sink down")}
	p := store.NewPublisher(st, sink)
	if _, err := p.PublishOnce(ctx); err == nil {
		t.Fatal("want delivery error on first pass")
	}
	if un, _ := st.UnpublishedEvents(ctx, allUnpublished); len(un) != 1 {
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
	_ = st.CompareAndSetState(ctx, "a", model.Creating, model.Running, ev("a", "e1", "created", model.Running), nil)
	_ = st.CompareAndSetState(ctx, "a", model.Running, model.Paused, ev("a", "e2", "paused", model.Paused), nil)

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
	un, _ := st.UnpublishedEvents(ctx, allUnpublished)
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
	un2, _ := st2.UnpublishedEvents(ctx, allUnpublished)
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

// --- A2: head-of-line blocking is kept; its operability is what got fixed ----

// blockingSink fails (or blocks) on demand, so a test can hold the head.
type blockingSink struct {
	mu       sync.Mutex
	failWith error         // non-nil → every Deliver fails
	block    chan struct{} // non-nil → Deliver waits on it (ignoring ctx) before returning
	calls    []string      // every Deliver attempt, in order
}

func (s *blockingSink) Deliver(_ context.Context, e store.Event) error {
	s.mu.Lock()
	s.calls = append(s.calls, e.ID)
	fail, block := s.failWith, s.block
	s.mu.Unlock()
	if block != nil {
		<-block // deliberately ignores ctx: models a sink that does not honor deadlines
	}
	return fail
}
func (s *blockingSink) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}
func (s *blockingSink) heal() {
	s.mu.Lock()
	s.failWith = nil
	s.mu.Unlock()
}

// seedEvents commits n transitions on one sandbox, returning the store.
func seedEvents(t *testing.T, n int) store.Store {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	seedPubTmpl(t, st)
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)
	state := model.Creating
	for i := 0; i < n; i++ {
		next := model.Running
		if state == model.Running {
			next = model.Paused
		}
		if err := st.CompareAndSetState(ctx, "a", state, next,
			ev("a", fmt.Sprintf("e%d", i), kindFor(next), next), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		state = next
	}
	return st
}

// Head-of-line blocking is KEPT: a failing head must not let later events pass. Note this
// test asserts the STALL, not its absence (§4.4-C).
func TestPublisherHeadOfLineBlocksLaterEvents(t *testing.T) {
	ctx := context.Background()
	st := seedEvents(t, 3)
	sink := &blockingSink{failWith: errors.New("sink down")}
	p := store.NewPublisherWithConfig(st, sink, store.PublisherConfig{
		MaxAttempts: 5, BaseBackoff: 0, // no backoff: isolate the blocking property
	})

	if _, err := p.PublishOnce(ctx); err == nil {
		t.Fatal("want an error while the head is failing")
	}
	// exactly ONE attempt happened — e1/e2 were not tried past the stuck e0.
	if got := sink.attempts(); got != 1 {
		t.Fatalf("sink saw %d attempts (%v); want 1 — later events must NOT be attempted past a failing head", got, sink.calls)
	}
	if un, _ := st.UnpublishedEvents(ctx, allUnpublished); len(un) != 3 {
		t.Fatalf("%d rows unpublished; want all 3 (nothing may be marked while the head is stuck)", len(un))
	}

	// once the head heals, the whole backlog drains IN ORDER.
	sink.heal()
	n, err := p.PublishOnce(ctx)
	if err != nil || n != 3 {
		t.Fatalf("after heal n=%d err=%v; want 3,nil", n, err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	want := []string{"e0", "e0", "e1", "e2"} // e0 retried, then strict order
	if len(sink.calls) != len(want) {
		t.Fatalf("delivery order %v; want %v", sink.calls, want)
	}
	for i := range want {
		if sink.calls[i] != want[i] {
			t.Fatalf("delivery order %v; want %v", sink.calls, want)
		}
	}
}

// The poison-envelope exit: an event that cannot be delivered is retried a
// BOUNDED number of times, then the publisher halts with a distinguishable
// signal — instead of retrying forever with nothing behind it ever moving.
func TestPublisherPoisonEnvelopeHaltsAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	st := seedEvents(t, 2)
	sink := &blockingSink{failWith: errors.New("undecodable payload")}
	const maxAttempts = 4
	p := store.NewPublisherWithConfig(st, sink, store.PublisherConfig{
		MaxAttempts: maxAttempts, BaseBackoff: 0,
	})

	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if _, lastErr = p.PublishOnce(ctx); lastErr == nil {
			t.Fatalf("pass %d: want a delivery error", i)
		}
	}
	if !errors.Is(lastErr, store.ErrHalted) {
		t.Fatalf("after %d attempts err = %v; want ErrHalted", maxAttempts, lastErr)
	}
	if got := sink.attempts(); got != maxAttempts {
		t.Fatalf("sink saw %d attempts; want exactly %d (the bound must hold)", got, maxAttempts)
	}

	// Halted means STOPPED, not slowed: further passes do not even attempt
	// delivery, and they keep reporting ErrHalted rather than a generic failure.
	for i := 0; i < 3; i++ {
		if _, err := p.PublishOnce(ctx); !errors.Is(err, store.ErrHalted) {
			t.Fatalf("post-halt pass %d err = %v; want ErrHalted", i, err)
		}
	}
	if got := sink.attempts(); got != maxAttempts {
		t.Fatalf("sink saw %d attempts after halting; want %d (a halted publisher must stop retrying)", got, maxAttempts)
	}

	// The signal an operator has to notice.
	s := p.Stats()
	if !s.Halted {
		t.Fatal("Stats().Halted is false after the attempt bound was exhausted")
	}
	if s.HeadEventID != "e0" || s.Attempts != maxAttempts {
		t.Fatalf("Stats() = %+v; want the blocked head e0 with %d attempts", s, maxAttempts)
	}
	if s.LastError == "" {
		t.Fatal("Stats().LastError is empty; the halt must carry WHY")
	}
	// At-least-once is intact throughout: nothing was dropped or marked.
	if un, _ := st.UnpublishedEvents(ctx, allUnpublished); len(un) != 2 {
		t.Fatalf("%d rows unpublished; want 2 — a halt must not drop or mark anything", len(un))
	}

	// Resume is the operator action: a fresh attempt budget, and a healed sink
	// then drains normally.
	sink.heal()
	p.Resume()
	if s := p.Stats(); s.Halted {
		t.Fatal("Resume did not clear the halt")
	}
	n, err := p.PublishOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("after Resume n=%d err=%v; want 2,nil", n, err)
	}
}

// Backoff is respected (a retry inside the window is not attempted), capped, and
// jittered — the three properties store.PublisherConfig argues, each asserted here through
// the OBSERVABLE schedule rather than the internal formula.
func TestPublisherBackoffIsBoundedAndJittered(t *testing.T) {
	ctx := context.Background()
	st := seedEvents(t, 1)
	sink := &blockingSink{failWith: errors.New("sink down")}
	p := store.NewPublisherWithConfig(st, sink, store.PublisherConfig{
		MaxAttempts: 50, BaseBackoff: time.Hour, MaxBackoff: 2 * time.Hour, JitterFraction: 0.2,
	})

	// first pass fails and arms the backoff.
	if _, err := p.PublishOnce(ctx); err == nil {
		t.Fatal("want a delivery error")
	}
	if got := sink.attempts(); got != 1 {
		t.Fatalf("sink saw %d attempts; want 1", got)
	}
	// a pass INSIDE the backoff window must not attempt delivery again.
	for i := 0; i < 5; i++ {
		if _, err := p.PublishOnce(ctx); err != nil {
			t.Fatalf("pass inside the backoff window returned %v; want nil (nothing due yet)", err)
		}
	}
	if got := sink.attempts(); got != 1 {
		t.Fatalf("sink saw %d attempts; want still 1 — the backoff window was not honored", got)
	}
	// the head is still reported as blocked while it waits.
	if s := p.Stats(); s.Attempts != 1 || s.HeadEventID != "e0" || s.HeadSeq == 0 {
		t.Fatalf("Stats() = %+v; want the blocked head e0 visible while waiting", s)
	}

	// The cap holds: with Base=1s, Max=5s, no attempt count can schedule beyond
	// Max plus jitter. Measured through the observable schedule rather than the
	// internal formula: 30 rapid failing passes must never stop being retried,
	// which an uncapped exponential (2^30 s ≈ 34 years) would.
	st2 := seedEvents(t, 1)
	sink2 := &blockingSink{failWith: errors.New("sink down")}
	p2 := store.NewPublisherWithConfig(st2, sink2, store.PublisherConfig{
		MaxAttempts: 100, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, JitterFraction: 0.2,
	})
	deadline := time.Now().Add(2 * time.Second)
	for sink2.attempts() < 20 && time.Now().Before(deadline) {
		_, _ = p2.PublishOnce(ctx)
		time.Sleep(time.Millisecond)
	}
	if got := sink2.attempts(); got < 20 {
		t.Fatalf("only %d attempts in 2s with a 5ms cap; the exponential is not capped", got)
	}

	// Jitter: repeated arming of the SAME attempt number must not produce one
	// identical delay every time, or several drainers would come due together.
	// Observed via the wait: sample when each retry becomes due.
	delays := map[time.Duration]int{}
	for i := 0; i < 12; i++ {
		st3 := seedEvents(t, 1)
		sink3 := &blockingSink{failWith: errors.New("sink down")}
		p3 := store.NewPublisherWithConfig(st3, sink3, store.PublisherConfig{
			MaxAttempts: 5, BaseBackoff: 40 * time.Millisecond, MaxBackoff: time.Second, JitterFraction: 0.5,
		})
		_, _ = p3.PublishOnce(ctx) // arm attempt 1
		start := time.Now()
		for sink3.attempts() < 2 && time.Since(start) < time.Second {
			_, _ = p3.PublishOnce(ctx)
			time.Sleep(time.Millisecond)
		}
		delays[time.Since(start).Round(5*time.Millisecond)]++
	}
	if len(delays) < 2 {
		t.Fatalf("12 armings produced a single distinct delay (%v); the backoff is not jittered", delays)
	}
}

// Observability must survive the worker WEDGING (§4.4-C ③, argued on store.Publisher): a
// sink blocked inside Deliver freezes the drain goroutine, and Stats must keep telling the
// truth from an independent one.
func TestPublisherStatsStaySampleableWhileDrainIsWedged(t *testing.T) {
	ctx := context.Background()
	st := seedEvents(t, 2)
	release := make(chan struct{})
	sink := &blockingSink{block: release} // blocks, and IGNORES the deadline
	p := store.NewPublisherWithConfig(st, sink, store.PublisherConfig{
		MaxAttempts: 5, BaseBackoff: 0, DeliverTimeout: 20 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.PublishOnce(ctx) // wedges inside Deliver
	}()

	// Wait until the drain is actually inside Deliver.
	deadline := time.Now().Add(2 * time.Second)
	for sink.attempts() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sink.attempts() == 0 {
		t.Fatal("Deliver was never entered")
	}

	// The drain goroutine is now stuck. Sampling must still work — and must
	// report a GROWING in-flight age, which is the fact that distinguishes a
	// wedged worker from an idle one.
	var first, second store.Stats
	first = p.Stats() // this call must not block behind the wedged drain
	time.Sleep(60 * time.Millisecond)
	second = p.Stats()

	if second.InFlightFor <= first.InFlightFor {
		t.Fatalf("InFlightFor did not grow while the drain was wedged (%s → %s); a wedge is not distinguishable from idle",
			first.InFlightFor, second.InFlightFor)
	}
	if second.InFlightFor < 50*time.Millisecond {
		t.Fatalf("InFlightFor = %s after ~60ms wedged; want it to reflect the real in-flight age", second.InFlightFor)
	}
	// The deadline was exceeded but the sink ignored it — proving the timeout
	// alone is not sufficient and the independent sample is what surfaces this.
	if second.InFlightFor <= 20*time.Millisecond {
		t.Fatalf("InFlightFor = %s; expected it to exceed DeliverTimeout for a sink that ignores ctx", second.InFlightFor)
	}

	close(release)
	<-done

	// After release, a healthy pass moves the heartbeat and clears in-flight.
	if _, err := p.PublishOnce(ctx); err != nil {
		t.Fatalf("post-release pass: %v", err)
	}
	s := p.Stats()
	if s.InFlightFor != 0 {
		t.Fatalf("InFlightFor = %s after the sink returned; want 0", s.InFlightFor)
	}
	if s.SinceHeartbeat > time.Second {
		t.Fatalf("SinceHeartbeat = %s after a completed pass; the heartbeat is not advancing", s.SinceHeartbeat)
	}
}

// Monitor reports from its own goroutine and stays silent while healthy.
func TestPublisherMonitorReportsHaltAndStaysQuietWhenHealthy(t *testing.T) {
	ctx := context.Background()

	// healthy → no reports.
	st := seedEvents(t, 2)
	p := store.NewPublisherWithConfig(st, &countingSink{}, store.PublisherConfig{MaxAttempts: 3, BaseBackoff: 0})
	if _, err := p.PublishOnce(ctx); err != nil {
		t.Fatalf("healthy drain: %v", err)
	}
	var mu sync.Mutex
	var reports []store.Stats
	mctx, cancel := context.WithCancel(ctx)
	go p.Monitor(mctx, 5*time.Millisecond, time.Hour, func(s store.Stats) {
		mu.Lock()
		reports = append(reports, s)
		mu.Unlock()
	})
	time.Sleep(60 * time.Millisecond)
	cancel()
	mu.Lock()
	n := len(reports)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("monitor reported %d times on a healthy publisher; want silence", n)
	}

	// halted → reported, from a goroutine that is not the drain.
	st2 := seedEvents(t, 1)
	sink2 := &blockingSink{failWith: errors.New("poison")}
	p2 := store.NewPublisherWithConfig(st2, sink2, store.PublisherConfig{MaxAttempts: 2, BaseBackoff: 0})
	for i := 0; i < 2; i++ {
		_, _ = p2.PublishOnce(ctx)
	}
	reports = nil
	mctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	go p2.Monitor(mctx2, 5*time.Millisecond, time.Hour, func(s store.Stats) {
		mu.Lock()
		reports = append(reports, s)
		mu.Unlock()
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(reports)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) == 0 {
		t.Fatal("monitor never reported a HALTED publisher — the condition that must be noticed was silent")
	}
	if !reports[0].Halted || reports[0].HeadEventID != "e0" {
		t.Fatalf("monitor report = %+v; want Halted on head e0", reports[0])
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

// allUnpublished: see the note in internal/lifecycle — UnpublishedEvents requires a
// positive bound, and these tests want every row they wrote.
const allUnpublished = 1000

// alwaysFailingSink never delivers. countingSink's failNext clears itself after one
// failure, which would let the drain proceed and close the backlog this test needs open.
type alwaysFailingSink struct{}

func (alwaysFailingSink) Deliver(context.Context, store.Event) error {
	return errors.New("sink is down")
}

// TestBacklogGaugeReportsTheTotalNotTheDrainBatch is §4.4-C ③ at the publisher: the
// gauge must not share a source with the loop it observes, because len(the drain batch)
// saturates at store.DrainBatch (store.CountUnpublished).
//
// The sink here FAILS, which is what holds the backlog open: delivery stops at the first
// failure by design, so nothing drains and the rows stay unpublished across the pass.
func TestBacklogGaugeReportsTheTotalNotTheDrainBatch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedPubTmpl(t, st)
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)

	// More rows than one drain pass reads.
	total := store.DrainBatch + 7
	for i := 0; i < total; i++ {
		from, to := model.Paused, model.Running
		kind := model.EventKind("resumed")
		switch {
		case i == 0:
			from, to, kind = model.Creating, model.Running, model.EventKind("created")
		case i%2 == 1:
			from, to, kind = model.Running, model.Paused, model.EventKind("paused")
		}
		if err := st.CompareAndSetState(ctx, "a", from, to, ev("a", fmt.Sprintf("e%d", i), kind, to), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	p := store.NewPublisherWithConfig(st, alwaysFailingSink{}, store.PublisherConfig{MaxAttempts: 100})
	if _, err := p.PublishOnce(ctx); err == nil {
		t.Fatalf("PublishOnce with a failing sink returned nil error — the backlog would not stay open")
	}

	if got := p.Stats().Backlog; got != int64(total) {
		t.Errorf("Backlog = %d with %d rows unpublished and a drain bound of %d — the gauge is reading the drain batch's length, so it saturates at the bound and a large backlog reports as a small constant (§4.4-C ③: observability must not share a source with the loop being observed)", got, total, store.DrainBatch)
	}
}

// TestBacklogGaugeSurvivesASuccessfulPass covers the other arm of §4.4-C ③, which a
// failing sink cannot reach: the pass that DELIVERS everything it read.
//
// The sibling test above returns at the first delivery failure, before the end of the drain
// loop. That leaves the successful path — the one a healthy deployment takes every second —
// unasserted, and it is where a false zero is most harmful: "caught up" is exactly what a
// monitor is looking for.
func TestBacklogGaugeSurvivesASuccessfulPass(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedPubTmpl(t, st)
	_ = st.CreateSandbox(ctx, model.Sandbox{ID: "a", Template: "bare", State: model.Creating}, nil)

	// More rows than one pass can drain, so a completed pass leaves a real remainder.
	total := store.DrainBatch + 7
	for i := 0; i < total; i++ {
		from, to := model.Paused, model.Running
		kind := model.EventKind("resumed")
		switch {
		case i == 0:
			from, to, kind = model.Creating, model.Running, model.EventKind("created")
		case i%2 == 1:
			from, to, kind = model.Running, model.Paused, model.EventKind("paused")
		}
		if err := st.CompareAndSetState(ctx, "a", from, to, ev("a", fmt.Sprintf("e%d", i), kind, to), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	p := store.NewPublisherWithConfig(st, &countingSink{}, store.PublisherConfig{MaxAttempts: 100})
	n, err := p.PublishOnce(ctx)
	if err != nil {
		t.Fatalf("PublishOnce with a succeeding sink: %v", err)
	}
	if n != store.DrainBatch {
		t.Fatalf("delivered %d, want exactly the drain bound %d — the premise of this test is a pass that succeeds AND leaves a remainder", n, store.DrainBatch)
	}

	// The assertion is a LOWER bound, not equality, and the distinction is the point.
	// The gauge is sampled once per pass from its own source, so after a pass it is one
	// interval stale — it reports what was outstanding when the pass began. That is
	// honest gauge behaviour and it errs HIGH, which is the safe direction: a monitor
	// may see a backlog that has just been drained, but it can never be told it is
	// caught up while rows wait. Pinning equality here would instead demand a second
	// count per pass, buying precision the observation does not need.
	//
	// What must never happen is the opposite: a value BELOW what is outstanding, of
	// which 0 is the worst case.
	remaining := int64(total - store.DrainBatch)
	if got := p.Stats().Backlog; got < remaining {
		t.Errorf("Backlog = %d after a fully successful pass that left %d rows unpublished — the gauge under-reports. A completed pass does NOT mean the outbox is empty, because the drain read is bounded at %d, so anything below %d (0 most of all) tells a monitor 'caught up' while a backlog persists and grows: §4.4-C ③'s failure in its most dangerous form", got, remaining, store.DrainBatch, remaining)
	}
}
