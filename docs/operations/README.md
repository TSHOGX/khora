# Operations and verification

## Health and shutdown

`/healthz`, `/readyz` and `/metrics` expose process-level health, publisher health,
outbox depth and sandbox counts. A halted outbox publisher makes the instance not
ready while it remains alive. SIGTERM stops new requests, drains in-flight work and
leaves running sandboxes running so the next boot can adopt them; a second SIGTERM
exits immediately.

## Deterministic checks

The checks required by CI and by a ready-for-review change are:

```sh
go build ./...
go vet ./...
GOARCH=amd64 go build ./...
go test ./...
./scripts/check-generated.sh
```

`scripts/check-generated.sh` verifies the OpenAPI contract and regenerates protobuf
outputs from `proto/`. The OpenAPI output is regenerated with:

```sh
go run ./cmd/openapigen -out docs/api/openapi.json
```

## Substrate acceptance

Run `go run ./test/preflight/substrategate --base origin/main` against the complete
PR diff. If it reports a material substrate change, run the tagged suite on a
conforming Linux host (root, runsc, overlayfs, netns, cgroup v2, btrfs `squota`,
`nft`, `ip` and native filesystem storage). Record `passed`, `failed` or `not required`,
the exact command, host profile and result in the PR after the last material change.
Do not commit the transcript under `docs/` and do not treat hosted CI as a substrate
run.

## Recovery expectations

The orchestrator reconciles placements and storage on startup and periodically.
Failed transitions return to their source state, half-built placements are reclaimed,
and durable outbox rows are replayed. Diagnose substrate failures through the
preflight harness before attributing them to engine behavior.
