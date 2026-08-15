# Configure Templates

A Template is an immutable root filesystem layer registered by its directory name.
The orchestrator seeds built-in Template records from `--templates` during startup.

## Build the bare Template

Build the project first, then create the Template outside the orchestrator:

```sh
make all
sudo ./templates/bare/build.sh \
  /var/lib/khora/templates/bare \
  ./bin/sandboxd
```

The build script installs a minimal filesystem and the exact `sandboxd` binary that
will serve the guest transport. Its package list and layout are the canonical
definition; see [templates/bare](../../templates/bare).

The artifact deliberately omits resolver configuration. A deployment that needs
DNS supplies `/etc/resolv.conf` and permits the chosen resolver in the Sandbox
network declaration.

## Register and inspect

Point the orchestrator at the parent directory:

```sh
sudo ./bin/orchestrator --templates /var/lib/khora/templates
./bin/khora template list
```

Templates are read-only at runtime. A referenced Template cannot be deleted.
Promote a running or paused Sandbox Overlay into a new immutable Template with:

```sh
./bin/khora template promote --from sbx_...
```

Promotion excludes mounted Volume content and returns an engine-minted Template
ID. A running source is paused through the normal lifecycle path and remains paused.
Capacity and maximum lower-stack depth are checked before that state change; see
[Rootfs internals](../internals/rootfs.md).
