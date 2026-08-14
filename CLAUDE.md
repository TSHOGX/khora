# khora — working conventions

> **Naming.** `khora` = the project, the Go module (`github.com/TSHOGX/khora`), the CLI
> binary, the repository, and the protobuf wire package (`khora.transport.v1`) — one name for
> one thing. χώρα is the *Timaeus*'s third kind: the receptacle that has no character of its
> own and takes on the character of what enters it, which is this engine's first principle in
> Greek. The wire package is compiled into every RPC path and resolved by both ends, making it
> a compatibility surface between the host binary and every Template built before it changes.
>
> **`engine`** = the sandbox abstraction itself; **`orchestrator`** = the binary hosting it.
> Layer names, not other names for the project — don't rename them away.

## What this is

The sandbox engine implementation. First principle — the one invariant every rule below
serves:

> **Do one thing well: the sandbox. A clean, decoupled, extensible abstraction.**
> Product-layer concepts (session / agent / swarm / tenant / user) **do not enter the
> engine**. They layer on the ID-addressable atoms it exposes (sandbox / process / fs /
> volume / template).

Any design that pushes product semantics into the engine is **rejected by default** — unless
you can show it cannot be an adapter layered *outside* the engine. Reason from how the design
*should* be; you don't need a prior implementation to justify anything.

**Write for a reader who is not in your head.** Documentation, error messages and rulings go
to someone who does not share the context that produced them — including your own team.
Where the repo being private matters it is a mechanical fact about today (branch protection is
unavailable on this plan, so a pre-push hook is the enforcement), never a reason to explain
less.

## The principle every mechanism here is an instance of

> **Every claim needs a mechanical link to the thing that makes it true, and where the link
> is absent, the absence itself must be marked.**

A test asserting a behaviour links the behaviour to the code. A recorded substrate run links
"verified" to a SHA. A generated page links prose to its source. A registered gap marks a link
known-absent. Read the rules below as instances of that one, not as separate rituals.

It comes from failure, not taste: in one week a document granted a capability the code did not
have, five gates reported green while checking nothing, and the only recorded substrate run
predated everything it was taken to cover. Each was a *stated* state and an *actual* state
drifting apart with nothing forcing them together.

## The repo is mid-transition — read this before touching a document

The engine was built spec-first: `docs/spec/` said what should exist and the code followed.
That phase is over and the arrangement is being inverted now.

- **`docs/spec/` and `docs/decisions/` are being replaced, not edited.** Once moved they live
  at `scratch/old-docs/` (gitignored), as reference only. **They are not authoritative and
  nothing new cites them.** If you find yourself reconciling code against a page there, stop —
  that page is the thing being replaced.
- **`docs/` is being rewritten from the code**, organized by reader. Until a page exists the
  code is the only source: say "not documented yet" rather than pointing at the old tree.
- **A `.go` file may not cite a doc address** — no `§4.3`, no `volume.md D7.3`, no `ADR-0006`.
  A guard in `internal/arch` will fail on a new one. When you touch a line carrying one, take
  it out: of ~2900 such lines only 3 were ever pointer-*only*, so the surrounding sentence
  already states the constraint. That is what makes removal safe rather than lossy.
- **`docs/plans/` does not exist, and nothing goes in it.**

**The old section numbers are being retired as an address space.** In prose, name the thing:
*the reclamation predicate*, *the volume backend seam*, *the egress floor*. If you need to
know what a number meant, `scratch/old-docs/` is readable — but do not reintroduce it.

## Documents describe code. When they disagree, the code is right.

This inverts the rule that governed the build phase, and the inversion is the point. A page
**describes what the engine does**, so a mismatch is a **documentation bug** — fix the page,
in the same change if you caused it.

**One narrow carve-out.** Where a page states a *promise to someone outside this repo* — the
API contract, the security model, the compatibility promise in `CHANGELOG.md` — a mismatch is
a bug in the **code**. The test: would a consumer who believed this page have built something
now broken? If yes, the page wins and the code moves.

**A vocabulary mismatch is not a divergence** — align the name on one side, same change.

## Two checks before you add a mechanism

1. **The litmus.** Can the new need reduce to *register-an-implementation* /
   *add-a-join-table* / *add-a-subscriber*? If not, does it belong in the engine at all? If it
   can be an adapter outside, keep it outside.
2. **Ownership.** Does the new mechanism make one subsystem call another? That is the start of
   a dependency cycle. **Verbs belong to the mediator (`internal/lifecycle`), mechanisms
   belong to the subsystems, and subsystems NEVER call each other.**

## Engine vocabulary boundary

Banned as **engine-internal entity names** (types / packages / fields / API endpoints):
`generation`, `principal`, `tenant`, `owner` (in the ownership sense), `session`, `turn`,
`agent`, plus the whole credential family — `credential` / `secret` / `token` / `api_key`.
They keep product semantics out at naming time.

