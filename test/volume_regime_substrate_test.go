//go:build substrate

// L3 acceptance: the write regime through real guests, Promote
// with a volume mounted, and what a sweep must NOT disturb. Requires root and real
// btrfs.
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -p 1 -timeout 900s ./test/ -run VolumeRegime
//
// Same discipline as the mount suite: "the sandbox is still alive" is never the
// assertion — measured, it is vacuously true under every way of yanking a volume
// (volume.md D7.2). The assertions here are the ones that would pass SILENTLY.
package test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"engine/internal/btrfs"
	"engine/internal/lifecycle"
	"engine/internal/model"
)

// TestVolumeRegimeRefusesDirectWriteUnderALiveHolderAndAllowsItWhenPaused pins the write regime in BOTH directions.
//
// The second direction is the one that matters most: `paused` is NOT a live writer, and
// that is an explicitly written PROMISE rather than a detail that could be tightened
// away for convenience (§5.C / volume.md D5). Refusing on "any row in the mount table"
// would tear nothing and would delete this capability.
func TestVolumeRegimeRefusesDirectWriteUnderALiveHolderAndAllowsItWhenPaused(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })

	// Direction 1: a LIVE rw holder blocks the direct write.
	_, err = eng.Mediator.ImportVolume(ctx, vid, "from-host", strings.NewReader("HOST-BYTES"))
	if !errors.Is(err, lifecycle.ErrVolumeLiveWriter) {
		t.Fatalf("import under a live rw mount = %v; want ErrVolumeLiveWriter (§5.C write regime)", err)
	}
	// And nothing landed: a refusal that had already written part of the payload would
	// be the tear the invariant exists to prevent.
	var sink bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "from-host", &sink); err == nil {
		t.Errorf("the REFUSED import still left %d bytes in the volume", sink.Len())
	}
	// EXPORT is not gated — a read tears nothing.
	if out := guestSh(t, eng, sid, "echo GUEST > /data/g && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("guest write: %q", out)
	}
	var readBack bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "g", &readBack); err != nil {
		t.Errorf("export under a live rw mount = %v; want success — the write regime gates WRITES, not reads", err)
	} else if !strings.Contains(readBack.String(), "GUEST") {
		t.Errorf("export returned %q, want the guest's bytes", readBack.String())
	}

	// Direction 2: PAUSE the holder, and the SAME import must now succeed.
	if err := eng.Mediator.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The mount row is STILL THERE — this is "the writer is not live", not "the binding
	// went away". Without checking this the test would prove the wrong thing.
	rows, err := eng.Store.ListMounts(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the mount row vanished on pause (%d rows)", len(rows))
	}
	const payload = "WRITTEN-WHILE-PAUSED"
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "from-host", strings.NewReader(payload)); err != nil {
		t.Fatalf("import while the holder is PAUSED = %v; want success — `paused` is not a live writer, and that is a promise (§5.C)", err)
	}

	// And on resume the guest sees the new bytes, which is the second half of the
	// promise ("resume 后自然见到新内容").
	if err := eng.Mediator.Resume(ctx, sid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out := guestSh(t, eng, sid, "read L < /data/from-host; echo GOT:$L"); !strings.Contains(out, "GOT:"+payload) {
		t.Errorf("after resume the guest sees %q; want the bytes written while it was paused", out)
	}
}

