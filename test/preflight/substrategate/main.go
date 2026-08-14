// Command substrategate answers one question mechanically: does this diff require
// the substrate acceptance suite to be re-run?
//
// It answers yes unless every changed file is either documentation, a process-only
// repository file, or a .go file whose token stream is unchanged with no toolchain
// directive edited. Build inputs, templates and test harnesses remain fail-closed.
//
// Usage:
//
//	go run ./test/preflight/substrategate                    # working tree against HEAD
//	go run ./test/preflight/substrategate --base origin/main # complete PR plus working tree
//
// Exit codes: 0 = the gate is not required by this diff · 1 = the gate is required
// · 2 = this tool could not decide, so the gate is required. Both non-zero codes
// mean "run it"; they are distinct only so a caller can tell a decision from a
// malfunction. Never treat a crash as a licence to skip.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	base := ""
	switch {
	case len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help"):
		fmt.Print(usage)
		return
	case len(os.Args) == 3 && os.Args[1] == "--base":
		base = os.Args[2]
	case len(os.Args) > 1:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		fail("not a git repository: %v", err)
	}
	comparison, err := comparisonRef(base)
	if err != nil {
		fail("cannot resolve comparison: %v", err)
	}
	changes, err := collect(comparison)
	if err != nil {
		fail("cannot list changes: %v", err)
	}

	var reasons []string
	inert, code := 0, 0
	for _, c := range changes {
		switch {
		case inertPath(c.path):
			inert++
		case !strings.HasSuffix(c.path, ".go"):
			reasons = append(reasons, fmt.Sprintf("%s: not an inert repository file and not Go (%s)", c.path, c.statusWord()))
		case c.status != "M":
			reasons = append(reasons, fmt.Sprintf("%s: %s, so it carries new or removed code", c.path, c.statusWord()))
		default:
			oldSrc, newSrc, err := versions(root, comparison, c.path)
			if err != nil {
				fail("cannot read both versions of %s: %v", c.path, err)
			}
			if ok, why := sameCode(c.path, oldSrc, newSrc); ok {
				code++
			} else {
				reasons = append(reasons, c.path+": "+why)
			}
		}
	}

	what := "the working tree against HEAD"
	if base != "" {
		what = "the complete PR diff from " + base + " plus the working tree"
	}
	fmt.Printf("substrategate: %d changed file(s), %s\n", len(changes), what)
	fmt.Printf("  inert repository files: %d\n", inert)
	fmt.Printf("  token stream unchanged: %d .go\n", code)
	if len(reasons) > 0 {
		fmt.Printf("  requires the gate:      %d\n", len(reasons))
		for _, r := range reasons {
			fmt.Println("    - " + r)
		}
		fmt.Println("VERDICT: the substrate gate IS required for this diff.")
		os.Exit(1)
	}
	if len(changes) == 0 {
		fmt.Println("VERDICT: nothing changed, so there is nothing for the gate to verify.")
		return
	}
	fmt.Println("VERDICT: the substrate gate is NOT required for this diff.")
	fmt.Println("         Build, vet and unit gates still apply.")
}

// inertPath identifies files that can change how the repository is checked but
// cannot change a running sandbox. Hosted build and unit checks still apply; this
// classification only decides whether a real substrate run adds evidence.
func inertPath(path string) bool {
	if strings.HasSuffix(path, ".md") || path == ".gitignore" || strings.HasPrefix(path, ".github/") {
		return true
	}
	if strings.HasSuffix(path, "_test.go") && !strings.HasSuffix(path, "_substrate_test.go") {
		return true
	}
	if strings.HasPrefix(path, "test/preflight/substrategate/") {
		return true
	}
	// scripts/ is reserved for repository checks and local collaboration helpers.
	// Runtime fixtures and Template build inputs live under test/ and templates/.
	return strings.HasPrefix(path, "scripts/")
}

const usage = `usage: substrategate [--base <ref>]

  (no argument)             compare the working tree against HEAD
  --base <ref>              compare <ref>...HEAD, plus the working tree

exit: 0 gate not required · 1 gate required · 2 undecidable (gate required)
`

type change struct{ status, path string }

func (c change) statusWord() string {
	switch {
	case c.status == "A":
		return "added"
	case c.status == "D":
		return "deleted"
	case c.status == "T":
		return "type changed"
	case strings.HasPrefix(c.status, "R"):
		return "renamed"
	case strings.HasPrefix(c.status, "C"):
		return "copied"
	default:
		return "modified"
	}
}

func comparisonRef(base string) (string, error) {
	if base == "" {
		return "HEAD", nil
	}
	if _, err := git("rev-parse", "--verify", base+"^{commit}"); err != nil {
		return "", fmt.Errorf("base %q is not available: %w", base, err)
	}
	return git("merge-base", base, "HEAD")
}

// collect lists the paths changed from comparison to the current working tree.
// Untracked files count as additions: an untracked .go file is new code, and
// treating it as no change would silently license skipping the substrate suite.
func collect(comparison string) ([]change, error) {
	var out []change
	raw, err := git("diff", "--name-status", comparison)
	if err != nil {
		return nil, err
	}
	appendChanges := func(raw string) error {
		for _, line := range strings.Split(raw, "\n") {
			if line == "" {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) < 2 {
				return fmt.Errorf("unparsable --name-status line %q", line)
			}
			// For a rename or copy the last field is the new path, which is the one
			// whose content decides.
			out = append(out, change{status: f[0], path: f[len(f)-1]})
		}
		return nil
	}
	if err := appendChanges(raw); err != nil {
		return nil, err
	}
	// Untracked files count as additions in either comparison mode.
	untracked, err := git("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\n") {
		if p != "" {
			out = append(out, change{status: "A", path: p})
		}
	}
	return out, nil
}

// versions returns the before and after contents of one path.
func versions(root, comparison, path string) (oldSrc, newSrc []byte, err error) {
	o, err := git("show", comparison+":"+path)
	if err != nil {
		return nil, nil, err
	}
	n, err := os.ReadFile(filepath.Join(root, path))
	return []byte(o), n, err
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// fail exits 2: undecidable, which the caller must read as "run the gate".
func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "substrategate: "+format+"\n", a...)
	fmt.Fprintln(os.Stderr, "VERDICT: undecidable — run the substrate gate.")
	os.Exit(2)
}
