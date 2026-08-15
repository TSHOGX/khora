# Guest runtime internals

## Process topology

The same `sandboxd` binary has init and transport modes. Init is guest PID 1. It
starts the transport child, forwards lifecycle signals, and reaps orphaned children.
Keeping the roles separate allows transport restart without replacing PID 1 and
keeps user processes outside the supervisor's own protocol loop.

PID 1 behavior is implemented in
[cmd/sandboxd/init.go](../../cmd/sandboxd/init.go) and
[cmd/sandboxd/reap.go](../../cmd/sandboxd/reap.go). Tests cover signal forwarding,
orphan reaping, transport restart, and injected failure through the `testguest` tag.

## Quiescence

Pause first closes host-side admission to new mutating requests and drains admitted
ones. It then asks `sandboxd` to reach the guest quiescent point before runtime
teardown. The host barrier closes the deterministic race where an engine-issued
write has truncated a file but not completed it.

The drain has a timeout and pause proceeds after it. Filesystem-first persistence
does not claim application-consistent memory or arbitrary workload cooperation; the
same durable Overlay must remain recoverable after a crash where no guest handshake
was possible. The protocol is tested end to end in
[test/pause_drain_substrate_test.go](../../test/pause_drain_substrate_test.go).

## Process service

The transport role owns the child registry and event fan-out. A process may have
multiple subscribers. Each connection receives the bounded output tail before live
events. Each subscriber also has a bounded queue; a slow subscriber is detached
rather than blocking child output or other subscribers. Exit state remains available
for reconnect semantics, but emitted bytes are not a durable replay log.

PTY and pipe modes are separate paths. PTY merges terminal output and supports resize;
pipe mode preserves stdout and stderr. Unit coverage is in
[cmd/sandboxd/process_test.go](../../cmd/sandboxd/process_test.go), with the real
slow-reader case in `test/slow_subscriber_substrate_test.go`.

## Activity signal

Activity is measured inside the guest across all transport connections and returned
by the health probe as one idle duration. Host socket activity cannot substitute:
keepalives and proxy traffic are not workload activity, while guest work may occur
through more than one connection. Tests live in
[cmd/sandboxd/activity_test.go](../../cmd/sandboxd/activity_test.go).
