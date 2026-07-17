# engine

A persistent, mutable, root-capable agent sandbox engine — no-hypervisor
(gVisor/runsc systrap), self-hostable, state-transparent.

> **Architecture spec** (the source of truth for *what this should be*) lives
> separately in `docs/plan/` (§1–§6). **This repo** is the implementation.
> The current implementation milestone and its acceptance criteria are in
> [`docs/WALKING-SKELETON.md`](docs/WALKING-SKELETON.md) — read it first.

## Layout (canonical subsystem short-names, §5)

```
cmd/orchestrator/   single engine-host binary (§1.1): control plane + §4 mediator
cmd/sandboxd/       in-guest daemon: init (PID 1) + transport (process/fs) modes (§5.E.1)
internal/model/     §3 shared model: sandbox identity, lifecycle state (pure types)
internal/lifecycle/ §4 mediator: state machine + transition lock + cold-start chain
internal/store/     control-plane record seam (M1 memory → M2 SQLite)
internal/runtime/   §5.A: runtime provider seam + local_runsc
internal/rootfs/    §5.B: FS provider seam + host overlayfs assembly
internal/transport/ §5.E: host-side process/fs client + Health (Connect-RPC/h2c)
```

## Build & test

Runs on a Linux host with gVisor/runsc, kernel overlayfs, netns, cgroup v2
(dev environment: OrbStack machine `sbx`). Engine code + all overlay scratch
must live on native fs, not a virtiofs shared mount.

```sh
go build ./...
go test ./...
```

Milestone ladder (see `docs/WALKING-SKELETON.md` for M1 detail):
M1 walking skeleton → M2 store+§4.4 → M3 reaper+health → M4 Volume →
M5 network egress → M6 Promote+GC → M7 credentials.
