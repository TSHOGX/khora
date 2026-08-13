// Command substrategate answers one question mechanically: does this diff require
// the substrate acceptance suite to be re-run?
//
// It answers yes unless every changed file is either documentation (.md, which
// cannot affect a running sandbox) or a .go file whose TOKEN STREAM is unchanged
// with no toolchain directive edited (ADR-0005 argues why that is the right
// equivalence). Anything else — a new or deleted .go file, go.mod, a .proto, a
// shell script, a template — is a yes.
//
// Usage:
//
//	go run ./test/preflight/substrategate          # the working tree against HEAD
//	go run ./test/preflight/substrategate <rev>    # one commit against its parent
//
// Exit codes: 0 = the gate is not required by this diff · 1 = the gate is required
// · 2 = this tool could not decide, so the gate is required. Both non-zero codes
// mean "run it"; they are distinct only so a caller can tell a decision from a
// malfunction. Never treat a crash as a licence to skip — see ADR-0005.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	rev := ""
	switch {
	case len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help"):
		fmt.Print(usage)
		return
	case len(os.Args) == 2:
		rev = os.Args[1]
	case len(os.Args) > 2:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		fail("not a git repository: %v", err)
	}
	changes, err := collect(rev)
	if err != nil {
		fail("cannot list changes: %v", err)
	}

	var reasons []string
	docs, code := 0, 0
	for _, c := range changes {
		switch {
		case strings.HasSuffix(c.path, ".md"):
			docs++
		case !strings.HasSuffix(c.path, ".go"):
			reasons = append(reasons, fmt.Sprintf("%s: not documentation and not Go (%s)", c.path, c.statusWord()))
		case c.status != "M":
			reasons = append(reasons, fmt.Sprintf("%s: %s, so it carries new or removed code", c.path, c.statusWord()))
		default:
			oldSrc, newSrc, err := versions(root, rev, c.path)
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
	if rev != "" {
		what = rev + " against its parent"
	}
	fmt.Printf("substrategate: %d changed file(s), %s\n", len(changes), what)
	fmt.Printf("  documentation ignored:  %d .md\n", docs)
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
	fmt.Println("         The build, vet and unit gates still are (ADR-0005).")
}

const usage = `usage: substrategate [<rev>]

  (no argument)  compare the working tree against HEAD
  <rev>          compare that commit against its parent

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

// collect lists the changed paths. In working-tree mode untracked files count as
// additions: an untracked .go file is new code, and treating it as "no change"
// would be the one omission that silently licenses skipping the gate.
func collect(rev string) ([]change, error) {
	var out []change
	var raw string
	var err error
	if rev == "" {
		raw, err = git("diff", "--name-status", "HEAD")
	} else {
		raw, err = git("diff", "--name-status", rev+"^", rev)
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			return nil, fmt.Errorf("unparsable --name-status line %q", line)
		}
		// For a rename or copy the last field is the new path, which is the one
		// whose content decides.
		out = append(out, change{status: f[0], path: f[len(f)-1]})
	}
	if rev != "" {
		return out, nil
	}
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
func versions(root, rev, path string) (oldSrc, newSrc []byte, err error) {
	base, head := "HEAD", ""
	if rev != "" {
		base, head = rev+"^", rev
	}
	o, err := git("show", base+":"+path)
	if err != nil {
		return nil, nil, err
	}
	if head == "" {
		n, err := os.ReadFile(filepath.Join(root, path))
		return []byte(o), n, err
	}
	n, err := git("show", head+":"+path)
	return []byte(o), []byte(n), err
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
