//go:build substrate

// L3/L4 the documented invariant reaper acceptance against REAL runsc. Drives the assembled engine
// + reaper through each health state and TTL disposition on live containers:
// false-idle guard (the documented invariant), multi-connection idle (the documented invariant), idle→auto-pause,
// live killed-container→crash-pause, transport-down same-IP restart,
// transport crash-loop escalation (the documented invariantb), paused→evict, and the reaper-vs-verb
// lock seam. Each assertion states its property inline. Requires root.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 600s ./test/ -run Reaper
package test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/orchestrator"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/waitfor"
	"github.com/TSHOGX/khora/test/harness"
)

func dur(d time.Duration) *time.Duration { return &d }

// tickerSrc is a tiny static guest binary that prints a line every 100ms for N
// seconds, then exits. The bare rootfs is dash + libc only (no coreutils, no
// `sleep`), so a shell producer loop cannot pace itself — this injects real
// paced output to drive the activity signal deterministically (the documented invariant / the documented invariant).
const tickerSrc = `package main
import ("fmt";"os";"strconv";"time")
func main(){
  n,_ := strconv.Atoi(os.Args[1])
  end := time.Now().Add(time.Duration(n)*time.Second)
  for time.Now().Before(end){ fmt.Println("tick"); time.Sleep(100*time.Millisecond) }
}`

// lastKind returns the most recent outbox event Kind for a sandbox (the documented invariant:
// transitions are published to the outbox; a test reads it to assert the
// disposition Kind — auto-paused / crash-paused / destroyed).
func lastKind(t *testing.T, eng *orchestrator.Engine, sid model.SandboxID) string {
	t.Helper()
	evs, err := eng.Store.UnpublishedEvents(context.Background(), 100 /* a bound is required; well above what this test emits */)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	kind := ""
	for _, ev := range evs {
		if ev.Sandbox == sid {
			kind = string(ev.Kind)
		}
	}
	return kind
}

// killTransportChild kills the transport child from inside the guest (dash kill
// builtin), targeting the pid published to the tmpfs pidfile. PID1 restarts it.
func killTransportChild(t *testing.T, cl transport.Client) {
	t.Helper()
	ctx := context.Background()
	pidBytes, err := cl.Read(ctx, "/tmp/.sandboxd-transport.pid", 0, 0)
	if err != nil {
		t.Fatalf("read transport pidfile: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("empty transport pidfile")
	}
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/bin/sh", Args: []string{"-c", "kill -KILL " + pid}})
	if err != nil {
		t.Fatalf("start kill: %v", err)
	}
	for range ch {
	}
}

// idle sandbox (no I/O) → auto-paused after idle_timeout (the documented invariant idle state).
func TestReaperIdleAutoPauseRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(2 * time.Second), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	// Poll rather than sleep-once: the disposition needs BOTH the guest's last-I/O
	// stamp aged past idle_timeout AND an L2 probe that succeeds, because idle is
	// read only on the L2-up branch (the the documented invariant unreadable-idle guard). A probe that
	// times out under load classifies transport-down, not idle, so a single fixed
	// sleep can assert against a sandbox the classifier never called idle.
	//
	// Run this package with `-p 1` when running the whole substrate suite: other
	// packages' substrate tests launch their own runsc containers concurrently, and
	// that contention starves the probe for longer than any threshold set here.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	var stt model.Lifecycle
	// Until rather than Must: the failure has to report the state actually observed,
	// which a generic timeout message would bury.
	ok := waitfor.Until(30*time.Second, 500*time.Millisecond, func() bool {
		r.Sweep(ctx)
		stt, _ = m.State(ctx, sid)
		return stt == model.Paused
	})
	if !ok {
		t.Fatalf("idle sandbox state=%s; want paused (auto-pause) within 30s of a 2s idle_timeout", stt)
	}
	if k := lastKind(t, eng, sid); k != "auto-paused" {
		t.Fatalf("idle disposition Kind=%q; want auto-paused", k)
	}
	t.Logf("✓ idle (no I/O) → auto-paused after idle_timeout")
}

