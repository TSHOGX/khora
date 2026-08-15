package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Sink is where the publisher delivers outbox events: an in-process, replayable
// stream (the documented invariant). The engine only promises "events are reliably written to a
// replayable stream" — webhook / metrics / billing subscribers consume it OUTSIDE
// the engine and the engine neither tracks nor depends on them (the documented invariant / the documented invariant add
// a subscriber). Deliver returning an error means "not delivered": the publisher
// leaves the row unpublished and retries, so a flaky sink cannot drop an event.
type Sink interface {
	Deliver(ctx context.Context, ev Event) error
}

// Publisher drains the store outbox to a Sink with at-least-once delivery
// (the documented invariant). The algorithm is ONE piece of code against the Store interface — it
// never learns which backend (Memory / SQLite) it runs on.
//
// The ordering is the whole correctness argument:
//
//  1. read unpublished rows (in sequence order, oldest first)
//  2. Deliver to the sink
//  3. ONLY on success, MarkPublished
//
// A crash between (2) and (3) re-delivers the same event (same transition-unique ID) rather than
// dropping it — the at-least-once contract, with subscribers dedupping on the ID (see
// DedupSink). Hence the outbox must be durable (SQLite): after a crash the unpublished rows are
// still there to re-drain.
//
// # Head-of-line blocking is KEPT, deliberately
//
// One undeliverable event blocks every later one: order carries meaning for the billing
// subscribers reconciling from this stream, and skip-ahead is survivable only when the sink
// is idempotent AND versioned by logical time, which ours is not (the documented invariant).
//
// the documented invariant therefore makes three OPERABILITY properties hard requirements — the stall is the
// trade, an unbounded unalerted one is a defect — and the mechanisms all land here:
//
//  1. A POISON-ENVELOPE EXIT (MaxAttempts + Halted), cleared only by an operator (Resume).
//  2. BOUNDED BACKOFF WITH JITTER (PublisherConfig, backoffLocked): the ceiling and the
//     jitter fix different faults.
//  3. OBSERVABILITY THAT SURVIVES THE WORKER WEDGING (Stats / Monitor): sampled from an
//     INDEPENDENT goroutine, because a gauge updated on the drain goroutine freezes at its
//     last healthy value exactly when the drain blocks — a wedged worker then looks idle.
type Publisher struct {
	store Store
	sink  Sink
	cfg   PublisherConfig

	// mu guards the retry state of the CURRENT head. Only the head needs it:
	// nothing behind a blocked head is ever attempted, so there is no per-event
	// attempt table to grow (and no unbounded map — contrast DedupSink's `seen`).
	mu          sync.Mutex
	headID      string    // event ID the attempt count belongs to ("" = none pending)
	attempts    int       // failed attempts against headID
	nextAttempt time.Time // earliest time to retry headID (backoff + jitter)
	lastErr     error
	rnd         *rand.Rand // jitter source; guarded by mu, never used concurrently

	// halted latches the poison-envelope exit. Once set, the drain stops
	// attempting delivery entirely: the stream is STOPPED, not degraded, and
	// saying so plainly is the point. Reset only by Resume (operator action).
	halted atomic.Bool

	// --- observability, written by the drain, read by anyone (see Stats) ------
	// Separate atomics rather than a mutex-guarded struct so a reader can never
	// block behind the drain goroutine — a lock here would reintroduce exactly
	// the failure mode Monitor exists to detect.
	heartbeat    atomic.Int64 // unix nanos: last time the drain loop completed a pass
	deliverStart atomic.Int64 // unix nanos: current in-flight Deliver started (0 = none)
	published    atomic.Int64 // cumulative events delivered+marked
	backlog      atomic.Int64 // TOTAL unpublished rows (counted, not the drain batch's length)
	headSeq      atomic.Int64 // sequence of the blocked head (0 = nothing blocked)
}

// DrainBatch is how many unpublished rows one drain pass reads.
//
// A constant rather than a knob because no deployment has a reason to tune it: delivery is
// strictly in Seq order and stops at the first failure, so a pass can never usefully act on
// more than one batch, and the remainder is still there next tick. What the bound buys is
// on Store.UnpublishedEvents.
//
// It is NOT a cap on the backlog gauge — that comes from Store.CountUnpublished.
const DrainBatch = 256

