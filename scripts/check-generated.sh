#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

./scripts/check-retired-references.sh

command -v buf >/dev/null || { echo "check-generated: buf is required" >&2; exit 2; }
command -v protoc-gen-go >/dev/null || { echo "check-generated: protoc-gen-go is required" >&2; exit 2; }
command -v protoc-gen-connect-go >/dev/null || { echo "check-generated: protoc-gen-connect-go is required" >&2; exit 2; }

go test ./internal/orchestrator -run '^TestOpenAPIDocIsNotStale$'
go test ./cmd/orchestrator -run '^TestOrchestratorDocIsNotStale$'
go test ./cmd/khora -run '^TestCLIDocIsNotStale$'

generated_tmp=$(mktemp -d)
trap 'rm -rf "$generated_tmp"' EXIT
(cd proto && buf generate -o "$generated_tmp/proto")
diff -ru pkg/transport/gen "$generated_tmp/pkg/transport/gen"
