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

## Substrate validation matrix

| Substrate fact | Where validated | Arch | Status |
| --- | --- | --- | --- |
| overlayfs persists across container life; upper is a plain host dir (tar/diff/re-mount, byte-identical) | ECS (spec §2.5 / §5.B) | amd64 | validated |
| netns-internal TCP transport (unary + bidi stream, no vsock) | ECS (spec §5.E) | amd64 | validated |
| runsc systrap, no KVM, guest uid=0 | OrbStack `sbx` | **arm64** | validated (this ADR) |
| overlayfs + netns + cgroup v2 present & usable | OrbStack `sbx` | **arm64** | validated |

### Measured baseline on `sbx` (recorded facts, not a decision change)

The above matrix was later pinned to concrete values measured on the machine.
These calibrate the matrix; the decision (develop/test on `sbx` + systrap) is
unchanged.

| Fact | Measured value |
| --- | --- |
| arch / kernel | arm64 · `7.0.11-orbstack` |
| runsc | `release-20260714.0` (spec 1.2.1), systrap, no KVM |
| overlay module / cgroup | overlay present · cgroup **v2** (cgroup2fs) |
| engine scratch fs | **btrfs** on `/dev/vdb1` at `/home/hsw/engine` — native, not virtiofs |
| btrfs reflink | `cp --reflink=always` works → §5.B CoW snapshot/clone is cheap here (M6 uses it; substrate ready now) |

### M1 substrate spike validation (parallel spikes, arm64 `sbx`)

Four independent spikes exercised the M1 joints on real hardware. All passed;
these calibrate the matrix, not the decision.

| Seam validated | Result | Notes |
| --- | --- | --- |
| overlayfs assembly (lower=Template / upper / work), CoW, persist, re-mount byte-identical | PASS | upper is a plain host tree; **fresh workdir required per (re-)mount**; copy-up files are root-owned |
| pinned runsc flags launch + guest exec | PASS | guest `4.19.0-gvisor`, uid=0; all five pins are **global** flags (before `run`); config `root.path` = externally-merged overlay dir |
| netns/veth host-dials-guest by IP; new IP per cold start | PASS (host↔netns L3/L4) | IP discovery = `ip netns exec <ns> ip -j addr`; `ip netns del` cascades the peer veth |
| idempotent teardown / §4.4-A reconcile primitives | PASS | 5 per-class enumerate cmds; **overlay reconcile must be path-scoped, not `-t overlay`** (else hits OrbStack system overlays) |

**Privilege (recorded fact).** Every mount/umount/chroot, all netns/veth/cgroup
ops, and runsc launch require **root**; `--network=sandbox` explicitly rejects
`--rootless`. The **sentry itself stays unprivileged** (pillar ②) — but the
**orchestrator/launcher needs root or CAP_NET_ADMIN+CAP_SYS_ADMIN** to
materialize placement. Passwordless `sudo` is available on `sbx`, so preflight/
e2e run unattended. (Nuances §1.1 "runs in CI runner / inside a container": that
environment must grant those caps.)

**Not yet validated (task-A's job in M1).** No spike exercised the *combined*
path — a TCP listener **inside runsc's `--network=sandbox` netstack** dialed from
the host by veth IP. The netns spike used a host listener; the runsc spike tested
exec, not transport. So §5.E's "netstack 下实测" reachability stays **pending**
until task A closes it end-to-end.

**Caveat.** `sbx` is arm64; the ECS validations were amd64. M1 running on arm64
re-confirms the overlay + netns facts on a *second* architecture (a plus, not a
risk). **Before production, re-validate the substrate facts on amd64.** Go
cross-compiles cleanly, so this is an environment check, not a code port — and
therefore **out of M1 scope**, not an M1 acceptance gate.
