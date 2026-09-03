package main

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// activityClock is the sandbox-global "last I/O" timestamp behind the health and reclamation
// connection-activity signal (the activity signal). ONE per transport process,
// shared by every proc's fan-out and the healthServer. It is stamped on every
// output frame (proc.broadcast) and every input frame (proc.writeInput) — the
// two paths ALL process/pty I/O already flows through — and read by
// Health.Check as now-lastIO.
//
// Granularity is sandbox-global, NOT per-pid (the activity signal): the reaper's
// disposition is per-sandbox (auto-pause the whole sandbox), so a single
// "most recent I/O across all procs + ptys" is exactly what it needs — the max
// over every process, expressed as one lock-free atomic write per I/O event
// (O(1), independent of process count; same constant-cost rule as Health not
// listing processes, the Health probe).
//
// TRANSIENT and process-local (the transport data plane / the activity signal): it lives in the
// transport process's memory, is not persisted, does not cross resume, and is
// reborn (reset to "now") if PID1 restarts the transport child — same class as
// the output ring buffer (transport behavior "no persistent SoT"). It answers only "how long
// since this guest last did I/O", never "who connected / for how long".
type activityClock struct {
	lastIOUnixMilli atomic.Int64
	sharedPath      string
	sharedMu        sync.Mutex
	sharedScheduled bool
}

const activityStateFile = "/tmp/.sandboxd-activity"

// newActivityClock seeds lastIO to construction time, so a guest that has done
// NO I/O since boot reports "idle since boot" rather than a bogus huge duration
// from a zero timestamp (the activity signal).
func newActivityClock() *activityClock {
	c := &activityClock{}
	c.lastIOUnixMilli.Store(time.Now().UnixMilli())
	return c
}

// newSharedActivityClock uses the guest tmpfs as the tiny shared-memory seam
// between PID 1 and replaceable transport children.  It keeps Health's
// activity signal continuous when a transport child is restarted while
// retaining the in-process atomic fast path.
func newSharedActivityClock() *activityClock {
	c := newActivityClock()
	c.sharedPath = activityStateFile
	if b, err := os.ReadFile(c.sharedPath); err == nil {
		if v, err := strconv.ParseInt(string(b), 10, 64); err == nil && v > c.lastIOUnixMilli.Load() {
			c.lastIOUnixMilli.Store(v)
		}
	}
	return c
}

// stamp records "I/O happened now". Called from proc.broadcast (output) and
// proc.writeInput (input) — lock-free, off any hot mutex.
func (c *activityClock) stamp() {
	if c == nil {
		return
	}
	now := time.Now().UnixMilli()
	c.lastIOUnixMilli.Store(now)
	if c.sharedPath != "" {
		// Never put a filesystem write on the output/input hot path.  Coalesce
		// updates onto a short timer; the local atomic remains authoritative for
		// this process, while the file bridges a transport-child restart.
		c.sharedMu.Lock()
		if !c.sharedScheduled {
			c.sharedScheduled = true
			time.AfterFunc(5*time.Millisecond, c.flushShared)
		}
		c.sharedMu.Unlock()
	}
}

func (c *activityClock) flushShared() {
	if c == nil || c.sharedPath == "" {
		return
	}
	now := c.lastIOUnixMilli.Load()
	_ = os.WriteFile(c.sharedPath, []byte(strconv.FormatInt(now, 10)), 0o600)
	c.sharedMu.Lock()
	if c.lastIOUnixMilli.Load() != now {
		// A stamp raced the write. Keep the scheduled bit set and arrange one
		// more coalesced flush so a burst cannot leave the shared clock stale.
		time.AfterFunc(5*time.Millisecond, c.flushShared)
	} else {
		c.sharedScheduled = false
	}
	c.sharedMu.Unlock()
}

// idleMillis returns how long since the last stamped I/O (ms), clamped at 0 so
// clock skew never yields a negative idle. This is the value Health.Check
// carries back as CheckResponse.idle_millis (the activity signal).
func (c *activityClock) idleMillis() int64 {
	if c == nil {
		return 0
	}
	last := c.lastIOUnixMilli.Load()
	if c.sharedPath != "" {
		if b, err := os.ReadFile(c.sharedPath); err == nil {
			if v, err := strconv.ParseInt(string(b), 10, 64); err == nil && v > last {
				last = v
			}
		}
	}
	d := time.Now().UnixMilli() - last
	if d < 0 {
		return 0
	}
	return d
}
