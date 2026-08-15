# Configuration and deployment

## Host requirements

The orchestrator requires Linux with gVisor/runsc (systrap), root or the capabilities
needed by `--network=sandbox`, overlayfs, network namespaces, cgroup v2, btrfs in
`squota` mode, `btrfs-progs`, `nft` and `ip`. Keep the repository and overlay scratch
directories on a native filesystem, not a virtiofs shared mount. Go 1.23 or newer is
required to build the project.

## Orchestrator

Build with `make all`, then run:

```sh
sudo ./bin/orchestrator --state /srv/khora/state
```

Important flags include `--state`, `--store`, `--templates`, `--addr`,
`--insecure-listen`, `--node-disk-byte`, `--node-memory-byte`, `--idle-timeout`,
`--retention`, `--event-retention`, `--publish-ip` and `--shutdown-timeout`.
Run `./bin/orchestrator --help` for defaults and the complete list. A non-loopback
address is refused unless `--insecure-listen` explicitly opts into an unauthenticated
listener.

The bare Template is a definition under `templates/bare/`; build it with:

```sh
sudo ./templates/bare/build.sh /var/lib/khora/templates/bare ./bin/sandboxd
```

The artifact intentionally does not include DNS configuration. A deployment supplies
`/etc/resolv.conf` and an appropriate UDP/53 egress declaration.

## Outward-facing boundary

The engine's control plane is loopback and has no authentication or TLS. `deploy/`
contains the recommended nginx TLS/mTLS boundary and a systemd unit for the
orchestrator. Supply the deployment's PKI; caller identity and authorization remain
outside the engine.
