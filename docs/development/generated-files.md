# Generated files

Generated artifacts have one source, one documented update command, and a staleness
check. Do not edit generated output directly.

## Documentation

`docs/configuration/orchestrator.md` is rendered from the orchestrator flag set in
`cmd/orchestrator`. `docs/api/cli.md` is rendered from the Cobra command tree in
`cmd/khora`.

Regenerate both with:

```sh
./scripts/generate-docs.sh
```

`TestOrchestratorDocIsNotStale` and `TestCLIDocIsNotStale` compare checked-in bytes
with fresh rendering. `scripts/check-generated.sh` runs both checks.

## OpenAPI

`docs/api/openapi.json` is rendered from the route table and handler wire types in
[internal/orchestrator/openapi.go](../../internal/orchestrator/openapi.go).
[cmd/openapigen](../../cmd/openapigen) is the writer used to regenerate it:

```sh
go run ./cmd/openapigen -out docs/api/openapi.json
```

`internal/orchestrator.TestOpenAPIDocIsNotStale` detects drift between source and the
checked-in document.

## Protobuf and Connect

The canonical wire source is
[proto/khora/transport/v1/transport.proto](../../proto/khora/transport/v1/transport.proto).
The Buf configuration in `proto/` renders Go and Connect stubs under
`pkg/transport/gen`:

```sh
(cd proto && buf generate)
```

Use the pinned `buf`, `protoc-gen-go`, and `protoc-gen-connect-go` versions from
[CI](../../.github/workflows/ci.yml). `scripts/check-generated.sh` renders into a
temporary directory and compares those bytes with the checked-in stubs.

## Canonical check

Run the combined local gate after changing any source above:

```sh
./scripts/check-generated.sh
```

The script also runs the retired-reference guard. CI installs the required tools
before invoking it; a missing local generator is reported as a setup error rather
than mistaken for a clean result.
