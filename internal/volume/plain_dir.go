package volume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TSHOGX/khora/internal/allocbytes"
)

// PlainDir is the seam's SECOND ARM. It is an uncapped directory provider,
// suitable for development and contract tests; production deployments that
// need capacity enforcement should select LocalBtrfs. It lives beside the arm
// it falsifies and shares its accessor implementation, while the contract suite
// that drives it lives in a sibling package.
//
// IT IS NOT FILLER — IT IS THE ONLY FALSIFICATION INSTRUMENT THIS SEAM HAS
// (the volume backend seam): the three-actions shape rule stated on Backend was DERIVED from
// running this arm against the same shared contract, and with only one arm every one
// of those three failures would have looked like a reasonable simplification.
//
// It is also the CAPABILITY-NEGATIVE arm: it reports no Snapshot, no Clone, no
// Quota, so the contract suite can assert that an unsupported operation is refused
// with a TYPED error AND NO SIDE EFFECT — the path the API error contract maps to 400. Without a
// backend that genuinely lacks a capability, that whole branch would only ever be
// exercised by a mock agreeing with itself.
type PlainDir struct {
	// Dir holds one plain directory per volume: Dir/<volumeID>.
	Dir string
}

// NewPlainDir constructs the uncapped directory arm over dir.
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

// Validate is the registry startup readiness check for the directory arm.
func (p *PlainDir) Validate(context.Context) error {
	if p == nil || p.Dir == "" {
		return fmt.Errorf("Dir is required")
	}
	fi, err := os.Stat(p.Dir)
	if err != nil {
		return fmt.Errorf("volume root: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("volume root is not a directory")
	}
	return nil
}

// Capabilities: none of the three. A plain directory can neither snapshot nor
// enforce a byte ceiling, and saying so honestly is this arm's job — the Volume subsystem requires
// an unsupported call to be a clean 400 rather than a silent no-op.
func (p *PlainDir) Capabilities() Capabilities { return Capabilities{Usage: true} }

func (p *PlainDir) unitPath(volumeID string) string { return filepath.Join(p.Dir, volumeID) }

func (p *PlainDir) CreateVolume(_ context.Context, volumeID string, capBytes int64) error {
	if err := validateID(volumeID); err != nil {
		return err
	}
	if capBytes > 0 {
		// The cap is REFUSED, and refused BEFORE the directory is created, so an
		// unsupported request leaves nothing behind. Accepting and ignoring it is the
		// silent failure resource admission forbids.
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

// Usage measures allocation and reports NO cap. The zero limit is honest rather than a
// placeholder — this arm never has one — and it is the same value the capped arm returns
// for an uncapped unit, which is the ambiguity Backend.Usage warns about.
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

// Materialize is create-if-absent, same contract as the other arm (the Volume subsystem).
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

// OpenRoot uses the SAME accessor as the local arm, deliberately: root confinement is
// required of every arm, so sharing one implementation means the shared contract test
// exercises the real root-confinement mechanism on both rather than a copy on one.
func (p *PlainDir) OpenRoot(_ context.Context, volumeID string) (Root, error) {
	if err := validateID(volumeID); err != nil {
		return nil, err
	}
	return NewLocalRoot(volumeID, p.unitPath(volumeID))
}

// Snapshot / Clone: NOT SUPPORTED, and refusing them is this arm's job (the Volume subsystem requires
// an unsupported capability to be a clean typed refusal, never a silent no-op).
//
// NOT faked with a recursive byte copy: a snapshot's contract is atomic and
// instantaneous, and a tree walk of a live volume satisfies neither while looking like
// success.
func (p *PlainDir) Snapshot(_ context.Context, srcID, newID string, _ int64) error {
	return fmt.Errorf("volume: plain_dir cannot snapshot %s → %s: %w", srcID, newID, ErrUnsupported)
}

func (p *PlainDir) Clone(_ context.Context, srcID, newID string, _ int64) error {
	return fmt.Errorf("volume: plain_dir cannot clone %s → %s: %w", srcID, newID, ErrUnsupported)
}

// ReclaimQuotaRows / DeletedRowIDs: nothing to reclaim. A plain directory has no quota
// rows, so there is no residue and this arm costs the sweep nothing.
//
// It returns (0, nil) rather than ErrUnsupported deliberately: the sweep must not have
// to ASK a backend whether it has rows and branch on the answer (the transactional outbox's
// no-capability-branch rule). An empty list and a zero count say the same thing without
// putting a branch in the caller.
func (p *PlainDir) ReclaimQuotaRows(context.Context, []uint64) (int, error) { return 0, nil }

func (p *PlainDir) DeletedRowIDs(context.Context) []uint64 { return nil }

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
// its bytes land in the resource admission second term. This is the arm that makes the term's
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

func (p *PlainDir) ListUnitIDs(_ context.Context) ([]string, error) {
	ents, err := os.ReadDir(p.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("volume: enumerate units: %w", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// compile-time: PlainDir satisfies Backend.
var _ Backend = (*PlainDir)(nil)
