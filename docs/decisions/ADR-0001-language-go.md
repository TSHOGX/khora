# ADR-0001 — Implementation language: Go

- **Status:** accepted
- **Date:** 2026-07-17
- **Consumers:** the whole engine codebase; anyone evaluating a rewrite.

## Context

Language is the first load-bearing choice: it cascades through every subsystem,
the store backend, the provider seams, and the wire toolchain. It is a one-time
decision, hence an ADR rather than spec content.

## Decision

**Go.**

## Rationale

- **gVisor/runsc ecosystem is Go.** The substrate we build on (runsc, its OCI
  handling, `--overlay2`/`--file-access` flags) is Go; staying in-language keeps
  interop, debugging, and vendoring friction low.
- **A transactional embedded store is available in-language.** Pure-Go, no-cgo
  SQLite delivers the §4.4-C atomic commit and the §1.1 "single binary + local
  store" target without leaving Go (backend decided in ADR-0004).
- **Team familiarity** — existing domain and toolchain knowledge is in Go.

## Rejected / deferred

- **Rust.** Stronger concurrency/memory-safety guarantees, which would help the
  §4.4 concurrency invariants and the §4.3 health-classifier races. But: gVisor
  interop, ecosystem maturity for our needs, and development velocity all favor
  Go today. Not permanently excluded — if the §4.4/§4.3 invariants prove hard to
  hold correctly in Go under fault injection, revisit.

## Caveat carried forward

Go's type system does **not** protect the invariants that actually matter here
(§4.4 the three, §4.3 health classification, §2.4 fail-closed) — those live in
concurrency/failure behavior. See `CLAUDE.md` → Execution & verification: a green
build proves nothing about them; fault-injection/e2e tests at the seams do.
