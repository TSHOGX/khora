# Security model

## Trust boundary

The isolation boundary is the gVisor sandbox, its network namespace, cgroup, and
provider-owned filesystem mounts. Guest code runs as guest root. Guest user IDs
are convenience identity and are not an authorization boundary.

The orchestrator itself has no authentication or TLS and listens on loopback by
default. It refuses a non-loopback listener unless the operator explicitly enables
the insecure-listen override. For remote access, place an authenticating gateway in
front; the supplied gateway requires mTLS. See
[Deployment boundary](../configuration/deployment-boundary.md).

## Credentials

Anything handed into a Sandbox is readable by guest root. The engine therefore
does not store, inject, rotate, redact, or claim secrecy for credentials. An
adapter may supply material for a workload, but that material is not protected
from the workload itself.

## Filesystem and API edges

Provider paths stay behind rootfs and volume seams. Public API responses expose
opaque IDs and portable values, never host absolute paths. Direct volume file
access uses a root-confined accessor. API edge tests inspect response bodies and
seam structures in
[internal/orchestrator/edge_test.go](../../internal/orchestrator/edge_test.go),
[internal/runtime/seam_test.go](../../internal/runtime/seam_test.go), and
[internal/volume/seam_test.go](../../internal/volume/seam_test.go).

## Current limitations

- The local runtime depends on the host kernel, gVisor, nftables, and filesystem
  configuration being correct.
- The engine is single-node and does not implement cross-host migration.
- `shared-rw` Volumes do not promise coherent caches across sandbox boundaries.
- The `0.0.x` line does not promise durable state compatibility or migrations.
- Authorization decisions belong to the outward-facing adapter, not the engine.

Real-substrate claims are covered by developer-run acceptance tests. Hosted CI
does not substitute for those results; see [Testing](../development/testing.md).
