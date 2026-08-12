// Usage is the per-sandbox usage read the §6.1 disk-cap write point checks its strict
// inequality against (§3.4 / rootfs.ErrCapBelowUsage). Its PLAIN-DIRECTORY arm needs no
// root and no btrfs, so it runs in the default unit suite; the subvolume arm and the
// materialization-side guard are substrate tests (overlayfs_substrate_test.go).
package rootfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/warren/internal/btrfs"
)

// THE LOAD-BEARING PROPERTY IS THE `merged` TRAP, and it is the one an obvious
// implementation gets wrong: `merged` lives INSIDE the unit dir, and for a running
// sandbox it is the live overlayfs mount exposing lower ∪ upper. A `Usage` that walked
// the unit would therefore count the whole Template stack as this sandbox's usage —
// inflating the number the write point compares a requested cap against, and refusing
// cap edits that are perfectly safe. UncappedBytes documents the same trap; this test
// pins the sibling.
//
// Simulated WITHOUT mounting anything: plant bytes under the unit's `merged` subtree the
// way a mount would expose them. That is the honest root-free version of the hazard —
// what is being asserted is that the enumeration is BY NAME (upper, work) rather than a
// walk of the unit, and a planted `merged` tree distinguishes those two implementations
// exactly.
func TestUsageEnumeratesUpperAndWorkNotTheUnit(t *testing.T) {
	p, _ := newProvider(t)
	const id = "ov_usage"

	plant := func(rel string, size int) {
		t.Helper()
		full := filepath.Join(p.unitPath(id), rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The sandbox's own bytes: upper (guest writes) and work (copy-up staging — in the
	// SAME quota unit by D6.3, so it is this sandbox's usage too).
	plant("upper/data", 8192)
	plant("work/staging", 4096)
	// What a live merged mount would expose: the Template stack, which is NOT this
	// sandbox's usage (it is counted once from TemplatesDir by the §3.4 second term).
	plant("merged/usr/bin/huge", 512*1024)

	used, limit, err := p.Usage(context.Background(), id)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	// Allocated blocks, so compare against a bound rather than an exact byte count: the
	// two planted files cost at least their length and the merged file must not appear.
	if used < 12288 {
		t.Fatalf("used = %d; want at least the 12288 planted bytes of upper+work", used)
	}
	if used >= 512*1024 {
		t.Fatalf("used = %d — that is the `merged` subtree counted in. `merged` is the live overlayfs view of lower ∪ upper, so walking the unit charges this sandbox for the whole Template stack and makes the write point refuse safe cap edits; enumerate upper+work BY NAME (see UncappedBytes for the same trap)", used)
	}
	// A plain directory has no quota row, and "no cap" is the TRUTH there rather than a
	// guess — the one case where a 0 limit is not the ambiguous read D7.3 warns about.
	if limit != 0 {
		t.Fatalf("limit = %d; want 0 for a plain-directory unit", limit)
	}
}

// An absent unit is 0, not an error: a sandbox whose Overlay was swept has nothing to
// measure, and the write point's own gate (a positive cap on the record) is what decides
// whether that is anomalous.
func TestUsageOfAnAbsentUnitIsZero(t *testing.T) {
	p, _ := newProvider(t)
	used, limit, err := p.Usage(context.Background(), "ov_missing")
	if err != nil {
		t.Fatalf("Usage of an absent unit: %v", err)
	}
	if used != 0 || limit != 0 {
		t.Fatalf("used=%d limit=%d; want 0/0", used, limit)
	}
}

// The ID is a single path segment, like everywhere else in this subsystem: a traversal
// in an ID must not let a caller measure an arbitrary host path.
func TestUsageRejectsUnsafeID(t *testing.T) {
	p, _ := newProvider(t)
	for _, bad := range []string{"", "..", "a/b"} {
		if _, _, err := p.Usage(context.Background(), bad); err == nil {
			t.Fatalf("Usage(%q) was accepted; an unsafe segment must be refused", bad)
		}
	}
}

// TestUncappedBytesRefusesToWalkAUnitWhoseRowIsUnreadable is the §5.B half of the rule
// the §5.C arm pins in internal/volume. The two terms of §3.4's inequality are one
// design, so they must fail the same way: this term's shape IS the other's.
//
// The defect removed: an unreadable quota row used to fall through to enumerating
// upper+work. On that path the `limit > 0` skip is unreachable, so a CAPPED unit got
// enumerated as well — and a capped unit contributes ZERO here by contract, its cap
// being counted by §3.4's first term. So the same bytes were billed twice, strictly in
// the direction that inflates the left side of the admission inequality, and the symptom
// is a create or resume refused with 507 on a node with room.
//
// A CAPPED unit with an unreadable row is the fixture, because that is the combination
// the old code got wrong; an uncapped one would pass either way.
func TestUncappedBytesRefusesToWalkAUnitWhoseRowIsUnreadable(t *testing.T) {
	p, _ := newProvider(t)
	const id = "ov_unreadable"
	// Real bytes in upper, so a fallback walk would return something substantial.
	writeUpper(t, p, id, map[string]string{"payload": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"})

	p.Btrfs = btrfs.Client{Run: func(_ context.Context, argv []string) ([]byte, error) {
		switch {
		case len(argv) > 2 && argv[1] == "subvolume" && argv[2] == "show":
			return []byte(id + "\n\tSubvolume ID:\t\t256\n"), nil
		case len(argv) > 2 && argv[1] == "qgroup" && argv[2] == "show":
			// The substrate's own failure shape: it names an unrelated row, which is why
			// no caller can serialize against it (volume.md D7.1).
			return nil, fmt.Errorf("%v: exit status 1: ERROR: cannot find the qgroup 0/21680", argv)
		}
		return nil, fmt.Errorf("unexpected command %v", argv)
	}}

	total, err := p.UncappedBytes(context.Background())
	if err == nil {
		t.Fatalf("UncappedBytes returned (%d, nil) with a unit whose quota row is unreadable — it must return the error. The limit is exactly what could not be read, so enumerating cannot tell a capped unit from an uncapped one, and a capped unit's bytes then land in the second term while its cap is already in the first (§3.4.2 disjointness)", total)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("error %q does not name the overlay whose row could not be read", err)
	}
}