// the documented invariant false-idle guard: a DETACHED process still producing output with NO client
// draining it is NOT auto-paused. The guest stamps activity on broadcast even
// with no subscriber (the documented invariant), so idle_millis stays low; host-side stamping would
// false-idle a working sandbox (reaper-health the documented invariant).
func TestReaperFalseIdleGuardRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator
	harness.InjectStaticBin(t, filepath.Join(base, "templates"), "bare", "ticker", tickerSrc)

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(2 * time.Second), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// A process that keeps producing output for ~8s. We consume the Start event,
	// then STOP reading — the process keeps pumping into its ring guest-side with
	// no client draining, which is the exact the documented invariant detached case.
	ch, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/ticker", Args: []string{"8"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-ch // read only the start event; deliberately stop draining thereafter.

	// sweep across a window LONGER than idle_timeout — the producing process must
	// keep the sandbox non-idle throughout.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		r.Sweep(ctx)
		if stt, _ := m.State(ctx, sid); stt != model.Running {
			t.Fatalf("detached producing process was auto-paused (the documented invariant false-idle): state=%s", stt)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("✓ detached process still producing output → NOT auto-paused (the documented invariant guest-side activity)")
}

// multi-connection idle (the documented invariant): activity is sandbox-global = max over all procs
// + ptys. With one process idle and one still producing I/O, the sandbox is NOT
// idle; it becomes idle only once ALL go quiet past idle_timeout.
func TestReaperMultiConnectionIdleRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator
	harness.InjectStaticBin(t, filepath.Join(base, "templates"), "bare", "ticker", tickerSrc)

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(2 * time.Second), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// conn A: short-lived, goes quiet immediately. conn B: keeps producing ~8s.
	if _, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/ticker", Args: []string{"1"}}); err != nil {
		t.Fatalf("start A: %v", err)
	}
	chB, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/ticker", Args: []string{"8"}})
	if err != nil {
		t.Fatalf("start B: %v", err)
	}
	<-chB

	// while B produces, sweeps must NOT auto-pause (A idle but B active → sandbox
	// active, activity = max).
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.Sweep(ctx)
		if stt, _ := m.State(ctx, sid); stt != model.Running {
			t.Fatalf("sandbox auto-paused while one connection still active (the documented invariant max): state=%s", stt)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("✓ one connection active keeps the sandbox non-idle (the documented invariant activity = max over all)")

	// drain B to completion so ALL connections go quiet, then it must auto-pause.
	go func() {
		for range chB {
		}
	}()
	// wait for B to finish (~8s of output) + idle_timeout, sweeping.
	paused := waitfor.Until(20*time.Second, 500*time.Millisecond, func() bool {
		r.Sweep(ctx)
		stt, _ := m.State(ctx, sid)
		return stt == model.Paused
	})
	if !paused {
		t.Fatalf("sandbox did not auto-pause after ALL connections went quiet (the documented invariant)")
	}
	t.Logf("✓ once ALL connections idle past idle_timeout → auto-paused")
}

// killContainer kills+deletes the runsc container out from under the engine
// (simulates substrate death: L1 goes dead while the engine still holds the
// live handle). runscRoot is StateDir/runsc-root; the cid is sbxc_<sid>.
func killContainer(t *testing.T, base string, sid model.SandboxID) {
	t.Helper()
	root := filepath.Join(base, "state", "runsc-root")
	cid := "sbxc_" + string(sid)
	for _, args := range [][]string{
		{"runsc", "--root=" + root, "kill", cid, "KILL"},
		{"runsc", "--root=" + root, "delete", "--force", cid},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("killContainer %v: %v: %s (continuing)", args, err, out)
		}
	}
}

// a killed `running` container → crash-paused by the LIVE reaper (the documented invariant
// involuntary pause). Proves the reaper drives the demote, not only the
// boot-time reconcile path. L1 dead + L2 down → dead-recoverable → crash-pause.
func TestReaperLiveCrashPauseRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// long idle_timeout so this is NOT an idle auto-pause — it must be crash-pause.
	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(time.Hour), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	// kill the container: L1 dead. The engine still thinks it is running (holds
	// the live handle) until the reaper classifies it.
	killContainer(t, base, sid)

	// Sweep until the classification lands, rather than sleeping a guessed 500ms and
	// sweeping once. `runsc kill` returns before the sandbox is fully gone, so the
	// single-sweep version was betting that 500ms is always enough — a bet that fails as
	// a "the engine did not crash-pause" verdict, which is indistinguishable from the
	// defect this test hunts. Sweep is idempotent, so repeating it is free; the
	// assertion below still requires paused, only now it is not racing the substrate.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	var stt model.Lifecycle
	ok := waitfor.Until(20*time.Second, 200*time.Millisecond, func() bool {
		r.Sweep(ctx)
		stt, _ = m.State(ctx, sid)
		return stt == model.Paused
	})
	if !ok {
		t.Fatalf("killed running state=%s; want paused (live crash-pause)", stt)
	}
	if k := lastKind(t, eng, sid); k != "crash-paused" {
		t.Fatalf("dead-recoverable Kind=%q; want crash-paused (the documented invariant involuntary)", k)
	}
	t.Logf("✓ killed running container → crash-paused by the LIVE reaper")
}

