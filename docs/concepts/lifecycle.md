# Sandbox lifecycle

## States and transitions

```text
create:       absent -> creating -> running
pause:                         running -> paused
resume:                                   paused -> running
destroy:                       running/paused -> destroyed -> absent
```

`creating` is an internal durable transition state used while creation owns
resources. `destroyed` is a tombstone state used while reclamation completes; a
completed destroy removes the record. The transition implementation and its
rollback paths are in
[internal/lifecycle/lifecycle.go](../../internal/lifecycle/lifecycle.go) and
[internal/lifecycle/failpath_test.go](../../internal/lifecycle/failpath_test.go).

## Pause and resume

Pause reaches a guest quiescent point, drains data-plane work, destroys the live
Placement, and preserves the Sandbox record, Overlay, and Mount binding. It is
filesystem-first persistence: process memory, PIDs, open sockets, and the Placement
IP are not saved. The durable Placement remains `stopping` until teardown and
mount release succeed. A teardown error preserves that fence and returns an
error; `state: paused` alone does not prove quiescence. Retry Pause or allow
reconciliation to finish cleanup. `placement_state: lost` records verified
termination, after which Resume and direct Volume writes can proceed.

Destroy retains its record and Mount leases on unresolved placement teardown.
It removes them only after the previous writer is confirmed absent. Failed
runtime inspection remains unknown and cannot authorize cleanup or reuse.

Resume rechecks admission, resolves current bindings, materializes rootfs and
Volumes, derives network rules, starts a new runtime unit, and probes `sandboxd`
before committing `running`. A failure rolls back to `paused` and reclaims
half-built artifacts.

## Serialization and events

Lifecycle verbs for one Sandbox share a transition lock and recheck their source
state with compare-and-swap. Independent sandboxes may transition concurrently.
The resulting state and outbox Event commit atomically. Tests cover lock ordering,
failure rollback, and the outbox invariant in
[internal/lifecycle/transitionlock_test.go](../../internal/lifecycle/transitionlock_test.go),
[internal/lifecycle/stress_test.go](../../internal/lifecycle/stress_test.go), and
[internal/lifecycle/outbox_test.go](../../internal/lifecycle/outbox_test.go).

## Health and reclamation

The reaper reads guest-observed activity and runtime health. Idle running
sandboxes are auto-paused. A missing isolation unit demotes the record to paused
and records `gone`, `unknown`, or `resource-exceeded`; the public Sandbox state is
still `paused`, not a separate `dead` state. The engine never automatically
resumes after a missing unit. A persisted `resource-exceeded` cause blocks resume
to avoid a deterministic crash loop.

Paused sandboxes may be destroyed after their retention duration. A zero idle
timeout disables automatic pause; zero retention disables automatic eviction.
The classifier and concurrent sweep behavior are tested in
[internal/lifecycle/reaper_test.go](../../internal/lifecycle/reaper_test.go).

## Manual cold recovery

`--manual-cold-recovery` leaves a sandbox paused after its transport-down budget
expires. The reaper still detects the failure and crash-pauses the placement;
starting a replacement requires an explicit Resume. Same-placement transport
child repair remains automatic. With the flag unset, transport-down escalation
continues to crash-pause and cold-resume automatically.

The active setting is reported as `manual_cold_recovery` by `GET /v1/info` and the
Go client's `Info` method. A caller that must change external routing or revoke
external authority before a replacement starts can require this setting before
admitting work. Keep it pinned across engine restarts. Ordinary idle timeouts
and paused retention remain independently configured.
