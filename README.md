# khora

khora is a self-hostable sandbox engine. It exposes ID-addressed sandboxes,
processes, filesystems, volumes and immutable Templates; product concepts such as
users, sessions and credentials belong in an adapter outside the engine.

The orchestrator runs on Linux with gVisor/runsc, overlayfs, network namespaces,
cgroup v2, btrfs `squota`, `nft` and `ip`. The engine is intentionally unauthenticated
and listens on loopback by default. Put the TLS/authenticating gateway in `deploy/`
in front of it for an outward-facing deployment.

## Quick start

```sh
make all
sudo ./bin/orchestrator --state /srv/khora/state
khora sandbox create --template base --disk-byte 2147483648
```

The complete flag and deployment reference is in
[Configuration](docs/configuration/README.md). The API and CLI contract is in
[API reference](docs/api/contract.md); the checked-in OpenAPI document is generated
with `go run ./cmd/openapigen -out docs/api/openapi.json`.

## Documentation

- [Concepts](docs/concepts/architecture.md) — boundaries, security model, lifecycle and subsystem seams.
- [Configuration](docs/configuration/README.md) — host requirements, flags and deployment shape.
- [Operations](docs/operations/README.md) — testing, verification and recovery expectations.
- [API reference](docs/api/contract.md) — REST, Connect-RPC and CLI surfaces.
- [Internals](docs/internals/) — measurements, constraints and counterexamples behind the implementation.
- [Templates](templates/bare/README.md) — the definition and build process for the bare Template.

## Build and verify

```sh
go build ./...
go vet ./...
GOARCH=amd64 go build ./...
go test ./...
./scripts/check-generated.sh
```

Before a PR is ready, decide whether the real-substrate suite is required with:

```sh
go run ./test/preflight/substrategate --base origin/main
```

When required, run the tagged suite on a conforming Linux host and record the exact
command, host profile and result in the PR. Hosted CI runs the deterministic build,
vet, cross-build, unit-test and generated-output checks; it does not claim a local
substrate result.

## Scope and limitations

The engine is single-node and has no authentication, TLS, cross-host migration or
non-Go SDK. Credentials are not a secrecy boundary because guest root can read any
material placed in a sandbox. Persistent state is plain files and may be rebuilt in
the `0.0.x` line; there is no compatibility or migration promise.

See [CHANGELOG.md](CHANGELOG.md) for release notes and the repository's
[engineering conventions](CLAUDE.md) for contribution boundaries.
