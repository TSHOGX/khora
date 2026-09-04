# HTTP API

All resource routes are under `/v1`. JSON requests reject unknown fields, except
that read-only network members emitted by the engine may be echoed during a
read-modify-write update.

## Sandboxes

| Method and path | Result |
| --- | --- |
| `POST /v1/sandboxes` | create and cold-start a Sandbox |
| `GET /v1/sandboxes` | list Sandboxes without per-item backend usage reads |
| `GET /v1/sandboxes/{id}` | inspect one Sandbox, including readable Overlay usage |
| `PATCH /v1/sandboxes/{id}` | replace present Binding fields |
| `DELETE /v1/sandboxes/{id}` | destroy and reclaim a Sandbox |
| `POST /v1/sandboxes/{id}/pause` | quiesce and remove its Placement |
| `POST /v1/sandboxes/{id}/resume` | cold-start a new Placement |
| `POST /v1/sandboxes/{id}/activity` | stamp guest-observed activity |
| `GET /v1/sandboxes/{id}/telemetry` | page retained Sandbox usage samples by cursor |
| `GET /v1/sandboxes/{id}/snapshot` | read the Sandbox resource snapshot with availability flags |
| `GET /v1/sandboxes/{id}/placement` | read the current Placement epoch and resource snapshot |
| `GET /v1/sandboxes/{id}/processes` | list durable logical Process records for the Sandbox |

`GET /v1/processes/{id}` reads one durable logical Process snapshot, including
retained exit status and explicit resource-measurement availability.

Create requires `template_id`. Omitted network normalizes to default deny. Patch
distinguishes an omitted member from zero: omitted leaves the Binding unchanged;
zero removes a cap or disables the relevant TTL. Mounts and network declarations
are whole replacements.

`POST /v1/sandboxes` and `POST /v1/volumes` accept an `Idempotency-Key`
header. Repeating a key with the same normalized request returns the original
object; changing the request under that key returns `409`.

## Templates and Volumes

| Method and path | Result |
| --- | --- |
| `GET /v1/templates` | list immutable Templates |
| `GET /v1/templates/{id}` | inspect a Template and its typed manifest |
| `POST /v1/templates` | promote a running or paused Sandbox Overlay |
| `DELETE /v1/templates/{id}` | delete an unreferenced Template |
| `POST /v1/volumes` | create a Volume |
| `GET /v1/volumes` | list Volumes without usage reads |
| `GET/PATCH/DELETE /v1/volumes/{id}` | inspect, resize, or delete |
| `POST /v1/volumes/{id}/snapshot` | create a snapshot Volume |
| `POST /v1/volumes/{id}/clone` | create a clone Volume |
| `POST /v1/volumes/{id}/import?path=...` | stream bytes into a Volume |
| `GET /v1/volumes/{id}/export?path=...` | stream bytes from a Volume |
| `GET /v1/volumes/{id}/stat?path=...` | read root-confined metadata |
| `GET /v1/volumes/{id}/lstat?path=...` | read metadata without following a symlink |
| `GET /v1/volumes/{id}/list?path=...` | list a root-confined directory |
| `GET /v1/volumes/{id}/read?path=...&offset=&length=` | read a bounded byte range |
| `PUT /v1/volumes/{id}/write?path=...&offset=` | write raw bytes at an offset |
| `POST /v1/volumes/{id}/mkdir?path=&mode=` | create a directory |
| `POST /v1/volumes/{id}/move` | rename within the Volume |
| `DELETE /v1/volumes/{id}/remove?path=...` | remove a path idempotently |

Usage fields are absent when the backend cannot read them, not reported as zero.
Volume size is a capacity cap; a new cap must be strictly greater than current
usage. Any Mount binding and any descendant protect a Volume from deletion.

The telemetry route is the only persisted usage-sample lane. Placement and Process
resource data is available through their snapshot routes and is not retained as a
generic sample history by the engine.

## Network and files

| Method and path | Result |
| --- | --- |
| `GET /v1/sandboxes/{id}/network` | read the declaration and live diagnostics |
| `POST /v1/sandboxes/{id}/publish` | idempotently publish a guest port |
| `GET /v1/sandboxes/{id}/files?path=...` | stream a guest file |
| `PUT /v1/sandboxes/{id}/files?path=...` | replace a guest file |

The network declaration itself is replaced through the Sandbox `PATCH`. Egress
edits require `paused`; publish accepts `running` or `paused` and reports its effect
point. Sandbox file routes require a live Placement.

## Events and operations

`GET /v1/events?after=<seq>&limit=<n>` pages the durable outbox. `GET /healthz`,
`GET /readyz`, and `GET /metrics` are outside `/v1`. See
[Errors and events](errors-and-events.md) and
[Health and metrics](../operations/health-and-metrics.md).

For exact request and response schemas, status codes, and required fields, use the
generated [OpenAPI document](openapi.json).
