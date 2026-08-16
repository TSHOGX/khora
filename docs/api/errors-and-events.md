# Errors and events

## HTTP error classes

Error responses use a plain one-line message. Interpret the status code rather than
parsing message text:

| Status | Meaning |
| --- | --- |
| `400` | malformed, incomplete, contradictory, or unenforceable submitted value |
| `403` | the guest filesystem refused a streamed file operation |
| `404` | the addressed engine object does not exist |
| `405` | the route exists but does not support that method |
| `409` | current durable or Placement state conflicts with the operation |
| `410` | an event cursor is older than the retained window |
| `413` | a streamed Sandbox upload exceeds the 8 GiB request limit |
| `500` | an internal store or provider operation failed |
| `503` | the instance is draining or a required live endpoint is unavailable |
| `507` | a valid request exceeds configured node capacity |

The Go client exposes a typed `*khora.Error` and helpers such as `IsConflict` and
`IsUnavailable`; see [pkg/khora/client.go](../../pkg/khora/client.go). Mappings are
tested at the API edge in
[internal/orchestrator/api_test.go](../../internal/orchestrator/api_test.go) and
[internal/orchestrator/volumes_test.go](../../internal/orchestrator/volumes_test.go).

## Event stream

Every externally visible lifecycle transition writes one durable Event atomically
with its state change. Event kinds are:

`created`, `paused`, `auto-paused`, `crash-paused`, `resumed`, and `destroyed`.

Only a `destroyed` Event carries `cause`: `requested` or `retention-expired`.
`crash-paused` does not put the isolation-unit failure in that field. Read the
Sandbox's separate `death_cause`, which may be `gone`, `unknown`, or
`resource-exceeded`; it is absent when no death was observed.

`created` and `resumed` Events carry `placement_address`: the guest-visible
address assigned to the Placement established by that transition. It is a
historical stream fact suitable for attributing an observation made while that
Placement existed. Other Event kinds omit the key. Addresses may be reused, and
resume deliberately establishes a new Placement, so a current lookup would not
be equivalent and could attribute an older observation to the wrong Sandbox.

Delivery is at least once. Deduplicate with Event `id`; advance replay with the
monotonic `seq`. They are intentionally different fields because a redelivered Event
keeps its ID while the sequence defines its durable position.

## Cursor replay

```sh
./bin/khora events list --after 0
./bin/khora events follow --after 0
```

`after=0` begins at the start of the retained window. Each page reports `earliest`
and `latest`. If a nonzero cursor has fallen behind retention, the server returns
`410` with those bounds; resynchronize application state, then restart from a cursor
appropriate to that recovery. Returning a short successful page would silently hide
lost Events, so the gap is explicit.

The store assigns order and retention; contracts for both memory and SQLite backends
are in [internal/store/storetest/contract.go](../../internal/store/storetest/contract.go).

The stream does not currently sample per-object usage. That request is tracked in
[Issue #3](https://github.com/TSHOGX/khora/issues/3). Replay retention is configured
with `--event-retention`; the default `0s` retains the full outbox. Set a positive
duration only when the deployment has chosen a replay window.
