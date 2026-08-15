//go:build substrate

// L2: the engine-level second-hop table's TEARDOWN half, against real nft.
//
//	sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 \
//	  ./internal/runtime/ -run TestASecondHopTableIsNotInherited
package runtime

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/ruleset"
)

// A run started with NO second-hop uid must DESTROY the table a previous run installed,
// not merely decline to install one.
//
// An nft table is crash-surviving host state exactly like a netns (the documented invariant), and the
// engine-level one is the only table no sandbox owns: ReclaimOrphans skips it by
// construction (sandboxOfTable does not match it), so if this call does not remove it,
// nothing in the engine ever will. What survives is an `output` table narrowed by a uid
// the running deployment does not know — dropping traffic for a reason absent from the
// configuration, which is unattributable from the outside.
//
// It needs the real substrate rather than a fake: the property IS the nft state. A
// teardown that returns early without removing the table satisfies every caller-side
// assertion — no unit test can tell that shape from a real teardown, because the
// difference exists only in the kernel.
//
// The rule set here is a hand-built stand-in rather than network.SecondHopRules: the documented invariant
// subsystems never import each other (internal/arch enforces it), and the assertion is
// about the TABLE's existence, so any installable table serves.
func TestASecondHopTableIsNotInheritedByARunWithoutTheUID(t *testing.T) {
	mustRoot(t)
	p := &LocalRunsc{RunscRoot: t.TempDir(), StateDir: t.TempDir()}
	t.Cleanup(func() { p.deleteRuleSet(SecondHopTable) })

	rs := ruleset.RuleSet{
		Rules: []ruleset.Rule{{
			Hook:    ruleset.HookOutputFilter,
			Scope:   ruleset.ScopeSecondHopUID,
			Verdict: ruleset.Drop,
			Match:   ruleset.Match{DestV4: ruleset.FloorV4},
			Counter: "hop2_floor_v4",
			Why:     "stand-in for the real floor arm; this test asserts the table's lifecycle",
		}},
		Counters: []string{"hop2_floor_v4"},
	}
	uid := 61234 // a uid this host does not run anything as
	if err := p.InstallSecondHop(context.Background(), rs, &uid); err != nil {
		t.Fatalf("install the second-hop table: %v", err)
	}
	if !secondHopTablePresent(t) {
		t.Fatal("the table was never installed, so the teardown assertion below would be vacuous")
	}

	// The restart: same host, no uid.
	if err := p.InstallSecondHop(context.Background(), ruleset.RuleSet{}, nil); err != nil {
		t.Fatalf("boot with no second-hop uid: %v", err)
	}
	if secondHopTablePresent(t) {
		t.Error("a run with no second-hop uid INHERITED the previous run's engine-level `output` " +
			"table, narrowed by a uid it does not know. Nothing else enumerates that table — " +
			"ReclaimOrphans skips it because no sandbox owns it — so it would persist for the life " +
			"of the host, dropping traffic for a reason absent from the running configuration")
	}
}

func secondHopTablePresent(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("nft", "list", "tables").CombinedOutput()
	if err != nil {
		t.Fatalf("nft list tables: %v: %s", err, out)
	}
	return strings.Contains(string(out), SecondHopTable)
}
