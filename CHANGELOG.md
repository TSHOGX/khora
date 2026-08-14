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

What this does *not* mean: the tag is not a moving target. A tag is immutable, and the binaries
built from it report it via `--version`. Substrate verification is recorded with the reviewing
PR rather than committed as a document in the product tree.

**`v0.0.1` was re-cut once, deliberately, and that is the only exception.** The first cut
carried the project's former name; the tag was re-created on the renamed tree, pointing at
a different commit than it first did. What made it safe is a condition that cannot recur:
**no consumer had fetched it** — the repository is private, the one dependent was told to
pin it and had not yet, and there was no released artifact to migrate. Re-cutting a tag
someone holds would be the moving target this section rules out, so the immutability
statement above governs from here on, `v0.0.1` included.

## v0.0.1

First tagged release, and the first release under the name **Khora** — χώρα, the receptacle
that has no character of its own and takes on the character of what enters it, which is this
engine's first principle stated in Greek (see `README.md`). Substrate verification for release
work is recorded with the reviewing PR, not as a generated document in the product tree.

The project, the Go module (`github.com/TSHOGX/khora`), the CLI binary, the repository and
the protobuf wire package (`khora.transport.v1`) all carry that one name. It was renamed
here rather than after the release, because the wire package is a compatibility surface
between the host binary and every Template built before it changes — so this was the only
moment the change cost nothing. *engine* and *orchestrator* keep their meanings: the sandbox
abstraction and the binary hosting it, layer names rather than project names. Sandbox-scoped
runtime identifiers (`sbx*` nftables tables, netns and container names) are unchanged; they
never named the project.

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
- `khora` — the CLI, a peer client of the same API a frontend uses.
- `gateway` — the outward-facing mTLS boundary. **Not part of the engine**:
  authentication and TLS live in front of it, because every byte the engine can hand a
  guest is readable by uid 0 inside it, so the engine cannot be a secrecy boundary.
- `pkg/khora` — a Go client. `pkg/transport` — the generated wire stubs.
- All four binaries answer `--version`, stamped from `git describe` at build time.

### Known limits

- **No production Template ships in this repo.** You build one; see `templates/`.
- **No client libraries** outside Go.
- **The engine's own listener is loopback-only** unless `-insecure-listen`, which
  publishes it with no authentication and no TLS. Put the gateway or a reverse proxy in
  front of it (`deploy/` carries a worked config).
- **Single node.** There is no scheduler across hosts.
- **Linux only**, and the substrate is a hard requirement, not a preference: gVisor/runsc,
  kernel overlayfs, netns, cgroup v2, btrfs in `squota` mode, `nft`, `ip`.
