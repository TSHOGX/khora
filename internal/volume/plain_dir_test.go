package volume_test

import (
	"testing"

	"github.com/TSHOGX/warren/internal/volume"
	"github.com/TSHOGX/warren/internal/volume/volumetest"
)

// TestPlainDirContract runs the shared seam contract against the TEST-ONLY
// plain-directory arm — the seam's falsification instrument (volume.md D2).
//
// It needs no substrate and no root, which is the point: the arm exists so the
// contract has a second implementation to disagree with, and a second
// implementation that could only run under sudo on btrfs would not get run.
//
// It is also the only place the CAPABILITY-NEGATIVE branch is genuinely exercised:
// this arm really cannot enforce a cap, so "an unsupported request is refused with
// a typed error AND leaves no side effect" is asserted against a backend that means
// it, rather than against a mock agreeing with itself.
func TestPlainDirContract(t *testing.T) {
	volumetest.RunContract(t, func(t *testing.T) volume.Backend {
		b, err := volume.NewPlainDir(t.TempDir())
		if err != nil {
			t.Fatalf("new plain_dir: %v", err)
		}
		return b
	})
}
