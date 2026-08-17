# Troubleshooting

## The orchestrator is not ready

Read `/readyz` and process logs. `booting` means startup reconciliation has not
completed. `draining` means shutdown has begun. `outbox publisher halted` identifies
the blocked event, attempt count, and backlog. The standard orchestrator's in-process
log sink cannot report a delivery failure, so this state indicates an assembly that
installed a different Publisher. Repair that sink and use the assembly's operator
path to call `Publisher.Resume`; restarting does not remove the durable event. The
standard binary has no publisher-resume endpoint.

## Sandbox creation fails before runsc starts

- `400` with a required-cap message: a node capacity axis is metered but the request
  omitted its corresponding nonzero cap.
- `507`: the request is valid but declared caps plus measured uncapped bytes exceed
  node capacity.
- quota or filesystem errors: confirm the state root is btrfs with `squota` and is
  not on virtiofs or a shared mount.
- missing Template: build it under `--templates` and restart so boot seeding sees it.

## Placement or resume fails

Run the substrate preflight checks before assigning the failure to Khora. Inspect
`runsc`, cgroup v2 controllers, namespace privileges, veth creation, nftables, and
the guest transport probe. A disk-cap edit at or below current Overlay usage is an
invalid request; set it above usage or remove the cap before resuming.

The reusable host probes live in
[test/preflight](../../test/preflight), and substrate requirements are centralized
in [test/substratereq](../../test/substratereq).

## Published traffic hangs or refuses

The guest service must bind `0.0.0.0` or its sandbox interface. Binding only guest
loopback cannot accept a published connection. For egress, inspect the stored
declaration and placement-local counters; absence means there is no readable live
rule, while zero is inconclusive after a reinstall. A `via[]` counter means the
flow matched and was steered, not that the external second hop accepted it.

## Storage operations conflict

An exclusive Mount conflicts with every other Mount for the Volume. A Volume with
a Mount or descendant cannot be deleted. Direct import conflicts with a live
read-write Mount, and a read-write cold start conflicts with an import in flight.
Release the relevant binding or wait for the import before retrying. Snapshot and
clone may run against a live writer; treat their content as crash-consistent rather
than application-consistent.
