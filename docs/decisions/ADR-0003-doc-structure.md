# ADR-0003 — Documentation structure: spec / plans / decisions

- **Status:** accepted
- **Date:** 2026-07-17
- **Consumers:** everyone working in this repo; `CLAUDE.md` encodes the resulting rules.

## Context

Code and spec co-evolve in one repo. Spec, forward-looking plans, and one-time
decisions each have a different lifecycle; without separation they blur together
(a settled architecture statement gets edited like a scratch note, a shipped
plan lingers as if still authoritative).

## Decision

Three sibling doc trees under `docs/`, each defined by a different lifecycle:

- **`docs/spec/`** — the converged §1–§6 architecture (+ `deep-dives/`,
  `open-decisions.md`). Slow-moving, long-lived, the source of truth for *what
  the engine should be*.
- **`docs/plans/`** — one file per milestone, `M<N>-<capability>.md`.
  Forward-looking, **expires as shipped**. Named for the capability delivered,
  not the technique used.
- **`docs/decisions/`** — ADRs: one-time tool/environment/process choices.
  **The decision is immutable** — reversing it is a *new* ADR that supersedes,
  never an in-place rewrite of the old one. But an ADR's **prose may be corrected
  for factual accuracy** (a wrong cross-reference, phrasing that named an
  in-flight milestone as if unshipped, a superseded machine fact) — that keeps the
  record true to the decision it made, and is not a decision change. The test:
  does the edit alter *what was decided or why*? If yes → new ADR. If it only
  fixes a stale fact around an unchanged decision → correct in place.

`CLAUDE.md` lives at the engine root because Claude Code auto-loads it there.

## Rationale

- **The engine is self-contained.** Deep-dives keep the *conclusions* of any
  reference research; raw external evidence is not a repo dependency. When a
  rationale is needed, re-derive it from first principles into a living doc
  (spec / deep-dive / ADR), never a dangling link out.
- **Plans are named for the capability, not the technique.** A technique (e.g.
  "walking skeleton") is a one-time phase that expires; it survives as a note in
  the plan body while that plan lives, never as a document name — and once the plan
  expires, only the capability survives, as the ladder entry.
- **Deep-dive vs ADR don't overlap.** Deep-dives answer *why this **design***;
  ADRs answer *why this **tool / environment / process***. ADRs record facts not
  derivable from the code (language choice, execution environment).
- **Spec facts are re-validated by implementation, not up front.** Validation is
  milestone-gated (ADR-0002 matrix); a separate paper pass would be premature.
