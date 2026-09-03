# Transport internals

## Boundary

The transport subsystem exposes only process and filesystem mechanisms. The guest
endpoint is reachable from the orchestrator inside the Sandbox network namespace;
external callers use the orchestrator proxy. The fixed guest port is shared through
the leaf package `internal/guestport` so runtime and transport do not depend on one
another.

The seam is defined in
[internal/transport/transport.go](../../internal/transport/transport.go), with the
Connect implementation in `client.go`. Generated protobuf and Connect code lives in
`pkg/transport/gen` and is checked against its canonical proto source.

## Health and activity

One health RPC reports transport readiness and guest-observed idle duration. A
successful response implies both guest service and runtime unit are alive. Failure
does not: lifecycle then calls runtime adoption to distinguish a transport restart
from a dead Placement.

## Streaming behavior

Process start and connect are server streams. Input may be streamed or sent as a
single request. Filesystem operations are request/response, with bounded message
sizes; large whole-file traffic uses the HTTP stream routes. The generated protocol
also includes a separate bounded filesystem Watch stream with change-detection
semantics, an independent placement-scoped cursor, and explicit overflow/reset
notifications. The guest retains a bounded journal and snapshot baseline across
transport-child restarts. A cursor gap is returned as resource-exhausted after
an overflow/reset marker; callers must perform a full rescan. The orchestrator
polls the durable placement epoch while forwarding an open stream so a
pause/resume reset is visible even when no filesystem event arrives.

The client uses I/O readers for streaming rather than collecting full files in
memory. Compatibility between client calls and server methods is covered by
[internal/transport/client_test.go](../../internal/transport/client_test.go) and
[test/transport_substrate_test.go](../../test/transport_substrate_test.go).

## Lifecycle proxy barrier

Before proxying mutating data-plane work, the orchestrator registers it with the
lifecycle barrier. Pause stops new entries and drains existing ones. Endpoint lookup
and proxying occur within the admitted window, closing the race where pause destroys
the Placement between a state check and a request.

The barrier is intentionally outside the transport package because it is a lifecycle
mechanism. Tests cover admission ordering, unavailable endpoints, and preservation of
Connect error details in [internal/orchestrator](../../internal/orchestrator).