// TestVolumeRegimeSeedWithoutASandboxThenMountThenReadAfterDestroy is import → export in full, with a real mount in the middle.
//
// This is the SUBSTANCE of "a volume's lifecycle is decoupled from any sandbox" (§5.C):
// an object whose content can only move through a sandbox is independent on paper only.
func TestVolumeRegimeSeedWithoutASandboxThenMountThenReadAfterDestroy(t *testing.T) {
	ctx := context.Background()
	eng, _ := mountEngine(t)

	// ① A node with NO sandbox at all.
	recs, err := eng.Store.ListSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("this node already has %d sandbox(es); the property is that none is needed", len(recs))
	}
	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	payload := bytes.Repeat([]byte("seed-"), 200<<10/5) // 200 KiB, several stream chunks
	if _, err := eng.Mediator.ImportVolume(ctx, vid, "payload.bin", bytes.NewReader(payload)); err != nil {
		t.Fatalf("seed from zero: %v", err)
	}
	// import → export round-trips byte-for-byte before any sandbox exists.
	var out bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "payload.bin", &out); err != nil {
		t.Fatalf("export before any sandbox: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("zero-sandbox round trip mismatch: %d back, %d written", out.Len(), len(payload))
	}

	// ② A sandbox mounts it and sees THE SAME bytes. Compared by length and by a
	// checksum-ish tail rather than by dumping 200 KiB through the shell: the bare
	// rootfs has no coreutils, so only dash builtins are available.
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/seeded", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create sandbox onto the seeded volume: %v", err)
	}
	// A dash builtin read of the first line is enough to prove the content arrived: the
	// payload has no newline, so `read` returns the whole thing.
	got := guestSh(t, eng, sid, "read L < /seeded/payload.bin; echo LEN:${#L}")
	if !strings.Contains(got, fmt.Sprintf("LEN:%d", len(payload))) {
		t.Errorf("the guest sees %q; want LEN:%d — the bytes seeded with no sandbox must be exactly what a later sandbox reads", strings.TrimSpace(got), len(payload))
	}
	// The guest adds something, so the post-destroy read has a guest-authored fact in it.
	if out := guestSh(t, eng, sid, "echo GUEST-WAS-HERE > /seeded/guest && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("guest write: %q", out)
	}

	// ③ Destroy the sandbox — the LAST one on this node — and read the volume again.
	if err := eng.Mediator.Destroy(ctx, sid); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	recs, _ = eng.Store.ListSandboxes(ctx)
	if len(recs) != 0 {
		t.Fatalf("expected no sandboxes left, found %d", len(recs))
	}
	out.Reset()
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "payload.bin", &out); err != nil {
		t.Fatalf("export AFTER the last sandbox is gone: %v — this is the scenario the direct accessor exists for (§5.C)", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Errorf("post-destroy export differs: %d bytes back, %d expected", out.Len(), len(payload))
	}
	var guestBytes bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "guest", &guestBytes); err != nil {
		t.Errorf("the guest's own bytes are unreadable after its sandbox was destroyed: %v", err)
	} else if !strings.Contains(guestBytes.String(), "GUEST-WAS-HERE") {
		t.Errorf("guest-authored content = %q", guestBytes.String())
	}
}

