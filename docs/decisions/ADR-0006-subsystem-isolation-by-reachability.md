# ADR-0006 — Subsystem isolation is checked by reachability, not by a list of leaves

- **Status:** accepted
- **Date:** 2026-08-11
- **Consumers:** `internal/arch/coupling_test.go`; `CLAUDE.md` (Code conventions, the leaf boundary).

## Context

"Subsystems NEVER call each other" is enforced structurally by
`internal/arch/coupling_test.go` rather than by review. `TestSubsystemsDoNotImportEachOther`
scans each §5 package's `Imports`, `TestImports` and `XTestImports` for another §5
package, under every entry in `tagMatrix`. Its `allowedEdges` allowlist is empty and
CLAUDE.md says it should stay that way: when two subsystems need the same fact, the fact
goes in a **leaf** package they both depend on.

That check only sees **direct** imports, and the leaf construction is what makes the gap
matter. `TestLeavesImportNothingFromTheEngine` covers it for the four packages listed in
`leaves` — its comment states the failure mode exactly: the moment `ruleset` imports
`runtime`, every §5.D package depending on `ruleset` inherits that edge transitively and
the direct guard still passes.

The hole is that the protection is **membership-based**. A shared package that nobody
added to `leaves` gets neither check: not the direct one (it is not a subsystem), and not
the leaf one (it is not listed). Adding a package to `leaves` is a manual step with no
mechanical trigger, so the guard's coverage silently depends on someone remembering.

Measured on this tree, the transitive closure over production imports is currently
**empty** — no §5 subsystem reaches another by any path, under `""` or `substrate`. So
this ADR adds a tripwire to a clean tree rather than fixing a live violation.

## Rejected: derive `leaves` from "imported by ≥2 subsystems"

The first proposal was to compute the candidate leaf set — every non-subsystem package
imported by two or more subsystems — and fail when it differs from `leaves`, so a newly
shared package could not go unlisted.

It was rejected, and the reason is that **`≥2` is not the hazard**. Laundering needs only
one importer: `network → ruleset → runtime` is a real `network`/`runtime` edge whether or
not `runtime` also imports `ruleset`. A derived-`≥2` list would still miss a
single-importer package that reaches another subsystem, which is the very failure the
leaf guard exists to catch. Verified by simulation: injecting `ruleset → runtime` into the
import graph, a reachability check reports `network -> ruleset -> runtime` while the
direct-import guard reports nothing.

Three further defects, recorded so the option is not revisited:

1. **It fails on day one for the wrong reason.** `internal/model` is imported by every
   subsystem and lands in any derived set, but it is the shared type vocabulary, not a
   leaf, and cannot be model-ignorant by construction. The repair is a second hardcoded
   exempt list — trading a list one forgets to append to for a list one forgets to append
   to.
2. **Test-support packages break it.** Derived from the same
   `Imports ∪ TestImports ∪ XTestImports` union the existing guards use,
   `internal/store/storetest` becomes a candidate as soon as two subsystems' tests reach
   it — and `storetest` *must* import `store`, so `TestLeavesImportNothingFromTheEngine`
   would fail on it. Structurally correct, substantively wrong.
3. **It cannot express intent.** A derived list describes what is; it cannot pre-list a
   package that does not exist yet.

## Decision

**Subsystem isolation is checked by reachability: for each §5 subsystem, the transitive
closure of its production imports must contain no other §5 subsystem.** The failure
message prints the offending **path**, not just the endpoints.

`TestSubsystemsCannotReachEachOther` in `internal/arch` implements it. It runs alongside
the existing checks, which are kept:

- the **direct**-import check keeps scanning test and external-test imports, where the
  closure deliberately does not run;
- `leaves` and `inertLeaves` keep their guards, and keep carrying the *reason* each shared
  package is safe (inert, or model-ignorant). That classification is a real distinction
  the closure cannot make, so it stays documentation plus its own check, not something
  reachability replaces.

**The closure is scoped to production `Imports` only.** Test-import graphs are far more
connected — anything reaching `test/harness` transitively reaches every subsystem through
`orchestrator` — so a closure over them would report a path for every subsystem on a
correct tree. This is a genuine narrowing relative to the direct check, and it is the
price of the closure being meaningful at all.

**Tags:** the closure runs over the same `tagMatrix` as its siblings, and a path found
under any configuration is a failure. `substrate` adds packages to the graph, so a tag
absent from the matrix is a configuration where a transitive edge passes unseen — the
matrix comment already says this about the direct guard and it holds here.

## Consequences

A shared package now needs no registration to be covered: if it ever forms a path between
two subsystems, the closure reports it with the path spelled out. `leaves` stops being
load-bearing for *detection* and becomes what it already reads like — the record of why
each shared package is safe. Its list-completeness becomes a documentation concern rather
than a hole in the guard.

The cost is one more `go list` traversal per tag (~0.1 s per configuration, measured
against the existing ~3.5 s for the package).

## A tension this surfaced, left as it is

`TestGuardIsNotStale` requires every entry in `subsystems` to exist, while the
`subsystems` comment credits pre-listing `network` *before* its package landed so a bad
edge would fail "the day the package landed rather than the day someone remembered to add
it". Both cannot hold for a new package: pre-listing one today fails the staleness check.

That is a deliberate trade the `subsystems` comment already explains — "absent packages
are skipped" is a hole the moment the package is real, and a rename would disable the
checks while the test still passed. The staleness check is worth more than the ability to
pre-list, because it fails closed. Recorded here rather than changed, because reachability
does not depend on either: a package absent from every list is now covered anyway, which
removes most of pre-listing's value.
