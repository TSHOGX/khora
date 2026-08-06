# ADR-0004 — Persistent store backend: SQLite (modernc.org, pure-Go)

- **Status:** accepted
- **Date:** 2026-07-20
- **Consumers:** `internal/store` (the persistent implementation); everything that relies on the §4.4-C atomic-commit contract.

## Context

`store.Store` is a Behavior seam (§1.3): `store.Memory` (a mutex over a map)
serves unit and contract tests, and the persistent backend must be durable behind
the **same interface**. That backend must deliver the §4.4-C contract —
"lifecycle-state change + outbox append = single atomic commit" — and fit the §1.1
deployment target: single binary + one local store, no heavy external dependency.

## Decision

**SQLite via `modernc.org/sqlite`** (pure-Go, `CGO_ENABLED=0`).

## Rationale

- **Native multi-key transactions make §4.4-C the trivial case.** "Write the state
  row + append the outbox row" is one `BEGIN/COMMIT`. The store method promises
  atomicity and the backend delivers it directly — no branching in the publish
  algorithm on "does this backend support transactions" (the §4.4-C failure mode).
- **Pure-Go, zero-cgo** keeps `CGO_ENABLED=0`, the same static-ELF constraint
  sandboxd already ships under — one toolchain story across the repo.
- **Outbox is a first-class table**, queried by `published_at IS NULL`; the
  event-stream evolution (replay window, schema) has natural room to grow.
- **Single-file embedded**, which is what the §1.1 deployment target asks for.

## Consequences

The seam is the whole point: **no caller may branch on backend capability** (§4.4-C).
The one publish algorithm is written once against the interface; swapping SQLite for
bbolt (or anything else) must never edit that algorithm. Per ADR-0001's consequence,
the atomic-commit behavior is proven behaviorally, not by the type system: the shared
store contract suite that BOTH backends run (`internal/store/storetest`), plus
crash-reconciliation in `test/reconcile_substrate_test.go`.

## Rejected / deferred

- **bbolt.** Pure-Go, single-file, zero-cgo, simpler ops (no SQL). Rejected because
  multi-key atomicity is manual: §4.4-C would degrade to "pack state + event into
  one key/bucket in a single `Update()` tx." The spec explicitly anticipates that
  degrade path, so bbolt *could* satisfy the contract — but SQLite expresses the
  outbox and the atomic commit without contortion, and its query surface fits the
  event stream better. Not permanently excluded: the seam makes it a
  register-an-implementation swap if operational simplicity ever outweighs.
