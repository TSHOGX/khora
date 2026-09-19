# Lifecycle internals

## One coordinator for verbs

`internal/lifecycle.Mediator` is the only place that composes runtime, rootfs,
volume, network, transport, and store operations into lifecycle verbs. Mechanism
packages receive resolved facts and never call each other. The architectural tests
in [internal/arch/coupling_test.go](../../internal/arch/coupling_test.go) check direct
and transitive reachability under every registered build-tag combination.

## Transition serialization

Every Sandbox has a keyed transition lock. A verb reads and validates state again
after acquiring it, then commits with a store compare-and-swap. The keyed lock entry
is reclaimed when unused, so churn cannot grow an ID-indexed map forever.

The lock protects lifecycle state and binding updates for one Sandbox, not all
Sandboxes. `transitionlock_test.go` injects overlap and stale-state cases;
`stress_test.go` exercises independent concurrent transitions.

## Failure rollback

Create and resume collect cleanup handles while materializing a Placement. Failure
unwinds those handles in reverse ownership order and returns the durable record to
its source state. No intermediate `failed` state or compensating transaction exists.
If cleanup itself is interrupted, startup sweeps compare provider artifacts with
the keep set derived from the store.

[internal/lifecycle/failpath_test.go](../../internal/lifecycle/failpath_test.go)
injects failure at materialization boundaries. Reconciliation and orphan cases are
covered by `reconcile_test.go` and `sweep_test.go` in the same package.

## Pause barrier

Mutating engine-issued operations enter a per-Sandbox barrier before reaching the
guest or a Volume. Pause closes admission and waits up to `DrainGrace` for admitted
operations. A timeout logs named stragglers and proceeds with the preemptive freeze;
a stuck RPC must not make pause impossible. `barrier_test.go` also checks that every
entry site retains and releases the returned handle.

## Admission

Node memory admission sums committed Sandbox memory caps. Disk admission sums
Sandbox and Volume caps plus backend-reported uncapped bytes and reserved promotion
growth. The admission mutex spans check through durable visibility so concurrent
creates cannot both spend the same capacity. Measurement errors fail closed.

The arithmetic lives in
[internal/lifecycle/admission.go](../../internal/lifecycle/admission.go). Boundary,
overflow, and concurrent cases are in `limits_test.go`.

## Health and reconciliation

The classifier first probes guest transport. Success proves the runtime unit is
alive and supplies guest-observed idle duration. On transport failure it calls
`runtime.Provider.Adopt` to distinguish a live unit with a failed transport from a
dead unit and carries the volatile death cause into the disposition.

Uncertain results cause no transition. Periodic dispositions reacquire the Sandbox
lock and revalidate before auto-pause or crash-pause. This protect-on-uncertainty
behavior and restart budget are in
[internal/lifecycle/classify_test.go](../../internal/lifecycle/classify_test.go) and
[internal/lifecycle/reaper_test.go](../../internal/lifecycle/reaper_test.go).

## Writer termination fence

`termination.go` retains a durable `stopping` Placement and its live handles until
runtime teardown and mount release succeed. Direct Volume mutation checks both
durable placement state and the in-flight writer reservation. Cold start records
its placement before launch, and failed readiness keeps cleanup authority until
unwind succeeds. Restart reconciliation reclaims an addressed placement before
releasing its bindings. Mount edits require a terminated paused placement.

`termination_test.go` exercises blocked Stop, failed teardown, lost live-map state,
restart, direct mutation, resume and exclusive-lease preservation. The runtime
substrate test injects kill/delete failures while the guest remains reachable,
then verifies successful retry and idempotent release.
