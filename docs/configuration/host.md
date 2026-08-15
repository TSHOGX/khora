# Host requirements

The assembled local engine requires Linux and root privileges. It uses:

- gVisor `runsc` with sandbox networking;
- overlayfs and mount/network namespaces;
- cgroup v2 CPU and memory controllers;
- a btrfs filesystem mounted with `squota`, plus `btrfs-progs`;
- nftables and `iproute2` (`nft` and `ip`);
- Go 1.23 or newer to build the repository.

## Inspect the host

```sh
runsc --version
findmnt -no FSTYPE,OPTIONS /srv/khora/state
stat -fc %T /sys/fs/cgroup
nft --version
ip -Version
```

The state root used for capped Overlays and local Volumes must resolve to btrfs
with simple quotas enabled. Keep state, Template layers, and overlay scratch data
on native Linux storage. Virtiofs/shared-folder mounts do not provide the required
overlay and quota behavior.

The orchestrator must be able to create mounts, namespaces, veth pairs, cgroups,
and nftables rules. The supplied systemd unit therefore runs as root and deliberately
does not enable hardening options that remove those capabilities. Inspect
[deploy/khora-orchestrator.service](../../deploy/khora-orchestrator.service) before
changing its isolation settings.

## Acceptance profile

The real-substrate suite additionally expects native filesystem storage and the
features above to work together. Run its preflight decision and tagged tests as
described in [Testing](../development/testing.md); presence of binaries alone is not
evidence that the substrate behaves correctly.
