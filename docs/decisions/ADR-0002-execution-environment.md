# ADR-0002 — Execution & test environment: OrbStack `sbx` + runsc systrap

- **Status:** accepted
- **Date:** 2026-07-17
- **Consumers:** anyone building/testing the engine.

## Context

The engine's substrate is Linux-only (gVisor/runsc, kernel overlayfs, netns,
cgroup v2, root), while development happens from a macOS host. Milestone
acceptance must exercise that substrate for real, so we need a Linux target where
the build→run→test loop closes.

## Decision

Develop and test inside an **OrbStack Linux machine named `sbx`**. runsc runs in
**systrap** mode (seccomp trap, **no KVM / no nested virtualization**) as an
unprivileged process — validated end-to-end (matrix below).

Claude Code runs **inside** `sbx` for development, pointed at the user's own
Anthropic-compatible endpoint (not the macOS host's company-managed proxy, which
the machine can't reach).

## Consequences

- **Native fs only for engine + overlay scratch.** Engine repo lives at
  `/home/hsw/engine` on the machine's native fs. The macOS shared mount
  (`/Users/...`, virtiofs) **cannot** host overlayfs upper/work — building there
  fails.
- **DNS hijack workaround.** The macOS host runs a VPN/ZTNA (`utun4`) that
  hijacks **all** plain DNS (:53) → synthetic `198.18.x` IPs the NAT'd machine
  can't route. Direct real-IP + :443/:80 egress works. Fix: resolve real IPs via
  **Cloudflare DoH** and pin into the machine's `/etc/hosts` (idempotent block
  `# sbx-doh-pin`…`# end-sbx-doh-pin`). Cloudflare IPs rotate — re-pin if a host
  starts failing.
- **No Docker anywhere.** The engine needs only runsc/overlayfs/netns/cgroup.
  Minimal rootfs is built from the machine's own userland (replicate usrmerge:
  `/bin`,`/lib`→`usr/*` symlinks, or guest binary loads fail).

## Machine facts