// transport child killed → classified transport-down; PID1 restarts it on the
// SAME IP; NO crash-pause, no cold-start, no new epoch (the documented invariant). The reaper
// WAITS within budget.
func TestReaperTransportDownSameIPRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(time.Hour), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	epBefore, _ := m.Endpoint(sid)
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// kill the transport child (NOT the container). PID1 restarts it in place.
	killTransportChild(t, cl)

	// with a GENEROUS budget, the reaper must WAIT, not crash-pause. Sweep during
	// the restart window.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	for i := 0; i < 6; i++ {
		r.Sweep(ctx)
		if stt, _ := m.State(ctx, sid); stt != model.Running {
			t.Fatalf("transport-down sandbox left running-state=%s during in-place restart; want running (no crash-pause)", stt)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// endpoint unchanged (same IP — not a resume).
	epAfter, err := m.Endpoint(sid)
	if err != nil || epAfter != epBefore {
		t.Fatalf("endpoint changed %s → %s (err=%v); want SAME IP (the documented invariant, not a resume)", epBefore, epAfter, err)
	}
	// and it serves again after the in-place restart.
	if _, err := m.Client(sid); err != nil {
		t.Fatalf("client after restart: %v", err)
	}
	t.Logf("✓ transport child killed → transport-down, PID1 restarts SAME IP %s, no crash-pause", epAfter)
}

// the documented invariantb crash-loop escalation: a transport child that NEVER comes healthy
// (crash-loop, via the /tmp sentinel) past the restart budget is escalated to
// dead-recoverable → resume (new IP, new epoch) — not waited on forever.
func TestReaperTransportCrashLoopEscalationRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 20*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(time.Hour), Retention: dur(0)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })
	epBefore, _ := m.Endpoint(sid)
	cl, err := m.Client(sid)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// arm the crash-loop: write the sentinel to tmpfs /tmp, then kill the child.
	// PID1 re-execs it, it sees the sentinel and exits → crash-loops. L1 (the
	// container) stays alive, L2 never comes up = transport-down forever.
	if err := cl.Write(ctx, "/tmp/.sandboxd-crashloop", []byte("x")); err != nil {
		t.Fatalf("arm sentinel: %v", err)
	}
	killTransportChild(t, cl)

	// tight budget so escalation triggers quickly. Sweep on a real ticker cadence
	// until the sandbox comes back running at a NEW IP (escalation resumed it).
	r := lifecycle.NewReaper(m, time.Hour, 2*time.Second)
	var epAfter string
	escalated := waitfor.Until(20*time.Second, 500*time.Millisecond, func() bool {
		r.Sweep(ctx)
		if stt, _ := m.State(ctx, sid); stt != model.Running {
			return false
		}
		ep, err := m.Endpoint(sid)
		if err != nil || ep == epBefore {
			return false
		}
		epAfter = ep
		return true
	})
	if !escalated {
		stt, _ := m.State(ctx, sid)
		t.Fatalf("crash-loop transport not escalated to resume within window (state=%s); want running at NEW IP (the documented invariantb)", stt)
	}
	if epAfter == epBefore {
		t.Fatalf("escalation reused IP %s; want NEW IP/epoch (the documented invariant / the documented invariantb cold-start)", epBefore)
	}
	// escalation went through crash-pause then resume.
	evs, _ := eng.Store.UnpublishedEvents(ctx, 100 /* a bound is required; well above what this test emits */)
	sawCrashPause, sawResume := false, false
	for _, ev := range evs {
		if ev.Sandbox != sid {
			continue
		}
		switch ev.Kind {
		case "crash-paused":
			sawCrashPause = true
		case "resumed":
			sawResume = true
		}
	}
	if !sawCrashPause || !sawResume {
		t.Fatalf("escalation did not emit crash-paused+resumed (the documented invariantb)")
	}
	t.Logf("✓ crash-loop transport escalated past budget → resume at NEW IP %s → %s (the documented invariantb)", epBefore, epAfter)
}