Three shapes, and the difference is load-bearing:

- **`generation` is barred outright**, prose included. It is the one concept that keeps
  seeping back in from elsewhere.
- **Every other name is barred as an engine-internal identifier but permitted in prose that
  says the thing lives outside the engine.** That is what lets a page state "credentials are
  permanently excluded" or "a tenant is the platform's concept" at all.
- **`agent` is looser still**: fine for anything outside the engine, product layer included —
  "agent sandbox engine" is the engine's own positioning.

**Credentials are excluded permanently, not pending.** The engine sees a TCP flow to some
address, nothing more. Anything it handed a sandbox would be readable by guest root, so it
cannot be a secrecy boundary — which is *why* the exclusion is permanent rather than a gap.

**Canonical terms are fixed; don't coin a synonym for one already settled:** `sandbox` ·
`placement` · `Binding` · `provider` · `seam` · `squota` · `fail-closed` · `outbox` ·
`Overlay` · `Template` · `Volume`.

**Four things are deliberately distinct and must never be merged:** **Rootfs** (the
subsystem) · **`fs`** (the wire service: read/write/list/stat/mkdir/remove/move) · **rootfs
provider** (the rootfs storage seam) · **volume backend** (the volume seam, a *separate*
one). Never say "FS provider".

## Code conventions

- **Package names = the subsystem short-names:** `runtime` / `rootfs` / `volume` / `network`
  / `transport`, plus `model` / `lifecycle` / `store`, `orchestrator` (the assembly), and the
  non-subsystem leaves: `btrfs` (a substrate tool, model-ignorant), `guestport` (constants
  two subsystems must agree on — the guest port and the upload-temp prefix), `allocbytes`,
  `ruleset` (the abstract rule vocabulary the network subsystem generates and the runtime
  renders, plus the static address facts and the criteria both sides check), `arch`
  (structural guards, no production code).
- **Seams are interfaces**: `store.Store`, `runtime.Provider`, `rootfs.Provider`,
  `volume.Backend`. Swapping a backend = registering an implementation, not editing callers.
- **"Subsystems never call each other" is enforced structurally**, not by review.
  `internal/arch` fails if any subsystem imports another, or reaches into `lifecycle` /
  `store`. Its allowlist is empty and should stay that way — when two subsystems need the
  same fact, put it in a **leaf** package (`internal/guestport` is the precedent); don't
  excuse the edge.
  **Also checked TRANSITIVELY** (`TestSubsystemsCannotReachEachOther`), because the direct
  check is blind to `network → ruleset → runtime` — a real edge every package on the path
  inherits. Reachability needs no list, so an unlisted shared package is covered too, and the
  failure prints the path.
  **What may live in a leaf: facts, vocabulary, and pure CRITERIA — never a mechanism.** A
  leaf may **decide** things about values handed to it, and may do **no I/O, execute nothing,
  and touch no substrate** (no nft, no netns, no filesystem). A leaf that acts is a subsystem
  wearing a leaf's name, re-coupling two subsystems through the back door the empty allowlist
  keeps shut. `ruleset.Validate` is the criteria case: it must run on the generate side
  (`internal/network`, root-free) *and* the install side (`internal/runtime`, last check
  before the table goes in), so putting it in either *is* a `network`↔`runtime` edge.
  Leaf-ness is checked (`TestLeavesImportNothingFromTheEngine`) because the failure is
  transitive and invisible to the direct-import guard.
- **`runtime.Provider`'s contract = policy + handles, NEVER host primitives** — no cgroup
  path, netns fd, or host absolute path in the interface. A remote provider cannot produce
  those; leaking them binds the abstraction to host-local. **Enforced structurally:**
  `internal/runtime/seam_test.go` scans the interface's params, results *and* struct fields,
  so a host primitive smuggled in on a field fails a test rather than a review.
  `internal/volume/seam_test.go` applies the same guard to `volume.Backend` — it inherits the
  runtime token list VERBATIM plus the volume subsystem's storage spellings, so don't trim
  the copy. Same pattern at the outer edge: `internal/orchestrator/edge_test.go` reads the AST
  to keep raw error text (which interpolates host paths) out of API response bodies.
- **A comment carries the CONSTRAINT, not the argument for it.** State the obligation, the
  measurement that fixed it, or the counterexample that kills the obvious alternative — once,
  and stop. Symptom to watch for: prose that persuades (*"and that is the whole point"*, a
  paragraph closing a case). This does **not** license deleting measurements or non-obvious
  obligations — those are the irreplaceable part, and a shorter comment that drops one is a
  worse comment.
  **In this transition the rule loses its escape hatch:** where a comment used to compress by
  pointing at a document, there is no document — so the constraint is stated at the site in
  full, or it is lost. If it will not fit, the fact belongs in an internals page and goes on
  the extraction inventory, not into a pointer.
