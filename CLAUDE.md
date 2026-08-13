# warren — working conventions

> **Naming.** `warren` = the project; the Go module path is
> `github.com/TSHOGX/warren`, which is the same name made **remotely resolvable** — a
> bare `warren` cannot be fetched by `go get`, so a consumer could only reach it through
> a `replace` pointing at a local checkout. The project name did not change.
> `engine` = the sandbox
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
| `docs/spec/open-decisions.md` | Register of **unsettled** design questions — **one job: let a reader of a § discover it is not settled.** Keyed by the § it hangs on, three lines each (what is open · settlement criterion · other §§ that move with it). **Not the argument** — that goes in a deep-dive or the issue that raised it. ADR-0009. |
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

**Evidence is tiered by whether a reader must have it to avoid a mistake** (ADR-0009). The
**constraint, the measurement that fixed a number, the counter-example that killed an obvious
alternative** — tier 1, in the tree, at the site that decides it. The **derivation, option tour
and review argument** — in the issue or PR, with one line of claim plus a permalink here.
**The test: if it cannot be stated in one line on the repo side, it stays whole.** Links point
**repo → issue, never the reverse** — the repo is the durable side, and M11 ships it.

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

## Inbound asks from downstream consumers

The engine has consumers that are **separate repos with their own maintainers**: the platform
layered on it (`TSHOGX/meadow`, checkout `~/meadow`) and a Template repo under that
(`TSHOGX/tpl-pi`). When one of them needs something only the engine can provide, it files a
**change request as a GitHub issue on this repo**, labelled `ask:inbound`, carrying a
machine-readable `Hangs on: §<n>` line. **The issue number is the only identifier.** Full rule:
ADR-0009.

```bash
gh issue list --repo TSHOGX/warren --label ask:inbound --state open
```

**Nothing about the first principle relaxes here.** A downstream ask carries product vocabulary
by nature — its author says *session*, *tenant*, *workspace* — and the §1.3 litmus applies to it
exactly as to our own designs. **The ask states a need, not a design**, which is what makes it
answerable: reduce the need to engine atoms, or refuse it. An ask asking for a product concept
in the engine is refused **on that ground, stated**, not silently.

Triage, and what each outcome obliges:

- **`ask:granted`** — the need reduces to engine vocabulary and we will do it. **The spec § moves
  first**, then close the issue with the ruling. The consumer deletes its register entry against
  the closed issue, so **the closing comment is the whole hand-off** — a bare "done" leaves them
  nothing to verify.
- **`ask:refused`** — with the reason, and the reason is the load-bearing part: it tells them
  whether the fallback they hold must now be permanent.
- **`ask:duplicate`** — already live in `open-decisions.md`. Close pointing at **that row by its
  bold title, never by position**, and note the leaning it already carries. This is the common
  case, because a consumer hits a boundary at roughly the point we notice it from inside.

**A ruling is a receipt, and its absence is the failure mode this exists to prevent.** An ask
that sits open while our register independently converges on it means the consumer keeps building
against a fallback we have already decided to remove — and the same reasoning gets done twice, on
both sides, which has happened.

**A blocking ask is legitimate, and it still does not schedule the engine.** A consumer may state
that a need blocks it and stop and wait — pretending otherwise only manufactured fallbacks nobody
needed. What that buys is **a schedule in the reply**, not priority: milestone order stays ours.

**An inbound ask is not a question** — anything answerable by reading this repo should have been
read, and one that is really a question gets closed as such. Our own `open-decisions.md` remains
the register for what is unsettled here; **an issue is an address and an inbox, not a second
register.** **Never edit the consumer's repo** — the ruling on the issue is the hand-off.

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
  **Also checked TRANSITIVELY** (`TestSubsystemsCannotReachEachOther`): no subsystem
  may *reach* another by any path of production imports, because the direct check is
  blind to `network → ruleset → runtime` — a real edge every package on the path
  inherits. Reachability needs no list, so an unlisted shared package is covered too;
  the failure prints the path. Why this rather than deriving `leaves` mechanically:
  `docs/decisions/ADR-0006`.
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
  in the same change. Canonical terms (`sandbox` / `placement` / `Binding` / `provider` /
  `seam` / `squota` / `fail-closed` / `outbox` …) are what makes this checkable at all —
  don't coin a synonym for one that is already fixed.
