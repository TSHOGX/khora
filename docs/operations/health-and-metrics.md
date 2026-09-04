# Health and metrics

The orchestrator serves three unauthenticated operational endpoints on the same
listener as the API. Keep them behind the same deployment boundary.

## Liveness

`GET /healthz` returns `200` while the process is running and not draining. It
returns `503` during intentional shutdown. Store errors and a halted outbox
publisher do not fail liveness because restart does not remove their durable cause.

## Readiness

`GET /readyz` returns `503` while booting, draining, or when the push outbox
publisher has halted on a repeatedly failing event. A halted publisher does not
stop the HTTP API or cursor-based event reads. The standard orchestrator uses an
in-process log sink whose delivery does not return errors, and exposes no publisher
resume endpoint. An assembly that replaces `Engine.Publisher` with a fallible sink
must provide its own operator path to repair that sink and call `Publisher.Resume`;
restarting alone reads the same durable row and may halt again.

Both endpoints return JSON with `status`, optional `detail`, and `uptime`, and set
`Cache-Control: no-store`. Exact cases are tested in
[internal/orchestrator/health_test.go](../../internal/orchestrator/health_test.go).

## Metrics

`GET /metrics` uses Prometheus text format and reports process-level facts:

- `khora_up`, `khora_uptime_seconds`, `khora_ready`, `khora_draining`;
- outbox halted state, backlog, published count, head attempts, heartbeat age, and
  in-flight delivery age;
- `khora_sandboxes{state=...}` counts when the store can be read.

It does not duplicate per-Sandbox storage usage or placement-local network packet
counters. Machine-readable object snapshots are available under the `/v1`
Sandbox, Placement, and Process snapshot routes. `GET
`/v1/sandboxes/{id}/telemetry` is a separate cursorable Sandbox usage-sample
lane with a 10-second default cadence and bounded retention; Placement and
Process snapshots remain request/response reads, with no generic persisted sample
lanes for those objects. Unavailable measurements are not emitted as zero. If the
Sandbox count store read fails, those samples are omitted and the process log carries
the cause. Names and semantics are defined in
[internal/orchestrator/health.go](../../internal/orchestrator/health.go).
