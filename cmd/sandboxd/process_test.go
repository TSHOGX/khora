package main

import (
	"testing"
)

// L1 (no substrate): the D4.3 fan-out invariants as pure logic. The real
// runsc round-trip (C2/C3/C4) is in test/transport_substrate_test.go; these
// pin the hub mechanics deterministically and under -race.

func drain(ch <-chan frame) []frame {
	var out []frame
	for f := range ch {
		out = append(out, f)
	}
	return out
}

func TestRingBufferDropsOldestBytes(t *testing.T) {
	r := ringBuffer{cap: 10}
	r.append(frame{kind: 'd', data: []byte("aaaa")}) // 4
	r.append(frame{kind: 'd', data: []byte("bbbb")}) // 8
	r.append(frame{kind: 'd', data: []byte("cccc")}) // 12 → drop "aaaa" → 8
	snap := r.snapshot()
	if len(snap) != 2 || string(snap[0].data) != "bbbb" || string(snap[1].data) != "cccc" {
		t.Fatalf("ring = %v; want [bbbb cccc]", snap)
	}
	if r.bytes != 8 {
		t.Fatalf("ring bytes = %d; want 8", r.bytes)
	}
}

// invariant 2: a subscriber that attaches after output was produced replays the
// ring first, then joins live.
func TestConnectReplaysRingThenLive(t *testing.T) {
	p := newProc(1, nil)
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("past1")})
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("past2")})

	sub, snap, ended, _ := p.subscribe()
	if ended || sub == nil {
		t.Fatal("subscribe returned ended for a live proc")
	}
	if len(snap) != 2 || string(snap[0].data) != "past1" || string(snap[1].data) != "past2" {
		t.Fatalf("replay snap = %v; want [past1 past2]", snap)
	}
	// now live
	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("live1")})
	p.finish(0)
	got := drain(sub.ch)
	if len(got) != 2 || string(got[0].data) != "live1" || got[1].kind != 'e' {
		t.Fatalf("live frames = %v; want [live1 end]", got)
	}
}

// invariant 2 (multi-connection, §3.2): two subscribers each get an independent
// full copy; dropping one does not disturb the other.
func TestFanOutIndependentCopies(t *testing.T) {
	p := newProc(1, nil)
	a, _, _, _ := p.subscribe()
	b, _, _, _ := p.subscribe()
	p.broadcast(frame{kind: 'd', data: []byte("x")})
	p.unsubscribe(a) // one leaves
	p.broadcast(frame{kind: 'd', data: []byte("y")})
	p.finish(0)

	ga := drain(a.ch)
	gb := drain(b.ch)
	if len(ga) != 1 || string(ga[0].data) != "x" {
		t.Fatalf("a got %v; want [x]", ga)
	}
	if len(gb) != 3 || string(gb[0].data) != "x" || string(gb[1].data) != "y" || gb[2].kind != 'e' {
		t.Fatalf("b got %v; want [x y end]", gb)
	}
}

// invariant 1: a subscriber that never drains its window is dropped on overflow;
// the source (broadcast) NEVER blocks, and a subscriber that keeps up is
// unaffected. The fast reader drains in lockstep with each broadcast so the
// assertion is deterministic (a concurrently-scheduled reader can be transiently
// outpaced — that is not an invariant violation, only source-blocking is).
func TestSlowSubscriberDroppedSourceNeverBlocks(t *testing.T) {
	p := newProc(1, nil)
	slow, _, _, _ := p.subscribe() // never drained
	fast, _, _, _ := p.subscribe() // drained after each broadcast

	total := subWindowFrames * 4 // far past the window
	var fastFrames []frame
	for i := 0; i < total; i++ {
		// If broadcast blocked on the stuck subscriber, this loop never
		// completes and -timeout kills the test — the failure signal for
		// "source blocked".
		p.broadcast(frame{kind: 'd', data: []byte("z")})
		for {
			select {
			case f := <-fast.ch:
				fastFrames = append(fastFrames, f)
				continue
			default:
			}
			break
		}
	}
	p.finish(0)
	fastFrames = append(fastFrames, drain(fast.ch)...)

	if !slow.overflow.Load() {
		t.Fatal("slow subscriber was not marked overflow/dropped")
	}
	_ = drain(slow.ch) // closed by the drop; must terminate
	if n := len(fastFrames); n != total+1 {
		t.Fatalf("fast subscriber got %d frames; want %d (all data + end)", n, total+1)
	}
	if fastFrames[len(fastFrames)-1].kind != 'e' {
		t.Fatal("fast subscriber missing terminal end frame")
	}
}

// invariant 1 boundary: a subscriber whose window is exactly full at process
// exit cannot receive the end frame. It must be marked overflow (dropped), so
// serveSubscriber reports ResourceExhausted — NOT a silent clean-end that would
// hide both the drop and the exit code (process-result semantics).
func TestFinishOnFullWindowMarksOverflow(t *testing.T) {
	p := newProc(1, nil)
	sub, _, _, _ := p.subscribe()
	// Fill the window to the brim without draining, staying just under the
	// broadcast-drop threshold (broadcast drops on the send that can't fit).
	for i := 0; i < subWindowFrames; i++ {
		p.broadcast(frame{kind: 'd', data: []byte("z")})
	}
	if sub.overflow.Load() {
		t.Fatal("subscriber dropped before finish; window fill overshot the cap")
	}
	// Exit now: the end frame has nowhere to go → must mark overflow, not vanish.
	p.finish(0)
	if !sub.overflow.Load() {
		t.Fatal("full-window subscriber not marked overflow at finish; end frame silently dropped")
	}
	_ = drain(sub.ch) // closed by finish; must terminate
}

// a subscriber attaching after the process ended replays the ring and gets end.
func TestSubscribeAfterEnded(t *testing.T) {
	p := newProc(1, nil)
	p.broadcast(frame{kind: 'd', data: []byte("tail")})
	p.finish(7)
	sub, snap, ended, endF := p.subscribe()
	if !ended || sub != nil {
		t.Fatal("subscribe after finish should report ended with nil sub")
	}
	if len(snap) != 1 || string(snap[0].data) != "tail" {
		t.Fatalf("post-end replay = %v; want [tail]", snap)
	}
	if endF.kind != 'e' || endF.exitCode != 7 {
		t.Fatalf("end frame = %v; want end exit=7", endF)
	}
}
