# khora

khora is a self-hosted sandbox engine for Linux. It provides ID-addressed
sandboxes, processes, filesystems, volumes, immutable Templates, and lifecycle
events. An application composes those atoms outside the engine; khora does not
model callers, accounts, work units, or credentials.

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

The engine is currently single-node. The `0.0.x` line does not promise persistent
state compatibility or migrations. See [Security](docs/concepts/security.md) for
the trust boundary and [CHANGELOG.md](CHANGELOG.md) for release notes.
