# Engine boundaries

Khora provides a sandbox abstraction. Its stable vocabulary consists of
ID-addressable engine atoms: Sandbox, process, filesystem, Volume, Template, and
the bindings and events needed to compose them.

## Engine and adapter

The engine answers questions such as:

- which immutable Template and writable Overlay form a sandbox;
- which resource and network Binding is recorded for it;
- whether a Placement can be admitted and materialized;
- which processes and files are reachable through its data plane;
- which Volumes are mounted and with which access mode.

Application identity, policy, credentials, and higher-level work orchestration
belong in an adapter. The engine deliberately has no authentication model. This
boundary is enforced at the public model and API edges; see
[internal/orchestrator/edge_test.go](../../internal/orchestrator/edge_test.go) and
[pkg/khora/contract_test.go](../../pkg/khora/contract_test.go).

## Subsystem ownership

Five mechanism packages implement the sandbox:

- `runtime` creates and inspects isolated Placements;
- `rootfs` builds the Template plus Overlay filesystem view;
- `volume` materializes independent storage;
- `network` validates declarations and derives abstract rules;
- `transport` reaches process and filesystem services inside the sandbox.

Subsystems do not call one another and do not read the control-plane store.
Lifecycle verbs are composed in `internal/lifecycle`. Shared facts and pure
criteria live in leaf packages. Direct and transitive dependency checks, including
the build-tag matrix, are in
[internal/arch/coupling_test.go](../../internal/arch/coupling_test.go).

## Mechanism seams

`store.Store`, `runtime.Provider`, `rootfs.Provider`, and `volume.Backend` are the
replaceable seams. Their contracts carry policy and portable handles. Host paths,
cgroup locations, and namespace descriptors remain provider details and do not
cross the API boundary; structural seam tests inspect both interface signatures
and the exported data that crosses them.

| Seam | Responsibility | Standard assembly | Other implementation | Contract evidence |
| --- | --- | --- | --- | --- |
| `store.Store` | Durable control-plane facts and the Event outbox | SQLite | Memory, for tests | Both run `storetest` |
| `runtime.Provider` | Live Placement, resource, and network enforcement | `LocalRunsc` | None | Structural, unit, and runsc substrate tests |
| `rootfs.Provider` | Template and Overlay materialization | `HostOverlayFS` | None | Structural, unit, and overlayfs substrate tests |
| `volume.Backend` | Independent Volume storage and derivation | `LocalBtrfs` | `PlainDir`, for tests | Both run `volumetest`; the btrfs arm runs on the substrate |

The standard orchestrator constructs these implementations directly in
[internal/orchestrator/orchestrator.go](../../internal/orchestrator/orchestrator.go).
The interfaces and assembly are under `internal/`, so a different implementation
currently requires an in-tree code change. There is no runtime provider registry,
configuration selector, stable third-party Go contract, remote provider, or
cross-host migration.

Configurable selection and public provider contracts are tracked in
[Issue #25](https://github.com/TSHOGX/khora/issues/25). The input shape of a future
remote runtime provider is a separate question tracked in
[Issue #7](https://github.com/TSHOGX/khora/issues/7).

## Other boundaries

Not every subsystem is a provider. `network` validates declarations and derives
placement-free rules; `runtime.Provider` completes and installs those rules.
`transport.Client` is the domain boundary to `sandboxd`, and `store.Sink` is the
outbox delivery boundary. They are composition points, but they are not additional
deployment provider selections in the current assembly.
