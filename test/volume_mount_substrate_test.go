//go:build substrate

// L3 acceptance: Volumes actually MOUNTED into real guests. Requires
// root and a real btrfs state dir.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -p 1 -timeout 900s ./test/ -run VolumeMount
//
// THE DISCIPLINE FROM §5.C / volume.md D7.2 APPLIES THROUGHOUT: never assert "the
// sandbox is still alive". Measured, five separate ways of yanking a volume out from
// under a live guest killed NO sandbox — container still running, fresh exec rc=0,
// runtime silent. That assertion is vacuously true and worthless as a guard. What is
// asserted instead is the properties that would pass SILENTLY when broken.
package test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/orchestrator"
	"warren/internal/transport"
	"warren/test/harness"
)

// guestSh runs one shell command in a sandbox and returns its combined output.
//
// The command must be a SINGLE PHYSICAL LINE: a newline inside process.args makes
// runsc reject the spec as an illegal string literal. And the bare rootfs ships no
// coreutils, so only dash builtins are available — no `cat`, no `ls`.
func guestSh(t *testing.T, eng *orchestrator.Engine, id model.SandboxID, script string) string {
	t.Helper()
	if strings.Contains(script, "\n") {
		t.Fatalf("guest script must be a single physical line (runsc rejects a newline in process.args): %q", script)
	}
	cl, err := eng.Mediator.Client(id)
	if err != nil {
		t.Fatalf("client for %s: %v", id, err)
	}
	ch, err := cl.Start(context.Background(), transport.StartSpec{
		Cmd: "/bin/sh", Args: []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("exec in %s: %v", id, err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == "data" {
			out.Write(ev.Data)
		}
	}
	return out.String()
}

// mountEngine builds an engine on real btrfs with a generous disk budget, so the
// admission gate is not what refuses anything in these tests.
func mountEngine(t *testing.T) (*orchestrator.Engine, string) {
	t.Helper()
	base, stateDir := harness.BtrfsBase(t, "vol-mount-", "volume mounts are storage units", cleanupUnitsAndOverlays)
	eng, _ := harness.Engine(t, harness.EngineOpts{
		BaseDir:       base,
		HealthTimeout: 90 * time.Second,
	})
	return eng, stateDir
}

// TestVolumeMountTwoConcurrentWritersBothReallyWrite is the
// LOAD-BEARING FOUNDATION of the whole AccessMode enum (volume.md D1): if a second
// gofer could not attach the same source, SharedReadOnly's read fanout and
// SharedReadWrite would both be unimplementable and the enum would collapse to
// Exclusive alone.
//
// "Both mounts succeeded" is NOT the assertion — a silent downgrade to read-only would
// pass that. The assertion is that BOTH SIDES GENUINELY WRITE, and that killing one
// leaves the other able to read AND write (teardown is not a cross-sandbox side effect).
func TestVolumeMountTwoConcurrentWritersBothReallyWrite(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	var sids []model.SandboxID
	for i := 0; i < 2; i++ {
		sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
			Template: "bare",
			Volumes:  []model.Mount{{Volume: vid, Path: "/shared", Access: model.SharedReadWrite}},
		})
		if err != nil {
			t.Fatalf("create sandbox %d with a shared-rw mount: %v", i, err)
		}
		sids = append(sids, sid)
	}
	t.Cleanup(func() {
		for _, sid := range sids {
			_ = eng.Mediator.Destroy(ctx, sid)
		}
	})

	// Each writes its OWN file, then reads the other's. Both halves matter: writing
	// proves the mount is genuinely writable, reading the other's proves it is the same
	// real volume rather than a per-sandbox copy.
	for i, sid := range sids {
		got := guestSh(t, eng, sid, fmt.Sprintf("echo WRITER-%d > /shared/w%d && echo WROTE-%d", i, i, i))
		if !strings.Contains(got, fmt.Sprintf("WROTE-%d", i)) {
			t.Fatalf("sandbox %d could not write to its shared-rw mount: %q — a silent downgrade to read-only would pass a mere 'the mount succeeded' check", i, got)
		}
	}
	for i, sid := range sids {
		other := 1 - i
		got := guestSh(t, eng, sid, fmt.Sprintf("read L < /shared/w%d; echo GOT:$L", other))
		if !strings.Contains(got, fmt.Sprintf("GOT:WRITER-%d", other)) {
			t.Errorf("sandbox %d does not see sandbox %d's bytes: %q — the volume must be ONE real thing, not a copy per mount", i, other, got)
		}
	}

	// Kill one side hard, then force-destroy it: the survivor must still read AND write.
	// Teardown of one sandbox is not a cross-sandbox side effect (volume.md D1).
	if err := eng.Mediator.Destroy(ctx, sids[0]); err != nil {
		t.Fatalf("destroy the first holder: %v", err)
	}
	got := guestSh(t, eng, sids[1], "echo AFTER > /shared/after && read L < /shared/after; echo STILL:$L")
	if !strings.Contains(got, "STILL:AFTER") {
		t.Errorf("the survivor cannot write after the other holder was destroyed: %q — one sandbox's teardown must not disturb the other gofer's view of the same source", got)
	}
	// And the volume itself outlived both records (§5.C).
	if _, err := eng.Store.GetVolume(ctx, vid); err != nil {
		t.Errorf("the volume was reclaimed with its holder: %v", err)
	}
}

