//go:build substrate

package test

import (
	"os"
	"testing"

	"github.com/TSHOGX/khora/test/substratereq"
)

// Every file in this package is `substrate`-tagged, so the preconditions can be stated
// ONCE here rather than at the top of 34 test functions. The three internal/* packages
// cannot do this — they mix root-free test files into the same binary — which is why they
// keep a per-test guard instead.
//
// FAIL, NOT SKIP. `ok` from this package used to mean either "the end-to-end suite ran"
// or "all 34 tests skipped", and nothing distinguished them.
//
// The binaries are named because their absence otherwise surfaces as a failure deep
// inside a launch or a rule install, several hundred lines from the cause.
func TestMain(m *testing.M) {
	substratereq.Root(substratereq.Main)
	substratereq.Bin(substratereq.Main, "runsc", "nft", "ip", "btrfs")
	os.Exit(m.Run())
}