// TestVolumeRegimeSweepLeavesALiveGuestUntouched covers three properties in one pass: the sweep keeps a mounted volume, its content
// is unchanged, and its cap is untouched.
//
// The content assertion exists because of a measured trap: asserting "the guest's write returned
// ok" passes even when the volume has been COMPLETELY DELETED underneath — the guest
// writes into an unlinked inode and sees zero errors (volume.md D7.2). So the assertion
// is that the STEADY STATE is byte-for-byte identical across the sweep, read from the
// HOST.
//
// The cap assertion is the sharper one: `limit none` is the ONE operation that breaks an engine
// promise beyond data integrity, and it has no EBUSY self-protection, so a sweep must
// leave a live volume's cap exactly where it was.
func TestVolumeRegimeSweepLeavesALiveGuestUntouched(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)
	bc := btrfs.Client{}

	const capBytes = 16 << 20
	vid, err := eng.Mediator.CreateVolume(ctx, capBytes)
	if err != nil {
		t.Fatalf("create capped volume: %v", err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })

	// A steady state the guest wrote, and which the host can read independently.
	if out := guestSh(t, eng, sid, "echo STEADY-STATE-CONTENT > /data/steady && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("seed the steady state: %q", out)
	}
	unit := volumeUnitPath(stateDir, vid)
	before, err := os.ReadFile(filepath.Join(unit, "steady"))
	if err != nil {
		t.Fatalf("read the steady state from the host: %v", err)
	}
	_, limitBefore, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("read the cap: %v", err)
	}
	if limitBefore != capBytes {
		t.Fatalf("cap on storage = %d, want %d", limitBefore, capBytes)
	}

	// A TRUE ORPHAN alongside, so this pass has a positive control: without one, every
	// assertion below would pass identically for a sweep that does nothing.
	orphan, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Store.DeleteVolume(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	orphanPath := volumeUnitPath(stateDir, orphan)

	if err := eng.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Positive control first.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("the orphan survived the sweep (%v) — a no-op sweep passes every other assertion here", err)
	}
	// The steady state must be BYTE-FOR-BYTE identical, read from the host.
	after, err := os.ReadFile(filepath.Join(unit, "steady"))
	if err != nil {
		t.Fatalf("the live guest's volume content is unreadable after the sweep: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the live guest's steady state changed across the sweep: %q → %q — note that asserting 'the guest's write returned ok' would NOT catch this (a deleted volume still accepts writes into an unlinked inode)", before, after)
	}
	// The cap must be UNCHANGED.
	_, limitAfter, err := bc.Usage(ctx, unit)
	if err != nil {
		t.Fatalf("read the cap after the sweep: %v", err)
	}
	if limitAfter != limitBefore {
		t.Fatalf("the sweep changed a LIVE volume's cap: %d → %d — `limit none` is the one operation that breaks an engine promise beyond data integrity, and it has no EBUSY self-protection (volume.md D7.2)", limitBefore, limitAfter)
	}

	// And the cap is still ENFORCED afterwards: a guest that fills the volume must still
	// hit ENOSPC. A recorded-but-cleared cap is otherwise invisible.
	fill := fmt.Sprintf("i=0; while [ $i -lt %d ]; do echo %s >> /data/fill || { echo HIT_ENOSPC; break; }; i=$((i+1)); done; echo DONE", 200000, strings.Repeat("x", 120))
	out := guestSh(t, eng, sid, fill)
	if !strings.Contains(out, "HIT_ENOSPC") {
		t.Errorf("after the sweep the guest filled the volume without hitting the cap: %q — the cap survived in the quota row but is not being enforced", strings.TrimSpace(out))
	}
}

// TestVolumeRegimePromoteExcludesTheMountPoint pins the excluded set on the substrate: a
// Promote of a sandbox that had a volume mounted captures the mount point as an EMPTY
// PLACEHOLDER, the volume's content does NOT enter the layer, and the shadowed upper
// bytes are pruned.
func TestVolumeRegimePromoteExcludesTheMountPoint(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	sid, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/workspace", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(ctx, sid) })

	// Content in the VOLUME (must not enter the layer) and content in the OVERLAY
	// outside the mount point (must survive).
	if out := guestSh(t, eng, sid, "echo IN-VOLUME > /workspace/vol.txt && echo OUTSIDE > /etc/mine.conf && echo WROTE"); !strings.Contains(out, "WROTE") {
		t.Fatalf("seed: %q", out)
	}

	tid, err := eng.Mediator.Promote(ctx, sid)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	layer := filepath.Join(stateDir, "layers", string(tid))

	// The volume's content is NOT in the layer. This is the same thing as `tar` of a
	// directory with something mounted on it capturing only the empty shell (§5.B).
	if _, err := os.Stat(filepath.Join(layer, "workspace", "vol.txt")); !os.IsNotExist(err) {
		t.Errorf("the volume's content entered the frozen layer (%v) — a Volume lives in its own backend and is outside Promote's capture (§5.B / §5.C)", err)
	}
	// The mount point itself is at most an EMPTY placeholder.
	if ents, err := os.ReadDir(filepath.Join(layer, "workspace")); err == nil && len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the mount point in the layer is not empty: %v", names)
	}
	// Real guest data outside the mount point DID survive — otherwise this test would
	// pass for a Promote that captured nothing at all.
	if _, err := os.Stat(filepath.Join(layer, "etc", "mine.conf")); err != nil {
		t.Errorf("guest data outside the mount point was pruned from the layer: %v — only the mount's own subtree may be excluded", err)
	}
	// And the volume still holds its bytes: Promote read the Overlay, not the volume.
	var vol bytes.Buffer
	if _, err := eng.Mediator.ExportVolume(ctx, vid, "vol.txt", &vol); err != nil {
		t.Errorf("the volume's content is gone after a Promote: %v", err)
	} else if !strings.Contains(vol.String(), "IN-VOLUME") {
		t.Errorf("volume content = %q", vol.String())
	}
}

