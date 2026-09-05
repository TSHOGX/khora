# Rootfs internals

## Overlay carrier

The local provider mounts an outer overlayfs view with immutable Template lowerdirs,
a per-Sandbox `upper`, and `work` on the same storage unit. `merged` is handed to the
runtime provider as a rootfs source handle. The host directory layout never crosses
the public API.

For capped Sandboxes, the storage unit is a btrfs subvolume with an `squota` limit.
Both `upper` and `work` share that unit so metadata and copy-up activity count toward
the cap. The implementation is
[internal/rootfs/overlayfs.go](../../internal/rootfs/overlayfs.go).

## Usage accounting

Plain-directory usage walks `upper` and `work`, never `merged`. Walking `merged`
would charge the immutable lower stack to each Sandbox and reject valid cap edits.
For a quota-backed unit, usage and limit come from the backend quota row. An unreadable
row is an error; falling back to a walk cannot determine whether the bytes are already
represented by a cap.

These branches are pinned by
[internal/rootfs/usage_test.go](../../internal/rootfs/usage_test.go). Cap application
requires `new cap > current usage`; equality means full and is refused.

## Promotion

Promotion accepts a running or paused Sandbox and freezes its Overlay delta into a new
immutable Template layer. A running source is quiesced through the normal pause path
and remains paused. Admission and lower-depth refusals occur before that state change.
Mounted Volume paths, sandboxd runtime files, and interrupted upload temporary files
are excluded. Bytes are materialized before the Template row is inserted, because
Template existence is its ready state. When a caller supplies a content digest, the
provider verifies it against the newly materialized layer; a mismatch is necessarily
detected after quiesce/copy, so the source remains paused and the unclaimed layer is
left for the orphan sweep. No Template row is published for that refusal.

The provider forecasts uncapped growth before copying so lifecycle admission can
reserve disk without holding a node-wide lock across the copy. Promotion cleanup and
exclusion tests are in
[internal/rootfs/promote_test.go](../../internal/rootfs/promote_test.go).

## Lower depth and sweeps

Linux limits overlayfs lower stack depth. The provider checks the complete Template
lineage before placement and promotion; see
[internal/rootfs/lowerdepth_test.go](../../internal/rootfs/lowerdepth_test.go).

Orphan Overlay and layer sweeps receive keep sets derived by lifecycle. They reclaim
artifacts, mounts, subvolumes, and quota state idempotently without reading the store
inside the rootfs package.
