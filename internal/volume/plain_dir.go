package volume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"engine/internal/allocbytes"
)

// PlainDir is the seam's SECOND ARM, and it is TEST-ONLY: nothing in the
// production assembly registers it (internal/orchestrator builds LocalBtrfs). It
// is here rather than in a _test.go file because it must live beside the arm it
// falsifies and share its accessor implementation, and because the contract suite
// that drives it lives in a sibling package.
//
// IT IS NOT FILLER — IT IS THE ONLY FALSIFICATION INSTRUMENT THIS SEAM HAS
// (volume.md D2). The shape rule "allocate / set the cap / place are three actions
// and must not be compressed into one call" was DERIVED from running a second
// backend against the same shared contract: a merged shape fails in three separate
// places, and with only one arm every one of those failures would have looked like
// a reasonable simplification.
//
// It is also the CAPABILITY-NEGATIVE arm: it reports no Snapshot, no Clone, no
// Quota, so the contract suite can assert that an unsupported operation is refused
// with a TYPED error AND NO SIDE EFFECT — the path §6.2 maps to 400. Without a
// backend that genuinely lacks a capability, that whole branch would only ever be
// exercised by a mock agreeing with itself.
type PlainDir struct {
	// Dir holds one plain directory per volume: Dir/<volumeID>.
	Dir string
}

// NewPlainDir constructs the test-only arm over dir.
func NewPlainDir(dir string) (*PlainDir, error) {
	if dir == "" {
		return nil, fmt.Errorf("volume: PlainDir.Dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("volume: create PlainDir dir: %w", err)
	}
	return &PlainDir{Dir: dir}, nil
}

func (p *PlainDir) Name() string { return "plain_dir" }

// Capabilities: none of the three. A plain directory can neither snapshot nor
// enforce a byte ceiling, and saying so honestly is this arm's job — §5.C requires
// an unsupported call to be a clean 400 rather than a silent no-op.
func (p *PlainDir) Capabilities() Capabilities { return Capabilities{} }

func (p *PlainDir) unitPath(volumeID string) string { return filepath.Join(p.Dir, volumeID) }

func (p *PlainDir) CreateVolume(_ context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	if capBytes > 0 {
		// The cap is REFUSED, and refused BEFORE the directory is created, so an
		// unsupported request leaves nothing behind. Accepting and ignoring it is the
		// silent failure §3.4 forbids.
		return fmt.Errorf("volume: plain_dir cannot enforce a %d-byte cap on %s: %w", capBytes, volumeID, ErrUnsupported)
	}
	if err := os.MkdirAll(p.unitPath(volumeID), 0o755); err != nil {
		return fmt.Errorf("volume: create %s: %w", volumeID, err)
	}
	return nil
}

func (p *PlainDir) SetCapacity(_ context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	if _, err := os.Stat(p.unitPath(volumeID)); err != nil {
		return fmt.Errorf("volume: set capacity on %s: %w", volumeID, err)
	}
	if capBytes > 0 {
		return fmt.Errorf("volume: plain_dir cannot enforce a %d-byte cap on %s: %w", capBytes, volumeID, ErrUnsupported)
	}
	return nil // 0 = uncapped is the only state this arm can be in
}

// Usage measures allocation and reports NO cap. The zero limit here is honest
// rather than a placeholder — this arm never has one — and it is the same value the
// capped arm returns for an uncapped unit, which is exactly the ambiguity
// Backend.Usage warns about: the limit result is diagnostic, the record is the
// authority.
func (p *PlainDir) Usage(_ context.Context, volumeID string) (int64, int64, error) {
	if err := validateID(volumeID); err != nil {
		return 0, 0, err
	}
	used, err := allocbytes.Tree(p.unitPath(volumeID))
	if err != nil {
		return 0, 0, fmt.Errorf("volume: measure usage of %s: %w", volumeID, err)
	}
	return used, 0, nil
}

// Materialize is create-if-absent, same contract as the other arm (§5.C).
func (p *PlainDir) Materialize(_ context.Context, volumeID string) (*Materialized, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	unit := p.unitPath(volumeID)
	if err := os.MkdirAll(unit, 0o755); err != nil {
		return nil, fmt.Errorf("volume: materialize %s: %w", volumeID, err)
	}
	return &Materialized{Volume: volumeID, SourcePath: unit}, nil
}

func (p *PlainDir) Release(context.Context, *Materialized) error { return nil }

func (p *PlainDir) AdoptMaterialized(_ context.Context, volumeID string) (*Materialized, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	unit := p.unitPath(volumeID)
	if fi, err := os.Stat(unit); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("volume: adopt %s: unit not present", volumeID)
	}
	return &Materialized{Volume: volumeID, SourcePath: unit}, nil
}

// OpenRoot uses the SAME accessor as the local arm — both are
// openat2(RESOLVE_IN_ROOT), identically. That is deliberate: root confinement is a
// property the contract requires of every arm, so sharing one implementation means
// the shared contract test exercises the real mechanism on both rather than a copy
// on one of them (volume.md D5.1: a remote arm's FUSE mount point is a directory,
// so openat2 behaves identically there too).
func (p *PlainDir) OpenRoot(_ context.Context, volumeID string) (Root, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	return NewLocalRoot(volumeID, p.unitPath(volumeID))
}

func (p *PlainDir) DestroyVolume(_ context.Context, volumeID string) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	if err := os.RemoveAll(p.unitPath(volumeID)); err != nil {
		return fmt.Errorf("volume: destroy %s: %w", volumeID, err)
	}
	return nil
}

// UncappedBytes: every volume on this arm is uncapped by construction, so all of
// its bytes land in the §3.4 second term. This is the arm that makes the term's
// necessity visible — on a capped arm every unit contributes zero and a broken
// implementation returning a constant 0 would pass.
func (p *PlainDir) UncappedBytes(context.Context) (int64, error) {
	ents, err := os.ReadDir(p.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("volume: measure volumes: %w", err)
	}
	var total int64
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, err := allocbytes.Tree(p.unitPath(e.Name()))
		if err != nil {
			return 0, fmt.Errorf("volume: measure volume %s: %w", e.Name(), err)
		}
		total += n
	}
	return total, nil
}

func (p *PlainDir) SweepOrphanVolumes(ctx context.Context, live, building []string) (int, error) {
	keep := make(map[string]bool, len(live)+len(building))
	for _, id := range live {
		keep[id] = true
	}
	for _, id := range building {
		keep[id] = true
	}
	ents, err := os.ReadDir(p.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("volume: sweep list units: %w", err)
	}
	reclaimed := 0
	var failed []string
	for _, e := range ents {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := p.DestroyVolume(ctx, e.Name()); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		reclaimed++
	}
	if len(failed) > 0 {
		return reclaimed, fmt.Errorf("volume: sweep reclaim unit(s): %s", strings.Join(failed, "; "))
	}
	return reclaimed, nil
}

// compile-time: PlainDir satisfies Backend.
var _ Backend = (*PlainDir)(nil)
