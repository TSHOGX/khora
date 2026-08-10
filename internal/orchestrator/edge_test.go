package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// The §6.2 error-edge guard: the control plane must not put a raw error's text into
// a response body, because the errors arriving there interpolate ABSOLUTE HOST PATHS
// (internal/btrfs wraps every substrate call as `btrfs: <verb> <path>: %w`, and that
// path is VolumesDir/<volume-id>). §6.2 admits opaque IDs and bars host paths and
// placement.
//
// This is enforced STRUCTURALLY rather than by review for the same reason
// internal/arch and runtime/seam_test.go are: the defect was TEN call sites each
// writing their own body, so the safe shape was one someone had to remember at every
// new arm. A single funnel (writeErr) plus this scan inverts that — a new endpoint
// leaks only if someone deletes a test, never by forgetting one.
//
// It reads the AST rather than grepping so it constrains CALLS, not comments: the
// funnel's own doc comment necessarily quotes the leaky shape it replaced, and a
// text scan would fail on the explanation of why it is wrong.

// edgeSources returns the production files scanned: EVERY non-test .go file in this
// package, read from the directory rather than listed by hand.
//
// A hand-written list is the one failure mode this guard cannot survive, because it
// fails in the direction of passing. Measured: `network.go` arrived carrying two §6
// endpoints, was not added to the list, and a raw
// `http.Error(w, "leak "+err.Error(), 500)` planted in handleNetworkGet passed BOTH
// scanners green. That is precisely the "a new endpoint leaks only if someone deletes
// a test, never by forgetting one" property above — and a per-file list reinstates the
// forgetting. Enumerating the directory means a new production file is scanned the day
// it lands, with nothing to remember.
//
// Test files are excluded on purpose — a test asserting on a raw error is doing its job.
func edgeSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var srcs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		srcs = append(srcs, name)
	}
	// A glob that silently matches nothing is a guard that passes vacuously — the same
	// defect as the stale list, reached by a different route. api.go holds the §6 router
	// and cannot legitimately disappear, so it doubles as the liveness anchor.
	if !slices.Contains(srcs, "api.go") {
		t.Fatalf("enumeration found no api.go (got %v) — this scan is not reading the package", srcs)
	}
	return srcs
}

// errorTextExemptions are the functions permitted to read an error's text, keyed by
// name WITH THE REASON. Two entries, and both are the sanitizer or its logger — an
// exemption without a reason is a bare allowlist, and the next leak walks through the
// same door citing the precedent.
var errorTextExemptions = map[string]string{
	"sanitizeErr": "IS the sanitizer — reading the text is the whole job; it redacts host-shaped tokens before returning",
	"proxyErr":    "data-plane sanitizer (D5.1): logs the real cause engine-side and returns a placement-free message composed from the CODE alone — the inner text never crosses",
}

// scanErrorTextReads and scanHTTPErrorBodies ARE the two guards. They are functions
// rather than inline walks so that the reverse-injection test below drives the SAME code
// the production scan drives, over source carrying the defect on purpose.
//
// The alternative — a second copy of the walk inside that test — checks a copy, not a
// guard: the real scanner could narrow to nothing and the reverse-injection test would
// keep passing on its own duplicate, which is precisely the reassurance-without-coverage
// the volume/seam_test.go precedent exists to avoid.
//
// `report` receives the position and the enclosing function name; the caller decides
// whether that is a failure or a count.
func scanErrorTextReads(fset *token.FileSet, f *ast.File, report func(pos token.Position, enclosing string)) {
	// enclosing tracks which function body the walk is inside, so a violation can name
	// it and an exemption can be scoped to it.
	var enclosing string
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			enclosing = d.Name.Name
		case *ast.FuncLit:
			// Keep the enclosing name: a closure inside a handler is still that handler
			// as far as this rule is concerned.
		case *ast.CallExpr:
			sel, ok := d.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" || len(d.Args) != 0 {
				return true
			}
			if _, exempt := errorTextExemptions[enclosing]; exempt {
				return true
			}
			report(fset.Position(d.Pos()), enclosing)
		}
		return true
	})
}

// TestControlPlaneErrorsGoThroughTheFunnel: no production file in this package reads
// an error's text outside the sanitizer.
//
// `err.Error()` is the exact spelling of the leak — `http.Error(w, err.Error(), code)`
// was the defect in all ten arms of the old statusFor — so banning the READ, not just
// the write, is what makes the property hold: a message that never becomes an
// unsanitized string cannot be written into a body by any route, including
// fmt.Sprintf, string concatenation or a JSON field.
func TestControlPlaneErrorsGoThroughTheFunnel(t *testing.T) {
	for _, src := range edgeSources(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		scanErrorTextReads(fset, f, func(pos token.Position, enclosing string) {
			t.Errorf("%s: %s calls .Error() outside the sanitizer — an error's raw text "+
				"carries absolute host paths (btrfs wraps every call as `<verb> <path>: %%w`) "+
				"and §6.2 keeps those off the wire. Route it through writeErr/statusFor instead.",
				pos, enclosing)
		})
	}
}

