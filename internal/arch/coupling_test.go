// Package arch holds STRUCTURAL guards for the architecture rules that are
// otherwise enforced only by review. It has no production code — it exists so a
// violation fails a test run instead of surviving to a code review.
package arch

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// subsystems are the §5 packages. Each owns a mechanism; the §4 mediator owns the
// verbs. CLAUDE.md: "subsystems NEVER call each other" — an edge between any two
// of these is the start of the dependency cycle the ownership rule exists to
// prevent.
//
// Every entry is REQUIRED to exist (TestGuardIsNotStale). network (§5.D) was listed
// here before it existed, so the guard would catch a bad edge the day the package
// landed rather than the day someone remembered to add it; now that it has landed it
// is required like the rest, because "absent packages are skipped" is a hole the
// moment the package is real — a rename would leave §5.D unchecked with every test
// still green.
//
// Pre-listing is what gives the empty allowlist its force at DESIGN time, not just
// at review time: routing volume content through §5.E `fs` was ruled out for §5.C
// partly because this guard would have failed on it (volume.md D5.1).
var subsystems = []string{
	"warren/internal/runtime",   // §5.A
	"warren/internal/rootfs",    // §5.B
	"warren/internal/volume",    // §5.C
	"warren/internal/network",   // §5.D
	"warren/internal/transport", // §5.E
}

// mediatorOnly are packages a §5 subsystem must never import. A subsystem that
// reads the store has stopped being a mechanism: §4 hands it the facts it needs
// (the keep-set pattern), precisely so the subsystem cannot form its own opinion
// about authoritative state.
var mediatorOnly = []string{
	"warren/internal/lifecycle",
	"warren/internal/store",
}

// allowedEdges are cross-subsystem imports permitted with the reason they are
// legitimate. Keyed "importer -> imported". The reason is the point: an exemption
// without one is a bare allowlist, and the next edge arrives through the same
// door claiming the same precedent.
//
// Empty, and worth keeping empty. When two subsystems need the same fact, the
// answer is a leaf package they both depend on (internal/guestport, holding the
// guest netns port shared by §5.A and §5.E, is the precedent), never an entry
// here. A shared constant is a leaf, not a reason for a subsystem edge.
var allowedEdges = map[string]string{}

// pkg is the subset of `go list -json` we need.
type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// tagMatrix is every build configuration these guards must hold under. A tag that
// is not here is a configuration where a bad edge passes unseen, and the files
// behind a tag are exactly the ones most likely to reach for a shortcut: they are
// already allowed to touch the substrate.
//
//   - ""          the production build.
//   - substrate   the root-only tests against real runsc/overlay/btrfs.
//   - drainrace   the minute-scale tests, tagged separately so the routine gate
//     stays fast; they also carry `substrate`, hence the pair.
//   - quotachurn  the same bargain for the quota-row churn test, whose cost is the
//     substrate's `--sync` on a filesystem the dev box shares with other tenants.
//     It sits in `btrfs`, a LISTED LEAF, so leaving the tag out would take the leaf
//     guard off exactly the file most able to break leaf-ness unseen.
//   - testguest   the in-guest fault-injection seam, which must not exist in a
//     production sandboxd — a guard that never ran with it on would not see an
//     edge introduced by that seam.
var tagMatrix = []string{"", "substrate", "drainrace", "quotachurn", "testguest", "substrate,drainrace", "substrate,quotachurn", "substrate,testguest"}

