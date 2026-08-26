# Volume internals

## Backend seam

`volume.Backend` creates, materializes, snapshots, clones, measures, and destroys
ID-addressed storage. A materialized result is a provider-owned handle consumed by
runtime assembly; it is not a path returned to API callers. Structural tests scan
method parameters, results, and fields for leaked substrate vocabulary in
[internal/volume/seam_test.go](../../internal/volume/seam_test.go).

The plain-directory backend is a test-only, capability-negative implementation.
The local btrfs backend is the standard implementation and supports caps and
copy-on-write lineage. Both run the shared contract in
[internal/volume/volumetest/contract.go](../../internal/volume/volumetest/contract.go).

## Mount and write regime

Access is a Mount property. `shared-ro` becomes a read-only runtime bind.
`exclusive` is enforced by the store's atomic Mount insertion. `shared-rw` permits
multiple live writers but does not promise coherent caches across sandbox boundaries.

The write regime is narrower than derivation. A direct import is refused while a
live read-write Mount holds the Volume, and a cold start with a read-write Mount is
refused while an import is in flight. Read-only Mounts may coexist with an import.
Snapshot and clone do not take this gate: the local btrfs copy is atomic at the
filesystem level, but a concurrently written source gives crash-consistent rather
than application-consistent content. The mutual-exclusion cases are in
[internal/lifecycle/writeregime_test.go](../../internal/lifecycle/writeregime_test.go);
the live derivation case is in
[test/volume_snapshot_substrate_test.go](../../test/volume_snapshot_substrate_test.go).

## Direct access

Import/export open a root-confined accessor without starting a Sandbox. Paths are
relative to the Volume root; traversal is resolved inside that root. The interface in
[internal/volume/root.go](../../internal/volume/root.go) exposes file operations,
not an unrestricted host path.

## Capacity and lineage

Create, snapshot, and clone apply a requested cap in the same backend operation that
makes the new storage visible. A missed cap cannot be reconstructed later because
the substrate represents uncapped state as zero. Resize requires a readable backend
usage and a cap strictly greater than it.

Parent references prevent deletion while descendants exist. This order also avoids
the btrfs case where deleting an extent-owning source before its sharers can strand a
quota row. Backend details are in [btrfs internals](btrfs.md).
