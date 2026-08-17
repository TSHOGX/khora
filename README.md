# Khora

> Khora takes its name from χώρα, the receptacle in Plato's *Timaeus*: it has no
> character of its own, yet takes on the character of whatever enters it. The
> engine follows the same principle. Khora provides the place; applications
> decide what that place means.

Khora is a self-hosted sandbox engine for Linux. It provides ID-addressed
sandboxes, processes, filesystems, volumes, immutable Templates, and lifecycle
events. An application composes those atoms outside the engine; Khora does not
model callers, accounts, work units, or credentials.

Khora is under active development. The current release is a local, single-node
implementation, and the `0.0.x` line does not promise persistent-state
compatibility or migrations.

## What Khora provides

| Capability | Current behavior |
| --- | --- |
| Sandbox lifecycle | Create, inspect, list, pause, resume, and destroy ID-addressed Sandboxes |
| Processes | Start, stream, reconnect, signal, resize a PTY, and close stdin |
| Filesystems | Read, write, inspect, list, create, move, remove, upload, and download |
| Templates and Overlays | Derive from immutable Templates and promote a Sandbox filesystem into a new Template |
| Volumes | Create, mount, inspect, resize, import, export, snapshot, and clone independent storage |
| Resource control | Enforce CPU, memory, and disk ceilings and optionally admit against node capacity |
| Networking | Persist deny-by-default egress rules, steer declared traffic, and publish guest ports |
| Lifecycle automation | Auto-pause idle Sandboxes, reclaim expired state, and reconcile after restart |
| Events | Commit lifecycle Events with state changes and expose a replayable cursor |

The engine is organized around four mechanism seams: `store.Store`,
`runtime.Provider`, `rootfs.Provider`, and `volume.Backend`. Lifecycle code composes
their contracts without depending on their concrete mechanisms. These are in-tree
extension boundaries today, not a public plugin API. See [Engine
boundaries](docs/concepts/boundaries.md) for the current implementations and
[Adding a provider](docs/development/providers.md) for the contribution path.

The current runtime uses gVisor (`runsc`), overlayfs, network namespaces, cgroup
v2, btrfs `squota`, nftables, and `ip`. The API is intentionally unauthenticated
and listens on loopback by default. The supplied gateway terminates TLS and
authenticates callers with mTLS; authorization policy belongs in that outward-facing
adapter, not in the engine.

## Start a sandbox

```sh
make all
sudo ./templates/bare/build.sh /var/lib/khora/templates/bare ./bin/sandboxd
sudo ./bin/orchestrator --state /var/lib/khora/state \
  --templates /var/lib/khora/templates
./bin/khora template list
./bin/khora sandbox create --template bare \
  --cpu-millis 1000 --memory-byte 1073741824 --disk-byte 2147483648
```

Continue with [Getting started](docs/getting-started.md) to run a process, move a
file, and pause the sandbox.

## Documentation

- [Documentation index](docs/index.md)
- [Concepts](docs/concepts/boundaries.md)
- [Task guides](docs/guides/processes.md)
- [Configuration](docs/configuration/host.md)
- [Operations](docs/operations/startup-shutdown.md)
- [API reference](docs/api/overview.md)
- [Implementation internals](docs/internals/README.md)
- [Development workflow](docs/development/workflow.md)

See [Security](docs/concepts/security.md) for the trust boundary and
[CHANGELOG.md](CHANGELOG.md) for release notes.