// TestVolumeMountReadOnlyArmGetsEROFS pins the read-only arm: a read-only mount coexists
// with live read-write mounts, the guest's write fails with EROFS (a CLEAN POSIX
// refusal, not EIO and not a fault), and — the half that would pass silently — THE HOST
// NEVER SEES THAT WRITE.
func TestVolumeMountReadOnlyArmGetsEROFS(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	// Seed through the direct accessor, so the reader has something to read and no
	// sandbox is needed to put it there (§5.C).
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "seed", strings.NewReader("SEEDED")); err != nil {
		t.Fatalf("seed volume: %v", err)
	}

	var rw []model.SandboxID
	for i := 0; i < 2; i++ {
		sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
			Template: "bare",
			Volumes:  []model.Mount{{Volume: vid, Path: "/shared", Access: model.SharedReadWrite}},
		})
		if err != nil {
			t.Fatalf("create rw holder %d: %v", i, err)
		}
		rw = append(rw, sid)
	}
	ro, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/shared", Access: model.SharedReadOnly}},
	})
	if err != nil {
		t.Fatalf("create the ro holder alongside two live rw holders: %v", err)
	}
	t.Cleanup(func() {
		for _, sid := range append(rw, ro) {
			_ = eng.Mediator.Destroy(ctx, sid)
		}
	})

	// The reader reads.
	if got := guestSh(t, eng, ro, "read L < /shared/seed; echo GOT:$L"); !strings.Contains(got, "GOT:SEEDED") {
		t.Errorf("the ro mount cannot read: %q", got)
	}
	// Its write fails, and the shell reports the errno text. dash's redirect failure
	// message carries "read-only file system" for EROFS.
	got := guestSh(t, eng, ro, "echo NOPE > /shared/forbidden 2>&1; echo RC:$?")
	if strings.Contains(got, "RC:0") {
		t.Errorf("a write through the READ-ONLY mount reported success: %q", got)
	}
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "read-only") {
		t.Logf("note: the guest's refusal text was %q (checking the host below is the real assertion)", strings.TrimSpace(got))
	}

	// THE ASSERTION THAT MATTERS, because a silent success is the failure mode: the host
	// must not see that file. Checked through the direct accessor rather than the guest,
	// so a cached dentry cannot mask it.
	var sink bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "forbidden", &sink); err == nil {
		t.Fatalf("THE HOST SEES the file a read-only mount claimed to refuse (%d bytes) — a write that fails in the guest but lands on the host is the silent failure this asserts against", sink.Len())
	}
	// And the two rw holders are unaffected by the ro arm's presence.
	for i, sid := range rw {
		if out := guestSh(t, eng, sid, fmt.Sprintf("echo OK > /shared/rw%d && echo WROTE", i)); !strings.Contains(out, "WROTE") {
			t.Errorf("rw holder %d lost its writability while a ro arm was mounted: %q", i, out)
		}
	}
}

// TestVolumeMountTopologyPersistsAcrossPause pins the mount topology as
// a BINDING (§3.4), so a resume re-mounts per the CURRENT table — a NEW host
// materialization of the SAME real volume, never a restored copy.
func TestVolumeMountTopologyPersistsAcrossPause(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })

	if out := guestSh(t, eng, sid, "echo BEFORE > /data/f && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("initial write: %q", out)
	}
	if err := eng.Mediator.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Written from the HOST while the holder is paused — which is legal precisely
	// because `paused` is not a live writer (§5.C), and which proves the resume reads
	// the real volume rather than anything captured at pause time.
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "host-added", strings.NewReader("FROM-HOST")); err != nil {
		t.Fatalf("direct write while the holder is paused: %v", err)
	}
	if err := eng.Mediator.Resume(ctx, sid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Same mount point, same volume, INCLUDING the bytes added from outside.
	out := guestSh(t, eng, sid, "read A < /data/f; read B < /data/host-added; echo GOT:$A:$B")
	if !strings.Contains(out, "GOT:BEFORE:FROM-HOST") {
		t.Fatalf("after resume the guest sees %q; want its own bytes AND the host's — the topology re-materializes from the CURRENT table onto the SAME volume (§3.4)", out)
	}
}

