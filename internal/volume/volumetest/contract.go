// Package volumetest holds the volume.Backend CONTRACT test suite — one set of
// assertions EVERY arm must pass (§1.3 Behavior seam / volume.md D2).
//
// It exists because the seam has exactly one falsification instrument: a second
// backend. The local storage arm and the test-only plain-directory arm both call
// RunContract, so "both arms run the same assertions" is literally true — and the
// shape rule the seam is built on ("allocate / set the cap / place are three
// actions") was derived by watching a merged shape fail here in three places
// rather than by reasoning about it.
//
// It lives in its own package (not volume_test) so a substrate-tagged test file
// can import and run the same suite against the arm that needs real storage.
package volumetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"engine/internal/volume"
)

// Factory builds a fresh, empty backend for one subtest, plus the facts the suite
// needs to know about it.
//
// Caps says whether the arm can ENFORCE a capacity cap. It is not a
// capability-branch in the §4.4-C forbidden sense (the algorithm never asks a
// backend what it supports and changes its behavior): the CONTRACT itself is
// different for an arm without the capability — a non-zero cap must be REFUSED with
// volume.ErrUnsupported — and the suite asserts exactly that difference, from the
// arm's OWN Capabilities() report rather than from a flag the test author set.
type Factory func(t *testing.T) volume.Backend

// RunContract runs the full shared suite against the backend built by mk.
func RunContract(t *testing.T, mk Factory) {
	t.Run("CreateInspectDestroy", func(t *testing.T) { testCreateInspectDestroy(t, mk) })
	t.Run("DestroyIdempotent", func(t *testing.T) { testDestroyIdempotent(t, mk) })

	// The three-actions shape rule (volume.md D2): allocate / set the cap / place
	// are separate calls, and each one has to work on its own.
	t.Run("CapacityIsItsOwnWritePoint", func(t *testing.T) { testCapacityWritePoint(t, mk) })
	t.Run("CapabilityNegativeIsTypedAndClean", func(t *testing.T) { testCapabilityNegative(t, mk) })

	// Materialize / Release / Adopt (§5.C): create-if-absent, concurrent, and a
	// Release that keeps the bytes.
	t.Run("MaterializeIsCreateIfAbsent", func(t *testing.T) { testMaterializeCreateIfAbsent(t, mk) })
	t.Run("MaterializeIsConcurrent", func(t *testing.T) { testMaterializeConcurrent(t, mk) })
	t.Run("ReleaseKeepsBytesAndIsIdempotent", func(t *testing.T) { testReleaseKeepsBytes(t, mk) })
	t.Run("AdoptRequiresAPresentUnit", func(t *testing.T) { testAdopt(t, mk) })

	// OpenRoot (volume.md D5.1): the content accessor, and its root confinement.
	t.Run("RootReadWriteRoundTrip", func(t *testing.T) { testRootRoundTrip(t, mk) })
	t.Run("RootListStatRemoveRename", func(t *testing.T) { testRootTreeOps(t, mk) })
	t.Run("RootConfinesEscapingPaths", func(t *testing.T) { testRootConfinement(t, mk) })
	t.Run("RootConfinesEscapingSymlinks", func(t *testing.T) { testRootSymlinkConfinement(t, mk) })

	// §3.4 accounting: the second term, and the sweep that keeps it honest.
	t.Run("UncappedBytesGrowsWithWrites", func(t *testing.T) { testUncappedBytesGrows(t, mk) })
	t.Run("SweepReclaimsOnlyOrphans", func(t *testing.T) { testSweep(t, mk) })
	t.Run("SweepSkipsBuilding", func(t *testing.T) { testSweepBuilding(t, mk) })
	t.Run("UsageReportsWrittenBytes", func(t *testing.T) { testUsage(t, mk) })
}

func ctx() context.Context { return context.Background() }

// capable reports whether the arm under test enforces caps, read from the arm's own
// Capabilities() rather than from a test-side flag — so the suite cannot disagree
// with the backend about what it can do.
func capable(b volume.Backend) bool { return b.Capabilities().Quota }