| Fact | Value |
| --- | --- |
| arch / kernel | **arm64** · `7.0.14-orbstack` (OrbStack updates this in place; the engine depends on the capabilities below, not on an exact kernel build) |
| runsc | `release-20260714.0` (spec 1.2.1), systrap, no KVM |
| overlay / cgroup | overlay filesystem **builtin** (not a loadable module) · cgroup **v2** (cgroup2fs) |
| engine scratch fs | **btrfs** on `/dev/vdb1` (the machine's root subvolume; the repo at `/home/hsw/engine` sits on it, not on a mount of its own) — native, not virtiofs; `cp --reflink=always` works (cheap §5.B CoW) |
| btrfs quota mode | **`squota`** (`/sys/fs/btrfs/*/qgroups/mode`) — the capacity axis depends on this specific mode, not merely on "btrfs" (why squota and not full qgroup: rootfs-overlay D6.2). The mode **cannot be changed here**: `BTRFS_IOC_QUOTA_CTL` returns EPERM even as root with full `CAP_SYS_ADMIN` |
| btrfs-progs | **v6.17.1** — an explicit dependency: `internal/btrfs` shells out to `btrfs` and `findmnt` (both at `/usr/bin`) |
| nftables | **v1.1.6** — an explicit dependency, the whole of §5.D's enforcement: `internal/runtime` shells out to `nft` (`/usr/sbin`). Two version-sensitive facts are load-bearing and both are guarded by tests rather than by a version check: `fwd` is a reserved word and cannot be a chain name, and a named counter must be declared before the chain referencing it (network.md D5 ㉔) |
| iproute2 | **6.19.0** — an explicit dependency: `internal/runtime` shells out to `ip` (`/usr/sbin`) for netns / veth / addr / route |
| privilege | passwordless `sudo` available; mount/netns/veth/cgroup/runsc/nft all need root |

## Substrate validation matrix

| Substrate fact | Arch | Notes |
| --- | --- | --- |
| runsc systrap, no KVM, guest uid=0 | arm64 | guest reports `4.19.0-gvisor` |
| overlayfs: CoW correct, upper is a plain host dir, re-mount byte-identical | amd64 (ECS) + arm64 | fresh workdir required per (re-)mount; copy-up files root-owned (design detail → rootfs-overlay D5) |
| pinned runsc flags launch + exec `/bin/sh` | arm64 | all five pins are **global** flags (before the `run` subcommand); config `root.path` = the externally-merged overlay dir |
| network: host dials an in-guest listener through the sentry netstack by veth IP; fresh netns → new IP | amd64 (ECS) + arm64 | sentry-terminated (`ss` shows zero kernel listeners while host gets responses); `ip netns del` cascades the peer veth; mechanism in §5.A |
| §4.4-A reconcile: enumerate + idempotent teardown of placement resources | arm64 | primitives + reclaim order in §4.4-A + reaper-health D4 |
| cgroup v2 **leaf ownership**: engine creates the parent + delegates `subtree_control`, runsc creates the leaf and writes limits only to a leaf it created itself | arm64 | pre-creating the *full* path makes the cap **silently not apply while launch still succeeds** — fails OPEN, hence the mandatory post-launch read-back assertion (§5.A) |
| RAM cap is hard only with swap pinned: OCI `swap == limit` ⇒ `memory.swap.max=0`; `memory.high` unusable | arm64 | limit alone lets the cgroup page out into zram and the cap stops binding; `memory.high` + unreclaimable anon turns throttling into a hang (§5.A / §3.4) |
| squota capacity cap: guest write past the cap gets a clean `ENOSPC` through the gofer chain, **sandbox survives**, merged view uncorrupted | arm64 | reflinked copy-up is free under squota, so the cap governs newly-written bytes (rootfs-overlay D6.2/D6.5) |
| `memory.events`' `oom_group_kill` is readable as a death cause, and only while the placement is intact | arm64 | `oom_group_kill`, **not** `oom_kill` (the latter counts per-task and read 9 for one whole-group termination); `runsc delete` destroys the signal, which is why `Adopt` must read it before teardown (§4.3 / reaper-health D4c) |
| §5.D enforcement: a per-sandbox nft table narrows egress to the declaration, with the L0 floor's DROP counters moving on every refusal | arm64 | `policy accept` + `iifname`, never `policy drop` (a cross-sandbox kill switch) — an assertion pairs "unreachable" with "the counter moved", since a zero counter cannot be told from a bypassed floor (network.md D4.1) |
| the floor holds against a forged source address, across a veth rebuild, and on IPv6 with no `ip6` arm | arm64 | `iif` fails open across veth recreate, so the predicate is `iifname`; the v6 coverage is the same table, not a parallel one |
| `via[]` steering: the second hop is visible only on `output`, narrowed by `skuid`; a dead endpoint refuses rather than leaking out | arm64 | `forward` counts 0 for all three shapes (network.md D5 ㉔); `nft` v1.1.6 makes `fwd` a reserved word, and a named counter must precede the chain referencing it |
| publish DNAT/SNAT quartet: reachable from outside AND from the host itself, surviving resume; revoke closes the port at the next placement | arm64 | four rules installed as one unit — missing the SNAT works only where a default route happens to exist, so "the host could dial it" is not evidence publish works |
| control-plane restart: a live flow survives `Adopt`, and reconcile re-adopts a rule set while reclaiming an orphan's table | arm64 | only a WHOLE atomic reinstall is safe: `delete`-then-install kills the flow, and flushing ct is a live-connection killer (network.md D5 ㉒) |

**Privilege.** `--network=sandbox` rejects `--rootless`, so the
orchestrator/launcher needs root (or CAP_NET_ADMIN+CAP_SYS_ADMIN) to materialize
placement. The **sentry itself stays unprivileged** (pillar ② intact); this is a
deployment caveat on §1.1 "runs in CI runner / inside a container" — that
environment must grant those caps.

**Arch scope.** `sbx` is arm64; the matrix above records which facts are
additionally confirmed on amd64. Full amd64 re-validation before production is an
environment check (Go cross-compiles cleanly), not an engine change.
