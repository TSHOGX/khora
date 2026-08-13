# ADR-0009 — Inbound asks from downstream repos, and where evidence lives

- **Status:** accepted
- **Date:** 2026-08-13
- **Consumers:** everyone working in this repo; the two downstream repos that consume the engine
  (`TSHOGX/meadow`, the platform; `TSHOGX/tpl-pi`, a Template repo). `CLAUDE.md` encodes the
  resulting rules.

## Context

The engine has consumers that are **separate repos with their own maintainers**. Both carry a
full discipline for *sending* a change request upstream. **This repo had no stated discipline for
receiving one** — `CLAUDE.md` contained no rule about downstream needs at all, and the only trace
of them was the *Who consumes it* annotation on `open-decisions.md` rows.

That asymmetry lost traffic, measured rather than supposed. Four needs were outstanding from the
platform:

- **One had never arrived here in any form** — a tagged, fetchable release. It is also the
  cheapest of the four.
- **Three already had a corresponding row in this register**, each converged to a leaning: the
  first-class Overlay row reads *direction leans yes* with §3.4 admission accounting named as the
  half that must move with it; two others lean toward the event stream as the substrate.
  **None of those leanings had been sent back**, so the platform kept writing fallbacks for
  needs this repo had already decided to grant.
- One conclusion — that granting two of them separately buys one capability twice — had been
  **derived independently here and in `tpl-pi`.** That duplicated reasoning is the channel's bill.

A second cost surfaced in the same review: this register's §2 and §3 rows had grown to several
hundred words each, carrying their whole argument. That is what made it too heavy to read, which
is how the duplication survived.

## Decision

### 1. An inbound ask is a GitHub issue on this repo, and the ruling is the hand-off

Downstream files a need as an issue here, labelled `ask:inbound`. **The issue number is the only
identifier** and the title is a plain noun phrase naming the need. The body carries one
machine-readable line, `Hangs on: §<n>`, as a gate input (§4).

Triage, and what each outcome obliges:

- **`ask:granted`** — the need reduces to engine vocabulary and we will do it. **The spec § moves
  first**, then close with the ruling stated.
- **`ask:refused`** — with the reason. The reason is the load-bearing part: it tells the consumer
  whether its fallback is now permanent.
- **`ask:duplicate`** — already live in `open-decisions.md`. Close pointing at **that row by its
  bold title, never by position**, and **state the leaning it already carries** — that is the
  part a consumer cannot see from outside, and its absence is what this ADR exists to fix.

**A ruling is a receipt, and its absence is the failure mode.** An ask left open while this
register independently converges on it means a consumer is building against a fallback we have
already decided to remove.

**Nothing about the first principle relaxes.** An ask arrives in product vocabulary by nature —
its author says *session*, *tenant*, *workspace*. The §1.3 litmus applies to it exactly as to our
own designs, and one that requires a product concept inside the engine is **refused on that
ground, stated.** An ask states a need, not a design, which is what makes it answerable.

**A blocking ask is legitimate, and it does not schedule the engine.** A consumer may now say a
need blocks it — the old *never load-bearing* rule manufactured fallbacks nobody needed. What a
blocking ask buys is **a schedule in the reply**, not priority: the engine's milestone order stays
this repo's own.

**Never edit the consumer's repo.** The ruling on the issue is the hand-off.

### 2. `open-decisions.md` holds one thing: that a given § is not settled

Its irreplaceable function is **discoverability from the spec side** — a reader of a § must be
able to learn it is open, or they will cite it as converged architecture. **Entries are keyed by
the § they hang on**, with the settlement criterion, and **not the argument.** The argument
belongs in a deep-dive (which has a permanent home) or in the issue that raised it.

The certainty-state flow is unchanged: `open-decisions → deep-dive → spec §`. What changes is
that a register **row** stops being an essay; the essay's home was always
`deep-dives/`.

### 3. Evidence is tiered by whether a reader must have it to avoid a mistake

| Tier | Content | Home |
| --- | --- | --- |
| **Must be in the repo** | The constraint or obligation · **the measurement that fixed a number** · the counter-example that killed an obvious alternative · a frozen name · acceptance tests and fixtures | spec § / deep-dive / code comment / `test/` |
| **Belongs in the issue or PR** | The derivation · the option tour · spike transcripts · review argument | GitHub, with **one line of claim plus a permalink** on the repo side |
| **Belongs nowhere** | Restatement · status snapshots | — |

**The mechanical test: if the claim cannot be stated in one line on the repo side, it is tier 1
and stays whole.**

This is `CLAUDE.md`'s comment rule generalized, and this repo has the strongest evidence for the
tier-1 boundary: four independent trim attempts each deleted a **measured failure signature**
(`Nexthop has invalid gateway`; *5/5 died vs 5/5 survived*), and the gate caught all four where
review caught none. **A measurement is the irreplaceable part, and it lives at the site that
decides the constraint.**

**Links point repo → issue, never the reverse**, because the repo is the durable side. That
matters more here than downstream: **this repo is going open source (M11)**, and a constraint whose
justification lives only in a private tracker ships without its own justification.

### 4. What can be a gate is a gate

The moment a maintainer needs to know an ask is pending is **when the § is about to be edited**,
not when a session opens. Two mechanical checks, run as a **pre-push hook**:

- A register entry citing a **closed** issue fails.
- A PR touching a § with an **open** ask hung on it fails until the ruling is read.

**The hook is the enforcement.** Branch protection is unavailable on a private repo on this plan
(`403`), so CI cannot block a merge; CI is the second opinion.

## What is explicitly NOT decided here

- **No Action writes a ruling into a spec file.** Automation detects disagreement; it never
  manufactures agreement.
- **An issue is not a second register.** `open-decisions.md` remains the register for what is
  unsettled in the engine; an issue is an address and an inbox.
- **The four outstanding asks are not ruled on by this ADR.** It gives them a channel; each still
  needs its own ruling.

## Consequences

- `CLAUDE.md` gains an *Inbound asks* section — the half that was missing.
- Register rows shrink; their arguments move to `deep-dives/` or to the issue that raised one.
- A ruling that never comes is visible as an open issue rather than invisible.

## Counterparties

`TSHOGX/meadow` and `TSHOGX/tpl-pi` each carry the same decision as their own ADR-0001. **Stated
in each repo rather than centralized**: a pointer from here to a downstream file would invert the
dependency direction this repo maintains.
