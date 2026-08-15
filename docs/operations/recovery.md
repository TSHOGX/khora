# Recovery

## Control-plane restart

Running runsc units survive a graceful orchestrator stop. On startup, reconciliation
asks the runtime provider to adopt surviving Placements, restores provider handles,
reinstalls network policy, and probes the guest transport. Durable `running` state
is kept only when the Placement can be safely adopted.

Missing or unhealthy units are demoted to `paused`; the observed death cause is
committed with the transition Event. A recoverable Sandbox may be resumed by a
caller. `resource-exceeded` refuses resume to avoid a deterministic crash loop.
Raise that Sandbox's `memory_byte` cap to clear the cause, subject to node
admission, or destroy the Sandbox.

## Failed transitions

Lifecycle operations return to their source state when construction fails. Cleanup
uses the artifacts accumulated so far; it does not rely on a second compensating
transaction. Startup sweeps reclaim provider artifacts that no durable record owns,
including interrupted create and promote work.

The failure paths and orphan rules are exercised by
[internal/lifecycle/failpath_test.go](../../internal/lifecycle/failpath_test.go),
[internal/lifecycle/reconcile_test.go](../../internal/lifecycle/reconcile_test.go),
and [internal/lifecycle/sweep_test.go](../../internal/lifecycle/sweep_test.go).

## Periodic reconciliation

Periodic reconciliation uses the same health classifier under each Sandbox's
transition lock. Locked Sandboxes are skipped rather than racing a verb. Transport
failure has a bounded restart budget before the record is demoted. Reaper behavior
and concurrent transition cases are in
[internal/lifecycle/reaper.go](../../internal/lifecycle/reaper.go) and its tests.

## Persistent state

The `0.0.x` line permits rebuilding persistent state. There are no schema migrations
or compatibility shims. Before manual repair, stop the orchestrator and preserve a
copy of the store and state roots for diagnosis. Prefer reconciliation and supported
verbs over editing records or quota metadata by hand.
