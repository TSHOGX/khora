# Capacity and admission

## Three resource axes

Each Sandbox Binding has independent ceilings:

| Axis | Mechanism | When exceeded |
| --- | --- | --- |
| CPU | cgroup v2 CPU controller | execution is throttled |
| memory | cgroup v2 memory controller | the isolation unit is terminated |
| disk | btrfs `squota` on Overlay upper and work | guest writes return `ENOSPC` |

Zero means no ceiling on that axis. These limits are declarations in the store and
are materialized again for every Placement. The runtime contract exposes abstract
limits rather than cgroup paths; see
[internal/runtime/provider.go](../../internal/runtime/provider.go).

## Node admission

The orchestrator may meter node memory and disk capacity. For each metered axis,
create requires a nonzero corresponding Sandbox cap. A metered disk axis also
requires a nonzero Volume cap. Insufficient capacity is reported as HTTP `507`.

Memory admission sums committed Sandbox memory ceilings. Disk admission sums
committed Sandbox and Volume ceilings plus measured bytes held by uncapped storage
and pending uncapped growth. Reads and arithmetic fail closed. Paused sandboxes
remain committed because resume must be possible without displacing another
Sandbox.

The implementation and boundary tests are in
[internal/lifecycle/admission.go](../../internal/lifecycle/admission.go) and
[internal/lifecycle/limits_test.go](../../internal/lifecycle/limits_test.go).

## Editing caps

Resource ceilings are edited while paused. A cap raise re-runs admission. Disk cap
changes require readable current usage, and a new cap must be strictly greater
than usage. An uncapped Overlay cannot become capped later because no writable
quota row exists to enforce that declaration. These refusal criteria are covered
by lifecycle and API tests rather than inferred from backend error text.