- **And stated ONCE, at the site that makes the choice** — the field, the branch, the seam
  method. The dominant waste is not comments restating code; it is one correct constraint
  stated at five or six sites (measured: a streak-pruning rule 5×, a volume constraint 6×).
  Every copy is where drift starts, since editing one leaves the rest contradicting it.
- **A constraint that can be checked mechanically must BE a check.** A comment enforces
  nothing, so it carries what a reader cannot infer (a measurement, a substrate quirk, a
  counterexample), never what a test could assert. Hence the guards above — and hence the
  guards are themselves tested: an unexercised guard is a comment with a `func` keyword.
- **Operator-facing strings are one line.** A flag's help text, an error body, a log message:
  say what it does and what it costs, and stop. `--help` is not a document surface — a
  780-character `flag.Duration` help string was a real regression here. What this bars is a
  paragraph transcribed where no reader can navigate; it is **not a licence to compress**. A
  refusal, an API response field and a test's failure message each reach the one reader who
  needs that fact, so the fact stays — one line, but say it. Four independent trim attempts
  each deleted a measured failure signature this way (`Nexthop has invalid gateway`; *5/5 died
  vs 5/5 survived*), and a gate caught all four where review caught none.
  **A comment is not a token; a string literal is** — so SQL `--` in a schema string, or a
  comment inside an embedded program, still needs the substrate gate however comment-only the
  pass looks.
- **Error strings must not cite a document.** An operator has no spec to look up: *"the
  uncapped term cannot be measured"* is complete, *"§5.C's share of the uncapped term"* is
  not. Token-stream changes, so they need the substrate gate.

## Execution & verification

- Runs on **Linux + gVisor/runsc + kernel overlayfs + netns + cgroup v2 + btrfs (`squota`
  mode, plus `btrfs-progs`) + `nft` + `ip`**. Dev/test environment = the OrbStack machine
  `sbx`, with engine code **and all overlay scratch on native fs**, never the virtiofs shared
  mount.
- **Correctness lives in concurrency and failure invariants** — the transition lock, health
  classification, egress fail-closed — **not at compile time**. A green build proves nothing
  about these, so write **fault-injection / e2e tests at the risky seams**.
- **Verify on the real substrate before claiming done:** `go build` + `go vet` + the
  acceptance suite on `sbx`. Report failures with output; don't hedge a verified pass.
  - **One narrow exemption, proven mechanically and never claimed.** The substrate gate may be
    skipped when `go run ./test/preflight/substrategate` says so — it answers "not required"
    only if every changed file is a `.md` or a `.go` whose **token stream** is unchanged, with
    no toolchain directive edited. **`go build` / `go vet` / `go test ./...` are never
    skipped**, the verdict goes in the commit message so a reviewer can re-run it, and both
    non-zero exit codes mean run the gate.
  - **A substrate run is recorded, not asserted.** `scripts/record-substrate-run.sh` writes
    the SHA, host, toolchain, tag set, exit code and counts to `docs/substrate-runs/`, and
    `scripts/check-substrate-coverage.sh` decides whether an existing record still covers
    HEAD. The predecessor of that arrangement was an untracked log file, which rotted into a
    false `EXIT=0` — an untracked record is one nothing can keep honest.
- **Test build tags, on two axes.** *Cost:* `substrate` = needs real runsc/overlay/btrfs and
  root (the routine gate; run it with `-p 1`); a test costing *minutes* gets its OWN
  additional tag (`drainrace` and `quotachurn` are the precedents) so the routine gate stays
  fast — keep both tags, since those files borrow the shared substrate fixtures.
  *Compilation:* a tag may instead gate code that must not exist in a production binary —
  `testguest` compiles the in-guest fault-injection seam, absent from a production `sandboxd`
  because the guest can write its own fs. The harness applies that one itself; never pass it
  by hand. Exact invocations are in `README.md`.
- **Register every tag in `internal/arch`'s `tagMatrix`**, alone and paired with `substrate`.
  A tag missing from it is a build configuration where a bad import edge passes unseen — and
  files behind a tag are the ones most able to reach for a shortcut.
- **Prove a guard still bites when you touch it.** The guards embed import paths as string
  literals and `owner` silently skips a package it cannot find, so a stale guard passes
  vacuously — `TestGuardIsNotStale` is the mechanical defense, and its comment records that
  this already happened once. Inject a bad edge and watch the guard fail.
- **When a substrate test implicates the engine, suspect the harness first.** Two known
  shapes: a shell that forks its child (`sh -c "sleep 30"`) lets a SIGKILL orphan the child
  holding the stdout pipe, and `qgroup show --sync` exits 1 during a quota rescan the suite's
  own churn provokes. Both have presented as engine defects.
