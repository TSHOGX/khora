# ADR-0007 — The deployment topology: where authentication and TLS live, what the engine refuses to listen on, and how the process comes up

- **Status:** accepted
- **Date:** 2026-08-12
- **Consumers:** anyone deploying this engine; `cmd/gateway`, `cmd/orchestrator`,
  `deploy/` and the README's "Build and run" carry the resulting topology.

## Context

The §6.1 control-plane API has no authentication and no TLS. That is not an
oversight — §1.4 permanently excludes the credential family from the engine, and
§1.1 bars those four spellings as engine-internal names — but it does mean the
engine as shipped is deployable only where every caller is already authorized. A
release needs a component that terminates TLS and authenticates callers, and the
question this ADR settles is **where that component lives and what stops it from
being bypassed**.

Three facts constrain the answer, and the third is the one that decides the shape:

- **The engine must hold no credential material.** §1.4's second reason is
  structural rather than a policy preference: every byte the engine can hand a guest
  is readable by uid 0 inside it, so the engine cannot be a secrecy boundary for
  anything. A component that verifies callers therefore cannot be `internal/`.
- **The API is two protocols on one listener.** Control plane is REST/JSON on
  `/v1/…`; the process/fs data plane is Connect-RPC on
  `/khora.transport.v1.*/`, wrapped in h2c so server-streams work over cleartext
  HTTP/2 (transport D4.2). Anything in front of it has to carry streaming in both
  directions — `Start`/`Connect` are server-streaming and `StreamInput` is
  client-streaming — and carry it *incrementally*, because an interactive PTY whose
  events arrive in one buffered flush at exit is not a working PTY.
- **`--addr` already defaults to `127.0.0.1:8080`, and a default is not a boundary.**
  If the engine can also be told to listen on a routable address, then a gateway in
  front of it is a door standing beside an open wall: the operator who typed
  `--addr 0.0.0.0:8080` for a quick test has silently published an unauthenticated
  control plane that can create root-capable sandboxes.

## Decision

**Authentication and TLS live outside the engine, in front of a listener the engine
refuses to place anywhere but loopback.** Three parts, and the third is what makes
the first two mean anything.

1. **A reverse proxy is the primary, documented path.** The repo ships a working
   example configuration that terminates TLS and authenticates, plus the deployment
   notes for it. Most operators already run one, and this asks them to configure a
   component they already trust rather than to trust a new binary of ours on the
   security path.

2. **`cmd/gateway` is the in-repo option, and it authenticates with mTLS** — for
   deployments that would rather not add a proxy. mTLS rather than bearer tokens:
   a bearer token is a member of the family §1.4 excludes, and even in a binary
   outside `internal/` it would mean this repo ships a credential store, an issuance
   path, and a revocation story. mTLS pushes caller identity into the TLS layer,
   where the operator's existing PKI holds it and the gateway holds nothing at rest
   but a CA bundle it verifies against.

3. **The orchestrator refuses a non-loopback `--addr` at startup** unless an
   explicit override flag says so out loud. This is the enforcement half, and it is
   a refusal rather than a line of documentation because it is the only part that
   makes the gateway a boundary by construction rather than by convention. The
   insecure deployment stays reachable — some operators genuinely want it, inside a
   private network namespace or behind a host firewall — but it has to be *stated*,
   and the flag's name and help text say what is being given up.

   The refusal is in the assembly (`orchestrator.New`), not in `cmd/`, following the
   precedent `--via-uid` set: the substrate harness builds an `Engine` directly, so a
   check that lived in `main.go` would be bypassed by every test and by any embedder.

## Rationale

**§1.1's "single orchestrator binary" is not an obstacle, and this is not a spec
divergence.** That sentence carries its own qualifier: it describes the engine's own
deployment shape, not a claim that no other process may exist in the topology.
`egress.via[]` is the established precedent — the engine does not start that process,
does not health-check it, and does not know what it is — and `cmd/sandboxd` has always
been a second binary. The gateway adds a process to the deployment while leaving the
engine's dependency list unchanged, which is exactly the distinction the qualifier
draws.