// PublisherConfig tunes the retry and deadline behavior. The zero value is
// meaningful: NewPublisher fills every field with the default beside it.
type PublisherConfig struct {
	// MaxAttempts bounds retries of one event before the publisher halts (the
	// poison-envelope exit). Must be > 0. Default 10.
	MaxAttempts int
	// BaseBackoff is the first retry delay; it doubles per attempt up to
	// MaxBackoff. Default 1s. Zero is legal and means "retry immediately", which
	// only a test wants.
	BaseBackoff time.Duration
	// MaxBackoff caps the exponential. Default 60s. The cap is not a nicety: an
	// uncapped exponential reaches days after ~20 attempts, at which point the
	// event is neither delivered nor surfaced.
	MaxBackoff time.Duration
	// JitterFraction spreads each delay by ±this fraction so several drainers do
	// not synchronize on the same due time. Default 0.2 (±20%). 0 disables it.
	JitterFraction float64
	// DeliverTimeout bounds ONE Deliver call. Default 30s. It converts a sink that blocks
	// forever from an unbounded wedge of the drain goroutine into a bounded failure the
	// retry and halt logic can act on. It does NOT help against a sink that ignores ctx,
	// which is why Monitor reports the in-flight age too.
	DeliverTimeout time.Duration
}

func (c PublisherConfig) withDefaults() PublisherConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.BaseBackoff < 0 {
		c.BaseBackoff = 0
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 60 * time.Second
	}
	if c.JitterFraction < 0 {
		c.JitterFraction = 0
	}
	if c.DeliverTimeout <= 0 {
		c.DeliverTimeout = 30 * time.Second
	}
	return c
}

// ErrHalted is returned by PublishOnce when the poison-envelope exit has latched:
// one event exhausted MaxAttempts, so the stream is stopped at that event and
// nothing behind it will be delivered until an operator intervenes. It is
// deliberately a distinct error rather than a generic delivery failure — "the
// sink is flaky" and "this stream has stopped" call for different responses.
var ErrHalted = errors.New("store: publisher halted on an undeliverable event")

// NewPublisher builds a publisher with default retry policy. Use
// NewPublisherWithConfig to tune it.
func NewPublisher(s Store, sink Sink) *Publisher {
	return NewPublisherWithConfig(s, sink, PublisherConfig{})
}

func NewPublisherWithConfig(s Store, sink Sink, cfg PublisherConfig) *Publisher {
	p := &Publisher{
		store: s,
		sink:  sink,
		cfg:   cfg.withDefaults(),
		rnd:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.heartbeat.Store(time.Now().UnixNano())
	return p
}

// PublishOnce drains all currently-deliverable unpublished events once. It
// returns the number delivered+marked, and the first error that stopped the
// drain (the row that failed stays unpublished for the next pass —
// at-least-once). Safe to call repeatedly; that is exactly what recovery after a
// crash does.
//
// It stops at the first failure BY DESIGN (see the head-of-line reasoning on
// Publisher), and once an event exhausts MaxAttempts it returns ErrHalted on
// every subsequent call until Resume.
func (p *Publisher) PublishOnce(ctx context.Context) (int, error) {
	defer p.heartbeat.Store(time.Now().UnixNano())

	if p.halted.Load() {
		return 0, fmt.Errorf("%w: %s", ErrHalted, p.haltedEventID())
	}

	evs, err := p.store.UnpublishedEvents(ctx, DrainBatch)
	if err != nil {
		return 0, err
	}
	// The gauge comes from its OWN source, not from len(evs) — the documented invariant ③, argued on
	// Store.CountUnpublished. A count failure must not fail the pass either: the drain is
	// the job and the gauge is observation, so the documented invariant calls a stale gauge the better loss.
	if n, cerr := p.store.CountUnpublished(ctx); cerr == nil {
		p.backlog.Store(n)
	}
	if len(evs) == 0 {
		p.clearRetryState()
		return 0, nil
	}

	n := 0
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		// The head is where all retry state lives. If it is still inside its
		// backoff window, stop the whole pass — attempting anything BEHIND it
		// would be the skip-ahead this design rejects.
		due, attempts := p.headDue(ev.ID)
		if !due {
			p.headSeq.Store(ev.Seq)
			return n, nil
		}

		// (2) deliver BEFORE (3) mark. A crash here re-delivers the same ID.
		// The deadline bounds one attempt so a slow sink cannot wedge the drain
		// indefinitely.
		delCtx, cancel := context.WithTimeout(ctx, p.cfg.DeliverTimeout)
		p.deliverStart.Store(time.Now().UnixNano())
		err := p.sink.Deliver(delCtx, ev)
		p.deliverStart.Store(0)
		cancel()

		if err != nil {
			attempts++
			halted := p.recordFailure(ev, attempts, err)
			if halted {
				// Poison-envelope exit: the stream stops HERE rather than skipping past
				// the event and continuing minus one charge (see head-of-line above).
				return n, fmt.Errorf("%w: event %s (seq %d, %d attempts): %v",
					ErrHalted, ev.ID, ev.Seq, attempts, err)
			}
			return n, fmt.Errorf("deliver event %s (seq %d, attempt %d): %w", ev.ID, ev.Seq, attempts, err)
		}

		if err := p.store.MarkPublished(ctx, ev.ID); err != nil {
			return n, err
		}
		p.clearRetryState()
		p.published.Add(1)
		n++
	}
	// Deliberately NO `p.backlog.Store(0)` here. Reaching the end of the loop means "this
	// batch drained", not "the outbox is empty" — the read is bounded at DrainBatch, so a
	// larger backlog leaves a remainder. Storing 0 is the the documented invariant ③ false zero in its worst
	// form: a monitor reading "caught up" while a backlog grows. The gauge needs no update
	// on this path — the next pass re-reads it from its own source above.
	return n, nil
}

