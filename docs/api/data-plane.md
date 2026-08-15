# Data-plane API

The data plane is defined by
[transport.proto](../../proto/khora/transport/v1/transport.proto) and served by
`sandboxd` through the orchestrator's Connect-RPC proxy.

## Process service

`ProcessService` supports:

- `Start`: start a command and stream output and exit events;
- `Connect`: replay a tracked PID's bounded output tail, then subscribe to live events;
- `StreamInput`, `SendInput`, and `CloseStdin`: deliver stdin;
- `SendSignal`: signal a sandbox process;
- `Update`: change PTY size;
- `List`: list live PIDs.

PTY dimensions must be supplied together. Multiple subscribers can observe one
process. A connection replays the bounded per-process output tail before live
events; this is not an unbounded or durable log. Output fan-out is concurrent and
bounded, and a slow subscriber is detached instead of blocking the process. Process
semantics and reaping are implemented in
[cmd/sandboxd/process.go](../../cmd/sandboxd/process.go) and
[cmd/sandboxd/reap.go](../../cmd/sandboxd/reap.go).

## Filesystem service

`FsService` supports read, write, stat, directory listing, mkdir, remove, and move.
Read may include an offset and length. The service has no watch primitive.

Large whole-file transfers use the HTTP streaming routes instead of placing an
entire file in one RPC message. The Connect client also provides streamed reads and
writes; see [internal/transport/client.go](../../internal/transport/client.go).

## Availability and lifecycle barriers

The data plane exists only for `running`. Before proxying a call, the orchestrator
admits it through the Sandbox barrier. Pause closes admission, waits for admitted
operations to drain, then asks the guest to quiesce. Calls admitted before the close
may finish; later calls receive a conflict. Proxy failures after admission are
reported as unavailable rather than rewritten as a lifecycle result.

The barrier behavior, error mapping, and signal preservation are covered by
[internal/orchestrator/dataplane_barrier_test.go](../../internal/orchestrator/dataplane_barrier_test.go),
[internal/orchestrator/dataplane_error_test.go](../../internal/orchestrator/dataplane_error_test.go),
and [internal/orchestrator/dataplane_signal_test.go](../../internal/orchestrator/dataplane_signal_test.go).
