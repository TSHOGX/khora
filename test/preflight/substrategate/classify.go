package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"sort"
	"strings"
)

// tokenPair is one token as the compiler's front end sees it: its kind plus its
// literal text. Positions are deliberately absent — they are what makes two
// otherwise identical files differ after a comment above them grows.
type tokenPair struct{ tok, lit string }

func (p tokenPair) String() string {
	if p.lit == "" {
		return p.tok
	}
	if len(p.lit) > 40 {
		return p.tok + " " + p.lit[:40] + "…"
	}
	return p.tok + " " + p.lit
}

// sameCode reports whether two versions of one Go file are the same code to the
// toolchain: an identical token stream, and no change to a comment the toolchain
// itself acts on.
//
// The token stream is the right equivalence because it is exactly what the
// compiler's front end consumes — go/scanner discards comments and whitespace, so
// two files with the same token sequence differ only in what the compiler already
// throws away. That is both stronger and more honest than "comment-only", a
// property that cannot be stated precisely: a trailing comment shares its line
// with code, a block comment spans lines, and a string literal may contain "//".
//
// Directives are why this is not merely a token comparison. //go:build,
// //go:embed, //go:generate, //line and the cgo forms are comments the scanner
// discards and the toolchain still obeys — //go:build decides whether the file
// compiles at all, //go:embed decides what lands in the binary. A change to one of
// those is a build change wearing a comment's clothes, so it disqualifies.
//
// Every failure path returns false: the caller's question is "may the substrate
// gate be skipped", and the safe answer whenever this cannot be proven is no.
func sameCode(path string, oldSrc, newSrc []byte) (bool, string) {
	oldTok, err := tokens(path, oldSrc)
	if err != nil {
		return false, fmt.Sprintf("the old version does not scan (%v)", err)
	}
	newTok, err := tokens(path, newSrc)
	if err != nil {
		return false, fmt.Sprintf("the new version does not scan (%v)", err)
	}
	if len(oldTok) != len(newTok) {
		return false, fmt.Sprintf("token stream changed (%d → %d tokens)", len(oldTok), len(newTok))
	}
	for i := range oldTok {
		if oldTok[i] != newTok[i] {
			return false, fmt.Sprintf("token stream changed at token %d (%v → %v)",
				i+1, oldTok[i], newTok[i])
		}
	}
	// A cgo file's preamble comment is compiled as C source, so no comment in it
	// can be assumed inert. Detected from the token stream (a "C" string literal)
	// rather than by grepping the text, and deliberately over-broad: a stray "C"
	// literal costs one gate run, a missed cgo preamble costs the property.
	if hasCImport(oldTok) || hasCImport(newTok) {
		return false, `a "C" literal is present: in a cgo file the preamble comment is source`
	}
	if d := firstDiff(directives(path, oldSrc), directives(path, newSrc)); d != "" {
		return false, "toolchain directive changed: " + d
	}
	return true, ""
}

// tokens returns the file's token stream with comments and whitespace discarded.
// A scan error is reported rather than swallowed — an unparseable file is exactly
// the case where a silent "no tokens" would look like "nothing changed".
func tokens(path string, src []byte) ([]tokenPair, error) {
	var fset token.FileSet
	f := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	var scanErr error
	s.Init(f, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, 0) // mode 0: comments are skipped, which is the point
	var out []tokenPair
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		out = append(out, tokenPair{tok: tok.String(), lit: lit})
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return out, nil
}

// directives returns the sorted set of comment lines the toolchain acts on.
// Detection is liberal on purpose (leading spaces, tabs and block-comment '*'
// are trimmed first): over-detecting costs one gate run, under-detecting silently
// licenses a build change.
func directives(path string, src []byte) []string {
	var fset token.FileSet
	f := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, scanner.ScanComments)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		for _, line := range strings.Split(lit, "\n") {
			line = strings.TrimLeft(line, " \t*")
			if isDirective(line) {
				out = append(out, strings.TrimRight(line, " \t\r"))
			}
		}
	}
	sort.Strings(out)
	return out
}

func isDirective(line string) bool {
	for _, p := range []string{"//go:", "//line ", "/*line ", "//export ", "//sys ", "//sysnb "} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	// The legacy build-tag spellings go/build still honours.
	return strings.HasPrefix(line, "// +build") || strings.HasPrefix(line, "//+build")
}

func hasCImport(toks []tokenPair) bool {
	for _, t := range toks {
		if t.tok == token.STRING.String() && t.lit == `"C"` {
			return true
		}
	}
	return false
}

// firstDiff returns a human-readable description of the first difference between
// two sorted sets, or "" when they are equal.
func firstDiff(oldSet, newSet []string) string {
	for i := 0; i < len(oldSet) || i < len(newSet); i++ {
		switch {
		case i >= len(newSet):
			return "removed " + oldSet[i]
		case i >= len(oldSet):
			return "added " + newSet[i]
		case oldSet[i] != newSet[i]:
			return oldSet[i] + " → " + newSet[i]
		}
	}
	return ""
}
