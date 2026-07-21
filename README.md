# engine

A persistent, mutable, root-capable agent sandbox engine — no-hypervisor
(gVisor/runsc systrap), self-hostable, state-transparent.

> **Architecture spec** (the source of truth for *what this should be*) lives
> in `docs/spec/` (§1–§6). Milestone plans in `docs/plans/`, one-time decisions in `docs/decisions/`.
> Milestones ship in order along the ladder in
> [`docs/plans/README.md`](docs/plans/README.md) — start there for where the
> implementation currently stands.

## Layout (canonical subsystem short-names, §5)

```
cmd/orchestrator/   single engine-host binary (§1.1): control plane + §4 mediator
cmd/sandboxd/       in-guest daemon: init (PID 1) + transport (process/fs) modes (§5.E.1)
internal/model/     §3 shared model: sandbox identity, lifecycle state (pure types)
internal/lifecycle/ §4 mediator: state machine + transition lock + cold-start chain
internal/store/     control-plane record seam: SQLite + in-memory, one interface
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

Milestone ladder (see [`docs/plans/README.md`](docs/plans/README.md)):
M1 walking skeleton → M2 store+§4.4 → M3 transport depth → M4 reaper+health →
M5 Volume → M6 network egress → M7 Promote+GC → M8 credential store.