// listPackages runs `go list -json` for the given tags. Build tags matter: the
// substrate tests are behind `//go:build substrate` and are invisible to a
// default list, so a bad edge introduced in one of them would go unseen.
func listPackages(t *testing.T, tags string) []pkg {
	t.Helper()
	args := []string{"list", "-json"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "warren/...")
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		t.Fatalf("go list -tags %q: %v", tags, err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// owner returns the subsystem that owns an import path, or "" if none does. A
// package's OWN subtree does not count as another subsystem: internal/transport
// importing internal/transport/gen/... is one subsystem using its own generated
// wire stubs, not an edge. Getting this wrong is the guard's obvious false
// positive, so it is handled here rather than in each caller.
func owner(importPath string) string {
	for _, s := range subsystems {
		if importPath == s || strings.HasPrefix(importPath, s+"/") {
			return s
		}
	}
	return ""
}

// TestSubsystemsDoNotImportEachOther is the structural form of the §4 ownership
// rule. It checks production, test, and external-test imports, under both the
// default and substrate build tags.
func TestSubsystemsDoNotImportEachOther(t *testing.T) {
	for _, tags := range tagMatrix {
		for _, p := range listPackages(t, tags) {
			self := owner(p.ImportPath)
			if self == "" {
				continue // not a subsystem package
			}
			for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
				for _, imp := range group {
					got := owner(imp)
					if got == "" || got == self {
						continue
					}
					edge := self + " -> " + got
					if reason, ok := allowedEdges[edge]; ok {
						t.Logf("allowed edge %s (%s)", edge, reason)
						continue
					}
					t.Errorf("subsystem %s imports subsystem %s (via %q, tags=%q) — "+
						"§5 subsystems must not call each other (CLAUDE.md / §4 ownership). "+
						"If they need a shared fact, put it in a leaf package; if one needs the "+
						"other's behavior, the §4 mediator should be threading it.",
						self, got, imp, tags)
				}
			}
		}
	}
}

// TestSubsystemsDoNotReadTheControlPlane pins the dependency DIRECTION: the
// mediator drives subsystems, never the reverse. A subsystem reaching into
// lifecycle or store would let it read authoritative state directly instead of
// being handed it (§4: subsystems don't read the store).
func TestSubsystemsDoNotReadTheControlPlane(t *testing.T) {
	for _, tags := range tagMatrix {
		for _, p := range listPackages(t, tags) {
			self := owner(p.ImportPath)
			if self == "" {
				continue
			}
			for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
				for _, imp := range group {
					for _, banned := range mediatorOnly {
						if imp != banned && !strings.HasPrefix(imp, banned+"/") {
							continue
						}
						t.Errorf("subsystem %s imports %s (tags=%q) — a §5 subsystem must not "+
							"read the control plane; §4 hands it what it needs (the keep-set pattern)",
							self, imp, tags)
					}
				}
			}
		}
	}
}

// leaves are the packages that exist SO THAT the empty allowlist above can stay
// empty: when two subsystems need the same fact, it goes in a leaf they both
// depend on (CLAUDE.md), never behind an exemption.
//
// A leaf is only a leaf while it imports NOTHING from the engine. The failure mode
// is quiet and it defeats the whole construction: the moment `ruleset` imports
// `runtime` (say, to reach a placement type), every §5.D package that depends on
// it inherits that edge TRANSITIVELY, and the guard above still passes — it only
// looks at direct imports. So this is checked here rather than trusted to the
// package comment.
//
// The list is every package carrying that structural role, not only the ones whose
// doc comments say "leaf" — the transitive-edge argument is a property of being
// depended on by two subsystems, and it holds VERBATIM for all four:
//
//   - guestport — the precedent: the guest netns port, shared by §5.A and §5.E.
//   - ruleset — §2.4's static address facts plus the abstract rule vocabulary §5.D
//     generates and §5.A renders. Also the reason its Validate must live outside
//     both: it runs on the generate side and again before install.
//   - btrfs — the substrate tool, model-ignorant, shared by §5.B (rootfs/overlayfs.go)
//     and §5.C (volume/local_btrfs.go).
//   - allocbytes — shared by §5.B and §5.C for the same reason.
var leaves = []string{
	"warren/internal/guestport",
	"warren/internal/ruleset",
	"warren/internal/btrfs",
	"warren/internal/allocbytes",
}

// inertLeaves are the leaves CLAUDE.md's second half binds: "a leaf may DECIDE
// things about values handed to it, and may do NO I/O, execute nothing, and touch
// no substrate. A leaf that acts is a subsystem wearing a leaf's name."
//
// This is a SEPARATE guard from the import check above, because that one only looks
// for `engine/` imports — a leaf that imported `os/exec` and shelled out to `nft`
// would pass it, and every other check here, while re-coupling §5.D to §5.A through
// the back door the empty allowlist exists to keep shut. That is the whole loophole,
// so it is checked rather than left to the package comment.
//
// NOT every leaf: `btrfs` and `allocbytes` are the OTHER kind. CLAUDE.md names
// `btrfs` "a substrate tool, model-ignorant" — acting on the substrate is its
// purpose, and `allocbytes` walks a tree to count bytes. What makes them safe is not
// inertness but model-ignorance: neither knows a §5 type, so neither can carry a
// mechanism between two subsystems. The leaves listed here are the ones whose safety
// argument IS inertness — `ruleset` in particular must run identically on the
// generate side (root-free unit tests) and the install side, which is only true
// while it decides and never acts.
var inertLeaves = []string{
	"warren/internal/guestport",
	"warren/internal/ruleset",
}