// headDue reports whether the given head event may be attempted now, and how
// many failed attempts it has already accumulated. A DIFFERENT event ID at the
// head means the previous head was delivered (or the outbox moved on), so the
// attempt count resets — the bound is per event, not a global counter.
func (p *Publisher) headDue(id string) (due bool, attempts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.headID != id {
		p.headID = id
		p.attempts = 0
		p.nextAttempt = time.Time{}
		p.lastErr = nil
		return true, 0
	}
	if !p.nextAttempt.IsZero() && time.Now().Before(p.nextAttempt) {
		return false, p.attempts
	}
	return true, p.attempts
}

// recordFailure books an attempt against the head and schedules the next one.
// Returns true if the attempt bound is exhausted (the publisher is now halted).
func (p *Publisher) recordFailure(ev Event, attempts int, cause error) bool {
	p.mu.Lock()
	p.headID = ev.ID
	p.attempts = attempts
	p.lastErr = cause
	p.mu.Unlock()
	p.headSeq.Store(ev.Seq)

	if attempts >= p.cfg.MaxAttempts {
		p.halted.Store(true)
		return true
	}
	p.mu.Lock()
	p.nextAttempt = time.Now().Add(p.backoffLocked(attempts))
	p.mu.Unlock()
	return false
}

// backoffLocked computes Base * 2^(attempt-1), capped at MaxBackoff, then spread by
// ±JitterFraction (each fixes a different fault — see PublisherConfig).
// Caller holds p.mu (p.rnd is not concurrency-safe).
func (p *Publisher) backoffLocked(attempt int) time.Duration {
	if p.cfg.BaseBackoff <= 0 {
		return 0
	}
	d := p.cfg.BaseBackoff
	for i := 1; i < attempt && d < p.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > p.cfg.MaxBackoff {
		d = p.cfg.MaxBackoff
	}
	if p.cfg.JitterFraction > 0 {
		spread := float64(d) * p.cfg.JitterFraction
		d = time.Duration(float64(d) + (p.rnd.Float64()*2-1)*spread)
		if d < 0 {
			d = 0
		}
	}
	return d
}

func (p *Publisher) clearRetryState() {
	p.mu.Lock()
	p.headID = ""
	p.attempts = 0
	p.nextAttempt = time.Time{}
	p.lastErr = nil
	p.mu.Unlock()
	p.headSeq.Store(0)
}

func (p *Publisher) haltedEventID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.headID
}

// Resume clears the poison-envelope halt so the drain retries the stuck event
// from a fresh attempt budget. This is the OPERATOR action the halt demands;
// there is no automatic un-halt, because an automatic one would turn the bound
// back into the unbounded retry loop it exists to replace.
func (p *Publisher) Resume() {
	p.halted.Store(false)
	p.clearRetryState()
}

