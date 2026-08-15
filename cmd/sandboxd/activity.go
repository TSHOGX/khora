package main

import (
	"sync/atomic"
	"time"
)

// activityClock is the sandbox-global "last I/O" timestamp behind the the documented invariant
// connection-activity signal (reaper-health the documented invariant). ONE per transport process,
// shared by every proc's fan-out and the healthServer. It is stamped on every
// output frame (proc.broadcast) and every input frame (proc.writeInput) — the
// two paths ALL process/pty I/O already flows through — and read by
// Health.Check as now-lastIO.
//
// Granularity is sandbox-global, NOT per-pid (reaper-health the documented invariant): the reaper's
// disposition is per-sandbox (auto-pause the whole sandbox), so a single
// "most recent I/O across all procs + ptys" is exactly what it needs — the max
// over every process, expressed as one lock-free atomic write per I/O event
// (O(1), independent of process count; same constant-cost rule as Health not
// listing processes, transport the documented invariant).
//
// TRANSIENT and process-local (the documented invariant / reaper-health the documented invariant): it lives in the
// transport process's memory, is not persisted, does not cross resume, and is
// reborn (reset to "now") if PID1 restarts the transport child — same class as
// the output ring buffer (the documented invariant "no persistent SoT"). It answers only "how long
// since this guest last did I/O", never "who connected / for how long".
type activityClock struct {
	lastIOUnixMilli atomic.Int64
}

// newActivityClock seeds lastIO to construction time, so a guest that has done
// NO I/O since boot reports "idle since boot" rather than a bogus huge duration
// from a zero timestamp (reaper-health the documented invariant).
func newActivityClock() *activityClock {
	c := &activityClock{}
	c.lastIOUnixMilli.Store(time.Now().UnixMilli())
	return c
}

// stamp records "I/O happened now". Called from proc.broadcast (output) and
// proc.writeInput (input) — lock-free, off any hot mutex.
func (c *activityClock) stamp() {
	if c == nil {
		return
	}
	c.lastIOUnixMilli.Store(time.Now().UnixMilli())
}

// idleMillis returns how long since the last stamped I/O (ms), clamped at 0 so
// clock skew never yields a negative idle. This is the value Health.Check
// carries back as CheckResponse.idle_millis (reaper-health the documented invariant).
func (c *activityClock) idleMillis() int64 {
	if c == nil {
		return 0
	}
	d := time.Now().UnixMilli() - c.lastIOUnixMilli.Load()
	if d < 0 {
		return 0
	}
	return d
}
