// Package arch holds STRUCTURAL guards for the architecture rules that are
// otherwise enforced only by review. It has no production code — it exists so a
// violation fails a test run instead of surviving to a code review.
package arch

import (
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// subsystems are the §5 packages. Each owns a mechanism; the §4 mediator owns the
// verbs. CLAUDE.md: "subsystems NEVER call each other" — an edge between any two
// of these is the start of the dependency cycle the ownership rule exists to
// prevent.
//
// Every entry is REQUIRED to exist (TestGuardIsNotStale): "absent packages are
// skipped" is a hole the moment the package is real — a rename would leave §5.D
// unchecked with every test still green.
//
// Pre-listing a package before it exists is what gives the empty allowlist its force
// at DESIGN time, not just at review time: routing volume content through §5.E `fs`
// was ruled out for §5.C partly because this guard would have failed on it
// (volume.md D5.1).
var subsystems = []string{
	"github.com/TSHOGX/khora/internal/runtime",   // §5.A
	"github.com/TSHOGX/khora/internal/rootfs",    // §5.B
	"github.com/TSHOGX/khora/internal/volume",    // §5.C
	"github.com/TSHOGX/khora/internal/network",   // §5.D
	"github.com/TSHOGX/khora/internal/transport", // §5.E
}

// mediatorOnly are packages a §5 subsystem must never import. A subsystem that
// reads the store has stopped being a mechanism: §4 hands it the facts it needs
// (the keep-set pattern), precisely so the subsystem cannot form its own opinion
// about authoritative state.
var mediatorOnly = []string{
	"github.com/TSHOGX/khora/internal/lifecycle",
	"github.com/TSHOGX/khora/internal/store",
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

var packageCache = struct {
	sync.Mutex
	byTags map[string][]pkg
}{byTags: make(map[string][]pkg)}

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
	packageCache.Lock()
	if cached, ok := packageCache.byTags[tags]; ok {
		packageCache.Unlock()
		return cached
	}
	packageCache.Unlock()

	args := []string{"list", "-json"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "github.com/TSHOGX/khora/...")
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
	packageCache.Lock()
	if cached, ok := packageCache.byTags[tags]; ok {
		packageCache.Unlock()
		return cached
	}
	packageCache.byTags[tags] = pkgs
	packageCache.Unlock()
	return pkgs
}

// owner returns the subsystem that owns an import path, or "" if none does. A
// package's OWN subtree does not count as another subsystem — the case this was
// written for. The generated stubs now live at pkg/transport/gen/... (ADR-0008 part
// 3), owned by no subsystem, so importing them is not an edge for the simpler
// reason: `owner` returns "" for them. The own-subtree rule stays because it is a
// property of the guard, not of where `gen` happens to sit.
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

// TestSubsystemsCannotReachEachOther is the TRANSITIVE form of the rule above, and it
// closes that check's blind spot: it only looks at DIRECT imports, so `network` importing
// `ruleset` which imports `runtime` is a real network/runtime edge that passes it. The
// leaf guard below covers that only for the packages named in `leaves`, and membership is
// a manual step — a shared package nobody listed gets neither check. Reachability needs
// no registration, so coverage stops depending on someone remembering.
//
// The failure prints the PATH, not just the endpoints: "runtime reaches volume" is not
// actionable, and the hops are where the fix is.
//
// PRODUCTION IMPORTS ONLY, deliberately: anything reaching test/harness transitively
// reaches every subsystem through orchestrator, so a closure over test imports would
// report a path for every subsystem on a correct tree. The direct check above keeps
// scanning them, which is where that coverage stays. See ADR-0006, including why
// deriving `leaves` from "imported by ≥2 subsystems" was rejected instead.
func TestSubsystemsCannotReachEachOther(t *testing.T) {
	for _, tags := range tagMatrix {
		imports := map[string][]string{}
		for _, p := range listPackages(t, tags) {
			imports[p.ImportPath] = p.Imports
		}
		for _, start := range subsystems {
			// BFS, carrying the path so a hit can report how it got there.
			type step struct {
				pkg  string
				path []string
			}
			seen := map[string]bool{start: true}
			queue := []step{{pkg: start, path: []string{start}}}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, imp := range imports[cur.pkg] {
					if seen[imp] {
						continue
					}
					seen[imp] = true
					path := append(append([]string{}, cur.path...), imp)
					// owner() so a subsystem's own subtree (transport/gen/...) is not a hit,
					// matching the direct check's rule.
					if got := owner(imp); got != "" && got != start {
						t.Errorf("subsystem %s can REACH subsystem %s (tags=%q) via:\n    %s\n"+
							"§5 subsystems must not call each other, transitively included: every "+
							"package on that path inherits the edge while the direct-import guard "+
							"stays green. Put the shared fact in a leaf that imports nothing from "+
							"the engine (ADR-0006).",
							start, got, tags, strings.Join(path, " -> "))
						continue
					}
					queue = append(queue, step{pkg: imp, path: path})
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
//   - pkg/transport/gen/... — the generated wire stubs. Not a leaf by CLAUDE.md's
//     "shared fact" motive; listed because it lives OUTSIDE internal/ (ADR-0008 part
//     3, so a Go consumer can import the protocol) and the property that made that
//     move safe is exactly the one this guard checks: the generated tree imports
//     nothing from the engine. Left unchecked, a hand-edit adding an engine import
//     there would put an engine type in the published surface and hand every
//     importer a transitive edge.
//
// generatedTree is the root of the committed wire stubs (ADR-0008 part 3). Named
// rather than spelled inline because the leaf guard's exemption below has to mean
// exactly this prefix: a literal that drifted from the `leaves` entries would turn
// the exemption into a hole nobody could see.
const generatedTree = "github.com/TSHOGX/khora/pkg/transport/gen/"

var leaves = []string{
	"github.com/TSHOGX/khora/internal/guestport",
	"github.com/TSHOGX/khora/internal/ruleset",
	"github.com/TSHOGX/khora/internal/btrfs",
	"github.com/TSHOGX/khora/internal/allocbytes",
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1",
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect",
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
	"github.com/TSHOGX/khora/internal/guestport",
	"github.com/TSHOGX/khora/internal/ruleset",
}

// actsOnTheWorld are stdlib packages a leaf cannot reach without doing the thing
// CLAUDE.md forbids. Process execution and raw syscalls are the mechanism-smuggling
// routes that matter (shelling out to `nft` / `ip`, or an ioctl); the filesystem and
// network packages are here because "touch no substrate" covers reading it too — a
// leaf that stats a path has made its answer depend on the host it runs on, so the
// generate-side and install-side runs stop agreeing.
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
					if !strings.HasPrefix(imp, "github.com/TSHOGX/khora/") {
						continue
					}
					// The generated tree is two packages by protoc's construction — the
					// connect stubs necessarily import the message types — so an import
					// that stays INSIDE it is not the thing this guard is about. What is
					// checked there is the property that made ADR-0008's move safe:
					// nothing from the ENGINE, and every engine package is under the
					// module's internal/, so this exemption cannot widen to one.
					if strings.HasPrefix(p.ImportPath, generatedTree) && strings.HasPrefix(imp, generatedTree) {
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