**A proxy can carry this API, and that was measured rather than assumed.** The
failure mode worth ruling out was not "does the call complete" but "does it
interleave": a hop that buffers the response body passes a completion test while
making `Start` useless. Measured against the real generated handlers, an origin
emitting three events 300 ms apart, through a proxy with an HTTP/1.1 upstream and
through one with an h2c upstream:

| Leg | server-stream | client-stream |
| --- | --- | --- |
| direct h2c | 3 events, spread 600 ms | spread at origin 610 ms |
| proxy, HTTP/1.1 upstream | 3 events, spread 600 ms | spread at origin 610 ms |
| proxy, h2c upstream | 3 events, spread 610 ms | spread at origin 610 ms |

The spread survives every leg, so neither direction is being buffered. Repeated
across all three Connect protocols (`connect`, `grpc`, `grpc-web`), both stream
directions completed on all three legs. The consequence for the shipped config: an
ordinary `proxy_pass` is sufficient for the Connect protocol, and the config still
carries h2c upstream, because a consumer using the **gRPC** protocol requires HTTP/2
end to end and that consumer is not ours to constrain.

**"Loopback" must be decided on the parsed address, and the classifier agrees with
what the kernel actually does.** Measured, comparing `netip.ParseAddr(host).IsLoopback()`
against whether a listener on that spelling is reachable from this host's routable
address:

| `--addr` | `IsLoopback` | actually bound | reachable off-loopback |
| --- | --- | --- | --- |
| `127.0.0.1:8080` | true | `127.0.0.1:8080` | no |
| `127.0.0.2:8080` | true | `127.0.0.2:8080` | no |
| `[::1]:8080` | true | `[::1]:8080` | no |
| `[::ffff:127.0.0.1]:8080` | true | `127.0.0.1:8080` | no |
| `0.0.0.0:8080` | false | `[::]:8080` | **yes** |
| `[::]:8080` | false | `[::]:8080` | **yes** |
| `192.168.139.93:8080` | false | `192.168.139.93:8080` | **yes** |

The verdict matches reachability on every IP literal, including the v4-mapped form a
hand-rolled string comparison against `"127."` would have got wrong. Two spellings
are not literals and need their own treatment: **an empty host** (`:8080`) binds all
interfaces and is measured reachable, so it is refused as non-loopback rather than
read as a default; and **a name** (`localhost:8080`) resolves through deployment-
controlled configuration, so "localhost is loopback" is not a fact the engine may
assume. A name is accepted only when **every** address it resolves to is loopback —
`any` would let one `/etc/hosts` line that maps a name to both `127.0.0.1` and a
routable address open the wall while the check passes.

**Why enforce rather than document.** The README already documented the loopback
default and the reason for it, and documentation is the mechanism that was already in
place when this was written down as a gap. The asymmetry of consequences is what
settles it: the cost of enforcement is one flag for the operator who wants the
insecure shape, and the cost of documentation alone is a silently-published control
plane that can create root-capable sandboxes. This repo makes that trade the same way
elsewhere — subsystem isolation is a test rather than a review note, and the
substrate-gate exemption is a tool's verdict rather than a committer's claim.

## The orchestrator's own deployment shape

The topology above decides how the engine process itself comes up, so it is recorded
here rather than in a second document. `deploy/khora-orchestrator.service` is the
concrete form; three of its choices are decisions rather than boilerplate.

**The unit pins absolute paths; the flag defaults do not move.** `--state` and
`--store` default to `./scratch/state` and `./scratch/store.db`, relative to the
working directory, while `--templates` is already absolute. That asymmetry is
deliberate for the reason `--templates`' own flag help gives: a Template contains
mode-0700 root-owned directories, so one built inside a module tree breaks `go build
./...` for an ordinary user. The dev defaults are right for a dev run — `./scratch` is
where a developer wants scratch state — and wrong for a service, where they would
resolve against whatever working directory the unit happened to have. Pinning them in
the unit fixes the deployment without making the dev path awkward. **Whether the
defaults themselves should become absolute is left open, deliberately**: it would make
every existing test and dev invocation write outside the repo, which is a bigger change
than this stage needs and belongs to whoever wants it.