// TestHTTPErrorBodiesAreLiteralsOrSanitized: every http.Error in a production file
// passes either a string LITERAL (a message the engine authored, with nothing
// interpolated) or nothing at all — the dynamic case must go through writeErr.
//
// This is the second half of the same fence. The first half stops an error's text
// from being read; this one stops a host path from reaching a body by a route that
// never touches an error — `http.Error(w, "cannot open "+dir, 500)` reads perfectly
// innocently and leaks just as much.
func TestHTTPErrorBodiesAreLiteralsOrSanitized(t *testing.T) {
	for _, src := range edgeSources(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		scanHTTPErrorBodies(fset, f, func(pos token.Position, enclosing string) {
			t.Errorf("%s: %s calls http.Error with a non-literal message — anything composed at "+
				"runtime can carry a host path (§6.2). Use writeErr, which logs the full "+
				"error and sanitizes the body.", pos, enclosing)
		})
	}
}

// scanHTTPErrorBodies is the second guard — see scanErrorTextReads for why both are
// functions.
func scanHTTPErrorBodies(fset *token.FileSet, f *ast.File, report func(pos token.Position, enclosing string)) {
	var enclosing string
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok {
			enclosing = d.Name.Name
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Error" || len(call.Args) != 3 {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return true
		}
		if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return true
		}
		// writeErr is the ONE function that may write a composed body — it is the
		// funnel, and the composition it writes is sanitizeErr's output. Exactly one
		// exemption, and it is the sink every other site is required to use.
		if enclosing == "writeErr" {
			return true
		}
		report(fset.Position(call.Pos()), enclosing)
		return true
	})
}

// TestTheGuardsActuallyFail drives THE TWO SCANNERS THEMSELVES over synthetic source
// carrying the exact defect, because a guard whose failure path is never exercised is a
// guard nobody has checked (volume/seam_test.go's reverse-injection precedent).
//
// It calls scanErrorTextReads / scanHTTPErrorBodies rather than re-walking the AST: an
// inline duplicate of the walk would keep passing while the real scanner narrowed to
// nothing, which is the failure this test exists to rule out.
//
// The fixture also exercises the EXEMPTIONS in the direction that can go wrong. An
// exemption is keyed by enclosing function name, so a leak inside a function that happens
// to be called `sanitizeErr` or `writeErr` is waved through — which is correct for the
// real ones and would be a hole if the key ever matched more broadly.
func TestTheGuardsActuallyFail(t *testing.T) {
	const leaky = `package orchestrator

import "net/http"

func handle(w http.ResponseWriter, err error, dir string) {
	http.Error(w, err.Error(), 500)
	http.Error(w, "cannot open "+dir, 500)
}

func sanitizeErr(err error) string { return err.Error() }

func writeErr(w http.ResponseWriter, err error) {
	http.Error(w, "engine: "+sanitizeErr(err), 500)
}
`
	dir := t.TempDir()
	path := dir + "/leaky.go"
	if err := os.WriteFile(path, []byte(leaky), 0o644); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var rawReads, composedBodies []string
	scanErrorTextReads(fset, f, func(_ token.Position, enclosing string) {
		rawReads = append(rawReads, enclosing)
	})
	scanHTTPErrorBodies(fset, f, func(_ token.Position, enclosing string) {
		composedBodies = append(composedBodies, enclosing)
	})

	// EXACTLY the unexempted sites, named. A count alone would be satisfied by the
	// scanner finding the wrong site and missing the right one.
	if want := []string{"handle"}; !slices.Equal(rawReads, want) {
		t.Errorf("the .Error() scanner reported %v, want %v — `handle` reads the text and must be "+
			"caught; `sanitizeErr` reads it too and is the exempted sanitizer, so a scanner that "+
			"reports both has lost its exemption and one that reports neither would not have caught "+
			"the original defect", rawReads, want)
	}
	if want := []string{"handle", "handle"}; !slices.Equal(composedBodies, want) {
		t.Errorf("the http.Error scanner reported %v, want %v — both of `handle`'s bodies are "+
			"non-literal (one from an error, one from plain concatenation, which leaks just as much), "+
			"and `writeErr` is the single permitted composed body", composedBodies, want)
	}
	if !strings.Contains(leaky, "err.Error()") {
		t.Fatal("fixture no longer contains the defect it exists to reproduce")
	}
}
