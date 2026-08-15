#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/docs/spec" "$tmp/internal"
touch "$tmp/docs/spec/retired.md"
printf '%s\n' 'package p' '// §1 is an old address' > "$tmp/internal/example.go"

if "$script_dir/check-retired-references.sh" "$tmp" >/dev/null 2>&1; then
  echo "check-retired-references accepted a prohibited fixture" >&2
  exit 1
fi

echo "check-retired-references rejected the prohibited fixture"
