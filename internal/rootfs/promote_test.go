// PromoteToTemplate is a plain host-side copy (no mount, no root), so it runs in
// the default unit suite. It freezes a sandbox's at-rest upper into a new
// read-only layer dir, EXCLUDING the engine-injected runtime paths (§5.B / §3.1;
// rootfs-overlay D5).
package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"engine/internal/guestport"
)

func newProvider(t *testing.T) (*HostOverlayFS, string) {
	t.Helper()
	base := t.TempDir()
	p, err := NewHostOverlayFS(filepath.Join(base, "state"), filepath.Join(base, "tmpl"), filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("NewHostOverlayFS: %v", err)
	}
	return p, base
}

// writeUpper plants files into a sandbox's upper dir (the at-rest tree Promote
// reads). Returns the overlay id.
func writeUpper(t *testing.T, p *HostOverlayFS, id string, files map[string]string) {
	t.Helper()
	upper := p.upperPath(id)
	for rel, content := range files {
		full := filepath.Join(upper, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// §5.B / §3.1: Promote copies the upper into a new layer dir byte-for-byte, but
// EXCLUDES the engine-injected runtime debris (rootfs-overlay D5). bytes-first:
// the layer exists on disk after Promote, ready for the row insert.
//
// THE LOAD-BEARING HALF IS THE UPLOAD DEBRIS, not the static tmp/ paths. D5 records
// why, and it is worth stating here so this test is not read as stronger than it is:
//
//   - the two `tmp/.sandboxd-*` portal files CANNOT REACH THE UPPER at all. /tmp is
//     mounted as tmpfs by the OCI spec (internal/runtime/oci.go), so the guest daemon's
//     writes there never land in the upper's `tmp/`. The cases below plant them by
//     hand, which means they exercise the matching logic against a state the substrate
//     cannot produce. They stay because the list entries are harmless and cheap
//     insurance (one line beats minting a Template whose every sandbox is born unable
//     to serve), but they prove nothing about a real leak — do not treat them as the
//     reason the exclusion mechanism exists.
//   - the streaming upload's temp file is the REAL consumer: it must be created in the
//     caller's own destination directory, because rename(2) cannot cross filesystems
//     and /tmp is that separate tmpfs (cmd/sandboxd/files.go). So it lands in the
//     upper, anywhere the caller writes, and a crash mid-upload strands one. That is
//     the case with a live failure mode, and it is matched by PREFIX
//     (guestport.UploadTempPrefix) because the exact path is unknowable in advance.
func TestPromoteToTemplateExcludesRuntimePaths(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)

	writeUpper(t, p, "ov_src", map[string]string{
		"home/work/app.py":  "print('hello')",
		"etc/installed-pkg": "PKG",
		// Planted by hand; unreachable on the real substrate (/tmp is tmpfs). Matching
		// logic only — see the doc comment.
		"tmp/.sandboxd-transport.pid": "12345",
		"tmp/.sandboxd-crashloop":     "x",
		"tmp/user-scratch":            "keepme", // NOT excluded (only the sandboxd paths are)
		// Upload debris, excluded BY PREFIX rather than by exact path. A streaming
		// upload's temp file must be created in the caller's own destination
		// directory (rename(2) cannot cross filesystems), so a crash mid-upload can
		// strand one ANYWHERE the caller writes — two different directories here, to
		// pin that the rule is not tied to a location.
		"home/work/" + guestport.UploadTempPrefix + "a1b2": "partial upload",
		"etc/" + guestport.UploadTempPrefix + "deadbeef":   "partial upload",
		// NEGATIVE CONTROLS for the prefix rule, which is what makes the two lines
		// above falsifiable rather than satisfiable by a blunter implementation.
		// Without these, "exclude every dotfile" or "exclude the whole directory that
		// held debris" would both pass — and either would silently discard real guest
		// data (a `.env`, a `.git`, the user's own work beside a stranded upload).
		"home/work/.env":         "SECRET=1", // a dotfile, NOT debris
		"etc/keep-beside-debris": "REAL",     // shares a directory with debris
		"home/work/notes-" + guestport.UploadTempPrefix + "x": "REAL", // prefix mid-name, not at the start
	})

	ov := &Overlay{ID: "ov_src", Template: "bare", Lower: []string{"bare"}}
	if err := p.PromoteToTemplate(ctx, ov, "tmpl_new", nil); err != nil {
		t.Fatalf("PromoteToTemplate: %v", err)
	}

	layer := filepath.Join(p.LayersDir, "tmpl_new")
	// captured content present + byte-identical.
	if b, err := os.ReadFile(filepath.Join(layer, "home/work/app.py")); err != nil || string(b) != "print('hello')" {
		t.Fatalf("app.py not captured byte-identical: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(layer, "etc/installed-pkg")); err != nil || string(b) != "PKG" {
		t.Fatalf("installed-pkg not captured: %q %v", b, err)
	}
	// non-excluded tmp file survives.
	if _, err := os.Stat(filepath.Join(layer, "tmp/user-scratch")); err != nil {
		t.Errorf("non-excluded tmp file was dropped: %v", err)
	}
	// excluded sandboxd paths must NOT be in the layer (§5.B / D5).
	if _, err := os.Stat(filepath.Join(layer, "tmp/.sandboxd-transport.pid")); !os.IsNotExist(err) {
		t.Errorf("sandboxd pidfile leaked into layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer, "tmp/.sandboxd-crashloop")); !os.IsNotExist(err) {
		t.Errorf("sandboxd crashloop sentinel leaked into layer: %v", err)
	}
	// Upload debris is excluded BY PREFIX, wherever it was stranded. Freezing it
	// would ship one caller's interrupted upload to every sandbox created from this
	// Template.
	for _, debris := range []string{
		"home/work/" + guestport.UploadTempPrefix + "a1b2",
		"etc/" + guestport.UploadTempPrefix + "deadbeef",
	} {
		if _, err := os.Stat(filepath.Join(layer, debris)); !os.IsNotExist(err) {
			t.Errorf("upload temp file %s leaked into layer: %v (a crash mid-upload strands one anywhere the caller writes; freezing it ships one caller's interrupted upload to every sandbox created from this Template)", debris, err)
		}
	}
	// The negative half of the prefix rule: it matches a BASE NAME that STARTS WITH
	// the prefix, and nothing wider. Each of these would be swept up by a plausible
	// over-broad implementation, and each is real guest data whose loss would be
	// silent — the Template would simply be missing a file nobody notices until a
	// sandbox built from it fails.
	for _, kept := range []string{
		"home/work/.env",
		"etc/keep-beside-debris",
		"home/work/notes-" + guestport.UploadTempPrefix + "x",
	} {
		if _, err := os.Stat(filepath.Join(layer, kept)); err != nil {
			t.Errorf("%s was excluded: %v — the upload-debris rule must match a base name STARTING WITH the prefix, not every dotfile, not a whole directory that happened to hold debris, and not a mid-name occurrence", kept, err)
		}
	}
	// no leftover staging dir.
	if _, err := os.Stat(layer + ".staging"); !os.IsNotExist(err) {
		t.Errorf("staging dir not cleaned up: %v", err)
	}
}

// A promoted layer is then usable as a lower: a sandbox created over
// [tmpl_new] mounts it (multi-lowerdir needs a real dir). We assert the layer
// dir resolves via layerPath (the mount itself is substrate-only).
func TestPromotedLayerResolvesAsLower(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	writeUpper(t, p, "ov_src", map[string]string{"etc/x": "1"})
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_new", nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	lp, err := p.layerPath("tmpl_new")
	if err != nil {
		t.Fatalf("promoted layer not resolvable: %v", err)
	}
	if lp != filepath.Join(p.LayersDir, "tmpl_new") {
		t.Fatalf("layerPath = %s; want under LayersDir", lp)
	}
}

// PromoteCostBytes is the forecast the §3.4 admission gate runs a Promote against
// (rootfs-overlay D6.5: a frozen layer is charged to no sandbox cap, so it grows the
// measured term). The property that matters is the DIRECTION of its error: it must
// never under-report what its own copy will cost, or the gate's bound points the
// wrong way and the overshoot lands on an unrelated sandbox as ENOSPC (§2.3).
//
// The sharp case is a SPARSE file. blockBytes (used for bytes already on disk)
// reports allocated blocks, which for a hole-punched file is ~0 — but copyFile is an
// io.Copy and does not punch holes, so the layer pays the file's full length. The
// forecast must therefore project apparent size, and this test pins that by
// comparing the forecast against what the freeze ACTUALLY costs.
func TestPromoteCostBytesDoesNotUnderReportASparseUpper(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)

	// a 4 MiB sparse file: apparent size 4 MiB, allocated blocks ~0.
	const sparseLen = 4 << 20
	upper := p.upperPath("ov_sparse")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(upper, "sparse.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(sparseLen); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ov := &Overlay{ID: "ov_sparse", Template: "bare", Lower: []string{"bare"}}
	forecast, err := p.PromoteCostBytes(ctx, ov, nil)
	if err != nil {
		t.Fatalf("PromoteCostBytes: %v", err)
	}
	// The allocated measurement, for contrast: this is what a naive forecast would
	// have said, and it is the number that would have let the node overcommit.
	allocated, err := blockBytes(upper)
	if err != nil {
		t.Fatal(err)
	}
	if forecast < sparseLen {
		t.Fatalf("forecast = %d for a %d-byte sparse file (allocated reads %d) — under-reporting is the one direction the gate cannot tolerate",
			forecast, sparseLen, allocated)
	}

	// and the freeze really does cost that: measure the layer afterwards.
	if err := p.PromoteToTemplate(ctx, ov, "tmpl_sparse", nil); err != nil {
		t.Fatalf("PromoteToTemplate: %v", err)
	}
	actual, err := blockBytes(filepath.Join(p.LayersDir, "tmpl_sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if actual > forecast {
		t.Fatalf("layer actually cost %d bytes but the forecast said %d — the gate would have admitted %d bytes it could not honor",
			actual, forecast, actual-forecast)
	}
}

// The forecast is an UPPER BOUND, not an exact count, and the two deliberate
// overstatements are the safe direction (seam contract): excluded paths are counted
// even though the freeze drops them. Asserted as "forecast ≥ what the layer cost",
// which is the only property the gate depends on.
func TestPromoteCostBytesBoundsTheRealCost(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	writeUpper(t, p, "ov_b", map[string]string{
		"home/work/app.py":            "print('hello')",
		"etc/installed-pkg":           "PKG",
		"tmp/.sandboxd-transport.pid": "12345", // excluded from the layer, counted here
	})
	ov := &Overlay{ID: "ov_b", Template: "bare", Lower: []string{"bare"}}
	forecast, err := p.PromoteCostBytes(ctx, ov, nil)
	if err != nil {
		t.Fatalf("PromoteCostBytes: %v", err)
	}
	if forecast <= 0 {
		t.Fatalf("forecast = %d for a non-empty upper; want > 0", forecast)
	}
	if err := p.PromoteToTemplate(ctx, ov, "tmpl_b", nil); err != nil {
		t.Fatalf("PromoteToTemplate: %v", err)
	}
	actual, err := blockBytes(filepath.Join(p.LayersDir, "tmpl_b"))
	if err != nil {
		t.Fatal(err)
	}
	if actual > forecast {
		t.Fatalf("layer cost %d > forecast %d: the forecast must bound the real cost", actual, forecast)
	}
}

// An Overlay with no upper on disk yet forecasts 0 rather than failing: the gate
// must be answerable for a sandbox that has written nothing, and a missing tree
// costs nothing to copy.
func TestPromoteCostBytesOfAnAbsentUpperIsZero(t *testing.T) {
	p, _ := newProvider(t)
	n, err := p.PromoteCostBytes(context.Background(), &Overlay{ID: "ov_missing", Lower: []string{"bare"}}, nil)
	if err != nil {
		t.Fatalf("PromoteCostBytes on an absent upper: %v", err)
	}
	if n != 0 {
		t.Fatalf("forecast = %d; want 0", n)
	}
}

// Promote refuses to overwrite an existing layer id (bytes-first + immutability:
// a layer dir is written once).
func TestPromoteRefusesExistingLayer(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	writeUpper(t, p, "ov_src", map[string]string{"etc/x": "1"})
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_dup", nil); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: "ov_src", Lower: []string{"bare"}}, "tmpl_dup", nil); err == nil {
		t.Fatal("second promote to same id should fail (layer already exists)")
	}
}

// TestPromotePrunesShadowedBytes is the property the per-Promote excluded set exists
// for (M7 acceptance 18 / rootfs-overlay D5), and it is NOT "the mount point becomes
// an empty placeholder" — that is the DEFAULT outcome and needs no exclusion at all.
//
// What the exclusions do is prune bytes that were written into the upper BEFORE a
// volume was bound over that path. Those bytes are invisible to every guest (the guest
// sees only the volume's entries), yet they are still on disk, still in the upper, and
// would otherwise be copied into the frozen layer in full AND counted in both the
// sandbox's capacity cap and the cost forecast.
func TestPromotePrunesShadowedBytes(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	const id = "ov_shadow"

	writeUpper(t, p, id, map[string]string{
		// Bytes UNDER a future mount destination: shadowed once the volume is bound,
		// so they must not enter the layer.
		"data/stale":         "SHADOWED-BY-THE-MOUNT",
		"data/nested/deeper": "ALSO-SHADOWED",
		// Real guest data OUTSIDE any mount point: must survive.
		"etc/app.conf": "REAL-GUEST-DATA",
		// A sibling of the mount destination sharing its PARENT: this is what makes the
		// "prune the leaf subtree, not a top-level component" rule falsifiable rather
		// than merely asserted. Excluding `srv` wholesale would discard it.
		"srv/keepme":       "SIBLING-OF-A-NESTED-DEST",
		"srv/vol/shadowed": "SHADOWED-UNDER-A-NESTED-DEST",
	})

	const newID = "tmpl_shadow"
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: id}, newID, []string{"/data", "/srv/vol"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	layer := filepath.Join(p.LayersDir, newID)

	for _, gone := range []string{"data/stale", "data/nested/deeper", "srv/vol/shadowed"} {
		if _, err := os.Stat(filepath.Join(layer, gone)); !os.IsNotExist(err) {
			t.Errorf("%s entered the frozen layer (stat err = %v) — bytes shadowed by a mount are invisible to every guest but would be copied in full and charged to the cap and the forecast", gone, err)
		}
	}
	for _, kept := range []string{"etc/app.conf", "srv/keepme"} {
		if _, err := os.Stat(filepath.Join(layer, kept)); err != nil {
			t.Errorf("%s was pruned from the layer: %v — only the LEAF subtree may be excluded; a top-level component would discard real guest data living beside the mount", kept, err)
		}
	}
	// What happens to the mount DESTINATION DIRECTORY itself.
	//
	// SPEC/CODE DIVERGENCE, recorded here deliberately rather than silently reconciled
	// (CLAUDE.md: a divergence is a signal to STOP, not to fix in passing).
	//
	//	§5.B says the mount point survives in a frozen layer as an EMPTY DIRECTORY
	//	PLACEHOLDER ("挂载点在固化结果里只是一个空目录占位"), by analogy with `tar` of a
	//	directory that has something mounted on it.
	//
	//	copyTree ACTUALLY PRUNES IT ENTIRELY. The exclusion check runs BEFORE the
	//	MkdirAll that would create the target, and returns filepath.SkipDir — so no
	//	directory is created at the destination and the whole subtree, dest included, is
	//	absent from the layer. Measured: stat(layer/data) → ENOENT.
	//
	// Neither side is obviously right (§5.B's placeholder is the default outcome for an
	// UNSHADOWED dest, which needs no exclusion at all — see D5 — so the two statements
	// may simply be about different cases), and this is not the change to settle it in.
	// Production behavior is therefore LEFT ALONE and the test pins what the code DOES.
	//
	// The previous assertion here was `if ents, err := os.ReadDir(dest); err == nil &&
	// len(ents) != 0` — which passed VACUOUSLY on the real behavior: the directory is
	// absent, so err != nil and the body never ran. It would have kept passing if the
	// dest had been left full of shadowed content, since that path also depends on the
	// ReadDir succeeding. Asserted unambiguously now.
	dest := filepath.Join(layer, "data")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		ents, _ := os.ReadDir(dest)
		t.Errorf("mount destination %s exists in the layer (stat err = %v, %d entries) — copyTree returns SkipDir BEFORE creating the target dir, so the dest is pruned entirely. If this now behaves like §5.B's empty-placeholder instead, that is the divergence above RESOLVING; settle it in the spec and update this assertion deliberately, do not just relax it",
			"data", err, len(ents))
	}
	// Either way, the property that actually matters holds: no SHADOWED CONTENT reaches
	// the layer. Asserted independently of the dest's own existence, so it survives
	// whichever way the divergence above is settled.
	if ents, err := os.ReadDir(dest); err == nil && len(ents) != 0 {
		t.Errorf("mount destination %s in the layer holds %d entries — bytes shadowed by a mount must never be frozen, whether or not the dest itself remains as a placeholder", dest, len(ents))
	}
}

// TestPromoteCostUsesTheSameExclusions: the forecast must model exactly what the
// freeze does. A forecast that counts shadowed bytes is conservative in DIRECTION but
// known WRONG in value, so it refuses Promotes against capacity that was never going
// to be consumed — which is why both calls take the same set (rootfs-overlay D5).
func TestPromoteCostUsesTheSameExclusions(t *testing.T) {
	ctx := context.Background()
	p, _ := newProvider(t)
	const id = "ov_cost"

	const shadowedSize = 512 << 10 // 512 KiB, well above block-rounding noise
	big := make([]byte, shadowedSize)
	upper := p.upperPath(id)
	if err := os.MkdirAll(filepath.Join(upper, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "data", "shadowed.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	writeUpper(t, p, id, map[string]string{"etc/keep": "REAL"})

	withAll, err := p.PromoteCostBytes(ctx, &Overlay{ID: id}, nil)
	if err != nil {
		t.Fatalf("cost without exclusions: %v", err)
	}
	withExcluded, err := p.PromoteCostBytes(ctx, &Overlay{ID: id}, []string{"/data"})
	if err != nil {
		t.Fatalf("cost with exclusions: %v", err)
	}
	if drop := withAll - withExcluded; drop < shadowedSize/2 {
		t.Errorf("excluding the mount destination dropped the forecast by only %d bytes (want ≥ %d) — the forecast still charges shadowed bytes the freeze will prune, so it refuses Promotes against capacity that was never going to be consumed",
			drop, shadowedSize/2)
	}

	// And the forecast must NOT under-report what the freeze actually costs: it is an
	// UPPER BOUND by contract, so compare it against the layer that gets written.
	const newID = "tmpl_cost"
	if err := p.PromoteToTemplate(ctx, &Overlay{ID: id}, newID, []string{"/data"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	actual, err := blockBytes(filepath.Join(p.LayersDir, newID))
	if err != nil {
		t.Fatal(err)
	}
	if withExcluded < actual {
		t.Errorf("forecast %d UNDER-reports the actual layer cost %d — the contract requires an upper bound, and the gate's bound would point the wrong way", withExcluded, actual)
	}
}