// actsOnTheWorld are stdlib packages a leaf cannot reach without doing the thing
// CLAUDE.md forbids. Process execution and raw syscalls are the mechanism-smuggling
// routes that matter (shelling out to `nft` / `ip`, or an ioctl); the filesystem and
// network packages are here because "touch no substrate" covers reading it too — a
// leaf that stats a path has made its answer depend on the host it runs on, which is
// exactly what makes the generate-side and install-side runs stop agreeing.
var actsOnTheWorld = []string{
	"os/exec",
	"syscall",
	"golang.org/x/sys/unix",
	"net",
	"net/http",
	"os",
	"io/fs",
	"path/filepath",
}

// TestInertLeavesDoNotAct enforces the second half of the leaf boundary.
func TestInertLeavesDoNotAct(t *testing.T) {
	banned := map[string]bool{}
	for _, b := range actsOnTheWorld {
		banned[b] = true
	}
	want := map[string]bool{}
	for _, l := range inertLeaves {
		want[l] = true
	}
	for _, tags := range tagMatrix {
		for _, p := range listPackages(t, tags) {
			if !want[p.ImportPath] {
				continue
			}
			delete(want, p.ImportPath)
			// Production imports only: a leaf's own TEST may read a fixture off disk
			// without the leaf acting in production, and `go list` reports test imports
			// on the same package.
			for _, imp := range p.Imports {
				if !banned[imp] {
					continue
				}
				t.Errorf("leaf %s imports %s (tags=%q) — a leaf may DECIDE things about "+
					"values handed to it, but may do no I/O, execute nothing, and touch no "+
					"substrate (CLAUDE.md). A leaf that acts is a subsystem wearing a leaf's "+
					"name, and it re-couples the two subsystems that share it through a door "+
					"the import guard above cannot watch", p.ImportPath, imp, tags)
			}
		}
	}
	for l := range want {
		t.Errorf("inert leaf %s not found — this guard is stale, and a stale guard passes vacuously", l)
	}
}

// TestLeavesImportNothingFromTheEngine keeps the leaf packages leaves.
func TestLeavesImportNothingFromTheEngine(t *testing.T) {
	want := map[string]bool{}
	for _, l := range leaves {
		want[l] = true
	}
	for _, tags := range tagMatrix {
		for _, p := range listPackages(t, tags) {
			if !want[p.ImportPath] {
				continue
			}
			delete(want, p.ImportPath)
			for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
				for _, imp := range group {
					if !strings.HasPrefix(imp, "warren/") {
						continue
					}
					t.Errorf("leaf %s imports %s (tags=%q) — a leaf exists so two subsystems can "+
						"share a fact WITHOUT an edge between them, and it only works while it "+
						"depends on nothing: an import here becomes a TRANSITIVE edge that the "+
						"direct-import guard above cannot see", p.ImportPath, imp, tags)
				}
			}
		}
	}
	for l := range want {
		t.Errorf("leaf package %s not found — this guard is stale, and a stale guard passes vacuously", l)
	}
}

// TestGuardIsNotStale fails if the subsystem list has drifted from reality —
// otherwise renaming a package silently disables the checks above, which is worse
// than not having them (they would still pass, and read as coverage).
func TestGuardIsNotStale(t *testing.T) {
	present := map[string]bool{}
	for _, p := range listPackages(t, "") {
		present[p.ImportPath] = true
	}
	// EVERY listed subsystem must exist — the list is read straight from `subsystems`
	// rather than restated, so the two cannot drift apart. A second hand-written copy
	// is how §5.D came to be exempt here after its package had already landed: the
	// checks above key off `subsystems`, and a package missing from `present` is simply
	// skipped by `owner`, so a rename disables them while this test still passes.
	for _, s := range subsystems {
		if !present[s] {
			t.Errorf("subsystem package %s not found — this guard is stale (renamed or moved), "+
				"and a stale guard passes vacuously", s)
		}
	}
	for _, m := range mediatorOnly {
		if !present[m] {
			t.Errorf("control-plane package %s not found — this guard is stale", m)
		}
	}
}
