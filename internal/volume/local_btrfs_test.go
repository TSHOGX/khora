package volume_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/volume"
)

// TestUsageRefusesToInventALimitWhenTheQuotaRowIsUnreadable is the fault-injection
// half of Backend.Usage, and it is a UNIT test on purpose: the failure it pins is
// UNREPRODUCIBLE ON DEMAND against real storage. Measured on the substrate,
// `qgroup show -f <path>` enumerates EVERY row on the filesystem and filters
// afterwards, so a subvolume deleted concurrently anywhere on that filesystem fails
// the read wholesale — 157 failures in 2142 reads under churn, naming a row that is
// not even ours. The substrate's cleaner makes that churn asynchronous, so no test
// can schedule it; injecting the failure at the tool boundary is the only way to
// assert the behavior deterministically.
//
// WHAT IT GUARDS: an unreadable row must surface as an ERROR, never as
// `(walkedBytes, 0, nil)`. That synthesized answer is wrong in both halves — the walk
// counts file data only while the row also charges metadata extents, so `used` comes
// back UNDER the substrate's own number and the the documented invariant cap write point's strict
// inequality would admit a cap that wedges the unit irreversibly (the relevant internals page the documented invariant),
// while limit 0 is byte-for-byte identical to a deliberately uncapped volume. All
// three callers already branch on the error (inspect degrades, PATCH fails closed,
// RepairVolumeCaps draws no conclusion); answering definitely is what made those
// branches unreachable.
func TestUsageRefusesToInventALimitWhenTheQuotaRowIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	b, err := volume.NewLocalBtrfs(filepath.Join(dir, "volumes"))
	if err != nil {
		t.Fatalf("new local arm: %v", err)
	}
	// The unit exists and PRESENTS AS a quota-capable one (IsSubvolume succeeds),
	// while the quota read fails the way the substrate fails it. That combination is
	// the whole point: a plain directory legitimately has no row, so the assertion
	// would be vacuous without the subvolume answer being true.
	unit := filepath.Join(dir, "volumes", "vol_q")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Create(filepath.Join(unit, "blob")); err != nil {
		t.Fatal(err)
	}
	b.Btrfs = btrfs.Client{Run: func(_ context.Context, argv []string) ([]byte, error) {
		switch {
		case len(argv) > 2 && argv[1] == "subvolume" && argv[2] == "show":
			return []byte("vol_q\n\tSubvolume ID:\t\t256\n"), nil
		case len(argv) > 2 && argv[1] == "qgroup" && argv[2] == "show":
			// Verbatim substrate failure, including the id of SOMEONE ELSE'S row —
			// which is why no caller can serialize against it.
			return nil, fmt.Errorf("%v: exit status 1: ERROR: cannot find the qgroup 0/21680\nERROR: can't list qgroups: No such file or directory", argv)
		}
		return nil, fmt.Errorf("unexpected command %v", argv)
	}}

	used, limit, err := b.Usage(context.Background(), "vol_q")
	if err == nil {
		t.Fatalf("Usage on a quota unit with an UNREADABLE row returned (used=%d, limit=%d, nil) — it must return an error instead of synthesizing a definite answer from a failure (the documented invariant). limit 0 is indistinguishable from a deliberately uncapped volume, and the walked `used` is UNDER the row's own number (which also charges metadata), so the the documented invariant strict inequality would admit a cap that wedges the unit (the relevant internals page the documented invariant)", used, limit)
	}
	// The error has to name the volume, because the operator-facing symptom of this
	// failure is otherwise a bare btrfs message about an unrelated qgroup id.
	if !strings.Contains(err.Error(), "vol_q") {
		t.Errorf("error %q does not name the volume", err)
	}
}

