package main

import (
	"testing"
	"time"
)

// The activity clock is the guest side of the the documented invariant signal (reaper-health the documented invariant):
// stamped on I/O, read as idle duration. These pin its mechanics deterministically
// without the runsc round-trip (that lands in the substrate e2e).

// A fresh clock reports "idle since construction", not a bogus huge duration
// from a zero timestamp (reaper-health the documented invariant: no-I/O-since-boot = idle since boot).
func TestActivityClockSeededToNow(t *testing.T) {
	c := newActivityClock()
	if idle := c.idleMillis(); idle < 0 || idle > 1000 {
		t.Fatalf("fresh clock idle=%dms; want ~0 (seeded to now)", idle)
	}
}

// stamp resets idle toward zero: an output/input event marks the sandbox active.
func TestActivityClockStampResetsIdle(t *testing.T) {
	c := newActivityClock()
	c.lastIOUnixMilli.Store(time.Now().Add(-5 * time.Second).UnixMilli())
	if idle := c.idleMillis(); idle < 4000 {
		t.Fatalf("pre-stamp idle=%dms; want ≥4000 (simulated stale I/O)", idle)
	}
	c.stamp()
	if idle := c.idleMillis(); idle > 1000 {
		t.Fatalf("post-stamp idle=%dms; want ~0 (stamp marks activity now)", idle)
	}
}

// proc.broadcast of a DATA frame stamps activity — this is the detached-process
// path (the documented invariant): output flows through broadcast with no client attached, and it
// must still count as active (reaper-health the documented invariant, the whole reason for guest-side).
func TestBroadcastStampsActivity(t *testing.T) {
	c := newActivityClock()
	c.lastIOUnixMilli.Store(time.Now().Add(-5 * time.Second).UnixMilli())
	p := newProc(1, nil)
	p.activity = c

	p.broadcast(frame{kind: 'd', stream: "stdout", data: []byte("output")})
	if idle := c.idleMillis(); idle > 1000 {
		t.Fatalf("broadcast did not stamp activity: idle=%dms (detached output must count, the documented invariant)", idle)
	}
}

// An END frame ('e') is process exit, not I/O — it must NOT stamp activity (a
// process that ended long ago should not read as freshly active).
func TestBroadcastEndFrameDoesNotStamp(t *testing.T) {
	c := newActivityClock()
	c.lastIOUnixMilli.Store(time.Now().Add(-5 * time.Second).UnixMilli())
	p := newProc(1, nil)
	p.activity = c

	p.broadcast(frame{kind: 'e', exitCode: 0})
	if idle := c.idleMillis(); idle < 4000 {
		t.Fatalf("end frame stamped activity: idle=%dms; want ≥4000 (exit is not I/O)", idle)
	}
}

// writeInput stamps activity on INPUT: a keystroke is I/O even with no output.
func TestWriteInputStampsActivity(t *testing.T) {
	c := newActivityClock()
	c.lastIOUnixMilli.Store(time.Now().Add(-5 * time.Second).UnixMilli())
	bw := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	close(bw.release) // let the write return immediately
	p := newProc(1, bw)
	p.activity = c

	if err := p.writeInput([]byte("x"), false); err != nil {
		t.Fatalf("writeInput: %v", err)
	}
	if idle := c.idleMillis(); idle > 1000 {
		t.Fatalf("writeInput did not stamp activity: idle=%dms (input is I/O)", idle)
	}
}

// nil-safe: a proc built without a clock (unit tests, pre-register) does not
// panic on broadcast/writeInput. The assertion is the absence of a panic, so it
// is made explicit — otherwise this reads like a test that checks nothing.
func TestActivityNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil activity clock panicked: %v — every stamp site must nil-check, "+
				"since a proc exists before register() attaches the clock", r)
		}
	}()
	p := newProc(1, nil) // no activity set
	p.broadcast(frame{kind: 'd', data: []byte("x")})
	// writeInput with nil stdin errors, but must not panic on the nil clock.
	_ = p.writeInput([]byte("y"), false)
}
