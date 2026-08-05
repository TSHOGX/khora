package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"engine/internal/model"
)

// TestWriteRegimeRefusesWhileALiveRWMountHolds is the first direction of §5.C's
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
// an explicit PROMISE rather than a convenience (§5.C / volume.md D5): pausing the
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
		t.Fatalf("import after pausing the holder = %v; want success — `paused` is NOT a live writer, and that is a promise, not an implementation detail (§5.C)", err)
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
// cannot tear anything. Whether to warn about its gofer cache going stale is a
// PRECISION question left deliberately open (volume.md D8) — and settling it by
// quietly refusing here would again trade a capability for convenience.
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

// TestDirectWriteMarkerProtectsFromTheSweep: the in-flight marker and the write-regime
// decision are published in ONE critical section, and the marker is what keeps a
// concurrent sweep from reclaiming bytes mid-write (M7 acceptance 24).
//
// A volume being written to right now is IN USE even though nothing has it mounted —
// which is exactly the case no mount row can express.
func TestDirectWriteMarkerProtectsFromTheSweep(t *testing.T) {
	ctx := context.Background()
	m, _, vb, _ := mountFixture(t)
	vid, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the marker by blocking inside the import's own read, so the sweep runs while
	// the write is genuinely in flight rather than merely nearby.
	released := make(chan struct{})
	sweepDone := make(chan []string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = m.ImportVolume(ctx, vid, "f", &blockingReader{gate: released})
	}()

	// Wait until the marker is actually published, so this is not a timing race.
	for {
		if ids := m.inFlightWriteIDs(); len(ids) == 1 && ids[0] == string(vid) {
			break
		}
	}
	if err := m.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The keep-set handed to the backend must contain the in-flight volume.
	for _, c := range vb.Calls() {
		if strings.HasPrefix(c, "SweepOrphanVolumes:") {
			sweepDone <- []string{c}
		}
	}
	close(released)
	wg.Wait()

	select {
	case calls := <-sweepDone:
		if !strings.Contains(calls[0], string(vid)) {
			t.Errorf("the in-flight write was not in the sweep's keep-set: %q — a volume being written to is in use even with nothing mounted", calls[0])
		}
	default:
		t.Fatal("the backend sweep was never called")
	}
	// And the marker is RELEASED afterwards, so it does not pin the volume forever.
	if ids := m.inFlightWriteIDs(); len(ids) != 0 {
		t.Errorf("the in-flight marker survived the write: %v", ids)
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

// blockingReader yields one byte, then blocks until its gate closes, then reports EOF.
// It holds an import open for as long as the test needs without any sleeping.
type blockingReader struct {
	gate  chan struct{}
	wrote bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
		p[0] = 'x'
		return 1, nil
	}
	<-r.gate
	// io.EOF, not a look-alike: ImportVolume tells "the source ended" from "the source
	// failed", and only the real sentinel means the former.
	return 0, io.EOF
}
