# Durable state core

The state core persists three records beside Sandbox rows:

- `Placement` is the current realization identified by `(sandbox_id, epoch)`. A
  cold create starts at epoch 1, Resume increments it, and Adopt keeps it.
  Pausing marks the record lost but retains the high-water epoch so an old
  data-plane request cannot be mistaken for the next realization.
- `Process` is an opaque logical handle. Its PID is only a locator inside one
  placement epoch. Exit status and the bounded retention state remain
  addressable after the guest process disappears; the parent Sandbox delete
  removes its process rows.
- `Operation` is the idempotency ledger. It is inserted before external bytes
  or placements are materialized, keyed by `(scope, idempotency_key)` and a
  canonical request hash. Retries replay a succeeded result, reject a changed
  request, or take over an expired lease without erasing the side-effect phase.

Memory and SQLite implement the same Store methods and run the shared
state-core contract suite. SQLite stores manifests, cursors, and operation
results as strict JSON/text values in the canonical schema; the 0.0.x rebuild
policy intentionally has no migration path.

`ReconcileStateCore` runs before placement reconciliation. It reclaims exited
processes at the configured boundary and takes over expired operation leases;
phase-specific cleanup remains explicit so an uncertain side-effect window is
never treated as a clean retry.

The HTTP control plane accepts `Idempotency-Key` on Sandbox and Volume create
requests. The key is not authentication and carries no product identity; it is
only a retry correlation value. Placement epoch and state are included on
inspect/event reads, while provider handles and host paths remain internal.
