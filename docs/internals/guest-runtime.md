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

Guest PID 1 owns the child registry, descriptors, and event fan-out. Transport roles
are replaceable RPC frontends connected to that supervisor over a private Unix socket.
A process receives an opaque handle and a Placement epoch; the PID remains only a
local locator. A process may have multiple subscribers. Each connection receives the
bounded output tail before live events, with monotonically increasing sequence
numbers. A cursor gap emits an explicit overflow/reset marker and requires a rescan.
Subscriber queues remain bounded, so a slow reader is detached rather than blocking
child output or other subscribers. Handle, output-ring, and exit metadata are
persisted under the guest runtime scratch area, while stdin/PTY descriptors remain
owned by PID 1; a transport-child restart therefore preserves live output and input
state instead of reconstructing an inert process record.

PTY and pipe modes are separate paths. PTY merges terminal output and supports resize;
pipe mode preserves stdout and stderr. Unit coverage is in
[cmd/sandboxd/process_test.go](../../cmd/sandboxd/process_test.go), with the real
slow-reader case in `test/slow_subscriber_substrate_test.go`.

## Filesystem watch

The filesystem watch source is conservative change detection: a placement-local
poller snapshots each requested path (recursively when requested), compares
metadata and emits an independent sequence. A bounded journal is retained in
the guest runtime scratch area, so a transport-child restart can replay changes
after a caller's cursor. The journal is not workload content and is excluded
from Template promotion.

The journal and each subscriber queue are bounded. If a cursor falls below the
retained watermark, or a subscriber cannot keep up (including when the terminal
exit frame cannot fit), the stream emits an `overflow`/`reset` marker with
sequence zero and returns a resource-exhausted cursor-gap error; callers must
rescan and reconnect. A placement epoch change
invalidates the journal and closes existing streams after a `placement` reset.
The orchestrator checks the durable epoch while an otherwise idle stream is
open, so pause/resume does not depend on a later filesystem event to make the
reset visible. Watch semantics deliberately do not claim syscall-complete
history; provider/source improvements can preserve this cursor and recovery
contract.

## Activity signal

Activity is measured inside the guest across all transport connections and returned
by the health probe as one idle duration. Host socket activity cannot substitute:
keepalives and proxy traffic are not workload activity, while guest work may occur
through more than one connection. Tests live in
[cmd/sandboxd/activity_test.go](../../cmd/sandboxd/activity_test.go).
