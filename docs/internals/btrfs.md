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

Each reclamation pass reads the quota listing once and considers only IDs recorded
by this provider. It clears absent or live records, destroys stale rows, and leaves
pending deletions and live sharers for a later pass. It never waits for subvolume
deletion. An empty set performs no substrate I/O. Reads and cleanup share the
caller's deadline; that bounds cooperative execution, not uninterruptible kernel I/O.

Deletion attempts record their captured IDs after returning, including failures.
A per-record sequence prevents an older cleanup snapshot from discarding newer
work. These records stay in provider memory: a process crash loses the pending set.
Startup can record surviving orphan units as it deletes them, but does not rediscover
quota rows whose units were already removed before the crash. No whole-filesystem
scan or Store metadata is used to infer authority over those rows.
