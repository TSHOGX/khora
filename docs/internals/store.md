# Store internals

## Contract

`store.Store` owns durable Sandboxes, Templates, Volumes, Mounts, and the Event
outbox. The in-memory and SQLite implementations run the same suite in
[internal/store/storetest/contract.go](../../internal/store/storetest/contract.go).
The contract covers compare-and-swap transitions, foreign-reference restrictions,
Mount uniqueness and exclusivity, ordering, and event retention.

The store does not call providers or make lifecycle decisions. The mediator chooses
the mutation and supplies the Event that belongs to it.

## Atomic state and Event commits

Every externally observable lifecycle transition mutates its record and appends its
Event in one store operation. SQLite performs both in one database transaction; the
memory store holds one mutex. Event sequence is assigned by the backend. A committed
state cannot lack its Event, and an Event cannot describe an uncommitted state.

Mount changes and cap edits do not emit Events because they are Binding writes, not
lifecycle transitions. Tests for this boundary are in
[internal/lifecycle/outbox_test.go](../../internal/lifecycle/outbox_test.go).

## Reference integrity

Template parents, Volume parents, Sandbox Template references, and Mount rows use
restricting relationships. Deletability is derived from references rather than a
stored count. SQLite schema constraints and equivalent memory-store checks are both
part of the shared contract.

An exclusive Volume lease is the existence of an `exclusive` Mount row. Atomic
insertion is required: a check followed by insert would allow two contenders to win.

## Publisher failure

The publisher reads the durable outbox in order and retries a failing head Event.
It halts after the configured attempt budget rather than skipping the Event and
breaking order. Halt state makes the process unready but not dead. Restarting reads
the same poison Event; recovery requires repairing the sink and calling
`Publisher.Resume` through the assembly that installed it. The standard orchestrator
uses an in-process log sink that cannot return a delivery error and exposes no resume
endpoint. A custom assembly that supplies a fallible sink also owns that operator
path.

Delivery is at least once because a sink may accept an Event before marking it
published fails. Consumers deduplicate by Event ID. Failure and concurrency tests
are in [internal/store/publisher_test.go](../../internal/store/publisher_test.go).

## Durable identity

The Store contract exposes `Identity(ctx)`. SQLite initializes the singleton
identity atomically with a new empty database; Memory assigns one per instance.
Opening existing state without its identity, or with an invalid identity, fails
instead of assigning a replacement. The `0.0.x` policy requires rebuilding old
state rather than migrating it. The identity lives in the same database as the
objects; a separate directory marker is not an authority.

Sandbox records carry a durable positive mutation revision. Lifecycle changes,
resource/network binding writes and mount-binding changes advance it in the
same transaction as the corresponding mutation. Memory and SQLite share the
`SandboxMutationRevision` contract test; SQLite reopen preserves it. The integer
cannot wrap or silently become a floating-point value. The revision does not
replace Placement epochs, and binding edits do not reset retention timestamps.