// TestVolumeRegimeConcurrentImportAndSweep pins the in-flight marker: an in-flight direct
// write is not reclaimed as an orphan, because the marker is published in the SAME
// critical section that decided the write was allowed.
//
// A volume being written to right now is IN USE even though nothing has it mounted —
// exactly the case no mount row can express.
func TestVolumeRegimeConcurrentImportAndSweep(t *testing.T) {
	ctx := context.Background()
	eng, stateDir := mountEngine(t)

	vid, err := eng.Mediator.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	unit := volumeUnitPath(stateDir, vid)

	// A payload big enough that the import is genuinely in flight while the sweep runs,
	// fed through a reader that yields slowly rather than through a sleep.
	payload := bytes.Repeat([]byte("concurrent-"), 4<<20/11) // ~4 MiB
	var wg sync.WaitGroup
	importErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := eng.Mediator.ImportVolume(ctx, vid, "big.bin", &trickleReader{data: payload, chunk: 4096})
		importErr <- err
	}()

	// Sweep repeatedly while the import runs. Repeating rather than trying to hit one
	// instant is deliberate: the property must hold for the WHOLE write, not just at a
	// lucky moment.
	sweeps := 0
	for {
		select {
		case err := <-importErr:
			wg.Wait()
			if err != nil {
				t.Fatalf("the concurrent import failed: %v", err)
			}
			t.Logf("ran %d sweeps during the import", sweeps)
			if sweeps == 0 {
				t.Fatal("no sweep ran during the import, so this test proved nothing")
			}
			// The unit survived every sweep, and its content is complete.
			if _, err := os.Stat(unit); err != nil {
				t.Fatalf("the volume was reclaimed during an in-flight direct write: %v — the in-flight marker exists precisely to prevent this (§5.C)", err)
			}
			var out bytes.Buffer
			if _, err := eng.Mediator.ExportVolume(ctx, vid, "big.bin", &out); err != nil {
				t.Fatalf("export after the concurrent sweeps: %v", err)
			}
			if !bytes.Equal(out.Bytes(), payload) {
				t.Errorf("the imported content is not intact: %d bytes back, %d written", out.Len(), len(payload))
			}
			// And the marker is released now, so it does not pin the volume forever.
			if err := eng.SweepOrphanVolumes(ctx); err != nil {
				t.Fatalf("post-import sweep: %v", err)
			}
			if _, err := os.Stat(unit); err != nil {
				t.Errorf("the volume was reclaimed after the write finished: %v — it has a live record, so it is not an orphan", err)
			}
			return
		default:
			if err := eng.SweepOrphanVolumes(ctx); err != nil {
				t.Fatalf("sweep during import: %v", err)
			}
			sweeps++
		}
	}
}

// trickleReader yields data in small chunks so a write stays in flight long enough for
// a concurrent sweep to overlap it, without any sleeping.
type trickleReader struct {
	data  []byte
	chunk int
	off   int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		// io.EOF, not a custom sentinel: ImportVolume distinguishes "the source ended"
		// from "the source failed", and a look-alike error is treated as a failure.
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if r.off+n > len(r.data) {
		n = len(r.data) - r.off
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}
