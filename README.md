# engine

A persistent, mutable, root-capable agent sandbox engine — no-hypervisor
(gVisor/runsc systrap), self-hostable, state-transparent.

> **Architecture spec** (the source of truth for *what this should be*) lives in
> `docs/spec/` (§1–§6); one-time tool/environment choices in `docs/decisions/`.
> For where the implementation stands and what ships next, start at the ladder:
> [`docs/plans/README.md`](docs/plans/README.md).

## Layout (canonical subsystem short-names, §5)

```
cmd/orchestrator/      thin entrypoint: flags → Engine → serve the §6.1 API
cmd/sandboxd/          in-guest daemon: init (PID 1) + transport (process/fs) modes (§5.E.1)
internal/orchestrator/ the single engine-host assembly (§1.1): holds the seams, serves §6.1
                       (incl. the streaming large-file bypass, the one non-RPC route)
internal/model/        §3 shared model: sandbox identity, lifecycle state (pure types)
internal/lifecycle/    §4 mediator: state machine + transition lock + cold-start chain
internal/store/        control-plane record seam: SQLite + in-memory, one interface
internal/runtime/      §5.A: runtime provider seam + local_runsc
internal/rootfs/       §5.B: rootfs provider seam + host overlayfs assembly
internal/volume/       §5.C: volume.Backend seam + local btrfs arm + a TEST-ONLY plain-dir
                       second arm (the seam's only falsification instrument) + volumetest
internal/transport/    §5.E: host-side process client + the 7-RPC fs surface + Health (Connect-RPC/h2c)
internal/btrfs/        leaf package: subvolume + squota primitives (no engine model)
internal/allocbytes/   leaf package: "bytes actually allocated" for a tree — the one
                       measurement §5.B and §5.C both need for §3.4's second term
internal/guestport/    leaf constants two subsystems must agree on: the guest netns TCP
                       port (§5.A/§5.E) + the upload-temp prefix (§5.E/§5.B)
internal/arch/         structural guards: subsystems must not import each other, nor
                       read the control plane
proto/                 transport wire schema; generated code under internal/transport/gen
test/                  L2–L4 substrate acceptance + preflight fixtures
```

## Build & test

Runs on a Linux host with gVisor/runsc, kernel overlayfs, netns, cgroup v2, and
btrfs in `squota` mode + `btrfs-progs` — dev environment: OrbStack machine `sbx`.
Engine code + all overlay scratch must live on native fs, not a virtiofs shared
mount. Why each of those is a hard requirement, and the measured machine facts:
[`docs/decisions/ADR-0002`](docs/decisions/ADR-0002-execution-environment.md).

```sh
go build ./... && go vet ./...
go test ./...            # unit + contract tests only

# L2/L3 acceptance against the real substrate (root; runsc/overlay/btrfs/netns).
# Without root every overlay mount fails with EPERM. `-p 1` is required: these
# packages each launch real runsc containers, and running them concurrently
# starves the in-guest Health probes the reaper's idle classification needs.
sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 ./...

# The pause/write race (§4.2 drain barrier) is tagged separately: each round
# overlaps a 64 MiB write with a pause, so it costs minutes rather than seconds.
# Run it deliberately — the property it pins does not change per commit.
sudo env "PATH=$PATH" "HOME=$HOME" \
  go test -tags 'substrate drainrace' -count=1 -timeout 30m ./test/ -run TestPauseDrain
```

A third tag, `testguest`, compiles the in-guest fault-injection seam into `sandboxd`
(absent from a production guest, §5.E.1). The substrate harness builds the guest
binary with it automatically — you never pass it by hand.