func testCreateInspectDestroy(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_a", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A created volume is immediately usable — existence IS the state (§4.3), so
	// there is no ready flag to wait for.
	r, err := b.OpenRoot(ctx(), "vol_a")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	if _, err := r.Stat("."); err != nil {
		t.Errorf("stat root of a just-created volume: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("close root: %v", err)
	}
	if err := b.DestroyVolume(ctx(), "vol_a"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// After destroy the bytes are gone: OpenRoot must fail rather than silently
	// resurrect an empty volume (that resurrection is Materialize's job, and only
	// Materialize's — see testMaterializeCreateIfAbsent).
	if _, err := b.OpenRoot(ctx(), "vol_a"); err == nil {
		t.Errorf("OpenRoot succeeded on a destroyed volume — the accessor must not create")
	}
}

// testDestroyIdempotent: §4.4-A reaches DestroyVolume from delete, unwind and the
// sweep, so it must never fail because someone else already cleaned up.
func testDestroyIdempotent(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_i", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.DestroyVolume(ctx(), "vol_i"); err != nil {
			t.Fatalf("destroy #%d: %v", i+1, err)
		}
	}
	// Never created at all is the same verdict.
	if err := b.DestroyVolume(ctx(), "vol_never"); err != nil {
		t.Errorf("destroy of a volume that never existed: %v", err)
	}
}

