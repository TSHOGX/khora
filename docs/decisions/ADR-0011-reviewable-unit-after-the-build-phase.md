# ADR-0011 — main moves only through a reviewable unit, now that the spec no longer leads the code

- **Status:** accepted
- **Note on this document's own history:** a first version of it decided "one
  PR per issue" and was committed marked `accepted` while the maintainer was still
  refining the decision — that status was claimed prematurely, by the author rather
  than the decider. It was rewritten rather than superseded because nothing had been
  decided yet to supersede. **The one-to-one binding it asserted is withdrawn**; §1
  carries what replaced it.
- **Date:** 2026-08-13
- **Consumers:** everyone working in this repo; the two repos that consume it read its spec as a
  statement of what exists. Supersedes nothing — **ADR-0009 stands**; this ADR changes how a ruling
  lands, not what a ruling is.

## Context

Through the M1–M8 ladder the spec **legitimately ran ahead of the code.** `docs/plans/` tracked
"built vs not-yet-built", and a § describing an intended shape was the normal state of a document
whose job was to say where the engine was going. `CLAUDE.md` still records the rule that made that
safe: *"Settled + built" and "settled, not yet built" are both spec — the acceptance tests are the
record of which is which.*

**That phase is over.** M10 is complete and M11 is all but; the engine is substantially built. Under
those conditions the same rule produces a different outcome: **a spec change now almost always
implies a code change**, and "settled, not yet built" stops being a normal state and becomes a debt
that **reads as done from outside this repo.**

This was not reasoned out in advance — it was demonstrated. Ruling on an inbound ask (the
first-class Overlay), the grant was written into four places — §4.3's reclamation predicate, the
orphan sweep's predicate, §3.4.2's accounting, and a new deep-dive section — across three commits
that touched **zero test files**, while `open-decisions.md` still asserted **"Implementation gaps:
None"**. In the code, `Overlay` remained a TEXT column on `sandboxes` and the orphan sweep still
derived its live set from `ListSandboxes`, so a detached Overlay would have been **reclaimed by the
next sweep** — the exact opposite of what the spec now said. The consumer was also told, in the
closing comment, to stop holding `Paused` to protect bytes; acting on that would have **lost the
bytes.**

Nothing in the process caught any of it. The Relationship rule names this divergence and says to
stop rather than reconcile in passing, but it relies on a person noticing.

## Decision

### 0. The principle the rest of this ADR is an instance of

Every failure this week had **one shape**: a *stated* state and an *actual* state drifting apart with
nothing forcing them together. The spec granted a capability the code did not have; three rulings
converged upstream and never returned to the asker; five gates reported green while checking nothing;
a measurement was cited as settled input with no test producing it; the only recorded substrate run
predated every milestone it was taken to cover.

⇒ **Every claim needs a mechanical link to the thing that makes it true, and where the link is
absent, the absence itself must be marked.** The mechanisms already in this repo are all instances of
that one rule, which is why they should be read together rather than as separate rituals: `Hangs on:`
links an ask to a §; a test citing a § links the § to the code; the digest pin links our copy of an
upstream artifact to theirs; **`Spec-only:` marks a link deliberately absent**; **Implementation gaps
marks a link known-absent**. A PR is not a unit of work in this scheme — it is **the place where the
links are visible before they land.**

### 1. The invariant: main moves only through a reviewable unit

**Not "one PR per issue."** An issue is a question and a PR is a change, and the two are genuinely
**many-to-many**: one PR may close several related issues (two asks landing on the same substrate
should be ruled together, not twice), and one issue may need several PRs (a milestone-sized
implementation). Binding them one-to-one confuses the unit of *tracking* with the unit of *work*, and
it was the wrong half of this decision.

**A ruling that changes nothing in the tree needs no PR.** The test is not granted-versus-refused —
it is **does this ruling change the tree.** Refusing an ask by writing the reasoning into a § *is* a
tree change and goes through a PR; closing something as "this is a question, not an ask", or as a
duplicate of a live register entry, changes nothing and is just a closing comment.

**What is forbidden is narrower than either, and is the only part that cannot be recovered
afterwards: a change reaching `main` with nothing to look at.** `scripts/check-no-direct-main.sh`
reads the push plan from stdin and refuses `refs/heads/main`, with `ALLOW_MAIN_PUSH=<reason>` for work
that genuinely is not reviewable. It deliberately does **not** check that a branch has a PR or that a
PR was reviewed — a hook cannot know either at push time, and asserting it would build exactly the
green-but-checking-nothing gate this ADR's Context is about.

`.github/pull_request_template.md` asks for three fields, chosen because **a diff cannot show them**:
the issues closed (zero or more), **the §§ touched** (the dual of `Hangs on:` — how a later round
finds the argument from the spec side), and **whether the substrate ran**.

### 2. A spec change with no test change must declare itself

`scripts/check-spec-code-drift.sh`, in the pre-push hook: a change under `docs/spec/` touching no
test file fails unless a commit message in the range carries **`Spec-only: <reason>`**.

**Deliberately coarse.** It does not infer which § a hunk belongs to — a diff also carries
cross-references to §§ it merely cites, so a gate that guesses either over-fires until it is
switched off or under-fires silently. This one can only over-fire, and the escape is one line.
Wording fixes and corrected cross-references stay cheap; what changes is that **"I was only
rewording" becomes a written claim instead of a default.**

**Not installed in the spec-only sibling repos.** With no code at all, every commit there is
spec-only, and a gate that fires every time is one that gets switched off — the same reason the
per-§ inference above was rejected.

### 3. A granted-but-not-built ruling is registered as an implementation gap, every time

`open-decisions.md`'s gap section is not a resting place. An entry states **what the code does
today**, what closing it takes, and **what the consumer must not do yet** — that last part is what
was missing and is what would have cost real data.

## Rationale

**Why not require green CI on the PR.** Branch protection is unavailable on a private repo on this
plan (`403`, rulesets too), so a required status check does not exist here and a PR can be
self-merged regardless. The **hook is the enforcement**; the PR is the unit of review and the record.
Pretending otherwise would build a ritual whose authority nobody can point at.

**Why not simply re-verify by hand.** The Relationship rule already asks for that and it did not
fire. The gate is not a substitute for judgement — it is the thing that makes the absence of
judgement visible.

**Why the acceptance tests remain the authority.** A PR proves spec and code changed **together**;
it does not prove they **agree**. Only a test citing the § does that, and this repo already has the
convention — every §-heading in the spec is cited by at least one test. The gate leans on that
convention rather than inventing one.

## Consequences

- Direct pushes to `main` stop, mechanically. `#8` and `#1` were the last rulings to land that way
  and are **not** being redone as PRs — the rulings are sound and their arguments are on the issues,
  so redoing them would manufacture a second history. **The first-class Overlay's implementation PR
  will reference `#1` as the ruling it implements.**
- **Not installed in the two spec-only sibling repos.** There the spec *is* the product, so the
  spec/test gate would fire on every commit, and a gate that always fires is one that gets switched
  off. Whether `main` should be PR-only in those repos is a separate call with a weaker argument
  behind it (self-review, single maintainer, no code) and is **left to their maintainer** rather than
  installed here by inference.
- A ruling that cannot be implemented in the current milestone is now visibly a debt rather than
  silently a claim.
- `CLAUDE.md`'s "both are spec" sentence stays true but is no longer the whole story, and now points
  here.
