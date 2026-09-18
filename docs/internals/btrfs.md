# btrfs quota internals

Rootfs and Volume use the leaf package `internal/btrfs` for the same storage
primitive: a subvolume carrying a simple-quota limit. The leaf performs filesystem
operations and parses btrfs output; it owns no lifecycle or store policy.

## Why `squota` is a correctness constraint

Simple quotas charge newly owned extents without the full recursive relationship
tracking cost of classic qgroups. Capacity enforcement requires usable `squota`;
a capped unit on an unsupported filesystem is refused instead of running uncapped.
The kernel quota status item, not the filesystem type alone, determines the mode.

`local_btrfs` accepts btrfs with quotas disabled for uncapped Volumes, including
native snapshots and clones. It reports usage as unavailable in this mode and
cannot be combined with a node disk admission budget. Ordinary directories use
`plain_dir`. Classic quotas, inconsistent accounting and unknown probe results
are refused. The engine never enables quotas on the host.

Providers retain the validated mode and recheck it at quota read/write points.
A mode change returns unavailable until configuration is restored or the engine
restarts and validates its durable requirements. A zero requested cap skips quota
writes only when quotas are confirmed disabled; with quotas enabled it clears
any existing limit. Command failures never substitute an ordinary directory for
a requested subvolume.

Quota creation and cap assignment are separate btrfs operations. Providers therefore
clean up a subvolume if cap assignment fails, before a durable object becomes visible.

## Usage reads

`btrfs qgroup show --sync -f` may fail while another subvolume on the same filesystem
is being deleted. The client retries a nonzero command exit at most three times. A
successful response with no matching row is a semantic failure and is not retried;
an unreadable row remains an error rather than being replaced with a filesystem walk
that cannot recover the missing limit.

The retry bound and semantic-failure distinction are pinned in
[internal/btrfs/btrfs_test.go](../../internal/btrfs/btrfs_test.go). Real quota reads
are exercised by
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
