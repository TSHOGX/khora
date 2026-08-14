# Changelog

## Versioning promise

**Through all of `0.0.x`, this project promises nothing about compatibility.**

`CLAUDE.md`'s no-compatibility rule ends with "this rule lifts when we deploy". A tag a
consumer can pin is closer to that trigger than an internal instance is, so the answer is
stated here rather than left to inference: **the rule stays in force for the whole of
`0.0.x`.** Concretely, and for any `0.0.x` → `0.0.y`:

- **No schema migrations.** A breaking store change edits `CREATE TABLE` and deletes the
  old path in place. There is no `ALTER`, and no code that reads an older file.
- **No compatibility shims**, and no version negotiation on the wire or in the store.
- **Persistent state is disposable.** `store.db`, overlays, volumes and runsc state can be
  discarded and rebuilt between any two `0.0.x` versions, and sometimes must be.
- **The REST and Connect surfaces may change shape**, including removals.

A compatibility branch would be a negative asset at this stage: it widens the surface and
manufactures invariants the code does not actually hold, which is the silent drift the
spec discipline exists to prevent. **If you pin a `0.0.x` tag, treat an upgrade as a
redeploy from empty state.**

What this does *not* mean: the tag is not a moving target. A tag is immutable, the binaries
built from it report it via `--version`, and `docs/substrate-runs/` records the substrate
evidence for the tree it was cut from.

## v0.0.1

First tagged release. The engine is feature-complete against `docs/spec/` §1–§6 and
verified on the real substrate; see `docs/substrate-runs/` for the run this tag was cut
from, including its counts and skips.

This is the first tag, so there is no "changed since" — what follows is what the tag
contains.

### The engine

- **Sandbox lifecycle** — create / get / list / pause / resume / destroy, ID-addressed.
  Pause drains in-flight engine writes and then freezes; persistence is the overlay on
  disk, not a memory snapshot, so a sandbox is portable across hosts, CPUs and runsc
  versions.
- **Processes** — start, stream stdout/stderr, signal, PTY, and reconnect to a process
  that is still running.
- **Filesystem** — read / write / stat / list / mkdir / remove / move, plus streaming
  upload and download.
- **Volumes** — independent of any sandbox: create, mount, snapshot, clone, import,
  export.
- **Templates** — promote a sandbox to an immutable base other sandboxes derive from.
- **Resource caps** — CPU, memory and disk, enforced through cgroup v2 and btrfs quotas.
- **Networking** — deny-by-default egress declarations enforced as per-sandbox nftables
  rules under a floor a declaration cannot lift, and `publish` for a guest port.
- **Events** — an outbox with a replay window.

### Surfaces

- `orchestrator` — the single engine-host binary, serving the §6.1 REST + Connect API.
- `sandboxd` — the in-guest daemon: init mode as PID 1, transport mode as its child.
- `warren` — the CLI, a peer client of the same API a frontend uses.
- `gateway` — the outward-facing mTLS boundary. **Not part of the engine** (ADR-0007):
  authentication and TLS live in front of it, because every byte the engine can hand a
  guest is readable by uid 0 inside it, so the engine cannot be a secrecy boundary.
- `pkg/warren` — a Go client. `pkg/transport` — the generated wire stubs.
- All four binaries answer `--version`, stamped from `git describe` at build time.

### Known limits

- **No production Template ships in this repo.** You build one; see `templates/`.
- **No client libraries** outside Go.
- **The engine's own listener is loopback-only** unless `-insecure-listen`, which
  publishes it with no authentication and no TLS. Put the gateway or a reverse proxy in
  front of it (`deploy/` carries a worked config).
- **Single node.** There is no scheduler across hosts.
- **Linux only**, and the substrate is a hard requirement, not a preference: gVisor/runsc,
  kernel overlayfs, netns, cgroup v2, btrfs in `squota` mode, `nft`, `ip` (ADR-0002).
