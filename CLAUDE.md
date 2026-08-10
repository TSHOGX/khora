# warren — working conventions

> **Naming.** `warren` = the project and the Go module path. `engine` = the sandbox
> abstraction itself, and that is what `docs/spec/` means by the word throughout
> (§1.1 terminology: *engine* = the abstraction, *orchestrator* = the binary hosting
> it). Different layers, not synonyms — so "the engine" below still reads exactly as
> the spec intends. The protobuf wire package stays `engine.transport.v1`: it is a
> protocol identifier, not a project label.

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
| `docs/plans/` | **Normally absent, and that is the healthy state.** It held the M1–M8 ladder's per-milestone plans; the ladder shipped and each plan was deleted as it shipped. Create a file here only for work whose scope genuinely needs agreeing in advance, and delete it in the change that lands the milestone's last acceptance test (see Milestone discipline). Prefer an ADR or a deep-dive — those have permanent homes. |
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

"Settled + built" and "settled, not yet built" are *both* spec. **The acceptance
tests are the record of which is which** — a §-claim with no substrate test behind it
is the thing to be suspicious of.

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

## Language

**English for everything the code carries:** identifiers, comments, error strings,
log messages, commit messages, and every **new** document. This is the repo's own
original convention restored, not a new imposition — 178 of its 230 commits are
pure ASCII, and the drift to Chinese subjects started only 2026-08-06.

`docs/spec/` prose is Chinese today. **It is not translated as a side effect of
anything.** A wholesale translation of ~5k lines of load-bearing argument is the
largest possible casual spec edit, and the Relationship rule above forbids exactly
that. Translate it — if at all — as its own deliberate work with its own plan.

Two consequences of that split:

- **Canonical terms are already English on both sides** (`sandbox` / `placement` /
  `Binding` / `provider` / `seam` / `squota` / `fail-closed` / `outbox` …). That is
  what makes "code matches spec vocabulary" checkable at all — keep it true. What is
  Chinese in the spec is the connective argument, never the vocabulary.
- **A comment citing the Chinese spec quotes it VERBATIM**, Chinese included (~60
  such lines today). A verbatim citation keeps drift detectable; a paraphrase
  silently manufactures a second statement of the same rule. Quote, don't translate
  in place.

## Code conventions

