package main

import "testing"

func TestInertPath(t *testing.T) {
	cases := map[string]bool{
		"docs/operations/testing.md":           true,
		".github/workflows/ci.yml":             true,
		".gitignore":                           true,
		"scripts/check-no-direct-main.sh":      true,
		"scripts/install-hooks.sh":             true,
		"scripts/check-generated.sh":           true,
		"internal/arch/coupling_test.go":       true,
		"test/preflight/substrategate/main.go": true,
		"test/e2e_substrate_test.go":           false,
		"templates/bare/build.sh":              false,
		"test/preflight/build-rootfs.sh":       false,
		"proto/khora/transport/v1/x.proto":     false,
		"go.mod":                               false,
	}
	for path, want := range cases {
		if got := inertPath(path); got != want {
			t.Errorf("inertPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// The whole tool rests on sameCode, and sameCode licenses skipping a gate — so the
// cases that matter are the ones where a naive implementation says "comment-only"
// about something that changes the build. Each false case below is a real way to
// smuggle a behaviour change past a line-prefix grep of the kind this replaces.
func TestSameCode(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     bool
	}{{
		name: "a rewritten line comment is the case this exists to allow",
		old:  "package p\n\n// old wording\nvar X = 1\n",
		new:  "package p\n\n// new wording entirely\nvar X = 1\n",
		want: true,
	}, {
		// A go/printer-based implementation gets this wrong: growing the comment
		// moves every code position after it, and the printer's blank-line
		// decisions follow positions. Token streams carry no positions.
		name: "a comment that grows by many lines still leaves the tokens identical",
		old:  "package p\n\n// one line\nvar X = 1\n",
		new:  "package p\n\n// one\n// two\n// three\n// four\n// five\nvar X = 1\n",
		want: true,
	}, {
		name: "a block comment is not special",
		old:  "package p\n\n/* before */\nvar X = 1\n",
		new:  "package p\n\n/* after\n   and more */\nvar X = 1\n",
		want: true,
	}, {
		// The line does not start with "//", so a prefix grep counts it as code.
		name: "a trailing comment shares its line with code",
		old:  "package p\n\nvar X = 1 // was\n",
		new:  "package p\n\nvar X = 1 // is\n",
		want: true,
	}, {
		name: "deleting a comment outright is still comment-only",
		old:  "package p\n\n// doomed\nvar X = 1\n",
		new:  "package p\n\nvar X = 1\n",
		want: true,
	}, {
		name: "a changed value is code",
		old:  "package p\n\nvar X = 1\n",
		new:  "package p\n\nvar X = 2\n",
		want: false,
	}, {
		name: "a renamed identifier is code",
		old:  "package p\n\nvar X = 1\n",
		new:  "package p\n\nvar Y = 1\n",
		want: false,
	}, {
		// The "+" line starts with "//" on the removed side and with code on the
		// added side, so a grep that only inspects added lines can miss it.
		name: "uncommenting code is code",
		old:  "package p\n\n// var X = 1\nvar Y = 2\n",
		new:  "package p\n\nvar X = 1\nvar Y = 2\n",
		want: false,
	}, {
		// The line contains "//" and a grep for that substring calls it a comment.
		name: "a string literal containing a slash-slash is code",
		old:  "package p\n\nvar U = \"http://a\"\n",
		new:  "package p\n\nvar U = \"http://b\"\n",
		want: false,
	}, {
		// The sharpest case in this repo: build tags decide whether a file
		// compiles at all, and this repo gates whole test files on them.
		name: "a //go:build constraint is a build change in a comment's clothes",
		old:  "//go:build substrate\n\npackage p\n\nvar X = 1\n",
		new:  "//go:build substrate && drainrace\n\npackage p\n\nvar X = 1\n",
		want: false,
	}, {
		name: "the legacy // +build spelling counts too",
		old:  "// +build linux\n\npackage p\n\nvar X = 1\n",
		new:  "// +build linux,amd64\n\npackage p\n\nvar X = 1\n",
		want: false,
	}, {
		// //go:embed decides what bytes land in the binary.
		name: "a //go:embed directive decides the binary's contents",
		old:  "package p\n\nimport _ \"embed\"\n\n//go:embed a.txt\nvar B []byte\n",
		new:  "package p\n\nimport _ \"embed\"\n\n//go:embed b.txt\nvar B []byte\n",
		want: false,
	}, {
		name: "a //go:generate directive changes what regeneration produces",
		old:  "package p\n\n//go:generate buf generate\nvar X = 1\n",
		new:  "package p\n\n//go:generate buf generate --template x.yaml\nvar X = 1\n",
		want: false,
	}, {
		// PERMUTING two directives, with the same two still present: the token stream AND
		// the set of directives both stay identical, so only a positional comparison
		// catches it (the measurement is on `directives`).
		name: "permuting two //go:embed directives changes the binary",
		old:  "package p\n\nimport _ \"embed\"\n\n//go:embed a.txt\nvar First string\n\n//go:embed b.txt\nvar Second string\n",
		new:  "package p\n\nimport _ \"embed\"\n\n//go:embed b.txt\nvar First string\n\n//go:embed a.txt\nvar Second string\n",
		want: false,
	}, {
		name: "adding a directive where there was none is still a directive change",
		old:  "package p\n\nvar X = 1\n",
		new:  "package p\n\n//go:noinline\nfunc f() {}\n\nvar X = 1\n",
		want: false, // also a token change; both reasons point the same way
	}, {
		// In a cgo file the preamble comment IS C source.
		name: "a comment in a cgo file cannot be assumed inert",
		old:  "package p\n\n/*\n#include <stdio.h>\n*/\nimport \"C\"\n",
		new:  "package p\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\n",
		want: false,
	}, {
		name: "an unscannable new version is undecidable, so not proven",
		old:  "package p\n\nvar X = 1\n",
		new:  "package p\n\nvar S = `unterminated\n",
		want: false,
	}, {
		name: "an unscannable old version is undecidable, so not proven",
		old:  "package p\n\nvar S = `unterminated\n",
		new:  "package p\n\nvar X = 1\n",
		want: false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := sameCode("x.go", []byte(c.old), []byte(c.new))
			if got != c.want {
				t.Fatalf("sameCode = %v (reason %q), want %v", got, why, c.want)
			}
			if !got && why == "" {
				t.Error("a false verdict must carry a reason — the reason is what a reviewer reads")
			}
		})
	}
}

// Identical input must be proven same, or the tool would demand the gate for a
// no-op and nobody would keep using it.
func TestSameCodeOnIdenticalInputIsProven(t *testing.T) {
	src := []byte("//go:build substrate\n\npackage p\n\n// c\nvar X = 1 // trailing\n")
	if ok, why := sameCode("x.go", src, src); !ok {
		t.Fatalf("identical sources judged different: %s", why)
	}
}
