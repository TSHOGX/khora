#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

command -v buf >/dev/null || { echo "check-generated: buf is required" >&2; exit 2; }
command -v protoc-gen-go >/dev/null || { echo "check-generated: protoc-gen-go is required" >&2; exit 2; }
command -v protoc-gen-connect-go >/dev/null || { echo "check-generated: protoc-gen-connect-go is required" >&2; exit 2; }

go test ./internal/orchestrator -run '^TestOpenAPIDocIsNotStale$'
(cd proto && buf generate)
git diff --exit-code -- pkg/transport/gen