- **Package names = the §5 canonical short-names:** `runtime` / `rootfs` /
  `volume` / `network` / `transport`, plus `model` / `lifecycle` / `store`,
  `orchestrator` (the §1.1 assembly), and the non-subsystem leaves: `btrfs` (a
  substrate tool, model-ignorant), `guestport` (shared constants two subsystems
  must agree on — the guest port and the upload-temp prefix), `allocbytes`,
  `ruleset` (the abstract rule vocabulary §5.D generates and §5.A renders, plus
  §2.4's static address facts and the criteria both sides check), `arch`
  (structural guards, no production code).
  (`Rootfs` = the §5.B subsystem; `fs` = the §5.E wire service; `rootfs provider` =
  its §5.B storage seam; `volume backend` = §5.C's *separate* seam — four distinct
  things, don't merge, and never say "FS provider". Spelled out in §1.1's
  terminology block — the §1.1 *table* is the vocabulary boundary, a different thing.)
- **Seams are interfaces** (§1.3 Behavior seam): `store.Store`,
  `runtime.Provider`, `rootfs.Provider`, `volume.Backend`. Swapping a backend =
  registering an implementation, not editing callers.
- **"Subsystems never call each other" is enforced structurally**, not by review:
  `internal/arch` fails if any §5 package imports another, or reaches into
  `lifecycle`/`store`. Its allowlist is empty and should stay that way — when two
  subsystems need the same fact, put it in a **leaf** package (`internal/guestport`
  is the precedent), don't excuse the edge.
  **What may live in a leaf: facts, vocabulary, and pure CRITERIA — never a
  mechanism.** `ruleset.Validate` is the criteria case and it is the right shape, not
  a compromise: it must run on the **generate** side (`internal/network`, root-free
  unit tests) *and* on the **install** side (`internal/runtime`, the last check before
  the table goes in), and `RuleSet` is the type `SteeringSpec.Rules` carries — so
  putting it in either subsystem *is* a `network`↔`runtime` edge and fails the guard.
  The boundary that keeps this from becoming a loophole: a leaf may **decide** things
  about values handed to it, and may do **no I/O, execute nothing, and touch no
  substrate** (no nft, no netns, no filesystem). A leaf that acts is a subsystem
  wearing a leaf's name, and it re-couples §5.B/§5.C or §5.D/§5.A through the back
  door the empty allowlist exists to keep shut. Leaf-ness is checked
  (`TestLeavesImportNothingFromTheEngine`) because the failure is transitive and
  invisible to the direct-import guard.
- **`runtime.Provider` contract = policy + handles, NEVER host primitives** — no
  cgroup path / netns fd / host absolute path in the interface (§5.A). A remote
  provider cannot produce those; leaking them binds the abstraction to
  host-local. **Also enforced structurally:** `internal/runtime/seam_test.go` scans
  the interface's params, results *and* struct fields, so a host primitive smuggled
  in on a field fails a test rather than a review. `internal/volume/seam_test.go`
  applies the same guard to `volume.Backend` — it inherits the §5.A token list
  VERBATIM plus §5.C's storage spellings, so don't trim the copy. Same pattern at
  the outer edge: `internal/orchestrator/edge_test.go` reads the AST to keep raw
  error text (which interpolates host paths) out of §6.2 response bodies.
- **Engine vocabulary boundary (§1.1).** Banned as engine-internal entity names
  (types / packages / fields / API endpoints): `generation`, `principal`,
  `tenant`, `owner` (ownership sense), `session`, `turn`, `agent`, plus the whole
  credential family — `credential` / `secret` / `token` / `api_key` (§1.4 excludes
  credentials permanently; the engine sees only a TCP flow to some address). They keep
  product semantics out at naming time. Prose is looser and the §1.1 table is
  authoritative on exactly how much — consult it rather than guessing. Two shapes:
  `generation` alone is barred **outright**, prose included (it is the one concept
  that keeps seeping back in from elsewhere); every other row bars the engine-internal
  NAME while permitting the word in prose that says "this lives outside the engine"
  — `principal` / `tenant` and the four credential spellings included, which is why
  §1.4 and §5.D can state those non-goals at all. `agent` is looser still: fine for
  anything outside the engine, product layer included ("agent sandbox engine" is the
  engine's own positioning).
- **Code matches spec vocabulary.** If code and spec disagree on a name, fix one
  in the same change.

## Execution & verification

- Runs on **Linux + gVisor/runsc + kernel overlayfs + netns + cgroup v2 + btrfs
  (`squota` mode, plus `btrfs-progs`) + `nft` + `ip`**; dev/test environment = OrbStack machine
  `sbx`, with engine code + **all** overlay scratch on **native fs**, never the
  virtiofs shared mount. Why each is a hard requirement, and the measured machine
  facts: `docs/decisions/ADR-0002`.
- **Correctness lives in concurrency/failure invariants** (§4.4 the three, §4.3
  health classification, §2.4 egress fail-closed), **not at compile time**. A green
  build proves nothing about these — write **fault-injection / e2e tests at the
  risky seams**.
- **Verify on the real substrate before claiming done:** `go build` + `go vet` +
  the milestone's e2e acceptance on `sbx`. Report failures with output; don't
  hedge a verified pass.
- **Test build tags**, on two axes. *Cost:* `substrate` = needs real
  runsc/overlay/btrfs and root (the routine gate; run it with `-p 1`); a test that
  costs *minutes* gets its OWN additional tag (`drainrace` is the precedent) so the
  routine gate stays fast — keep both tags, since these files borrow the shared
  substrate fixtures. *Compilation:* a tag may instead gate code that must not exist
  in a production binary — `testguest` compiles the in-guest fault-injection seam,
  absent from a production `sandboxd` because the guest can write its own fs (§5.E.1).
  The harness applies that one itself; it is never passed by hand. Exact invocations
  in `README.md`.

## Milestone discipline

- **One milestone at a time.** Don't pull later-milestone work forward.
- **Per-milestone review gate** before advancing.
- **A plan file is NOT the default.** `docs/plans/` existed to track "built vs
  not-yet-built" across the M1–M8 ladder; that ladder is done and the acceptance
  tests are now that record. Write a scope document only when the work has
  **competing viable approaches or a scope that needs agreeing first** — and then it
  belongs in `docs/decisions/` (ADR: a one-time tool/architecture choice) or
  `deep-dives/` (design rationale under a §), which are permanent homes, not a plan
  that has to be remembered and deleted later. Mechanical work with a self-evident
  scope (a translation pass, a rename, a registered gap with its design already
  settled) gets no document — it gets a commit.
- **If you do write a `docs/plans/M<N>-*.md`, deleting it is part of finishing the
  milestone**, in the same change that lands the last acceptance test — not a
  follow-up. A shipped plan is stale by construction: it describes intent that the
  code and tests now state better, so leaving it creates a second, drifting account
  of the same work. This is the mechanism that emptied `docs/plans/` for M1–M8.
- If an approach **fails twice, root-cause it** — don't stack patches. Surface
  the cause and a different approach.

## No compatibility burden (staged, pre-deployment)

**There are no live instances.** Persistent data (store.db, overlays, runsc
state) can be discarded and rebuilt at any time. Therefore write **no schema
migration / no version-compat / no backward-compat code** — a breaking change
edits the schema and deletes the old path in place, full stop.

A compat branch is a **negative asset right now**: it widens the surface and
manufactures fake invariants — the comment claims one thing, the code does another —
exactly the silent drift the spec discipline exists to prevent. When the schema grows
a column, put it in `CREATE TABLE` and regenerate; do not `ALTER` an imagined old
file. This rule lifts when we deploy.

## Commits

Conventional Commits, scoped to the engine (`feat(runtime):`, `docs(spec):`,
`test(lifecycle):`…). Detailed messages — say what changed and why.
