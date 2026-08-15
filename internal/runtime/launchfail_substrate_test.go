//go:build substrate

// L2: a FAILED launch leaves no the documented invariant rule-set table behind, against real nft.
//
//	sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 \
//	  ./internal/runtime/ -run TestAFailedLaunchLeavesNoRuleSetTable
package runtime

import (
	"context"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/ruleset"
)

// A launch that fails AFTER the rule set is installed must take the table with it.
//
// # Why this is a separate property from destroy
//
// Destroy runs on a COMPLETE placement; this runs on a half-built one, so it is the only
// thing that exercises teardownPlacement's two half-built provisions — `nft destroy` over
// `delete`, and dropping the table before the veth. A destroy path that always finds
// everything present exercises neither. What a leak costs is in the failure message.
//
// # Why it is driven at this layer
//
// The control plane has no knob that fails a launch after the rules go in: an admission
// refusal happens before the netns exists, and a bad declaration is refused at the write
// point. Here the runsc start can simply be given a rootfs it cannot use, which fails at
// the unit-start step — the first step AFTER installRuleSet, which is where the window
// this asserts against would be.
func TestAFailedLaunchLeavesNoRuleSetTable(t *testing.T) {
	mustRoot(t)
	ctx := context.Background()
	base := t.TempDir()

	p, err := NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewLocalRunsc: %v", err)
	}

	const sid = "sbx_launchfail0000000000"
	table := "sbxnet_" + sid
	// Belt and braces: if the assertion below fails, the table must not outlive the test
	// and start applying to some later sandbox's recycled veth name.
	t.Cleanup(func() { p.deleteRuleSet(table) })

	before := countTables(t)

	// A rootfs directory that EXISTS but holds no guest at all, so the netns, the veth and
	// the rule set are all built and the runsc start is what fails. MergedPath must be
	// non-empty or Launch refuses before building anything, which would assert nothing.
	empty := filepath.Join(base, "empty-rootfs")
	if err := exec.Command("mkdir", "-p", empty).Run(); err != nil {
		t.Fatal(err)
	}

	// A minimal but REAL rule set, so there is genuinely a table to leak. Hand-built
	// rather than taken from network.Generate: the documented invariant subsystems never import each other
	// (internal/arch enforces it), and the assertion is about the table's existence.
	spec := LaunchSpec{
		Sandbox: sid,
		Rootfs:  RootfsSource{MergedPath: empty},
		Steering: SteeringSpec{Rules: ruleset.RuleSet{
			Rules: []ruleset.Rule{{
				Hook:    ruleset.HookForward,
				Scope:   ruleset.ScopeGuestIn,
				Verdict: ruleset.Drop,
				Match:   ruleset.Match{DestV4: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}},
				Counter: "fwd_floor_v4",
				Why:     "a stand-in floor arm, so the table this test is about is a real one",
			}},
			Counters: []string{"fwd_floor_v4"},
		}},
	}

	pl, lerr := p.Launch(ctx, spec)
	if lerr == nil {
		// Not the property under test, but leaving a live placement behind would poison
		// every later test on this host.
		if pl != nil {
			_ = p.Destroy(ctx, pl)
		}
		t.Fatalf("the launch SUCCEEDED with an empty rootfs (%v) — this test needs a failing launch "+
			"to have anything to assert; if runsc has become able to start on an empty directory, "+
			"find another way to fail the unit start", pl)
	}
	t.Logf("launch failed as intended: %v", lerr)

	// THE PROPERTY: no table survived.
	if n := countTables(t); n != before {
		t.Errorf("a failed launch left the rule-set table behind (tables %d → %d) — a table is "+
			"crash-surviving host state (the documented invariant) and its rules are narrowed by `iifname`, so once that "+
			"veth name is recycled by a later sandbox the stale rules apply to IT: a sandbox enforcing "+
			"a declaration nobody wrote, with no record naming the table\n%s", before, n, dumpTable(t, table))
	}
	// And specifically not THIS one, since the count alone would be satisfied by an
	// unrelated table disappearing in the same window.
	if out, err := exec.Command("nft", "list", "table", "inet", table).CombinedOutput(); err == nil {
		t.Errorf("the failed launch's own table %s is still installed:\n%s", table, out)
	}
}

// countTables counts engine per-sandbox rule-set tables. Local to this file rather than
// borrowed from the harness: internal/runtime cannot import engine/test/harness (it would
// be an import cycle through the harness's own use of the runtime).
func countTables(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nft", "list", "tables", "inet").Output()
	if err != nil {
		t.Fatalf("nft list tables: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " sbxnet_") {
			n++
		}
	}
	return n
}

func dumpTable(t *testing.T, table string) string {
	t.Helper()
	out, _ := exec.Command("nft", "list", "table", "inet", table).CombinedOutput()
	return string(out)
}
