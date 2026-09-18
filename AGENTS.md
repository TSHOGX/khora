# khora engineering conventions

`khora` is the project, Go module (`github.com/TSHOGX/khora`), CLI and protobuf wire package
(`khora.transport.v1`). `engine` means the sandbox abstraction; `orchestrator` is the binary
that assembles and serves it. Keep these names stable.

## First principles

The engine provides one thing: a clean, decoupled sandbox abstraction. Product concepts such as
users, tenants, sessions, turns, agents and credentials belong in an adapter outside the engine.
The engine exposes ID-addressable atoms (sandbox, process, fs, volume and Template) for that
layer to compose. A need that can be implemented by registering an implementation, adding a join
table or adding a subscriber should use that seam; otherwise prove why it belongs here.

Every claim has a mechanical link to what makes it true: tests link behavior to code, generated
files link output to a canonical source.
Do not use prose, checkboxes or duplicated metadata as a substitute for evidence.

## Architecture

Subsystems never call one another. Mechanisms stay in their subsystem; lifecycle verbs belong to
`internal/lifecycle`. Shared facts, vocabulary and pure criteria belong in leaf packages. A leaf
does no I/O, process execution or substrate access. `internal/arch` checks direct and transitive
reachability, and its allowlist stays empty.

Seams are interfaces: `store.Store`, `runtime.Provider`, `rootfs.Provider` and `volume.Backend`.
Their contracts expose policy and portable handles, never cgroup paths, netns descriptors or host
absolute paths. Structural seam tests inspect parameters, results and fields; API edge tests keep
raw host paths out of response bodies.

Use the established package vocabulary: subsystems `runtime`, `rootfs`, `volume`, `network` and
`transport`; supporting packages `model`, `lifecycle`, `store`, `orchestrator`, `btrfs`,
`guestport`, `allocbytes`, `ruleset` and `arch`. Do not coin a second name for a settled concept.
Keep Rootfs, the `fs` wire service, the rootfs provider seam and the volume backend distinct.
Canonical terms are `sandbox`, `placement`, `Binding`, `provider`, `seam`, `squota`,
`fail-closed`, `outbox`, `Overlay`, `Template` and `Volume`.

Do not introduce product semantics as engine identifiers. In particular, `generation`,
`principal`, `tenant`, ownership `owner`, `session`, `turn`, `agent`, `credential`, `secret`,
`token` and `api_key` are excluded from engine entities. Credentials are permanently outside the
engine: anything handed to a sandbox is readable by guest root and cannot be a secrecy boundary.
Other product terms may appear when prose explicitly places them outside the engine;
`generation` is excluded from prose as well.

## Documentation and code

Documentation describes current code. Fix an ordinary mismatch in the document. If a page makes
an external API, security or compatibility promise, fix code when it would otherwise break a
believing consumer.

Organize maintained docs by reader: concepts, configuration, operations, API reference and
internals. Internals record constraints, measurements and counterexamples, not design essays or
facts already enforced by tests. Generated pages state their source and regeneration command and
are checked for staleness.

New Go code must not cite section numbers, Markdown paths or ADR IDs. State the constraint at the
code site that makes the choice, once. If it cannot fit there, put the fact in maintained internals
documentation. Comments carry obligations and measurements, not arguments for a rule.
Operator-facing flags, errors and logs are one line; error strings do not cite documents.

## Verification

Validate changes with the applicable checks:

```text
go build ./...
go vet ./...
GOARCH=amd64 go build ./...
go test ./...
```

CI owns these cheap, repeatable checks plus architectural/build-tag and generated-output checks.
`scripts/check-generated.sh` is the canonical local generated-output check; CI installs its
pinned generator versions before invoking it.
Run the substrate acceptance suite for changes that affect substrate behavior, on a host
with Linux, runsc, overlayfs, netns, cgroup v2, btrfs `squota`, `nft` and `ip`; native filesystem
storage is required. Report the command, host profile, result and any skip reason after the
last material change. Do not commit substrate logs or make CI claim a local run.

Run `go run ./test/preflight/substrategate --base <ref>` to classify the complete change
since its comparison base, including the working tree. Without `--base`, it compares against HEAD.
Build, vet and unit tests validate the code independently of the substrate decision.
Use `internal/waitfor` for polling, and test concurrency and failure invariants at risky seams.
A new structural guard needs a committed negative test or fixture; register every build tag in `internal/arch`'s matrix alone and, where
applicable, paired with `substrate`. Diagnose a suspected substrate failure through the harness
before assigning it to the engine.

The `0.0.x` line has no compatibility burden. Persistent state may be rebuilt; do not add schema
migrations, compatibility shims, version negotiation or backward-compatibility branches. Keep
repository artifacts in English, and do not create `docs/plans/`.
