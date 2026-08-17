# Development workflow

## Start from the engine boundary

khora exposes a decoupled sandbox abstraction. Before adding an engine concept,
check whether the need can be implemented by registering a provider, adding a store
relationship, or subscribing to Events. Caller identity, authorization, credentials,
and higher-level work orchestration stay in an adapter outside this repository.

Mechanisms belong to `runtime`, `rootfs`, `volume`, `network`, or `transport`.
Lifecycle verbs belong to `internal/lifecycle`. Shared vocabulary and pure criteria
belong in leaf packages. [Architecture tests](../../internal/arch/coupling_test.go)
keep the cross-subsystem allowlist empty.

## Use Issues and PRs as records

An Issue records a need, ruling, or known gap. A PR records implementation, review,
and verification. Use `Refs #N` for progress and `Closes #N` only when the PR fully
delivers the Issue. Do not create a second open-question register or encode a
document section address in code.

For inbound asks, record the need and fallback before proposing engine design.
Keep a granted ask open until its implementing PR merges; close a refusal with its
reason and a duplicate with the canonical Issue.

## Make a reviewable change

Keep code, focused tests, public documentation, and generated output in the same PR.
Use existing package vocabulary and provider seams. New comments should state a
local obligation, failure mode, or measurement, not cite a Markdown page as the
reason code is correct.

When adding an implementation behind an existing seam, follow
[Adding a provider](providers.md). The current seams are internal extension
boundaries; do not describe an implementation as externally selectable until the
standard assembly can select it.

New structural guards require a committed negative fixture proving the prohibited
shape is rejected. Register a new build tag only in the architectural tag matrix,
alone and paired with `substrate` where applicable. Use `internal/waitfor` instead
of custom polling loops.

Commit messages use Conventional Commits scoped to the engine. `main` changes
through review; the pre-push hook is only a local safeguard.

## Keep evidence dependencies one-way

Documentation describes code; technical sources do not depend on documentation.
State each code constraint beside the symbol, branch, or test that enforces it so a
reviewer can verify the claim in the same change. A comment that points to a Markdown
page becomes stale without making a build or test fail, so maintained non-Markdown
sources reject such citations.

Literal Markdown paths are allowed only when they are data: a generator names its
output, a staleness check invokes that generator, or a test classifies a filename.
Those exceptions do not carry a technical claim. The repository guard and its
negative fixture are [check-retired-references.sh](../../scripts/check-retired-references.sh)
and [test-check-retired-references.sh](../../scripts/test-check-retired-references.sh).

## Prepare the PR

Run the deterministic checks and make the substrate decision described in
[Testing](testing.md) after the last material change. Regenerate and check derived
files as described in [Generated files](generated-files.md). Fill in the PR template
with the Issue link, change summary, CI or command results, and exact substrate
decision evidence.
