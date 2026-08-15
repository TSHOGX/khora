#!/usr/bin/env bash
set -euo pipefail

root=${1:-$(git rev-parse --show-toplevel)}
fail=0

if [[ -e "$root/docs/spec" || -e "$root/docs/decisions" ]]; then
  echo "retired documentation tree is present" >&2
  fail=1
fi

if rg -n --hidden --glob '!.git/**' --glob '!scripts/check-retired-references.sh' \
  --glob '!scripts/test-check-retired-references.sh' \
  'docs/spec|docs/decisions' "$root"; then
  echo "retired documentation path is referenced" >&2
  fail=1
fi

# Technical code comments must state the constraint where it is enforced. They may
# not point at a section, ADR or retired documentation path that can move independently.
if rg -n --hidden --glob '!.git/**' --glob '!docs/**' --glob '!README.md' \
  --glob '!CLAUDE.md' --glob '!CHANGELOG.md' --glob '!scripts/check-retired-references.sh' \
  --glob '!scripts/test-check-retired-references.sh' \
  'ADR-[0-9]+|§[0-9]+|docs/(spec|decisions)/[^[:space:]`\)]+' "$root"; then
  echo "technical source contains a section, ADR or retired documentation address" >&2
  fail=1
fi

exit "$fail"
