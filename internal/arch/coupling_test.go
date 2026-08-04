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
// volume (§5.C) and network (§5.D) are listed before they exist on purpose: the
// guard should catch a bad edge the day the package lands, not the day someone
// remembers to add it here. Absent packages are skipped, and staleness is checked
// separately below.
var subsystems = []string{
	"engine/internal/runtime",   // §5.A
	"engine/internal/rootfs",    // §5.B
	"engine/internal/volume",    // §5.C (not yet built)
	"engine/internal/network",   // §5.D (not yet built)
	"engine/internal/transport", // §5.E
}

// mediatorOnly are packages a §5 subsystem must never import. A subsystem that
// reads the store has stopped being a mechanism: §4 hands it the facts it needs
// (the keep-set pattern), precisely so the subsystem cannot form its own opinion
// about authoritative state.
var mediatorOnly = []string{
	"engine/internal/lifecycle",
	"engine/internal/store",
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

// listPackages runs `go list -json` for the given tags. Build tags matter: the
// substrate tests are behind `//go:build substrate` and are invisible to a
// default list, so a bad edge introduced in one of them would go unseen.
func listPackages(t *testing.T, tags string) []pkg {
	t.Helper()
	args := []string{"list", "-json"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "engine/...")
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
	for _, tags := range []string{"", "substrate"} {
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
	for _, tags := range []string{"", "substrate"} {
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

// TestGuardIsNotStale fails if the subsystem list has drifted from reality —
// otherwise renaming a package silently disables the checks above, which is worse
// than not having them (they would still pass, and read as coverage).
func TestGuardIsNotStale(t *testing.T) {
	present := map[string]bool{}
	for _, p := range listPackages(t, "") {
		present[p.ImportPath] = true
	}
	// The three built subsystems must exist. volume/network are deliberately
	// absent until their milestones land, so they are not required here.
	for _, s := range []string{
		"engine/internal/runtime",
		"engine/internal/rootfs",
		"engine/internal/transport",
	} {
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
