# Engine objects

The canonical types are in [internal/model/model.go](../../internal/model/model.go).
IDs are opaque references. They do not encode a host, path, caller, or belonging.

## Sandbox

A Sandbox is the persistent unit. Its record names a Template, an Overlay, limits,
network declaration, lifecycle state, and retention settings. Its identity remains
stable while its Placement, IP address, cgroup, and processes may change.

## Template and Overlay

A Template is an immutable, read-only base. A promoted Template may name a parent,
forming an overlayfs lower stack. A Template exists only after its bytes have been
materialized; live references prevent deletion. Its typed manifest records schema,
platform, digest and capability declarations and is immutable with the bytes.

An Overlay is the sandbox's writable filesystem delta. It is an independent,
ID-addressed artifact, not a host path and not part of the Template. Pause preserves
it; destroy reclaims it after the record transition.

## Volume and Mount

A Volume is storage whose lifecycle is independent of any Sandbox. Snapshot and
clone create new Volumes with lineage. A Mount is the binding between one Sandbox
and one Volume. Access mode belongs to the Mount:

- `exclusive`: no other Mount may coexist for that Volume;
- `shared-ro`: read-only in the guest;
- `shared-rw`: writable by multiple sandboxes, without a cross-sandbox cache
  coherence guarantee.

Mount paths are absolute, normalized, non-root guest paths. References protect a
Volume with any Mount binding and a parent with descendants from deletion. Store contract tests
exercise those rules in
[internal/store/storetest/contract.go](../../internal/store/storetest/contract.go).

## Binding and Placement

A Binding is the mutable declaration consumed on Placement: CPU, memory and disk
ceilings, idle and retention durations, the complete network declaration, and the
Mount table. A patch changes only fields present in the request; zero remains a
meaningful value.

A Placement is the live, replaceable runtime realization of a Sandbox. Pause
destroys it. Resume produces a new Placement from current durable bindings.
The durable `(sandbox_id, epoch)` pair identifies one realization: adoption
keeps the epoch, while resume advances it. Requests carrying an old epoch are
rejected as stale rather than redirected. The pure contract and reset rules are
maintained in [Durable contract foundation](../internals/contracts.md).

## Process and Operation

A Process is addressed by an opaque handle. The guest PID is only a locator
inside one Placement epoch; retained exit state belongs to the Process record.
Pause removes the current locator, and Sandbox deletion reclaims its remaining
Process records.

An Operation records one caller-supplied idempotency key and normalized request
hash before external effects. Its phase makes crash recovery deterministic, and
its terminal result is replayed rather than materialized again.

## Event

Every externally visible lifecycle transition commits an Event in the same store
transaction as its state change. The outbox assigns a monotonic sequence cursor;
the Event ID remains stable across redelivery. See
[Errors and events](../api/errors-and-events.md).
