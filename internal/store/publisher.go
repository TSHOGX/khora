package store

import (
	"context"
	"sync"
	"time"
)

// Sink is where the publisher delivers outbox events: an in-process, replayable
// stream (§4.4-C). The engine only promises "events are reliably written to a
// replayable stream" — webhook / metrics / billing subscribers consume it OUTSIDE
// the engine and the engine neither tracks nor depends on them (§4.1 / §1.3 add
// a subscriber). Deliver returning an error means "not delivered": the publisher
// leaves the row unpublished and retries, so a flaky sink cannot drop an event.
type Sink interface {
	Deliver(ctx context.Context, ev Event) error
}

// Publisher drains the store outbox to a Sink with at-least-once delivery
// (§4.4-C). The algorithm is ONE piece of code against the Store interface — it
// never learns which backend (Memory / SQLite) it runs on.
//
// The ordering is the whole correctness argument:
//
//	1. read unpublished rows (oldest first)
//	2. Deliver to the sink
//	3. ONLY on success, MarkPublished
//
// A crash between (2) and (3) re-delivers the same event (same transition-unique
// ID) after restart — never drops it. "Redeliver rather than drop" is the
// at-least-once contract; subscribers dedup on the ID (see DedupSink). This is
// why the outbox must be durable (SQLite): after a crash the unpublished rows
// are still there to re-drain.
type Publisher struct {
	store Store
	sink  Sink
}

func NewPublisher(s Store, sink Sink) *Publisher {
	return &Publisher{store: s, sink: sink}
}

// PublishOnce drains all currently-unpublished events once. It returns the
// number delivered+marked, and the first error that stopped the drain (the row
// that failed stays unpublished for the next pass — at-least-once). Safe to call
// repeatedly; that is exactly what recovery after a crash does.
func (p *Publisher) PublishOnce(ctx context.Context) (int, error) {
	evs, err := p.store.UnpublishedEvents(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		// (2) deliver BEFORE (3) mark. A crash here re-delivers the same ID.
		if err := p.sink.Deliver(ctx, ev); err != nil {
			return n, err // leave unpublished; retry next pass
		}
		if err := p.store.MarkPublished(ctx, ev.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Run drives PublishOnce on an interval until ctx is cancelled. This is the
// async publisher loop the mediator wires in (§4.4-C: publish is decoupled from
// the transition commit — it reads the durable outbox after the fact).
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

// DedupSink is the reference subscriber-side idempotent consumer (§4.4-C). It
// wraps a real handler and forwards each event AT MOST ONCE by remembering the
// transition-unique IDs it has already accepted. This is what makes the
// publisher's at-least-once safe end to end: a redelivered event (crash between
// Deliver and MarkPublished) is a no-op here, so a billing subscriber never
// double-counts. It lives in the engine as a REFERENCE — real subscribers live
// outside the engine and dedup however they like (§1.3 add a subscriber).
//
// KNOWN LIMITATION (by design, not a defect — open-decisions §2, the
// §4.1/§4.4-C event-stream row):
// `seen` is in-process, UNBOUNDED, and NON-PERSISTENT. It grows with distinct
// event IDs and is empty after a restart, so it will NOT absorb a redelivery
// that straddles a process restart. That is fine precisely because it is a
// reference: the engine's contract is only "reliably write to a replayable
// stream", and a real subscriber does its own durable dedup outside the engine.
// Do not add persistence here — that would pull subscriber state into the engine
// (§1.1 vocabulary boundary).
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