// TestVolumeMountDetachReleasesTheLeaseBeforeTheTopologyChanges pins that THE TWO EFFECT TIMES ARE NOT SYNCHRONIZED — that is a design fact,
// not a detail to fold into one value (§6.1).
func TestVolumeMountDetachReleasesTheLeaseBeforeTheTopologyChanges(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	holder, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if err != nil {
		t.Fatalf("create holder: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, holder) })
	if out := guestSh(t, eng, holder, "echo MINE > /data/f && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("holder write: %q", out)
	}

	// Detach by rebinding to an empty binding.
	// The one PATCH verb, with a mounts-only spec: a non-nil pointer to a nil slice is
	// "detach everything" (§6.2), which is what this step asks for.
	var detachAll []model.Mount
	released, err := eng.Mediator.Rebind(ctx, holder, lifecycle.RebindSpec{Mounts: &detachAll})
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !released {
		t.Errorf("the detach did not report releasing the exclusive lease")
	}

	// THE LEASE IS FREE NOW: another sandbox takes it exclusively while the original
	// holder is still running with the volume in its guest.
	taker, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
	})
	if err != nil {
		t.Fatalf("a second sandbox could not take the released lease: %v — the lease releases when the ROW is deleted, not at the next resume (§6.1)", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, taker) })

	// AND THE TOPOLOGY HAS NOT CHANGED YET: the original holder still has the volume in
	// its guest, because runsc took its mounts at startup and does not hot-unplug.
	if out := guestSh(t, eng, holder, "read L < /data/f; echo GOT:$L"); !strings.Contains(out, "GOT:MINE") {
		t.Errorf("the detached sandbox lost its mount before resuming: %q — the topology change lands at next-resume, which is why one applies_at value cannot describe a detach", out)
	}
	// After a pause/resume cycle it is gone.
	if err := eng.Mediator.Pause(ctx, holder); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := eng.Mediator.Resume(ctx, holder); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out := guestSh(t, eng, holder, "read L < /data/f 2>&1; echo RC:$?"); strings.Contains(out, "RC:0") {
		t.Errorf("the detached volume is STILL mounted after a resume: %q", out)
	}
}

// TestVolumeMountExclusiveLeaseUnderConcurrentAttach is the exclusive lease end-to-end:
// 24 racers attach one volume exclusively, exactly one wins, the rest get the lease
// verdict, and ZERO get a driver error.
//
// The store-level version of this is in storetest; this one runs through the real
// mediator with real cold starts, so it also proves the winner is the one that
// actually got a sandbox.
func TestVolumeMountExclusiveLeaseUnderConcurrentAttach(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	const racers = 24
	start := make(chan struct{})
	type result struct {
		sid model.SandboxID
		err error
	}
	results := make(chan result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
				Template: "bare",
				Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
			})
			results <- result{sid, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winners []model.SandboxID
	var conflicts, other int
	for r := range results {
		switch {
		case r.err == nil:
			winners = append(winners, r.sid)
		case strings.Contains(r.err.Error(), "already exclusively mounted"):
			conflicts++
		default:
			other++
			t.Errorf("racer got neither success nor the lease verdict: %v", r.err)
		}
	}
	t.Cleanup(func() {
		for _, sid := range winners {
			_ = eng.Mediator.Destroy(ctx, sid)
		}
	})
	if len(winners) != 1 {
		t.Fatalf("%d racers won the exclusive lease, want exactly 1 (conflicts=%d other=%d)", len(winners), conflicts, other)
	}
	if other != 0 {
		t.Errorf("%d racers got a driver error rather than a clean verdict; the measured run had ZERO", other)
	}
	t.Logf("24 racers → 1 winner, %d lease conflicts, %d driver errors", conflicts, other)

	// The winner really has the volume.
	if out := guestSh(t, eng, winners[0], "echo WON > /data/f && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Errorf("the lease winner cannot write to the volume: %q", out)
	}
	// And exactly one mount row exists for it.
	rows, err := eng.Store.ListMountsByVolume(ctx, vid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("the contended volume has %d mount rows, want 1: %+v", len(rows), rows)
	}
}
