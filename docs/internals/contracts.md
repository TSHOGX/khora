# Durable contract foundation

This page records the Phase 0 contract shared by the control plane and its
protocol edges.  The pure types live in
[`internal/model/contracts.go`](../../internal/model/contracts.go); they do not
perform I/O and do not select a provider.  Store, protobuf, OpenAPI, and client
schemas may project these values in later phases, but none may change their
meaning independently.

## Durable objects

| Object | Durable identity | Placement-local facts excluded from the record |
| --- | --- | --- |
| Sandbox | `SandboxID` | provider handle, address, PID, and transport connection |
| Placement | `(SandboxID, PlacementEpoch)` | host paths, namespace descriptors, and provider handles |
| Process | opaque `ProcessID` | guest PID is only a locator within its Placement epoch |
| Template | immutable bytes plus `TemplateManifest` and optional digest | mutable runtime state |
| Operation | `(scope, idempotency key)` plus request hash | transient worker state |

The first cold realization uses epoch `1`.  `Resume` increments the epoch and
`Adopt` preserves it.  A request that carries a different epoch fails with the
`stale_epoch` class; it is never silently redirected to the current
Placement.  Epoch wraparound is refused.

An exited Process remains addressable for 24 hours by default. At the inclusive
deadline cleanup changes the durable Process state to `reclaimed` and clears its
PID locator and exit code. This is not a Sandbox lifecycle event: the existing
outbox remains the closed six-kind Sandbox transition stream. Destroying the
parent Sandbox removes all remaining Process records immediately. The exit
status is part of the logical Process record until its retention deadline, not
inferred from a later PID lookup.

The process-retention setting is a launch-time deployment policy. Each new
Placement receives the selected value in its guest PID-1 configuration, while
the control-plane Store uses the current deployment value for reclamation.
Operators changing the setting must create new Placements (pause/resume) before
expecting an existing guest supervisor to use the new window.

## Operation ledger

Sandbox and Volume creation records an `Operation` in the `recorded` phase
before materializing external bytes.  The request hash is the canonical
`sha256:<hex>` hash of JSON.  A retry with the same scope, key, and hash replays
the durable result.  Reusing a key with another hash returns
`idempotency_conflict` and cannot start a second side effect.

The operation lease is 30 seconds by default.  On restart, an expired
`in_progress` row may be taken over, incrementing `Attempt` while preserving the
key and hash.  Recovery is deterministic from `Phase`:

- `recorded`: retry; no external side effect was started;
- `materializing`, `materialized`, or `cleaning`: clean up the possible side
  effect, then retry;
- `committed`: verify the durable object and replay the result.

Only the terminal state and stable error class are replayed.  Diagnostic error
text is not part of the idempotency contract.

## Cursors and resets

`Cursor` is a versioned, URL-safe encoding of `(stream, object, epoch,
sequence)`.  Lifecycle, Process output, filesystem watch, and telemetry each
have an independent sequence.  Process-output and watch cursors carry the
current Placement epoch; lifecycle and telemetry cursors do not.

Sequence zero is the initial position.  A retained window reports its earliest
and latest sequence.  A cursor below the retained window returns
`cursor_expired` instead of a short successful page.  A bounded stream that
overflows returns `cursor_gap` and `rescan=true`.  Pause/resume creates a
placement reset with sequence zero in the new epoch; output and watch state is
not carried across that boundary.

The encoding currently uses the `v1.` prefix followed by unpadded URL-safe
base64 of the canonical JSON payload.  Decoding rejects unknown fields and
unknown versions, so generated or handwritten edge drift fails closed.

## Decision-gate policies

The following values are the smallest bounded policies that satisfy the ask
without making completeness claims the substrate has not measured:

| Gate | Contract decision | Evidence anchor |
| --- | --- | --- |
| Process retention | 24-hour exited-handle window by default; launch-time policy is shared by guest replay and Store reclamation; durable `reclaimed` state at expiry; Sandbox destroy removes remaining records | `TestProcessExitRetentionAndReclaimBoundary`; `TestColdStartCarriesProcessRetentionToRuntime`; Store contract `ProcessCore` |
| Filesystem watch | change-detection semantics, recursive by default, 4096-event bound, explicit overflow gap and full rescan, reset on epoch change | `TestDecisionGatePolicies`; `WatchPolicy.Validate` |
| Telemetry | 10-second Sandbox usage-sample interval, 24-hour retention, per-Sandbox timestamp and independent cursor lane; Placement and Process expose snapshots without persisted sample lanes; lifecycle publication does not wait on samples | `TestDecisionGatePolicies`; `TelemetryPolicy.Validate` |
| Template manifest | schema version 1, optional SHA-256 digest, `os/arch` platform, sorted unique capabilities, provenance, and SBOM reference | `TestTemplateManifestNormalizationValidationAndDigest` |
| Operation recovery | 30-second lease, takeover only after expiry, and phase-based cleanup classification | `TestOperationHashKeyReuseAndRestartTakeover` |

Watch source selection remains behind the provider/guest seam.  The contract
therefore promises change detection and recovery behavior, not syscall-level
completeness.  A future substrate spike may improve the source without
changing cursor, epoch, overflow, or rescan semantics.

## Error and measurement vocabulary

`ErrorClass` is protocol-neutral.  HTTP and Connect edges map classes to their
own status/code values; callers do not parse diagnostic text.  The foundation
distinguishes invalid input, ordinary state conflict, idempotency conflict,
stale epoch, unavailable endpoint, unsupported capability, expired cursor,
cursor gap, in-progress operation, not found, and internal failure.

`Measurement` always carries an explicit `available` bit.  `{available:true,
value:0}` is a measured zero; `{available:false,value:0}` is unavailable.  An
unavailable reading is never converted to zero by a serializer or admission
decision.

Negative fixtures in
[`internal/model/testdata`](../../internal/model/testdata) keep these rules
mechanical: stale epochs, idempotency-key reuse, unavailable measurements, and
an unexpected generated field are all refused by tests.