// TestUncappedBytesRefusesToWalkAUnitWhoseRowIsUnreadable is the AGGREGATE half of
// the rule the test above pins per volume, and the defect it guards is worse than an
// imprecise number.
//
// UncappedBytes used to fall through to a walk when the quota read failed, justified
// as "omitting real bytes would overcommit, so measuring is conservative". But on that
// path the `limit > 0` skip is UNREACHABLE, so a CAPPED unit got walked too — and a
// capped unit's correct contribution to this term is ZERO, because the documented invariant's cap term
// already counts it. The fallback therefore billed the same bytes twice (the documented invariant
// disjointness), at the magnitude of the whole unit, and strictly in ONE direction:
// the left side of the admission inequality grows, so the symptom is a create or
// resume refused with 507 on a node that has the room.
//
// The fixture is deliberately a CAPPED unit with an unreadable row, because that is
// the combination the old code got wrong. Asserting on an uncapped unit would pass
// either way.
func TestUncappedBytesRefusesToWalkAUnitWhoseRowIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	b, err := volume.NewLocalBtrfs(filepath.Join(dir, "volumes"))
	if err != nil {
		t.Fatalf("new local arm: %v", err)
	}
	unit := filepath.Join(dir, "volumes", "vol_capped")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real bytes, so a walk would return something substantial rather than 0 — without
	// this the assertion could pass on an empty directory by accident.
	if err := os.WriteFile(filepath.Join(unit, "blob"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	b.Btrfs = btrfs.Client{Run: func(_ context.Context, argv []string) ([]byte, error) {
		switch {
		case len(argv) > 2 && argv[1] == "subvolume" && argv[2] == "show":
			return []byte("vol_capped\n\tSubvolume ID:\t\t256\n"), nil
		case len(argv) > 2 && argv[1] == "qgroup" && argv[2] == "show":
			return nil, fmt.Errorf("%v: exit status 1: ERROR: cannot find the qgroup 0/21680", argv)
		}
		return nil, fmt.Errorf("unexpected command %v", argv)
	}}

	total, err := b.UncappedBytes(context.Background())
	if err == nil {
		t.Fatalf("UncappedBytes returned (%d, nil) with a unit whose quota row is unreadable — it must return the error. Falling through to a walk cannot tell a capped unit from an uncapped one (the limit is exactly what could not be read), so a capped unit's bytes land in the second term while its cap is already in the first: the same bytes twice, and the node refuses admission it could have granted", total)
	}
	if !strings.Contains(err.Error(), "vol_capped") {
		t.Errorf("error %q does not name the volume whose row could not be read", err)
	}
}

// TestUsageStillWalksAPlainDirectoryUnit is the other side of the same branch: a
// unit that is NOT a quota-capable one has no row to read, so "no cap" is the truth
// rather than a guess and the walk is the right answer. Without this the fix above
// could be satisfied by erroring on everything, which would break the uncapped arm.
func TestUsageStillWalksAPlainDirectoryUnit(t *testing.T) {
	dir := t.TempDir()
	b, err := volume.NewLocalBtrfs(filepath.Join(dir, "volumes"))
	if err != nil {
		t.Fatalf("new local arm: %v", err)
	}
	unit := filepath.Join(dir, "volumes", "vol_plain")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	const size = 8 << 10
	if err := os.WriteFile(filepath.Join(unit, "blob"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every btrfs call fails: a plain directory on a non-btrfs filesystem is exactly
	// the shape the uncapped arm runs in.
	b.Btrfs = btrfs.Client{Run: func(_ context.Context, argv []string) ([]byte, error) {
		return nil, fmt.Errorf("not btrfs: %v", argv)
	}}

	used, limit, err := b.Usage(context.Background(), "vol_plain")
	if err != nil {
		t.Fatalf("Usage of a plain-directory unit: %v — it has no row to read, so the walk is the answer, not a failure", err)
	}
	if limit != 0 {
		t.Errorf("limit = %d, want 0 — a plain directory carries no cap", limit)
	}
	if used < size/2 {
		t.Errorf("used = %d after writing %d bytes, want the walk to see them", used, size)
	}
}