**Stop behavior is the graceful-shutdown decision made concrete.** `KillSignal=SIGTERM`
with `TimeoutStopSec=45` sitting *above* the engine's own `--shutdown-timeout` of 30 s,
so the engine's bounded drain completes on its own terms instead of being SIGKILLed
partway; the two are raised together or not at all. `KillMode=process` ensures both the
initial signal and any `TimeoutStopSec` escalation target only the orchestrator.
`KillMode=mixed` would SIGKILL the whole service cgroup on escalation, including runsc
sentries for guests this shutdown is designed to leave running.
Measured on a real box: `systemctl stop` drains and exits with `Result=success` and
`ExecMainStatus=0` in about 0.2 s on an idle node.

**The hardening directives that look missing are refused on purpose.** No
`PrivateTmp`, no `PrivateDevices`, no `NoNewPrivileges`, no `ProtectSystem=full`. runsc
needs device access and the ability to create mount and network namespaces, and the
engine assembles overlays, so each of those breaks sandbox creation — and each would
pass `systemd-analyze verify` and fail at the first create, which is the worst shape a
misconfiguration can take. Exposure is narrowed by naming writable paths
(`ReadWritePaths`, `ReadOnlyPaths` on the Template tree) instead.

Two prerequisites the engine cannot supply for itself, so provisioning must: the state
root has to be **btrfs in squota mode** (disk caps *are* btrfs quotas, so a non-btrfs
root fails at the first create rather than at boot), and a **Template must be built
before the first start**, since the boot seed registers what it finds and an empty
directory is a running engine with nothing to run.

**The instance stays discardable.** Provisioning may say "rebuild the store"; it may
never say "migrate it". `store.db`, the overlays and the runsc state are all
reconstructible, and an internal deployment is not the trigger for compatibility work.

## Consequences

- **The gateway is the only outward-facing surface, by construction.** With the
  listener pinned to loopback, there is no second path to the API for the gateway to
  be bypassed by — which is what lets the README stop qualifying every deployment
  statement with "assuming you put something in front of it".
- **The override flag is a documented insecure mode, not a hidden one.** It exists
  because refusing outright would push operators toward patching the source, and a
  patched binary is worse than a flag that appears in `ps` output and in the startup
  log.
- **`internal/` gains nothing from this ADR.** No credential type, field, package or
  endpoint enters the engine; the engine's view of a caller is still "a request
  arrived on the loopback listener". The vocabulary boundary is untouched.
- **The gateway is not on the engine's startup path.** Like the process behind
  `egress.via[]`, the engine does not start it, health-check it, or know it exists. A
  gateway that is down is an unreachable API, not a degraded engine.
- **mTLS shifts one operational burden onto the operator**: issuing and rotating
  client certificates. That is the deliberate trade for holding no credential
  material in this repo, and it is why the reverse proxy stays the primary documented
  path — an operator with an existing identity provider should use it.

## Rejected / deferred

- **Bearer tokens or an API-key header in `cmd/gateway`.** The whole family is
  excluded by §1.4, and while that exclusion binds the engine rather than a binary
  under `cmd/`, shipping a token verifier still means shipping issuance, storage and
  revocation — three surfaces whose absence is the point. mTLS gets caller identity
  from a layer the operator already operates.
- **Authentication inside `internal/`, behind an interface.** This is the shape that
  looks cleanest and is refused hardest: it would put a credential concept in the
  engine, and §1.4's reasoning does not weaken for being interface-shaped.
- **Terminating TLS in the orchestrator while authenticating elsewhere.** It splits
  one boundary across two components, and it would give the engine a certificate and
  a key to hold — the exact material this ADR keeps out.
- **Documenting the loopback requirement without enforcing it.** That was the state
  when this was written; see the rationale for why the asymmetry settles it.
- **Refusing a non-loopback `--addr` outright, with no override.** Rejected as
  self-defeating: it invites a source patch, which removes the operator's decision
  from the record entirely.
- **Making the gateway mandatory — for example, having the engine refuse to serve
  until one announces itself.** That would make the engine depend on the component,
  reversing §1.3's one-way dependency rule and contradicting the `egress.via[]`
  precedent this ADR rests on.
