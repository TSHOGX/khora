# Use files and volumes

Sandbox files live in the Sandbox Overlay. Volumes are independent objects that
can be populated before a Sandbox exists and mounted into later Placements.

## Stream a Sandbox file

```sh
./bin/khora files upload sbx_... ./input.tar /tmp/input.tar
./bin/khora files download sbx_... /tmp/output.tar ./output.tar
./bin/khora files download sbx_... /etc/os-release -
```

These operations require a running Sandbox. The HTTP request and response bodies
stream bytes instead of encoding a whole file in a message. Upload replaces the
target through a guest-side temporary file and rename. The client implementation
is [pkg/khora/stream.go](../../pkg/khora/stream.go). A single upload is limited to
8 GiB at the orchestrator edge; the Sandbox disk cap remains the capacity control.

For directory operations, offsets, metadata, move, and remove, use the Connect-RPC
filesystem service described in [Data-plane API](../api/data-plane.md).

## Create and seed a Volume

```sh
./bin/khora volume create --size 10737418240
./bin/khora volume import vol_... ./seed.db data/seed.db
./bin/khora volume export vol_... data/seed.db ./seed-copy.db
```

Import and export use paths relative to the Volume root and do not require a
Sandbox. The accessor is confined to that root. Import is refused while a live
read-write Mount holds the Volume; pause that Sandbox and retry. A live read-only
Mount does not block import. A zero size creates an uncapped Volume only when the
node disk axis is not metered.

## Attach a Volume

The current CLI manages Volume objects but does not expose Mount bindings. Use the
HTTP create or patch body:

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'content-type: application/json' \
  -d '{
    "template_id": "bare",
    "volumes": [
      {"volume_id":"vol_...","path":"/data","access":"exclusive"}
    ]
  }'
```

On patch, `volumes` replaces the complete Mount binding. Omit the member to leave
Mounts unchanged; send an empty array to detach all Volumes. Detaching releases an
exclusive lease immediately, while the guest topology changes at the next
placement. The patch is accepted for a running or paused Sandbox. Pause first when
the old live topology must be gone before the binding or lease changes.

## Snapshot, clone, and delete

```sh
./bin/khora volume snapshot vol_... --size 10737418240
./bin/khora volume clone vol_... --size 10737418240
./bin/khora volume delete vol_...
```

Snapshot and clone both create an ordinary writable, independently addressed Volume
with a `parent`; the local btrfs backend uses the same copy-on-write mechanism for
both. The new `--size` is not inherited from the source and is required when the
node disk axis is metered. Derivation is subject to backend capability and node
capacity, but does not require the source to be unmounted or paused. Treat the copy
as crash-consistent, not application-consistent, if a guest is writing concurrently;
the live-writer case is exercised in
[test/volume_snapshot_substrate_test.go](../../test/volume_snapshot_substrate_test.go).

Any Mount binding prevents deleting that Volume, and a live descendant prevents
deleting its source. Delete descendants before their ancestors. The lineage and
write-path constraints are covered by
[internal/lifecycle/snapshot_test.go](../../internal/lifecycle/snapshot_test.go) and
[internal/lifecycle/writeregime_test.go](../../internal/lifecycle/writeregime_test.go).