// Stats is a point-in-time snapshot of publisher health (the documented invariant operability ③).
// Everything here is read from atomics, so a caller sampling it NEVER blocks
// behind the drain goroutine.
type Stats struct {
	// Halted is the poison-envelope exit: one event exhausted its attempt budget
	// and the stream is stopped at it. This is the condition that must be noticed.
	Halted bool
	// HeadEventID / HeadSeq identify the blocked event ("" / 0 = nothing blocked).
	HeadEventID string
	HeadSeq     int64
	// Attempts is how many delivery attempts the blocked head has failed.
	Attempts int
	// LastError is the most recent delivery failure ("" = none).
	LastError string
	// Backlog is the number of unpublished rows seen on the last completed pass.
	Backlog int64
	// Published is the cumulative count delivered+marked since process start.
	Published int64
	// SinceHeartbeat is how long ago the drain loop last COMPLETED a pass. This
	// is the wedge detector: it grows without bound if the drain goroutine is
	// stuck, whereas an idle-but-healthy publisher keeps it near the tick
	// interval.
	SinceHeartbeat time.Duration
	// InFlightFor is how long the CURRENT Deliver call has been running (0 = no
	// call in flight). It separates the two failure modes that a single staleness
	// number cannot: a sink blocked inside Deliver (InFlightFor grows) versus a
	// drain loop wedged elsewhere (only SinceHeartbeat grows).
	InFlightFor time.Duration
}

// Stats samples publisher health. Safe to call from any goroutine at any time,
// including while a delivery is wedged.
func (p *Publisher) Stats() Stats {
	p.mu.Lock()
	head, attempts, lastErr := p.headID, p.attempts, p.lastErr
	p.mu.Unlock()

	s := Stats{
		Halted:      p.halted.Load(),
		HeadEventID: head,
		HeadSeq:     p.headSeq.Load(),
		Attempts:    attempts,
		Backlog:     p.backlog.Load(),
		Published:   p.published.Load(),
	}
	if lastErr != nil {
		s.LastError = lastErr.Error()
	}
	now := time.Now()
	if hb := p.heartbeat.Load(); hb > 0 {
		s.SinceHeartbeat = now.Sub(time.Unix(0, hb))
	}
	if ds := p.deliverStart.Load(); ds > 0 {
		s.InFlightFor = now.Sub(time.Unix(0, ds))
	}
	return s
}

// Run drives PublishOnce on an interval until ctx is cancelled. This is the
// async publisher loop the mediator wires in (the documented invariant: publish is decoupled from
// the transition commit — it reads the durable outbox after the fact).
//
// It does NOT sample its own health; that is Monitor's job, on its own
// goroutine, for the reason stated on Publisher.
func (p *Publisher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = p.PublishOnce(ctx)
		}
	}
}

// Monitor samples Stats on its OWN goroutine — never the drain's — and calls report for
// every sample that describes a problem. That placement IS the mechanism (the documented invariant ③, and
// the reasoning is on Publisher).
//
// stallThreshold is how stale a heartbeat (or how long an in-flight delivery)
// must be before it is reported. Pick it well above interval; a few multiples of
// the drain's tick is right.
//
// Nothing is reported while healthy — report fires on: halted, an attempt count
// above zero, a stale heartbeat, or a long in-flight delivery.
func (p *Publisher) Monitor(ctx context.Context, interval, stallThreshold time.Duration, report func(Stats)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := p.Stats()
			if s.Halted || s.Attempts > 0 ||
				s.SinceHeartbeat > stallThreshold || s.InFlightFor > stallThreshold {
				report(s)
			}
		}
	}
}

// DedupSink is the reference subscriber-side idempotent consumer (the documented invariant). It
// wraps a real handler and forwards each event AT MOST ONCE by remembering the
// transition-unique IDs it has already accepted. This is what makes the
// publisher's at-least-once safe end to end: a redelivered event (crash between
// Deliver and MarkPublished) is a no-op here, so a billing subscriber never
// double-counts. It lives in the engine as a REFERENCE — real subscribers live
// outside the engine and dedup however they like (the documented invariant add a subscriber).
//
// KNOWN LIMITATION, by design: `seen` is in-process, UNBOUNDED, and NON-PERSISTENT, so it
// will NOT absorb a redelivery that straddles a process restart. Acceptable only because
// this is a reference. DO NOT add persistence here — that pulls subscriber state into the
// engine (the documented invariant vocabulary boundary).
type DedupSink struct {
	mu      sync.Mutex
	seen    map[string]bool
	handler func(Event)
}

// NewDedupSink wraps handler; handler is invoked exactly once per distinct event
// ID, no matter how many times the publisher delivers it.
func NewDedupSink(handler func(Event)) *DedupSink {
	return &DedupSink{seen: make(map[string]bool), handler: handler}
}

func (d *DedupSink) Deliver(_ context.Context, ev Event) error {
	d.mu.Lock()
	first := !d.seen[ev.ID]
	if first {
		d.seen[ev.ID] = true
	}
	d.mu.Unlock()
	if first {
		d.handler(ev) // dedup: the handler sees each transition exactly once
	}
	return nil
}
