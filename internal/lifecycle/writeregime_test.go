package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
)

// TestWriteRegimeRefusesWhileALiveRWMountHolds is the first direction of the documented invariant's
// write-regime invariant: a live read-write mount blocks a direct content change.
func TestWriteRegimeRefusesWhileALiveRWMountHolds(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = m.ImportVolume(ctx, vid, "f", strings.NewReader("host-side write"))
	if !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("import while a live rw mount holds the volume = %v; want ErrVolumeLiveWriter", err)
	}
	// The message must name the holder: the remedy is to pause THAT sandbox, and a
	// caller cannot act on a refusal that does not say which one.
	if !strings.Contains(err.Error(), string(sid)) {
		t.Errorf("the refusal does not name the holding sandbox: %v", err)
	}
	// EXPORT is NOT gated: the invariant is about tearing, and a read tears nothing.
	if _, err := m.ExportVolume(ctx, vid, "f", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not exist") {
		// The file was never written, so a not-exist error is the expected outcome; what
		// must NOT happen is a write-regime refusal.
		if errors.Is(err, ErrVolumeLiveWriter) {
			t.Errorf("export was refused by the write regime — a read cannot tear anything, so gating it costs a capability and buys nothing")
		}
	}
}

// TestPausedSandboxIsNotALiveWriter is the OTHER direction, and it is the half that is
// an explicit PROMISE rather than a convenience (the documented invariant / the relevant internals page the documented invariant): pausing the
// holder must make the same import succeed, and the sandbox sees the new bytes on
// resume.
//
// Tightening the gate to "any row in the mount table" would be more conservative and
// would tear nothing — and would delete this documented capability. That is why the
// predicate reads the live-map rather than the table.
func TestPausedSandboxIsNotALiveWriter(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.ImportVolume(ctx, vid, "f", strings.NewReader("x")); !errors.Is(err, ErrVolumeLiveWriter) {
		t.Fatalf("precondition: import should be refused while running, got %v", err)
	}

	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The MOUNT ROW IS STILL THERE — this is not "the binding went away", it is "the
	// writer is not live".
	rows, _ := st.ListMounts(ctx, sid)
	if len(rows) != 1 {
		t.Fatalf("the mount row vanished on pause (%d rows) — that would make this test prove the wrong thing", len(rows))
	}
	payload := "written while the holder was paused"
	if _, err := m.ImportVolume(ctx, vid, "f", strings.NewReader(payload)); err != nil {
		t.Fatalf("import after pausing the holder = %v; want success — `paused` is NOT a live writer, and that is a promise, not an implementation detail (the documented invariant)", err)
	}
	var out bytes.Buffer
	if _, err := m.ExportVolume(ctx, vid, "f", &out); err != nil {
		t.Fatalf("export: %v", err)
	}
	if out.String() != payload {
		t.Errorf("content = %q, want %q", out.String(), payload)
	}
	// And the sandbox resumes onto the same volume, so it will see those bytes.
	if err := m.Resume(ctx, sid); err != nil {
		t.Fatalf("resume after the direct write: %v", err)
	}
}

// TestLiveReadOnlyMountDoesNotBlockDirectWrite: a live READER is not a writer, so it
// cannot tear anything. SETTLED in the documented invariant — no warning, no version stamp, no refusal:
// the reader's exposure is bounded (stale-but-whole / a momentary ENOENT / a prefix,
// never torn) and freshness beyond that is the documented invariant's existing non-promise. Refusing here
// would trade a capability for convenience.
func TestLiveReadOnlyMountDoesNotBlockDirectWrite(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/ro", Access: model.SharedReadOnly}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.ImportVolume(ctx, vid, "f", strings.NewReader("x")); err != nil {
		t.Errorf("import with only a live READ-ONLY mount = %v; want success — a reader cannot tear the bytes", err)
	}
}

