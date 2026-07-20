# ADR-0003 — Documentation structure: spec / plans / decisions

- **Status:** accepted
- **Date:** 2026-07-17
- **Consumers:** everyone working in this repo; `CLAUDE.md` encodes the resulting rules.

## Context

The architecture spec was authored in a separate Mac-side directory
(`docs/plan/`, §1–§6) under a doc-only discipline. Moving into the engine repo —
where code and spec now co-evolve — is the moment to fix how docs are organized
so spec, forward plans, and one-time decisions don't blur together.

## Decision

Three sibling doc trees under `docs/`, each defined by a different lifecycle:

- **`docs/spec/`** — the converged §1–§6 architecture (+ `deep-dives/`,
  `open-decisions.md`). Slow-moving, long-lived, the source of truth for *what
  the engine should be*.
- **`docs/plans/`** — one file per milestone, `M<N>-<capability>.md`.
  Forward-looking, **expires as shipped**. Named for the capability delivered,
  not the technique used.
- **`docs/decisions/`** — ADRs: one-time tool/environment/process choices.
  Immutable once accepted.

`CLAUDE.md` moves to the engine root (Claude Code auto-loads it there) and is
rewritten from scratch to cover a code+spec repo (not the doc-only original).

## What was done in the move

- Spec moved **verbatim** (`docs/plan/*` → `docs/spec/*`, keeping relative
  layout so internal `§`/deep-dive links resolve unchanged). **Migration and
  rewriting were kept separate** — content evolves later under normal discipline,
  not during the move, preserving the git reasoning trail.
- **Cross-repo external references neutralized** (chosen approach): links to
  `../research/e2b-*` and `../ref-repo/sandbox0` became neutral text markers
  ("本仓外参考实现/调研, 证据留存于外部, 不作本仓依赖"). Deep-dive reasoning is
  **unchanged**; only the dangling paths are gone. Originals stay Mac-side; if
  their evidence is ever needed in-repo, re-derive it into a living doc (spec /
  deep-dive / ADR) rather than re-adding a link.
- `WALKING-SKELETON.md` renamed to `docs/plans/M1-sandbox-lifecycle.md` —
  "walking skeleton" is a **technique** (a one-time phase that expires), not a
  document name. It survives as a one-line technique note in the M1 body.
- Added `docs/plans/README.md` (the milestone ladder) and seeded these three
  ADRs (which pull project/environment facts out of ephemeral notes into the
  repo).

## Why not other options

- **Rewrite spec during the move** — rejected: mixes structural and content risk,
  loses the reasoning trail. Content evolves via the certainty-state flow
  (`CLAUDE.md`), not opportunistically.
- **Copy research/ref-repo into the engine** — rejected: bloats the repo and
  forces maintaining a possibly-stale copy. Deep-dives keep the *conclusions*;
  the raw evidence stays external.
- **Re-validate spec facts up front** — deferred: implementation is the real
  re-validation and it's milestone-gated (ADR-0002 matrix). A separate pass now
  is premature.

## Separation of concerns (deep-dive vs ADR)

Deep-dives answer *why this **design***; ADRs answer *why this **tool /
environment / process***. They don't overlap: ADR-0001/0002 record facts that
lived only in ephemeral notes and are not derivable from the code.
