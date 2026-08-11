# ADR-0005 — When the substrate gate may be skipped, and how that is proven

- **Status:** accepted
- **Date:** 2026-08-11
- **Consumers:** anyone committing to this repo; `CLAUDE.md` (Execution & verification) points here.

## Context

Correctness in this engine lives in concurrency and failure invariants, not at
compile time, so the acceptance suite runs against the real substrate: `sudo …
-tags substrate -p 1 ./...`, ~280 s, root, real runsc/overlayfs/btrfs/nftables.
The standing rule is to run it before committing anything that touches a
substrate subsystem or `test/`.

The M9 translation pass made a gap in that rule visible. Translating `docs/spec/`
requires moving each document's verbatim code citations in the same commit, so
those commits edit `.go` files — only inside comments, so that a quote and its
source stay in step. Seven such commits each ran the full substrate gate, about
32 minutes, on diffs the compiler cannot distinguish from their parents. Two of
those runs surfaced **pre-existing harness flakes** that cost real investigation
and produced two genuine fixes, but neither was information about the diff under
test. Gate time spent on a diff that cannot change behaviour is not caution; it
trains the habit of treating a red gate as noise, which is the expensive failure.

The rule cannot simply be relaxed by judgement. "It is only comments" is a claim
a committer makes about their own diff, and the reviewer has no way to re-check
it — which is exactly the shape this repo refuses elsewhere (subsystem isolation
is a test, not a review note).

## Decision

**The substrate gate may be skipped for a diff only when a mechanical check
proves the diff cannot change compiled behaviour, and the committer records that
tool's verdict in the commit message.** The check is
`test/preflight/substrategate`:

```sh
go run ./test/preflight/substrategate          # the working tree against HEAD
go run ./test/preflight/substrategate <rev>    # one commit against its parent
```

It answers "not required" only when **every** changed file is either:

- a `.md` file — documentation cannot affect a running sandbox; or
- a `.go` file whose **token stream is unchanged** and in which **no toolchain
  directive was edited**.

Everything else is "required": a new or deleted `.go` file, `go.mod`, a `.proto`,
a shell script, a template, an untracked file, or any file the tool cannot read.

**The build, vet and unit gates are never skipped** (`go build ./... && go vet
./... && go test ./...`). This ADR narrows one gate, not the discipline.

Exit codes: `0` not required · `1` required · `2` undecidable. **Both non-zero
codes mean run it**; they differ only so a caller can tell a decision from a
malfunction. A crash is never a licence to skip.

## Rationale

**The token stream is the right equivalence, and "comment-only" is not.** The
compiler's front end consumes tokens: `go/scanner` discards comments and
whitespace. Two files with the same token sequence therefore differ only in what
the compiler already throws away, which is a statement about the program rather
than about the diff's appearance. "Comment-only" cannot be stated precisely — a
trailing comment shares its line with code, a block comment spans lines, a string
literal may contain `//`, and deleting a `//` turns a comment into code. Every one
of those is a case where a line-prefix grep gives the wrong answer, and each is
pinned as a test case in `classify_test.go`.

**Directives are the exception that keeps this honest.** `//go:build`,
`//go:embed`, `//go:generate`, `//line` and the cgo forms are comments the scanner
discards and the toolchain still obeys. In this repo that is not hypothetical:
`//go:build substrate` and `testguest` decide whether a file compiles at all. A
directive edit is a build change wearing a comment's clothes, so it disqualifies —
and cgo disqualifies any comment change in the file, because a cgo preamble *is*
source.

**Why an unchanged token stream cannot change what this gate probes.** The
substrate suite verifies behaviour against real runsc, overlayfs, btrfs and
nftables. An identical token stream yields an identical program up to source
positions; positions reach the binary only as diagnostics (panic text, stack
frames, DWARF). Nothing the substrate suite asserts reads those. And a
position-sensitive assertion, if one ever existed, would fail in the unit gate —
which still runs. So the exemption removes a run that could not have distinguished
the two commits, and removes nothing else.

**This refines the rule rather than reversing it.** New behaviour changes the
token stream by construction, so acceptance for new work always runs the gate. The
only thing exempted is re-running an unchanged suite against an unchanged program.

**Fail-closed, because the incentives run one way.** Over-detection costs one gate
run; under-detection silently licenses skipping acceptance. So detection is liberal
(leading whitespace and block-comment `*` are trimmed before matching a directive;
a bare `"C"` literal anywhere is treated as cgo), an unscannable file on either
side is "not proven", and untracked files count as additions.

## Consequences

- **The verdict goes in the commit message**, so a reviewer re-runs
  `go run ./test/preflight/substrategate <rev>` rather than trusting a claim. A
  skipped gate with no recorded verdict is an unverified commit.
- **The tool is part of the repo's own gates** — it is built by `go build ./...`,
  vetted by `go vet ./...`, and its adversarial table runs in `go test ./...`. A
  tool that decides whether to skip a gate is exactly the wrong place for an
  untested claim.
- **Its scope is deliberately narrow.** It answers one question and holds no
  opinion about whether the *other* gates ran; those are unconditional.
- **A diff mixing a comment sweep with a logic change gets no exemption**, which
  reinforces the existing rule that the two do not share a commit: separating them
  is now also what makes the sweep cheap to verify.
- **`internal/arch` and the two `seam_test.go` token guards are unaffected.** They
  read source and run in the unit gate, so a comment change that moved one of them
  still fails, gate exemption or not.

## Rejected / deferred

- **A line-prefix grep** (`^\+\s*//`), the obvious first reach and the reason this
  needed a tool at all. Wrong on the four counts listed above, each now a test case,
  and it cannot see a build-tag edit as anything but a comment.
- **Comparing `go/printer` output with comments stripped.** Position-dependent: the
  printer's blank-line decisions follow node positions, so growing a comment can
  change the printed form of unchanged code. That produces false "required"
  verdicts — safe, but frequent enough to make the tool useless. Pinned as the
  "comment that grows by many lines" test case.
- **Comparing full ASTs.** Positions are fields on the nodes, so this needs a
  custom position-blind walker to buy exactly what a token comparison already gives.
- **A git pre-commit hook.** The per-stage loop is where this discipline lives, and
  a hook would have to make the same decision with less context (it cannot know
  whether the substrate gate already ran). Deferred, not refused: the tool is
  hook-shaped on purpose (one exit code, no interactive output).
- **Extending the exemption to other file types.** `.sh`, `.proto`, `go.mod` and
  templates can all change substrate behaviour, and none has an equivalence as
  clean as a token stream. They stay "required".
