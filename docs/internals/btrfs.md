# btrfs quota internals

Rootfs and Volume use the leaf package `internal/btrfs` for the same storage
primitive: a subvolume carrying a simple-quota limit. The leaf performs filesystem
operations and parses btrfs output; it owns no lifecycle or store policy.

## Why `squota` is a correctness constraint

Simple quotas charge newly owned extents without the full recursive relationship
tracking cost of classic qgroups. The state root must be mounted with `squota`; a
capped unit on an unsupported filesystem is refused instead of running uncapped.

Quota creation and cap assignment are separate btrfs operations. Providers therefore
clean up a subvolume if cap assignment fails, before a durable object becomes visible.

## Usage reads

`btrfs qgroup show --sync` may transiently omit a newly created row. The client makes
three bounded reads because measurement found 8 misses in 300 first attempts and no
miss after the retry window. An unreadable row remains an error after the bound; a
filesystem walk cannot supply the missing limit.

The code and real-filesystem test are
[internal/btrfs/btrfs.go](../../internal/btrfs/btrfs.go) and
[internal/btrfs/btrfs_substrate_test.go](../../internal/btrfs/btrfs_substrate_test.go).

## Irreversible lowering

btrfs accepts a limit at or below current referenced usage, but the unit can then be
unable to make the writes needed to recover. Providers read usage first and require
strictly `cap > usage`; equality is a full filesystem, not spare capacity. If usage
cannot be read, the cap change fails closed.

## Quota-row reclamation

Deleting a subvolume that owns extents still shared by another subvolume can leave a
`<squota space holder>` row. Waiting 25 seconds and six syncs did not remove the
measured row. A stale row is a correctness hazard because a reused subvolume ID can
inherit an old limit.

Volume lineage prevents the source-before-descendant deletion order. The backend
also records deleted subvolume IDs and retries only rows whose label says they are
reclaimable; busy holders remain for a later periodic pass. Churn behavior is tested
by [internal/btrfs/quotachurn_substrate_test.go](../../internal/btrfs/quotachurn_substrate_test.go).
