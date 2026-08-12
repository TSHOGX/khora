# warren

**A self-hostable agent sandbox engine that needs no hypervisor.** Sandboxes are
persistent, mutable, and root-capable inside; their whole state is plain files on
your disk. It runs on gVisor/runsc, so it works where KVM is unavailable — an
unprivileged process, a CI runner, inside a container.

```sh
# create a sandbox
sandbox_id=$(warren sandbox create --template base --disk-byte 2147483648)

# upload a file into it, download it to stdout
printf 'warren works\n' > /tmp/warren-hello
warren files upload "$sandbox_id" /tmp/warren-hello /tmp/hello
warren files download "$sandbox_id" /tmp/hello
# → warren works

# or use the API directly
curl -X POST localhost:8080/v1/sandboxes \
     -d '{"template_id":"base","disk_byte":2147483648}'
curl 'localhost:8080/v1/sandboxes/sbx_b2aaead92a907e0d/files?path=/tmp/hello'
```

Sandboxes survive being paused: `POST /v1/sandboxes/{id}/pause` stops the world and
`/resume` brings it back with the filesystem exactly as it was. There is no memory
snapshot — persistence is the overlay on disk, which is why a sandbox is portable
across hosts, CPUs and runsc versions rather than pinned to a set of CPU features.

> **Status: pre-release.** The engine is complete and verified against the real
> substrate (908 end-to-end acceptance tests on actual runsc/overlayfs/btrfs/nftables),
> and [`deploy/`](deploy/) carries the authenticating TLS boundary that goes in front of
> it. A CLI ships in `cmd/warren`. Still missing before you deploy this: no production
> Template ships in this repo, and there are no client libraries for other languages yet.
> See [What is missing](#what-is-missing-before-you-deploy-this).

## Why this instead of a managed sandbox service

- **No hypervisor.** gVisor's systrap platform intercepts syscalls in userspace, so
  there is no `/dev/kvm` requirement and no nested-virtualization problem.
- **Self-hosted, single binary.** One `orchestrator` process, one config, a local
  SQLite store. Your code and data never leave your infrastructure.
- **State is transparent.** A sandbox's persistent state is an overlayfs upper
  directory you can read, back up, and move. Not an opaque RAM image.
- **Two primitives, not a framework.** The engine exposes `process` and `fs` and
  nothing about agents, sessions, or users. Those belong in a layer above it.
- **Egress is deny-by-default and enforced in the kernel.** A sandbox reaches only
  what its declaration allows, via per-sandbox nftables rules with a floor that a
  declaration cannot lift.

## What it does

| Capability | Shape |
| --- | --- |
| Sandbox lifecycle | create / get / list / pause / resume / destroy, ID-addressed |
| Run processes | start, stream stdout/stderr, signal, PTY, reconnect to a running process |
| Filesystem | read / write / stat / list / mkdir / remove / move, plus streaming upload |
| Volumes | independent of any sandbox: create, mount, snapshot, clone, import/export |
| Templates | promote a sandbox to an immutable base image other sandboxes derive from |
| Resource caps | CPU, memory, and disk, enforced via cgroup v2 and btrfs quotas |
| Networking | deny-by-default egress declarations; `publish` a guest port to the host |
| Autonomous reaping | idle sandboxes auto-pause; paused ones expire on a retention TTL |
| Activity | `POST /v1/sandboxes/{id}/activity` stamps the guest's idle clock, for work the engine cannot see |
| Event stream | a durable, replayable outbox of lifecycle transitions with a cursor, long-pollable via `wait=` |

Nothing about *agents* is in the engine — no session, tenant, or credential concept.
Credentials in particular are permanently out of scope: anything the engine handed a
sandbox would be readable by guest root, so it cannot be a secrecy boundary. The
supported pattern is to keep the secret in a process outside the sandbox and narrow
the sandbox's egress to that endpoint.

## Requirements

A Linux host with:

- **gVisor/runsc** (systrap platform), and **root** — `--network=sandbox` rejects
  `--rootless`, so materializing a sandbox needs root or `CAP_NET_ADMIN` +
  `CAP_SYS_ADMIN`. The sentry itself stays unprivileged.
- **kernel overlayfs**, **netns**, **cgroup v2**
- **btrfs in `squota` mode**, plus `btrfs-progs` — this is how disk caps are enforced
- **`nft`** and **`ip`** (nftables + iproute2)
- **Go 1.23+** to build

Engine code and all overlay scratch must sit on a native filesystem, never a
virtiofs shared mount. Why each requirement is hard, with the measurements behind it:
[`docs/decisions/ADR-0002`](docs/decisions/ADR-0002-execution-environment.md).

Development and testing happen on an OrbStack machine (`sbx`, arm64). The substrate
facts are additionally confirmed on amd64 where noted in ADR-0002's validation matrix.

## Build and run

```sh
# Reproducible build: produces bit-identical binaries on repeated invocations
make all
# → bin/orchestrator, bin/gateway, bin/sandboxd

# Or manually:
go build -trimpath -ldflags="-buildid= -s -w" -o bin/orchestrator ./cmd/orchestrator
go build -trimpath -ldflags="-buildid= -s -w" -o bin/gateway ./cmd/gateway
go build -trimpath -ldflags="-buildid= -s -w" -o bin/sandboxd ./cmd/sandboxd

# Verify reproducibility (optional):
make verify-reproducible

# A Template is a directory holding a guest rootfs, and it needs the sandboxd binary,
# which becomes the guest's PID 1. The repo ships the bare Template's DEFINITION
# (templates/bare/) rather than the ~730 MB tree it builds — see that directory's
# README for why. Needs skopeo + umoci:
sudo ./templates/bare/build.sh /var/lib/warren/templates/bare ./bin/sandboxd

# --state MUST be on a btrfs filesystem: disk caps are btrfs quotas, and the engine
# refuses to run a capped sandbox uncapped rather than silently ignoring the cap.
# --templates defaults to /var/lib/warren/templates, so it can be omitted here.
sudo ./bin/orchestrator --state /srv/warren/state
# serving §6.1 API on 127.0.0.1:8080
```

For a real deployment, [`deploy/`](deploy/) has the systemd unit and the
authenticating TLS boundary that goes in front. Note that `--state` and `--store`
default to paths relative to the working directory, which is fine for a dev run and
wrong for a service — the unit pins them absolutely.

The listen address must be loopback, and that is **enforced at startup**, not merely
defaulted: the API is unauthenticated, so a routable listener has to be a decision
stated out loud (`--insecure-listen`) rather than a flag value nobody noticed. Put an
authenticating TLS gateway in front of it — [`deploy/`](deploy/) has both options.
Notable flags — `--node-disk-byte` / `--node-memory-byte` turn on no-oversubscribe admission,
`--idle-timeout` / `--retention` set the reaping TTLs, `--publish-ip` sets the host
address published guest ports bind to, `--shutdown-timeout` bounds the drain.
`--help` lists all of them; the reasoning behind each value lives in the § or deep-dive
the help line cites.

```sh
curl -s localhost:8080/healthz   # liveness: is this the right process to keep running
curl -s localhost:8080/readyz    # readiness: should traffic come here now
curl -s localhost:8080/metrics   # Prometheus text: publisher health, outbox depth, state counts
```

**SIGTERM leaves running sandboxes running.** It stops the listener, lets in-flight
requests finish, and exits; the next boot re-adopts the live sandboxes with the same
placement and IP (§4.4-A takeover). It deliberately does *not* pause them — a pause
discards guest memory, so restarting the orchestrator to change a flag would destroy
every guest's in-memory work. Streams still open when `--shutdown-timeout` expires are
closed, and callers reconnect. A second SIGTERM exits immediately.

The reaper and the outbox publisher stop *before* the request drain, so a TTL sweep
cannot start a fresh transition on the way out. Events written by requests still
finishing during the drain are therefore published on the **next** boot rather than this
one — the outbox is durable and delivery is at-least-once (§4.4-C), so a pull subscriber
on `GET /v1/events` loses nothing either way.

## Test

```sh
go test ./...            # unit + contract tests, no root needed

# End-to-end acceptance against the real substrate (needs root).
# -p 1 is required: these packages launch real runsc containers, and running them
# concurrently starves the in-guest health probes the reaper's idle logic needs.
sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 ./...

# The pause/write race (§4.2 drain barrier), tagged separately because each round
# overlaps a 64 MiB write with a pause. Run it deliberately.
sudo env "PATH=$PATH" "HOME=$HOME" \
  go test -tags 'substrate drainrace' -count=1 -timeout 30m ./test/ -run TestPauseDrain

# Quota-row reclamation across churn, tagged separately for the same reason: its cost
# is btrfs `--sync` settling a qgroup tree this box shares with other tenants, so it
# swings 3.5-26.5s and was 87% of internal/btrfs's runtime.
sudo env "PATH=$PATH" "HOME=$HOME" \
  go test -tags 'substrate quotachurn' -count=1 ./internal/btrfs/
```

A third tag, `testguest`, compiles an in-guest fault-injection seam into `sandboxd`
that is absent from a production guest. The substrate harness applies it itself; you
never pass it by hand.

The acceptance suite is the honest record of what works. It asserts adversarial
properties, not happy paths — a forged source address does not bypass the egress
floor, the floor survives a veth rebuild, a live TCP flow survives a control-plane
restart, a crash between writing snapshot bytes and inserting its row is repaired on
the next boot.

## What is missing before you deploy this

Known and deliberate, not oversights:

- **No authentication, no TLS *in the engine*** — permanently, by design, because
  credentials are excluded from it (§1.4). That boundary lives in front of the engine
  instead, and [`deploy/`](deploy/) now provides it: a tested nginx config (the
  recommended path) or `cmd/gateway`, an mTLS binary from this repo. The engine
  enforces its own half — it **refuses a non-loopback `--addr` at startup** unless
  `--insecure-listen` says otherwise out loud — so the gateway is the only
  outward-facing surface by construction. What you still supply is the PKI: a server
  certificate and the CA that issues caller certificates.
- **One Go client and a machine-readable contract, and no SDK beyond that** —
  deliberately, and the boundary is [`ADR-0008`](docs/decisions/ADR-0008-outward-client-boundary.md).
  What ships: [`pkg/warren`](pkg/warren/) — imported as
  `github.com/TSHOGX/warren/pkg/warren`, an ordinary `go get`-able path, which is the
  whole reason the module is named that way rather than a bare `warren` — a thin client
  over the §6.1 surface, with
  `cmd/warren` (the CLI) as a shell over it — which is what keeps it honest rather than
  merely present, since a client call no command can reach shows up immediately. Plus
  the contract itself in two forms: `proto/` with stubs at `pkg/transport/gen/` for the
  guest transport, and [`docs/spec/openapi.json`](docs/spec/openapi.json) — generated
  from the handler types by `cmd/openapigen`, and guarded against going stale — for the
  REST surface. An ergonomic SDK in any language belongs in the repo that consumes it:
  "thin" here is a *rule* (every field 1:1 with a §6.1 field, no invented verb, no state
  across calls), and retry, backoff, and wait-until-running are each a promise about
  time the engine never made.
- **The bare Template resolves no names out of the box**, and that is by design rather
  than an omission: it ships no `/etc/resolv.conf` and the engine injects none, so a
  deployment supplies both the file and a UDP/53 `egress.allow[]` entry. The reasoning,
  and the two alternatives that are worse, are in
  [`templates/bare/README.md`](templates/bare/README.md). Note also that the repo holds
  the Template's *definition*, not the built tree — you build it once.
- **Operational signal is deliberately process-level.** `/healthz`, `/readyz` and
  `/metrics` (Prometheus text, no dependency) report publisher health, outbox depth,
  and sandbox counts by state; per-object numbers stay on the §6.1 read surfaces
  rather than being exported twice. The two probes diverge on purpose: a halted outbox
  publisher makes the instance **not ready but still alive**, because a restart cannot
  clear a poison envelope in a durable outbox. What is missing is anything richer —
  there are no traces, and no per-sandbox metrics. **Per-sandbox series are refused on
  label cardinality**: a sandbox is short-lived and there may be thousands, so one series
  per sandbox per metric grows the scrape without bound and outlives the object it
  describes. Sandbox-level numbers stay where a caller asks about one object by ID. Traces
  are a different question and an open one (`open-decisions.md`): their cost does not scale
  with object count, so the cardinality argument does not reach them.
- **The event outbox does not trim by default.** `--event-retention` bounds it, but the
  default is 0 (never trim), because the right window is the reconnect duration you
  promise your subscribers and the engine must not invent that number. Left unset, the
  table grows with every lifecycle transition. The reference dedup sink is separately an
  in-process map, deliberately — durable dedup belongs to a subscriber, outside the
  engine.
- **Single node.** One orchestrator, no HA, no cross-host migration. The runtime
  provider seam is where a scheduled remote isolation unit would plug in.

## Documentation

- [`docs/spec/`](docs/spec/) — the architecture spec (§1–§6): what the engine should
  be, and why. The best current statement of intent, revisable but never edited
  casually.
- [`docs/spec/deep-dives/`](docs/spec/deep-dives/) — single-topic rationale: the
  measurements, the falsified alternatives, the options rejected and on what evidence.
- [`docs/spec/open-decisions.md`](docs/spec/open-decisions.md) — everything still
  genuinely unsettled. Settled and unsettled never overlap.
- [`docs/decisions/`](docs/decisions/) — ADRs for one-time tool and environment
  choices.

## Layout

```
cmd/orchestrator/      the engine host: flags → Engine → serve the API
cmd/sandboxd/          in-guest daemon: init (PID 1) + the process/fs transport
cmd/gateway/           OUTSIDE the engine: mTLS + TLS termination in front of the API
deploy/                deployment: the nginx/gateway boundary, plus the systemd unit
internal/orchestrator/ the assembly: holds the seams, serves the API
internal/model/        shared model: sandbox identity, lifecycle state (pure types)
internal/lifecycle/    the mediator: state machine, transition lock, cold-start chain
internal/store/        control-plane records: SQLite + in-memory behind one interface
internal/runtime/      runtime provider seam + the local runsc implementation
internal/rootfs/       rootfs provider seam + host overlayfs assembly
internal/volume/       volume backend seam + local btrfs arm (+ a test-only second arm)
internal/network/      egress declaration validation + the nftables rule generator
internal/transport/    host-side process/fs client over Connect-RPC, plus Health
internal/btrfs/        leaf: subvolume + squota primitives
internal/allocbytes/   leaf: bytes actually allocated for a tree
internal/guestport/    leaf: constants two subsystems must agree on
internal/ruleset/      leaf: the rule vocabulary, static address facts, shared criteria
internal/arch/         structural guards: subsystems must not import each other
internal/waitfor/      the one polling loop the suites share (re-checks past the deadline)
proto/                 transport wire schema
test/                  end-to-end acceptance against the real substrate
```

Subsystems never call each other — that is enforced by a test, not by review. When two
need the same fact it goes in a leaf package, and a leaf may decide things but may not
do I/O or touch the substrate.

> **On the name.** `warren` is the project and the Go module. Inside
> [`docs/spec/`](docs/spec/), *engine* means the sandbox abstraction itself and
> *orchestrator* means the binary hosting it — different layers, not other names for
> the project, so the spec was not rewritten. The protobuf wire package stays
> `engine.transport.v1` because that string is a protocol identifier.