- **A comment citing the spec quotes it VERBATIM.** A paraphrase silently manufactures a
  second statement of the same rule, which is the drift the spec discipline exists to
  prevent — and it is easy to do by accident, since restating a sentence in your own words
  reads like an improvement. Quote instead. **Deep-dive rows are cited by bold title,
  never by position** (their own headers refuse positional numbering), so re-wording a
  title is a pointer edit across every `.go` file citing it — `network.md` alone carries
  over a hundred such pointers.
- **A comment carries the CONSTRAINT, not the argument for it.** State the obligation,
  the measurement that fixed it, or the counterexample that kills the obvious
  alternative — one pass, then the pointer (`§`, `ADR`, deep-dive row). The argument
  itself has a home, and it is not a `.go` file: a comment that re-derives what
  `volume.md D7` already establishes creates the **second statement of the same rule**
  the verbatim-citation rule above exists to prevent, arriving by a route that rule does
  not catch. Symptom to watch for: prose that persuades (*"and that is the whole point"*,
  *"not a matter of taste"*, a paragraph closing the case a §-doc already closed). The
  rule does NOT license deleting measurements or non-obvious obligations — those are the
  irreplaceable part, and a shorter comment that drops one is a worse comment.
- **And stated ONCE, at the site that makes the choice** — the field, the branch, the seam
  method. The dominant waste is not comments restating code; it is one correct constraint
  stated at five or six sites (measured: a streak-pruning rule 5×, `volume.md` D7.3 6×).
  Every copy is where drift starts, since editing one leaves the rest contradicting it.
  Elsewhere: the constraint in one line, plus the pointer. A pointer ACROSS a §5 boundary
  is a liability — the subsystems cannot import each other, so nothing keeps it honest.
- **A pointer must carry the SENTENCE you are deleting, not its topic.** "The § covers
  this" is not enough: a row stating a measurement's *conclusion* was read as licence to
  delete its *provenance*, which lived nowhere else. If the specific claim is not at the
  target, the `.go` file is the primary source — keep it, and say so.
- **A constraint that can be checked mechanically must BE a check.** A comment enforces
  nothing, so it should carry what a reader cannot infer (a measurement, a substrate
  quirk, a counterexample), never what a test could assert. Hence the guards above, and
  hence the guards are themselves tested — an unexercised guard is a comment with a
  `func` keyword.
- **Operator-facing strings are one line.** A flag's help text, an error body, a log
  message: say what it does and what it costs, cite the § that argues it, stop. `--help`
  is not a document surface — a 780-character `flag.Duration` help string was a real
  regression here, transcribing a deep-dive paragraph into a place no reader can navigate
  and no doc discipline governs. If the reasoning matters (and for a destructive default
  it does), it belongs in the deep-dive, with the flag citing it; `reaper-health` D5's own
  instruction is *annotate it at the configuration point*, and an annotation is not a copy.
  What it bars is a paragraph transcribed where no reader can navigate — a claim about the
  surface, **not a licence to compress**. A refusal, a §6.2 response field and a test's
  failure message each reach the one reader who needs that fact, so the fact stays; one
  line, but say it. `+`-wrapped Go source is formatting, not a multi-line string. Four
  independent trim attempts here each deleted a measured failure signature this way
  (`Nexthop has invalid gateway`; *5/5 died vs 5/5 survived*), and the gate caught all
  four where review caught none — **a comment is not a token, a string literal is.** So
  SQL `--` in a schema string and comments inside an embedded program still require the
  gate, however comment-only the pass.

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
  - **One narrow exemption, and it is proven mechanically, never claimed.** The
    substrate gate may be skipped when `go run ./test/preflight/substrategate`
    says so — it answers "not required" only if every changed file is a `.md` or a
    `.go` whose **token stream** is unchanged with no toolchain directive edited
    (a translation pass moving verbatim citations is the case that motivated it).
    **`go build` / `go vet` / `go test ./...` are never skipped**, the verdict goes
    in the commit message so a reviewer can re-run it, and both non-zero exit codes
    mean run the gate. Why a token stream is the right equivalence, and why
    "comment-only" is not: `docs/decisions/ADR-0005`.
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

**English, like everything else the repo carries** — identifiers, comments, error
strings, log messages, documents. Worth stating only because a commit message is the
one artifact written *about* a conversation: chat may be in any language, the repo is
English.