// paused sandbox past retention → evict → destroyed (the documented invariant evict = reaper's
// automatic destroy of a paused sandbox).
func TestReaperEvictPastRetentionRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, base := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// short retention so the paused sandbox crosses it quickly.
	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(time.Hour), Retention: dur(2 * time.Second)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	up := upperDir(t, eng, base, sid)
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// The 2s retention has to genuinely elapse — this wait is the test's subject, not a
	// guess about substrate latency, so it stays a wait. What changes is that the sweep
	// polls instead of firing once at a hardcoded 3s: the eviction becomes observable the
	// moment retention passes, and a slow box no longer reports "still present" for a
	// record that was merely a few ms short.
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	evicted := waitfor.Until(20*time.Second, 200*time.Millisecond, func() bool {
		r.Sweep(ctx)
		_, err := m.State(ctx, sid)
		return err != nil
	})
	if !evicted {
		t.Fatalf("paused-past-retention record still present; want destroyed (evict)")
	}
	if k := lastKind(t, eng, sid); k != "destroyed" {
		t.Fatalf("evict Kind=%q; want destroyed (the documented invariant)", k)
	}
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatalf("upper survived evict (want reclaimed via synchronous Destroy): %v", err)
	}
	t.Logf("✓ paused past retention → evict → destroyed, upper reclaimed")
}

// reaper-vs-verb seam: a sandbox whose transition lock is HELD by an in-flight
// verb must be SKIPPED by a concurrent sweep (TryLock-skip, the documented invariant).
//
// The scenario is chosen so the skip is actually REACHABLE, which took some care.
// An in-flight resume leaves the record `paused` for its whole cold start, so a
// concurrent sweep dispatches to the PAUSED arm, not the running one — and that
// arm returns before it ever reaches TryLock unless the sandbox is also past its
// retention. So the reachable race is: a paused sandbox that IS evict-eligible,
// resumed at the same moment.
//
// That race is the severe one. Without the skip the reaper would evict — destroy
// the record and DELETE the Overlay bytes — out from under a resume that is
// mounting exactly those bytes. refcount cannot save it, because an in-flight
// mount is not a persistent reference row (the documented invariant states this precisely).
//
// A retention of 1ns makes the sandbox evict-eligible the instant it pauses, so
// every sweep in the storm below is a real eviction attempt.
func TestReaperSkipsVerbHeldLockRealRunsc(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	// idle_timeout long (this is not an idle test); retention 1ns so the sandbox is
	// evict-eligible the moment it is paused — that is what carries a sweep past
	// sweepPaused's retention gate and into the TryLock the test is about.
	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare", IdleTimeout: dur(time.Hour), Retention: dur(time.Nanosecond)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = m.Destroy(ctx, sid) })

	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Hammer sweeps while the resume holds the lock. Each one finds a paused,
	// past-retention sandbox and must decline to evict it because the lock is held.
	stop := make(chan struct{})
	done := make(chan struct{})
	r := lifecycle.NewReaper(m, time.Hour, time.Hour)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				r.Sweep(ctx)
			}
		}
	}()

	err = m.Resume(ctx, sid) // holds the transition lock for the whole cold start
	close(stop)
	<-done // no sweep still running when we inspect state

	// The resume must have SUCCEEDED. If a sweep evicted mid-cold-start, the record
	// and its Overlay vanish underneath it and this fails — which is the whole point.
	if err != nil {
		t.Fatalf("resume failed while concurrent sweeps ran: %v — a sweep evicted the sandbox out from under an in-flight resume, deleting the Overlay it was mounting (the documented invariant TryLock-skip)", err)
	}
	if stt, _ := m.State(ctx, sid); stt != model.Running {
		t.Fatalf("state=%s; want running — the resume completed, so no sweep may have disposed of it (the documented invariant)", stt)
	}
	// And it was never destroyed behind the resume's back: a `destroyed` event would
	// be the durable trace of an eviction that raced the verb.
	for _, k := range eventKindsFor(t, eng, sid) {
		if k == "destroyed" {
			t.Fatalf("a concurrent sweep EVICTED a lock-held sandbox (events=%v) — the reaper must TryLock-skip a sandbox a verb is driving (the documented invariant / reaper-health the documented invariant)",
				eventKindsFor(t, eng, sid))
		}
	}
	t.Logf("✓ reaper TryLock-skips an evict-eligible sandbox held by an in-flight resume (the documented invariant)")
}

// eventKindsFor returns every outbox event Kind for a sandbox, in commit order.
func eventKindsFor(t *testing.T, eng *orchestrator.Engine, sid model.SandboxID) []string {
	t.Helper()
	evs, err := eng.Store.UnpublishedEvents(context.Background(), 100 /* a bound is required; well above what this test emits */)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Sandbox == sid {
			out = append(out, string(ev.Kind))
		}
	}
	return out
}