// TestColdStartRefusedWhileADirectWriteIsInFlight is the SYMMETRIC half of the documented invariant's
// write regime, and the half whose absence left the hazard fully open in one order: an
// import admitted at T0 plus a resume completing at T1 produces the two concurrent
// writers the invariant forbids, without passing through any `paused` state that the
// direct-write side could have caught.
//
// The refusal is asserted on BOTH cold-start entry points, because they are different
// verbs with different fallback states (the documented invariant) and a gate wired into only one of them
// would be a gate on half the hazard.
func TestColdStartRefusedWhileADirectWriteIsInFlight(t *testing.T) {
	ctx := context.Background()

	t.Run("resume is refused and STAYS PAUSED", func(t *testing.T) {
		m, st, _, rt := mountFixture(t)
		vid, err := m.CreateVolume(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		sid, err := m.Create(ctx, CreateSpec{
			Template: "bare",
			Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.SharedReadWrite}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := m.Pause(ctx, sid); err != nil {
			t.Fatalf("pause: %v", err)
		}
		// A direct write is now legal (a `paused` sandbox is not a live writer) and takes
		// the marker. Held open across the resume below — the real import holds it for the
		// whole stream, so this is the state a resume genuinely races.
		release, err := m.admitDirectWrite(ctx, vid)
		if err != nil {
			t.Fatalf("admit the direct write on a paused holder: %v", err)
		}

		launchesBefore := len(rt.Calls())
		err = m.Resume(ctx, sid)
		if !errors.Is(err, ErrVolumeDirectWriteInFlight) {
			t.Fatalf("resume while a direct write is in flight = %v; want ErrVolumeDirectWriteInFlight — otherwise this resume and that import are the two concurrent writers the documented invariant forbids", err)
		}
		// The message must name the volume: the remedy is to wait for THAT import.
		if !strings.Contains(err.Error(), string(vid)) {
			t.Errorf("the refusal does not name the volume: %v", err)
		}
		// the documented invariant: atomic return to the SOURCE state. The refusal is taken before anything
		// is materialized, so there is nothing half-built to leave behind either.
		if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Paused {
			t.Fatalf("state = %s; want it to stay paused (the documented invariant source state)", rec.State)
		}
		if len(rt.Calls()) != launchesBefore {
			t.Fatalf("the refused resume still touched the runtime: %v", rt.Calls()[launchesBefore:])
		}

		// And the refusal is RETRYABLE, which is what makes "refuse, do not wait" an
		// honest answer: the documented remedy really does clear it.
		release()
		if err := m.Resume(ctx, sid); err != nil {
			t.Fatalf("resume after the import finished = %v; want success — the refusal must be retryable", err)
		}
	})

	t.Run("create is refused and leaves NO record", func(t *testing.T) {
		m, st, _, _ := mountFixture(t)
		vid, err := m.CreateVolume(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		release, err := m.admitDirectWrite(ctx, vid)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		_, err = m.Create(ctx, CreateSpec{
			Template: "bare",
			Volumes:  []model.Mount{{Volume: vid, Path: "/data", Access: model.Exclusive}},
		})
		if !errors.Is(err, ErrVolumeDirectWriteInFlight) {
			t.Fatalf("create mounting a volume mid-import = %v; want ErrVolumeDirectWriteInFlight (exclusive is an rw mode)", err)
		}
		// create has no fallback state, so the documented invariant means the record goes away entirely —
		// including the mount rows, or the record would be an undeletable zombie.
		if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
			t.Fatalf("refused create left %d sandbox record(s); want none (the documented invariant)", len(recs))
		}
		rows, err := st.ListMountsByVolume(ctx, vid)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("refused create left %d mount row(s) behind: %+v", len(rows), rows)
		}
	})
}

// TestColdStartWithAReadOnlyMountIsNotRefused is the permissive side of the same gate,
// and it is the assertion that keeps the symmetry from costing a capability: a cold
// start whose mount of the volume is READ-ONLY tears nothing, so a direct write in
// flight must NOT block it. This mirrors admitDirectWrite letting a live ro holder
// through.
//
// Refusing here as well would be more conservative and would tear nothing — and would
// delete the ability to boot a sandbox that merely READS a volume being seeded, which
// the relevant internals page the documented invariant calls the third kind of wrong.
func TestColdStartWithAReadOnlyMountIsNotRefused(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	release, err := m.admitDirectWrite(ctx, vid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// The marker really is in flight — otherwise this test would pass for the wrong
	// reason (nothing to refuse) and would keep passing if the gate refused ro mounts.
	if ids := m.inFlightWriteIDs(); len(ids) != 1 {
		t.Fatalf("precondition: in-flight writes = %v; want exactly this volume", ids)
	}

	sid, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes:  []model.Mount{{Volume: vid, Path: "/ro", Access: model.SharedReadOnly}},
	})
	if err != nil {
		t.Fatalf("create with a READ-ONLY mount during an import = %v; want success — a reader cannot tear the bytes, and refusing would trade away a capability", err)
	}
	if rec, _ := st.GetSandbox(ctx, sid); rec.State != model.Running {
		t.Fatalf("state = %s; want running", rec.State)
	}
	// The mixed case pins that the gate reads the ACCESS MODE per row rather than
	// "this sandbox touches that volume": one ro row on the in-flight volume plus an rw
	// row on an unrelated one is still admissible.
	other, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, CreateSpec{
		Template: "bare",
		Volumes: []model.Mount{
			{Volume: vid, Path: "/ro", Access: model.SharedReadOnly},
			{Volume: other, Path: "/rw", Access: model.SharedReadWrite},
		},
	}); err != nil {
		t.Fatalf("create with ro on the in-flight volume + rw on another = %v; want success", err)
	}
}

