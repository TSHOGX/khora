# ADR-0002 — Execution & test environment: OrbStack `sbx` + runsc systrap

- **Status:** accepted
- **Date:** 2026-07-17
- **Consumers:** anyone building/testing the engine; the substrate validation matrix below.

## Context

The engine's substrate is Linux-only (gVisor/runsc, kernel overlayfs, netns,
cgroup v2, root). Development happens from a macOS host. We need a real Linux
target where the build→run→test loop closes, otherwise milestone acceptance
(which must exercise runsc/overlay/netns on real hardware) cannot run.

## Decision

Develop and test inside an **OrbStack Linux machine named `sbx`**. runsc runs in
**systrap** mode (seccomp trap, **no KVM / no nested virtualization**) as an
unprivileged process — validated end-to-end (guest reports `4.19.0-gvisor`,
`Starting gVisor...`, guest uid=0, no KVM).

Claude Code runs **inside** `sbx` for development, pointed at the user's own
Anthropic-compatible endpoint (not the macOS host's company-managed proxy, which
the machine can't reach).

## Consequences / operating rules

- **Native fs only for engine + overlay scratch.** Engine repo lives at
  `/home/hsw/engine` on the machine's native fs. The macOS shared mount
  (`/Users/...`, virtiofs) **cannot** host overlayfs upper/work — building there
  fails. The Mac-side spec is readable via the mount for reference only.
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
| arch / kernel | **arm64** · `7.0.11-orbstack` |
| runsc | `release-20260714.0` (spec 1.2.1), systrap, no KVM |
| overlay / cgroup | overlay kernel module present · cgroup **v2** (cgroup2fs) |
| engine scratch fs | **btrfs** on `/dev/vdb1` at `/home/hsw/engine` — native, not virtiofs; `cp --reflink=always` works (cheap §5.B CoW) |
| btrfs quota mode | **`squota`** (`/sys/fs/btrfs/*/qgroups/mode`) — the capacity axis depends on this specific mode, not merely on "btrfs" (why squota and not full qgroup: rootfs-overlay D6.2). The mode **cannot be changed here**: `BTRFS_IOC_QUOTA_CTL` returns EPERM even as root with full `CAP_SYS_ADMIN` |
| btrfs-progs | **v6.17.1** — an explicit dependency: `internal/btrfs` shells out to `btrfs` and `findmnt` (both at `/usr/bin`) |
| privilege | passwordless `sudo` available; mount/netns/veth/cgroup/runsc all need root |

## Substrate validation matrix

| Substrate fact | Arch | Notes |
| --- | --- | --- |
| runsc systrap, no KVM, guest uid=0 | arm64 | guest reports `4.19.0-gvisor` |
| overlayfs: CoW correct, upper is a plain host dir, re-mount byte-identical | amd64 (ECS) + arm64 | fresh workdir required per (re-)mount; copy-up files root-owned (design detail → rootfs-overlay D5) |
| pinned runsc flags launch + exec `/bin/sh` | arm64 | all five pins are **global** flags (before the `run` subcommand); config `root.path` = the externally-merged overlay dir |
| network: host dials an in-guest listener through the sentry netstack by veth IP; fresh netns → new IP | amd64 (ECS) + arm64 | sentry-terminated (`ss` shows zero kernel listeners while host gets responses); `ip netns del` cascades the peer veth; mechanism in §5.A |
| §4.4-A reconcile: enumerate + idempotent teardown of placement resources | arm64 | primitives + reclaim order in model.md §4.4-A + reaper-health D4 |
| cgroup v2 **leaf ownership**: engine creates the parent + delegates `subtree_control`, runsc creates the leaf and writes limits only to a leaf it created itself | arm64 | pre-creating the *full* path makes the cap **silently not apply while launch still succeeds** — fails OPEN, hence the mandatory post-launch read-back assertion (§5.A) |
| RAM cap is hard only with swap pinned: OCI `swap == limit` ⇒ `memory.swap.max=0`; `memory.high` unusable | arm64 | limit alone lets the cgroup page out into zram and the cap stops binding; `memory.high` + unreclaimable anon turns throttling into a hang (§5.A / §3.4) |
| squota capacity cap: guest write past the cap gets a clean `ENOSPC` through the gofer chain, **sandbox survives**, merged view uncorrupted | arm64 | reflinked copy-up is free under squota, so the cap governs newly-written bytes (rootfs-overlay D6.2/D6.5) |
| `memory.events`' `oom_group_kill` is readable as a death cause, and only while the placement is intact | arm64 | `oom_group_kill`, **not** `oom_kill` (the latter counts per-task and read 9 for one whole-group termination); `runsc delete` destroys the signal, which is why `Adopt` must read it before teardown (§4.3 / reaper-health D4c) |

**Privilege.** `--network=sandbox` rejects `--rootless`, so the
orchestrator/launcher needs root (or CAP_NET_ADMIN+CAP_SYS_ADMIN) to materialize
placement. The **sentry itself stays unprivileged** (pillar ② intact); this is a
deployment caveat on §1.1 "runs in CI runner / inside a container" — that
environment must grant those caps.

**Arch scope.** The earlier substrate validations were amd64; `sbx` is arm64, so
the milestone acceptance suites re-confirm overlay + netns on a *second*
architecture (a plus). amd64 re-validation before production is an environment
check (Go cross-compiles cleanly), out of scope for the current milestone work.
