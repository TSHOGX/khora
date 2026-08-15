#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

go run ./cmd/orchestrator __generate-docs docs/configuration/orchestrator.md
go run ./cmd/khora __generate-docs docs/api/cli.md