// TestDirectWriteMarkerProtectsFromTheSweep: the in-flight marker and the write-regime
// decision are published in ONE critical section, and the marker is what keeps a
// concurrent sweep from reclaiming bytes mid-write.
//
// A volume being written to right now is IN USE even though nothing has it mounted —
// exactly the case no mount row can express. Asserted on the BYTES rather than on the
// keep-set contents, because the bytes surviving is the property; the keep-set is one
// implementation of it.
func TestDirectWriteMarkerProtectsFromTheSweep(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The marker is taken through admitDirectWrite DIRECTLY rather than by racing a real
	// ImportVolume, and that is not a shortcut. Isolating the marker requires the volume's
	// ROW to be gone — otherwise the record alone protects the bytes and the test proves
	// nothing — but ImportVolume checks the record first (an unknown volume is a clean
	// 404), so a real import cannot even begin in that state. Waiting for a marker that
	// can never be published is how the first version of this test spun forever.
	release, err := m.admitDirectWrite(ctx, vid)
	if err != nil {
		t.Fatalf("admit the direct write: %v", err)
	}
	if err := st.DeleteVolume(ctx, vid); err != nil {
		t.Fatal(err)
	}

	if err := m.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	vb.mu.Lock()
	survived := vb.units[string(vid)]
	vb.mu.Unlock()
	if !survived {
		t.Fatalf("the volume was reclaimed mid-write — the in-flight marker exists precisely to cover a write to a volume nothing has mounted (the documented invariant)")
	}

	release()
	// The marker is RELEASED, so it does not pin the volume forever: with no row and no
	// write in flight, these bytes ARE an orphan now and the next sweep takes them.
	if ids := m.inFlightWriteIDs(); len(ids) != 0 {
		t.Fatalf("the marker survived its release: %v", ids)
	}
	if err := m.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	vb.mu.Lock()
	stillThere := vb.units[string(vid)]
	vb.mu.Unlock()
	if stillThere {
		t.Errorf("the volume survived a sweep after its write finished and its row was gone — the marker must not pin it permanently")
	}
}

// TestConcurrentDirectWritesEachHoldTheMarker: the marker is a COUNT, not a boolean,
// so one write finishing cannot drop another's protection. Same reason the transition
// lock counts handouts rather than holders.
func TestConcurrentDirectWritesEachHoldTheMarker(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	relA, err := m.admitDirectWrite(ctx, vid)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := m.admitDirectWrite(ctx, vid)
	if err != nil {
		t.Fatal(err)
	}
	relA()
	if ids := m.inFlightWriteIDs(); len(ids) != 1 {
		t.Fatalf("after ONE of two writes released, the marker is %v — a boolean would have dropped the second write's protection here", ids)
	}
	relB()
	if ids := m.inFlightWriteIDs(); len(ids) != 0 {
		t.Errorf("the marker survived both releases: %v", ids)
	}
}
