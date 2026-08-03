# engine — working conventions

## What this is

The sandbox engine implementation (code + spec, one repo). First principle —
the one invariant every rule below serves:

> **Do one thing well: the sandbox. A clean, decoupled, extensible abstraction.**
> Product-layer concepts (session / agent / swarm / tenant / user) **do not enter
> the engine**. They layer on the ID-addressable atoms it exposes
> (sandbox / process / fs / volume / template).

Any design that pushes product semantics into the engine is **rejected by
default** — unless you can show it cannot be an adapter layered *outside* the
engine. Reason from how the design *should* be; you don't need the old
implementation to justify anything.

## Where things live (and which wins)

| Path | Holds |
| --- | --- |
| `docs/spec/` | Converged architecture, §1–§6. The best current statement of *what the engine should be* — authoritative for intent, but revisable (see Relationship rule). Slow-moving. |
| `docs/spec/deep-dives/` | Single-topic design rationale hung under a § (the *why* / falsification / rejected options). |
| `docs/spec/open-decisions.md` | Register of **unsettled** design questions. |
| `docs/plans/` | One file per milestone, `M<N>-<capability>.md`: forward scope + acceptance + out-of-scope. **Expire as shipped.** Ladder index in `docs/plans/README.md`. |
| `docs/decisions/` | ADRs: one-time **tool / environment / process** choices (why Go, why OrbStack, validation matrix). The **decision** is immutable — reversing it is a *new* superseding ADR; stale prose around an unchanged decision is corrected in place. Full rule: ADR-0003. |
| `cmd/` `internal/` `test/` | The code, implementing the spec. |

**Relationship rule.** The spec is the **best current statement of intent**, not
scripture — it can be questioned and corrected, but never edited casually as a
side effect of a code change. **Neither side is automatically right: a divergence
is a signal to STOP, not to reconcile in passing.** When code and spec disagree,
decide deliberately which side moves — fix the code to meet the intent, or revise
the spec with its rationale (a real design shift is a deep-dive / open-decisions /
ADR moment, not a one-liner in a feature commit) — then adjust promptly. The two
anti-patterns: contorting code to satisfy a spec you never questioned, and
rewriting the spec to rubber-stamp what the code did. Both are silent drift.

(A pure **vocabulary** mismatch is not a divergence — align the name on one side
in the same change.)

"Settled + built" and "settled, not yet built" are *both* spec; `plans/` tracks
which is which.

## Certainty-state flow (where a topic lives = how settled it is)

```
unsettled / disagreement          → open-decisions.md
      └ worth expanding           → deep-dives/xxx.md   (keeps the reasoning)
            └ converged           → spec § (as settled), then drop the register entry
one-time tool/env/process choice  → decisions/ADR
```

Settled goes in the spec; unsettled stays in open-decisions; **the two never
overlap**. After deciding, update the deep-dive/spec **first**, then remove the
register entry. A deep-dive keeps its rationale; once written back to the spec it
slims to boundary + open agenda — nothing lives in full in both places.

## Two checks before you touch the spec

1. **Litmus (§1.3):** can the new need reduce to *register-an-implementation /
   add-a-join-table / add-a-subscriber*? If not → does it belong in the engine at
   all? If it can be an outside adapter, keep it outside.
2. **Ownership (§4):** does the new mechanism make one subsystem call another?
   That is the start of a dependency cycle. **Verbs belong to the §4 mediator,
   mechanisms to §5 subsystems; subsystems NEVER call each other.**

## Code conventions

- **Package names = the §5 canonical short-names:** `runtime` / `rootfs` /
  `volume` / `network` / `transport`, plus `model` / `lifecycle` / `store`,
  `orchestrator` (the §1.1 assembly) and `btrfs` (a leaf substrate tool, model-
  ignorant — not a subsystem).
  (`Rootfs` = the §5.B subsystem; `fs` = the §5.E wire service; `rootfs provider`
  = its §5.B storage seam — three distinct things, don't merge. Say `rootfs
  provider`, never "FS provider": §5.C's `volume backend` is a *separate* seam,
  not the other half of one shared FS seam.)
- **Seams are interfaces** (§1.3 Behavior seam): `store.Store`,
  `runtime.Provider`, `rootfs.Provider`, and `volume.Backend` (§5.C — a
  *different* shape from `rootfs.Provider`, not one shared FS seam). Swapping a
  backend = registering an implementation, not editing callers.
- **"Subsystems never call each other" is enforced structurally**, not by review:
  `internal/arch` fails if any §5 package imports another, or reaches into
  `lifecycle`/`store`. Its allowlist is empty and should stay that way — when two
  subsystems need the same fact, put it in a **leaf** package (`internal/guestport`
  is the precedent), don't excuse the edge.
- **`runtime.Provider` contract = policy + handles, NEVER host primitives** — no
  cgroup path / netns fd / host absolute path in the interface (§5.A). A remote
  provider cannot produce those; leaking them binds the abstraction to
  host-local.
- **Engine vocabulary boundary (§1.1).** Banned as engine-internal entity names
  (types / packages / fields / API endpoints): `generation`, `principal`,
  `tenant`, `owner` (ownership sense), `session`, `turn`, `agent`. They keep
  product semantics out at naming time. In prose the §1.1 table is finer-grained:
  `session` / `turn` / `agent` are fine when describing what the engine does *not*
  do, `owner` is fine only in the responsibility sense, and `generation` /
  `principal` / `tenant` stay barred outright.
- **Code matches spec vocabulary.** If code and spec disagree on a name, fix one
  in the same change.

## Execution & verification

- Runs on **Linux + gVisor/runsc + kernel overlayfs + netns + cgroup v2**. Dev/test
  environment = OrbStack machine `sbx` (see `docs/decisions/ADR-0002`). Engine
  code + **all** overlay scratch live on **native fs**, never the virtiofs shared
  mount (overlayfs upper/work won't work there).
- **Correctness lives in concurrency/failure invariants** (§4.4 the three, §4.3
  health classification, §2.4 egress fail-closed), **not at compile time**. A green
  build proves nothing about these — write **fault-injection / e2e tests at the
  risky seams**.
- **Verify on the real substrate before claiming done:** `go build` + `go vet` +
  the milestone's e2e acceptance on `sbx`. Report failures with output; don't
  hedge a verified pass.

## Milestone discipline

- **One milestone at a time.** Each has a plan (`docs/plans/M<N>-*.md`) with
  explicit scope + acceptance + out-of-scope. Don't pull later-milestone work
  forward.
- **Per-milestone review gate** before advancing.
- If an approach **fails twice, root-cause it** — don't stack patches. Surface
  the cause and a different approach.

## No compatibility burden (staged, pre-deployment)

**There are no live instances.** Persistent data (store.db, overlays, runsc
state) can be discarded and rebuilt at any time. Therefore write **no schema
migration / no version-compat / no backward-compat code** — a breaking change
edits the schema and deletes the old path in place, full stop.

A compat branch is a **negative asset right now**: it widens the code surface
and manufactures fake invariants — "the comment claims one thing, the code does
another" — exactly the silent drift the spec discipline exists to prevent. When
the schema grows a column, put it in `CREATE TABLE` and regenerate the store; do
not `ALTER` an imagined old file. This rule lifts when we actually deploy — at
that point compatibility becomes a real constraint and this section gets revised.

## Commits

Conventional Commits, scoped to the engine (`feat(runtime):`, `docs(spec):`,
`test(lifecycle):`…). Detailed messages — say what changed and why.
