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
| `docs/decisions/` | ADRs: one-time **tool / environment / process** choices (why Go, why OrbStack, validation matrix). The **decision** is immutable — reversing it is a *new* superseding ADR, never an in-place rewrite; but the prose **may be corrected for factual accuracy** (stale cross-ref, milestone named as unshipped, superseded machine fact) so long as the edit doesn't alter *what was decided or why*. |
| `cmd/` `internal/` `test/` | The code, implementing the spec. |

**Relationship rule.** The spec is the **best current statement of intent**, not
scripture. It captures what we mean the engine to be *at this phase*, and it will
keep changing as we iterate — it can be questioned and corrected. What it must
**not** be is edited casually as a side effect of a code change.

So: **the spec is not automatically right, and neither is the code. A divergence
between them is a signal to STOP, not to reconcile in passing.** When code and
spec disagree (implementation hits a wall, an invariant doesn't hold, the spec
turns out underspecified or plain wrong):

1. **Stop and analyze** — is the spec expressing the right intent and the code
   wrong, or did implementation teach us the spec's intent itself is wrong /
   incomplete?
2. **Decide which side moves** — fix the code to meet the intent, OR revise the
   spec as a **deliberate, separate** decision (with its rationale; if it's a
   real design shift, that's a deep-dive / open-decisions / ADR moment, not a
   one-liner buried in a feature commit).
3. **Then adjust promptly** — don't leave them diverged, and don't let code
   silently bend to a spec you suspect is wrong just because "spec wins."

The anti-patterns, both rejected: **(a) contorting code to satisfy a spec you
haven't stopped to question**, and **(b) rewriting the spec to rubber-stamp
whatever the code happened to do.** Both are silent drift. The discipline is the
conscious stop in between.

(Narrow, easy case: a pure **vocabulary** mismatch — code and spec using
different names for the same thing — isn't a design divergence; just align the
name on one side in the same change.)

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
  `volume` / `network` / `transport`, plus `model` / `lifecycle` / `store`.
  (`Rootfs` = §5.B subsystem; `fs` = the §5.E wire service; `FS provider` = the
  §5.B/§5.C storage seam — three distinct things, don't merge.)
- **Seams are interfaces** (§1.3 Behavior seam): `store`, `runtime.Provider`,
  `rootfs.Provider`. Swapping a backend = registering an implementation, not
  editing callers.
- **`runtime.Provider` contract = policy + handles, NEVER host primitives** — no
  cgroup path / netns fd / host absolute path in the interface (§5.A). A remote
  provider cannot produce those; leaking them binds the abstraction to
  host-local.
- **Engine vocabulary boundary (§1.1).** Banned as engine-internal entity names
  (types / packages / fields / API endpoints): `generation`, `principal`,
  `tenant`, `owner` (ownership sense), `session`, `turn`, `agent`. They keep
  product semantics out at naming time. Fine in prose describing what the engine
  does *not* do.
- **Code matches spec vocabulary.** If code and spec disagree on a name, fix one
  in the same change.

## Execution & verification

- Runs on **Linux + gVisor/runsc + kernel overlayfs + netns + cgroup v2**. Dev/test
  environment = OrbStack machine `sbx` (see `docs/decisions/ADR-0002`). Engine
  code + **all** overlay scratch live on **native fs**, never the virtiofs shared
  mount (overlayfs upper/work won't work there).
- **Correctness lives in concurrency/failure invariants** (§4.4 the three, §4.3
  health quadrant, §2.4 egress fail-closed), **not at compile time**. A green
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

## Out of scope (external / historical — mine lessons, never cite as a live dep)

The pre-move spec (Mac-side `docs/plan/`), `archieve/`, `ref-repo/`, `research/`.
Deep-dives that referenced them now carry neutral markers, not links. When you
need a rationale from there, **re-derive it from first principles into a living
doc here** (spec / deep-dive / ADR), don't add a dangling link back.

## Commits

Conventional Commits, scoped to the engine (`feat(runtime):`, `docs(spec):`,
`test(lifecycle):`…). Detailed messages — say what changed and why.
