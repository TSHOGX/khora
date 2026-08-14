# ADR-0008 — What the engine repo ships for its consumers: a machine-readable contract plus one thin Go client, and why the ergonomic SDK is not ours

- **Status:** accepted
- **Date:** 2026-08-12
- **Consumers:** anyone writing a program against the §6.1 API; `pkg/khora`,
  `cmd/khora`, `proto/`, and the README's client section carry the result.

## Context

`cmd/khora` already covers the §6.1 surface — sandbox lifecycle, files, volumes,
templates, network, events, process, health — but it is `package main`, so **no
program can import it**. Every Go consumer therefore re-derives the same wire
protocol, and the README says so plainly ("No client library ships yet for other
languages").

The obvious fix — move those files to an importable path — is **not** the whole fix,
and the reason is structural rather than stylistic:

- **The generated wire stubs are behind `internal/`.** `cmd/khora`'s process and
  health commands imported the module's `internal/transport/gen/...` directly, and Go's
  internal rule means an external module cannot. Relocating the CLI's files without
  relocating the stubs produces a package that is still unimportable for exactly the
  two subsystems (process, fs) a consumer most needs.
- **The REST half has no machine-readable contract at all.** `buf.gen.yaml`
  generates Go stubs for `khora.transport.v1`, which covers the Connect data plane;
  the control plane is REST/JSON described in prose in
  [`api-contract.md`](../spec/api-contract.md). A non-Go consumer has to transcribe
  thirty-odd endpoints by hand from a document — which is the "every consumer
  rewrites the wire protocol" cost, relocated rather than removed.
- **An SDK is where product semantics arrive.** The reference implementations put
  template resolution, per-call timeout defaults, and interpreter-level concepts in
  their SDK, and they are right to: those belong to a product. Here, CLAUDE.md's
  first principle and §1.1's vocabulary boundary mean such a layer **cannot live in
  this repo** — `session` / `agent` / `keepalive` are precisely the names the
  boundary exists to keep out, and an "ergonomics" package is the most natural door
  for them to walk back in through.

## Decision

**The engine repo ships the contract and one thin client. Ergonomics ship elsewhere.**
Three parts.

### 1. The contract is machine-readable on both protocols

`proto/` already is, for the Connect data plane. The control plane gains an
**OpenAPI document generated from, and checked against, the live handlers** — not a
hand-maintained parallel description, which would become a second drifting account
of §6.1 (the exact failure the spec discipline exists to prevent). A test pins the
document against the router so an endpoint added without its contract entry fails
the build rather than a consumer's integration.

`api-contract.md` stays authoritative for **intent and rationale**; the OpenAPI
document is the **transcription** of the shapes it fixes. When they disagree, the
divergence is resolved by CLAUDE.md's Relationship rule like any other, and the
generated artifact is never the side that gets to win by default.

### 2. One thin Go client, at `pkg/khora`

Thin has a precise meaning here, and it is the whole reason this package is allowed
to exist inside the engine repo:

- **Every field maps 1:1 onto a §6.1 field.** No composite convenience type that
  fuses two responses.
- **No invented verb.** If the API has no `waitUntilRunning`, neither does the
  client. Polling, retry policy, and backoff are the caller's, because each is a
  *promise about time* the engine never made.
- **No state that outlives a call.** The moment the client caches, leases, or tracks
  a subscriber it has become a product layer with a client's name.
- **§1.1's vocabulary boundary applies to it verbatim.** It is engine-facing code in
  the engine repo, so the banned entity names are banned here too.

`cmd/khora` becomes a shell over it, which is also what keeps the package honest:
the CLI exercises every call, so a client method no command can use is visible
immediately.

### 3. The generated stubs move out of `internal/`

`internal/transport/gen/...` → **`pkg/transport/gen/...`**, an importable path,
because part 2 is otherwise undeliverable for process and fs.

`pkg/transport/gen` rather than `pkg/khora/gen`: the tree is the **protocol**, and
part 2's thin client is one consumer of it rather than its owner. A Go consumer that
wants the Connect data plane and none of the client's opinions imports it directly,
which is the same relationship `internal/transport` has to it — and nesting it under
the client would have implied a dependency that runs the other way. Two structural consequences, both handled rather
than absorbed:

- **`internal/arch`'s guards keep their force.** The generated tree is not a
  subsystem and imports nothing from the engine, so relocating it creates no §4
  edge: `internal/transport` (§5.E) and `internal/orchestrator` reaching it is the
  same shape as before. It joins the leaf guard, since "imports nothing from the
  engine" is a property worth checking rather than assuming — see ADR-0006 for why
  these are tests and not review items.

  One exemption was needed and it is narrow: protoc emits the tree as **two**
  packages, so the connect stubs necessarily import the message types. The leaf guard
  therefore ignores an import that stays inside the generated tree, scoped by a named
  prefix constant so it cannot widen — every engine package is under the module's
  `internal/`, which is precisely what the guard still refuses there.
- **`buf.gen.yaml`'s output path moves with it**, in the same change as the import
  rewrite, so a regeneration cannot resurrect the old tree (the regeneration rule is
  in the `proto-regeneration` convention: regenerate in the same change as the edit).

### What is explicitly NOT ours

An ergonomic SDK — waiters, retries, streaming helpers, and any per-language idiom —
belongs in the **consuming** repo. For languages other than Go it needs nothing from
us but the contract in part 1: `protoc`/`buf` and any OpenAPI generator produce the
stubs, and Go's internal rule is irrelevant to them. So the engine repo does not
carry a TypeScript or Python build chain, and declining that is not a gap: it is the
same boundary that keeps authentication outside (ADR-0007) applied to client code.

## Consequences

- A Go consumer imports `pkg/khora` and writes no wire code. A non-Go consumer
  generates stubs from `proto/` plus the OpenAPI document.
- **Two artifacts now describe §6.1**, and the drift risk is real — mitigated by
  generating the OpenAPI document from the handlers and testing it against the
  router, never by promising to keep two documents in step.
- **`pkg/khora` is a compatibility surface the rest of the repo is not.** While the
  no-compatibility-burden rule holds (no live instances), that costs nothing; the
  first external consumer is when it starts to.
- The thinness rules above are prose today. If the package starts accreting
  helpers, the enforcement precedent to copy is `internal/orchestrator/edge_test.go`
  — an AST check at the boundary, not a review habit.