// testCapacityWritePoint is the shape rule made executable (volume.md D2): the cap
// is settable at create AND editable afterwards through its OWN method, WITHOUT the
// volume being materialized anywhere.
//
// That last clause is the load-bearing one. It is why capacity cannot ride a
// placement entry point: a volume has no placement of its own, so "re-materialize
// at the next placement" means NEVER for an unmounted volume, and an unmounted
// volume's cap must still be editable (§5.C).
func testCapacityWritePoint(t *testing.T, mk Factory) {
	b := mk(t)
	if !capable(b) {
		// The uncapped arm still has to accept the ZERO cap through the same method —
		// "no cap" is a legal state, not an unsupported operation.
		if err := b.CreateVolume(ctx(), "vol_c", 0); err != nil {
			t.Fatalf("create uncapped: %v", err)
		}
		if err := b.SetCapacity(ctx(), "vol_c", 0); err != nil {
			t.Errorf("SetCapacity(0) on an uncapped arm must be accepted: %v", err)
		}
		return
	}
	const cap1 = 32 << 20
	if err := b.CreateVolume(ctx(), "vol_c", cap1); err != nil {
		t.Fatalf("create capped: %v", err)
	}
	_, limit, err := b.Usage(ctx(), "vol_c")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if limit != cap1 {
		t.Errorf("cap after create = %d, want %d — a cap set at create must reach the storage", limit, cap1)
	}
	// Raise it through SetCapacity alone, with nothing mounted.
	const cap2 = 64 << 20
	if err := b.SetCapacity(ctx(), "vol_c", cap2); err != nil {
		t.Fatalf("set capacity: %v", err)
	}
	if _, limit, err = b.Usage(ctx(), "vol_c"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if limit != cap2 {
		t.Errorf("cap after SetCapacity = %d, want %d", limit, cap2)
	}
	// 0 removes it — the record is the authority, so an edit back to "uncapped" must
	// actually clear the storage's cap (§3.4).
	if err := b.SetCapacity(ctx(), "vol_c", 0); err != nil {
		t.Fatalf("clear capacity: %v", err)
	}
	if _, limit, err = b.Usage(ctx(), "vol_c"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if limit != 0 {
		t.Errorf("cap after clearing = %d, want 0", limit)
	}
}

// testCapabilityNegative: an arm that does NOT report a capability must refuse with
// a TYPED error and leave NO SIDE EFFECT (§6.2 maps it to 400).
//
// The no-side-effect half is the point, and it is why the check is here rather than
// at the HTTP boundary: an external implementation maps "unsupported" at its edge
// while the error is born deep inside, so the 400 arrives only AFTER bytes have
// moved — a window whose residue no sweep can classify (volume.md D2).
func testCapabilityNegative(t *testing.T, mk Factory) {
	b := mk(t)
	if capable(b) {
		t.Skip("this arm reports Quota capability; the negative case is asserted on the arm that lacks it")
	}
	err := b.CreateVolume(ctx(), "vol_n", 1<<20)
	if !errors.Is(err, volume.ErrUnsupported) {
		t.Fatalf("CreateVolume with a cap on a quota-less arm = %v, want volume.ErrUnsupported", err)
	}
	// AND no side effect: the refused create must not have left a volume behind.
	if _, err := b.OpenRoot(ctx(), "vol_n"); err == nil {
		t.Errorf("a refused capped create left the volume materialized — the refusal must precede any allocation")
	}
	// Same on the edit path.
	if err := b.CreateVolume(ctx(), "vol_n2", 0); err != nil {
		t.Fatalf("create uncapped: %v", err)
	}
	if err := b.SetCapacity(ctx(), "vol_n2", 1<<20); !errors.Is(err, volume.ErrUnsupported) {
		t.Errorf("SetCapacity on a quota-less arm = %v, want volume.ErrUnsupported", err)
	}
}

// testMaterializeCreateIfAbsent is §5.C's create-if-absent contract, and the reason
// it is a contract rather than an implementation detail: runsc hard-fails on a
// missing bind source, so an open-only Materialize would make a MIS-SWEPT sandbox
// PERMANENTLY unresumable. Create-if-absent restarts it clean — all data lost, but
// recoverable, which is the recovery conclusion volume.md D7.2 depends on.
func testMaterializeCreateIfAbsent(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_m", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the mis-sweep: the unit is gone while the record (not modelled here)
	// still names it.
	if err := b.DestroyVolume(ctx(), "vol_m"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	m, err := b.Materialize(ctx(), "vol_m")
	if err != nil {
		t.Fatalf("Materialize after the unit was removed: %v — an open-only contract would make this sandbox permanently unresumable (§5.C)", err)
	}
	if m.Volume != "vol_m" {
		t.Errorf("Materialized.Volume = %q, want vol_m — Release cannot resolve a handle without it", m.Volume)
	}
	if m.SourcePath == "" {
		t.Errorf("Materialized.SourcePath is empty — local_runsc has nothing to bind")
	}
	// And it is usable, not just present.
	r, err := b.OpenRoot(ctx(), "vol_m")
	if err != nil {
		t.Fatalf("open root after re-materialize: %v", err)
	}
	defer r.Close()
	if _, err := r.WriteAt("f", []byte("x"), 0); err != nil {
		t.Errorf("write into a re-materialized volume: %v", err)
	}
}

// testMaterializeConcurrent: the same volume must materialize CONCURRENTLY any
// number of times. The whole shared-fanout design rests on this (measured: two
// sandboxes rw-binding one source both succeed and the second is genuinely
// writable) — without it the AccessMode enum collapses to Exclusive alone.
func testMaterializeConcurrent(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_p", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 8
	errs := make(chan error, n)
	paths := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			m, err := b.Materialize(ctx(), "vol_p")
			if err != nil {
				errs <- err
				return
			}
			errs <- nil
			paths <- m.SourcePath
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Materialize: %v", err)
		}
	}
	// All materializations name the SAME source: a volume is ONE real thing, never
	// forked into per-mount copies (§5.C: resume re-mounts the same volume, it does
	// not restore a copy).
	first := <-paths
	for i := 1; i < n; i++ {
		if got := <-paths; got != first {
			t.Fatalf("concurrent materializations disagree on the source (%q vs %q) — a volume must be one real thing, not a copy per mount", first, got)
		}
	}
}

// testReleaseKeepsBytes pins the PROMISE Release makes, which is why it is not
// called Unmount: this materialization's source is no longer valid, THE BYTES
// SURVIVE, idempotent, tolerant of an already-gone target. A local no-op satisfies
// all four; the smell would be one name with two undefined contracts.
func testReleaseKeepsBytes(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_r", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_r")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	if _, err := r.WriteAt("keep", []byte("payload"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	r.Close()

	m, err := b.Materialize(ctx(), "vol_r")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Release(ctx(), m); err != nil {
			t.Fatalf("release #%d: %v", i+1, err)
		}
	}
	// The bytes are still there — Release ends a materialization, it does not reclaim.
	r2, err := b.OpenRoot(ctx(), "vol_r")
	if err != nil {
		t.Fatalf("open root after release: %v", err)
	}
	defer r2.Close()
	buf := make([]byte, 7)
	if _, err := r2.ReadAt("keep", buf, 0); err != nil && err != io.EOF {
		t.Fatalf("read after release: %v", err)
	}
	if string(buf) != "payload" {
		t.Errorf("bytes after Release = %q, want %q — Release must not reclaim", buf, "payload")
	}
	// Tolerates an already-gone target.
	if err := b.DestroyVolume(ctx(), "vol_r"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := b.Release(ctx(), m); err != nil {
		t.Errorf("Release after the volume was destroyed: %v — the contract tolerates a gone target", err)
	}
}

// testAdopt: AdoptMaterialized re-recognizes a PRESENT unit and refuses an absent
// one. The refusal is the useful half — it is what lets the mediator tell "recover
// the handle" from "materialize afresh" (§4.4-A: a provider must not collapse
// "uncertain" into a definite answer).
func testAdopt(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_ad", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	m, err := b.AdoptMaterialized(ctx(), "vol_ad")
	if err != nil {
		t.Fatalf("adopt a present unit: %v", err)
	}
	if m.Volume != "vol_ad" || m.SourcePath == "" {
		t.Errorf("adopted handle = %+v, want Volume set and a non-empty source", m)
	}
	if err := b.DestroyVolume(ctx(), "vol_ad"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := b.AdoptMaterialized(ctx(), "vol_ad"); err == nil {
		t.Errorf("adopt succeeded on an absent unit — Adopt must not create (that is Materialize's contract)")
	}
}

func testRootRoundTrip(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_rt", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_rt")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	payload := bytes.Repeat([]byte("abcd"), 4096) // 16 KiB, several chunks
	// Written in chunks at increasing offsets: this is exactly how import streams,
	// so the round trip proves the streaming shape and not just a single write.
	const chunk = 4096
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		n, err := r.WriteAt("d/f", payload[off:end], int64(off))
		if off == 0 && err != nil {
			// The parent does not exist yet: WriteAt does NOT create parents (stated in
			// the Root contract), so the caller must Mkdir first. Assert that shape
			// rather than silently depending on it.
			if err := r.Mkdir("d", 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			n, err = r.WriteAt("d/f", payload[off:end], int64(off))
		}
		if err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
		if n != end-off {
			t.Fatalf("short write at %d: %d of %d", off, n, end-off)
		}
	}
	got := make([]byte, 0, len(payload))
	buf := make([]byte, chunk)
	var off int64
	for {
		n, err := r.ReadAt("d/f", buf, off)
		got = append(got, buf[:n]...)
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip mismatch: %d bytes back, %d written", len(got), len(payload))
	}
	fi, err := r.Stat("d/f")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size != int64(len(payload)) {
		t.Errorf("stat size = %d, want %d", fi.Size, len(payload))
	}
	if fi.IsDir {
		t.Errorf("stat reports a file as a directory")
	}
}

func testRootTreeOps(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_tr", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_tr")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	if err := r.Mkdir("dir", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"dir/a", "dir/b"} {
		if _, err := r.WriteAt(name, []byte(name), 0); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ents, err := r.List("dir")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("list returned %d entries, want 2: %+v", len(ents), ents)
	}
	// Entry names are BASE names, never host paths: above the seam a volume is
	// addressed by ID and its contents relative to its root (§6.2).
	for _, e := range ents {
		if strings.Contains(e.Name, "/") {
			t.Errorf("list entry %q carries a path separator — entries must be base names", e.Name)
		}
	}
	if err := r.Rename("dir/a", "dir/c"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := r.Stat("dir/a"); err == nil {
		t.Errorf("stat of the renamed-away name succeeded")
	}
	if _, err := r.Stat("dir/c"); err != nil {
		t.Errorf("stat of the rename target: %v", err)
	}
	if err := r.Remove("dir/c"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Idempotent, like every other reclamation path in the engine.
	if err := r.Remove("dir/c"); err != nil {
		t.Errorf("second remove: %v — Remove of an absent path must be a no-op", err)
	}
	if err := r.Remove("dir/b"); err != nil {
		t.Fatalf("remove b: %v", err)
	}
	if err := r.Remove("dir"); err != nil {
		t.Errorf("remove empty dir: %v", err)
	}
	// The root itself is not removable through this accessor: it is the volume, and
	// reclaiming a volume is DestroyVolume's job.
	if err := r.Remove("."); err == nil {
		t.Errorf("Remove(\".\") succeeded — the accessor must not be able to delete the volume root")
	}
}

// testRootConfinement is the accessor's SECURITY boundary, and it is a pure unit
// test on purpose (a directory, no runsc — part of why this mechanism is cheap).
//
// FAILURE HERE IS SILENT: an escaping write SUCCEEDS and lands outside the volume.
// So the properties are asserted, never inferred from a flag being set.
func testRootConfinement(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_esc", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_esc")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	// A canary OUTSIDE any volume, whose bytes must be untouched afterwards. This is
	// the assertion that actually matters: "the write failed" is not the property —
	// "the host file did not change" is.
	outside := filepath.Join(t.TempDir(), "canary")
	const canary = "host-owned bytes"
	if err := os.WriteFile(outside, []byte(canary), 0o644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}

	// The parent of the escape target is created INSIDE the volume first, so the
	// escaping write can actually succeed. This matters: without it the write fails
	// with ENOENT (WriteAt does not create parents) and the test would only prove
	// "the write was refused" — a much weaker property that a broken accessor
	// returning an error for everything would also satisfy. The measured spec
	// property is that ".." CLAMPS at the root and the write LANDS INSIDE, so the
	// assertion has to be able to observe the landing.
	if err := r.Mkdir("etc", 0o755); err != nil {
		t.Fatalf("mkdir etc inside the volume: %v", err)
	}

	// ".." clamps at the root (measured: it resolves to the root itself, it is not an
	// error), so this write must land INSIDE the volume.
	const escaping = "../../../../../../etc/passwd"
	if _, err := r.WriteAt(escaping, []byte("owned"), 0); err != nil {
		t.Fatalf("escaping write returned %v — expected it to CLAMP into the volume and succeed, which is the measured RESOLVE_IN_ROOT property", err)
	}
	// An ABSOLUTE path is reinterpreted against the root, so this one lands inside too.
	if _, err := r.WriteAt("/etc/hosts", []byte("owned"), 0); err != nil {
		t.Fatalf("absolute write returned %v — expected it to be reinterpreted against the volume root", err)
	}

	// The positive half: both writes are now inside the volume.
	for _, inside := range []string{"etc/passwd", "etc/hosts"} {
		fi, err := r.Stat(inside)
		if err != nil {
			t.Errorf("the clamped write is not visible at %s inside the volume: %v", inside, err)
			continue
		}
		if fi.Size != int64(len("owned")) {
			t.Errorf("%s inside the volume is %d bytes, want %d", inside, fi.Size, len("owned"))
		}
	}
	// The negative half, and the one that would be a security defect: nothing outside
	// moved. Checked against the host, not through the accessor.
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if string(got) != canary {
		t.Fatalf("CONFINEMENT BREACH: the canary outside the volume changed to %q", got)
	}
	if hostEtc, err := os.ReadFile("/etc/hosts"); err == nil && string(hostEtc) == "owned" {
		t.Fatalf("CONFINEMENT BREACH: the host's /etc/hosts was overwritten through the accessor")
	}
	// Reading a host file through a traversal must not work either.
	buf := make([]byte, len(canary))
	if _, err := r.ReadAt(outside, buf, 0); err == nil && string(buf) == canary {
		t.Fatalf("CONFINEMENT BREACH: read an absolute host path through the accessor")
	}
}

// testRootSymlinkConfinement: a symlink aimed OUT of the volume resolves BACK INSIDE
// it (measured, both relative and absolute forms). Written through the accessor
// itself where possible, and otherwise through the host — the point is that the
// accessor cannot be steered by a link, whoever created it.
func testRootSymlinkConfinement(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_sym", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	m, err := b.Materialize(ctx(), "vol_sym")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The link has to be planted with a host symlink() — Root deliberately offers no
	// Symlink method (nothing in §5.C needs one), so the hostile artifact is created
	// out of band, which is also the realistic case: a guest with a rw mount can
	// create it.
	outside := filepath.Join(t.TempDir(), "canary")
	const canary = "host-owned bytes"
	if err := os.WriteFile(outside, []byte(canary), 0o644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(m.SourcePath, "abs-link")); err != nil {
		t.Fatalf("plant absolute symlink: %v", err)
	}
	if err := os.Symlink("../../../../../../../etc/passwd", filepath.Join(m.SourcePath, "rel-link")); err != nil {
		t.Fatalf("plant relative symlink: %v", err)
	}

	r, err := b.OpenRoot(ctx(), "vol_sym")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	// Mirror the canary's directory chain INSIDE the volume. The absolute symlink's
	// target is reinterpreted against the volume root, so it resolves to
	// <volume>/<that same chain>/canary — and without the parents present the write
	// would fail with ENOENT and prove only "it was refused", the weak property a
	// uniformly broken accessor also satisfies. With them present, the write SUCCEEDS
	// and its landing spot is observable, which is the actual measured property.
	var chain string
	for _, seg := range strings.Split(strings.Trim(filepath.Dir(outside), "/"), "/") {
		chain = path.Join(chain, seg)
		if err := r.Mkdir(chain, 0o755); err != nil {
			t.Fatalf("mirror the canary's parent chain inside the volume (%s): %v", chain, err)
		}
	}

	// The absolute link's target resolves back inside the volume, so the write lands
	// on a path INSIDE — measured, and the whole point. The relative link's clamped
	// target has no parent inside the volume, so it may legitimately fail with ENOENT;
	// either outcome is fine there as long as the host file is untouched, which is
	// what the canary check below decides.
	if _, err := r.WriteAt("abs-link", []byte("owned"), 0); err != nil {
		t.Fatalf("write through an absolute out-of-volume symlink returned %v — expected it to resolve back INSIDE the volume and succeed", err)
	}
	// The bytes landed at the CLAMPED path inside the volume, not at the host target.
	insideTwin := path.Join(chain, filepath.Base(outside))
	if fi, err := r.Stat(insideTwin); err != nil {
		t.Errorf("the symlink write is not visible at the clamped path %s inside the volume: %v", insideTwin, err)
	} else if fi.Size != int64(len("owned")) {
		t.Errorf("%s inside the volume is %d bytes, want %d", insideTwin, fi.Size, len("owned"))
	}
	if _, err := r.WriteAt("rel-link", []byte("owned"), 0); err != nil {
		t.Logf("write through the relative escaping symlink refused (its clamped parent does not exist inside the volume): %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if string(got) != canary {
		t.Fatalf("CONFINEMENT BREACH: a write through a symlink reached the host file (now %q)", got)
	}
	// Reading through the link must not surface host bytes either.
	buf := make([]byte, len(canary))
	if _, err := r.ReadAt("abs-link", buf, 0); err == nil && string(buf) == canary {
		t.Fatalf("CONFINEMENT BREACH: read host bytes through a symlink in the volume")
	}
}

// testUncappedBytesGrows proves the §3.4 second term is REAL and not decoration:
// bytes written into an UNCAPPED volume must become visible to the accounting term,
// because nothing else can see them (a pile of uncapped volumes could otherwise
// fill a node without the admission gate refusing once).
//
// It is also the assertion that FAILS on a seam missing the method at all — which
// is the proof that the method is load-bearing rather than ornamental.
func testUncappedBytesGrows(t *testing.T, mk Factory) {
	b := mk(t)
	before, err := b.UncappedBytes(ctx())
	if err != nil {
		t.Fatalf("uncapped bytes (empty): %v", err)
	}
	if err := b.CreateVolume(ctx(), "vol_u", 0); err != nil {
		t.Fatalf("create uncapped: %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_u")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	// 4 MiB: comfortably above any block-rounding noise.
	const size = 4 << 20
	if _, err := r.WriteAt("blob", bytes.Repeat([]byte("z"), size), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	r.Close()

	after, err := b.UncappedBytes(ctx())
	if err != nil {
		t.Fatalf("uncapped bytes (written): %v", err)
	}
	if after-before < size/2 {
		t.Errorf("UncappedBytes grew by %d after writing %d bytes into an UNCAPPED volume — those bytes are invisible to the §3.4 admission gate", after-before, size)
	}

	if !capable(b) {
		return
	}
	// The disjointness invariant, on the arm that can express it: once the volume
	// carries a declared cap its bytes must contribute ZERO here, because the
	// mediator already counts the cap (charging both bills the same bytes twice).
	if err := b.SetCapacity(ctx(), "vol_u", 64<<20); err != nil {
		t.Fatalf("set capacity: %v", err)
	}
	capped, err := b.UncappedBytes(ctx())
	if err != nil {
		t.Fatalf("uncapped bytes (capped): %v", err)
	}
	if capped > before+size/2 {
		t.Errorf("a CAPPED volume still contributes %d to UncappedBytes (was %d uncapped) — the cap term already counts it (§3.4 disjointness)", capped, after)
	}
}

// testSweep is the positive-AND-negative pair, together in one call. A sweep test
// that only checks "it did not delete the wrong thing" passes trivially for a sweep
// that does NOTHING, so the same pass must also reclaim a real orphan.
func testSweep(t *testing.T, mk Factory) {
	b := mk(t)
	for _, id := range []string{"vol_live", "vol_orphan"} {
		if err := b.CreateVolume(ctx(), id, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	n, err := b.SweepOrphanVolumes(ctx(), []string{"vol_live"}, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("sweep reclaimed %d, want exactly 1 (the orphan) — a sweep that reclaims nothing also passes a negative-only assertion", n)
	}
	if _, err := b.OpenRoot(ctx(), "vol_live"); err != nil {
		t.Errorf("the LIVE volume was reclaimed: %v", err)
	}
	if _, err := b.OpenRoot(ctx(), "vol_orphan"); err == nil {
		t.Errorf("the orphan volume survived the sweep")
	}
}

// testSweepBuilding: `building` protects an in-flight create. It matters MORE here
// than on the Overlay side — volume creation is BYTES-FIRST (the ID must name the
// unit before any row exists), so a perfectly healthy in-flight create presents as
// "bytes, no row" even with a completely fresh keep-set.
func testSweepBuilding(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_building", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := b.SweepOrphanVolumes(ctx(), nil, []string{"vol_building"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("sweep reclaimed %d units while one was in flight, want 0", n)
	}
	if _, err := b.OpenRoot(ctx(), "vol_building"); err != nil {
		t.Errorf("an in-flight create was swept out from under itself: %v", err)
	}
}

// testUsage: the `used` half must reflect bytes actually written, because the cap
// write point's refusal ("lower below current usage") is a STRICT inequality
// evaluated against it (§5.C / volume.md D7.3).
func testUsage(t *testing.T, mk Factory) {
	b := mk(t)
	if err := b.CreateVolume(ctx(), "vol_us", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	used0, _, err := b.Usage(ctx(), "vol_us")
	if err != nil {
		t.Fatalf("usage (empty): %v", err)
	}
	r, err := b.OpenRoot(ctx(), "vol_us")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	const size = 4 << 20
	if _, err := r.WriteAt("blob", bytes.Repeat([]byte("q"), size), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	r.Close()
	used1, _, err := b.Usage(ctx(), "vol_us")
	if err != nil {
		t.Fatalf("usage (written): %v", err)
	}
	if used1-used0 < size/2 {
		t.Errorf("Usage grew by %d after writing %d bytes — the cap write point's strict-inequality check reads this number", used1-used0, size)
	}
}