- **Poll with `internal/waitfor`; do not hand-roll the loop.** `Until` re-checks once after
  the deadline — the naive shape misses a condition that becomes true during its final sleep
  and reports a timeout that never happened. A sleep is only correct when the elapsed time
  **is** the assertion (holding a window open to prove something does not happen, or waiting
  out a retention TTL).
- **If an approach fails twice, root-cause it.** Don't stack patches — name the cause and
  change approach.

## Inbound asks from downstream consumers

The engine has consumers that are **separate repos with their own maintainers**: the platform
layered on it (`TSHOGX/meadow`, checkout `~/meadow`) and a Template repo under that
(`TSHOGX/tpl-pi`). When one needs something only the engine can provide, it files a change
request as a GitHub issue here, labelled `ask:inbound`. **The issue number is the only
identifier.**

```bash
gh issue list --repo TSHOGX/khora --label ask:inbound --state open
```

**Nothing about the first principle relaxes here.** A downstream ask carries product
vocabulary by nature — its author says *session*, *tenant*, *workspace* — and the litmus
applies to it exactly as to our own designs. **The ask states a need, not a design**, which is
what makes it answerable: reduce the need to engine atoms, or refuse it. An ask asking for a
product concept in the engine is refused **on that ground, stated**, not silently.

- **`ask:granted`** — **the code and its test are what close it.** The closing comment is the
  whole hand-off: what changed, what they must change, what they must NOT do yet. A bare
  "done" leaves them nothing to verify. **A grant whose code is not written is registered as
  an open gap, naming what the consumer must not rely on** — in this transition that is the
  likeliest failure, because a document edit can look like delivery.
- **`ask:refused`** — with the reason, which is the load-bearing part: it tells them whether
  the fallback they hold must now be permanent.
- **`ask:duplicate`** — already an open question here. Close pointing at it **by name, never by
  position**, and note the leaning it carries.

**A ruling is a receipt, and its absence is the failure this exists to prevent.** An ask left
open while we independently converge on it means the consumer keeps building against a
fallback we have already decided to remove — and the same reasoning gets done twice, on both
sides. That has happened.

**A blocking ask is legitimate and still does not schedule the engine.** What it buys is **a
schedule in the reply**, not priority. **An inbound ask is not a question** — anything
answerable by reading this repo should have been read. **Never edit the consumer's repo**; the
ruling is the hand-off.

**Evidence is tiered by whether a reader must have it to avoid a mistake.** The constraint,
the measurement that fixed a number, the counter-example that killed an obvious alternative —
in the tree, at the site that decides it. The derivation, option tour and review argument — in
the issue or PR, with one line of claim plus a link from here. Test: if it cannot be stated in
one line on the repo side, it stays whole. Links point **repo → issue, never the reverse** —
the repo is the durable side.

## No compatibility burden

**`0.0.x` promises nothing**, and `CHANGELOG.md` states that as a public commitment.
Persistent data (`store.db`, overlays, volumes, runsc state) can be discarded and rebuilt at
any time. Therefore write **no schema migration, no version-compat, no backward-compat
code** — a breaking change edits the schema and deletes the old path in place, full stop.

A compat branch is a **negative asset**: it widens the surface and manufactures fake
invariants — the comment claims one thing, the code does another — exactly the silent drift
these rules exist to prevent. When the schema grows a column, put it in `CREATE TABLE` and
regenerate; do not `ALTER` an imagined old file. **A tag a consumer pins does not lift this
rule** — that is what the promise being public buys.

## Commits and PRs

Conventional Commits, scoped to the engine (`feat(runtime):`, `docs(operations):`,
`test(lifecycle):` …). Detailed messages: say what changed and why, and if a test pins a fix,
say what it asserts and how you confirmed it fails without the fix.

**English, like everything else the repo carries** — identifiers, comments, error strings, log
messages, documents. Worth stating only because a commit message is the one artifact written
*about* a conversation: chat may be in any language, the repo is English.

**`main` moves only through a reviewable unit.** A PR may close several issues or none; what
is forbidden is a change reaching `main` with nothing to look at.
`scripts/check-no-direct-main.sh` (pre-push) refuses `refs/heads/main`, with
`ALLOW_MAIN_PUSH=<reason>` for work that genuinely is not reviewable. It deliberately does
**not** check that a branch has a PR or that a PR was reviewed — a hook cannot know either at
push time, and asserting it would build exactly the green-but-checking-nothing gate this is
about. Branch protection is unavailable on a private repo on this plan, so the hook is the
enforcement and the PR is the record.

**Force-changing a published tag needs no permission** — the repo is private and no external
consumer holds a ref. `main` still does not take a direct push. If the tree claims a ref is
immutable, correct that claim in the same change that violates it.
