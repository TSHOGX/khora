# API overview

Khora exposes two protocol surfaces on one listener:

- HTTP/JSON for durable resources, lifecycle verbs, events, health, and streaming
  whole-file transfers;
- Connect-RPC for interactive process and filesystem operations inside a running
  Sandbox.

Clients connect to the orchestrator, never directly to `sandboxd`. The orchestrator
proxies data-plane calls to the current Placement and applies lifecycle barriers so
pause cannot race an admitted stream. See
[internal/orchestrator/dataplane.go](../../internal/orchestrator/dataplane.go) and
[internal/orchestrator/dataplane_barrier_test.go](../../internal/orchestrator/dataplane_barrier_test.go).

## Authorities

The generated [OpenAPI document](openapi.json) is the machine-readable control-plane
contract. [HTTP API](http.md) explains resource semantics, and
[Errors and events](errors-and-events.md) explains interpretation rules. The protobuf
source [transport.proto](../../proto/khora/transport/v1/transport.proto) defines the
data plane. The generated [CLI reference](cli.md) documents the current command tree.

The Go client in [pkg/khora](../../pkg/khora) provides typed control-plane methods,
streaming file access, error classification, and a Connect transport client.

## Deployment

The listener has no authentication or TLS. It defaults to loopback and refuses a
routable address unless explicitly overridden. Put the configured gateway in front
for remote use; see [Deployment boundary](../configuration/deployment-boundary.md).

## Engine identity

`GET /v1/info` returns `store_id` and `version` with `Cache-Control: no-store`.
The Go SDK exposes the same response through `Client.Info(ctx)` without caching.
`store_id` identifies the durable collection of engine records: a restart over
the same Store preserves it; a new Store receives a new opaque ID. `version`
identifies the running binary and is `dev` when no build version was supplied.

Adapters can associate object references and event cursors with this identity.
Invalidation and recovery policy remains outside the engine. Copying or restoring
the entire Store preserves its identity, so this endpoint does not detect backup
rollback, partial data loss, or cloned deployments. It is not an atomic request
target guard and does not prevent orphan cleanup. No schema compatibility or
version negotiation is implied.
