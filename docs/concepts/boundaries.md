# Engine boundaries

khora provides a sandbox abstraction. Its stable vocabulary consists of
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

## Provider seams

`store.Store`, `runtime.Provider`, `rootfs.Provider`, and `volume.Backend` are the
replaceable seams. Their contracts carry policy and portable handles. Host paths,
cgroup locations, and namespace descriptors remain provider details and do not
cross the API boundary; structural seam tests live beside each provider package.

The current assembled implementation is local and single-node. A different
provider may be registered without changing the object model, but no remote
provider or cross-host migration is currently implemented.
