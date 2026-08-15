#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

check_rejected() {
  name=$1
  text=$2
  root="$tmp/$name"
  mkdir -p "$root/docs" "$root/internal"
  printf '%s\n' "$text" > "$root/docs/page.md"
  if "$script_dir/check-retired-references.sh" "$root" >/dev/null 2>&1; then
    echo "check-retired-references accepted prohibited fixture: $name" >&2
    exit 1
  fi
}

check_source_rejected() {
  name=$1
  text=$2
  root="$tmp/$name"
  mkdir -p "$root/docs" "$root/internal"
  printf '%s\n' '# Clean page' > "$root/docs/page.md"
  printf '%s\n' 'package p' "// $text" > "$root/internal/example.go"
  if "$script_dir/check-retired-references.sh" "$root" >/dev/null 2>&1; then
    echo "check-retired-references accepted prohibited source fixture: $name" >&2
    exit 1
  fi
}

tree="$tmp/retired-tree"
mkdir -p "$tree/docs/spec" "$tree/internal"
touch "$tree/docs/spec/retired.md"
if "$script_dir/check-retired-references.sh" "$tree" >/dev/null 2>&1; then
  echo "check-retired-references accepted prohibited fixture: retired-tree" >&2
  exit 1
fi

check_rejected section-symbol 'See §12 for the rule.'
check_rejected adr-address 'See ADR-12 for the rule.'
check_rejected d-address 'The counterexample is in D4.3.'
check_rejected circled-address 'See item ⑧ for the rule.'
check_rejected numbered-heading '## 6.1 Resources and endpoints'
check_rejected old-page 'See ../concepts/model.md for the object model.'
check_rejected retired-path-reference 'See docs/spec/resources.md for the rule.'
check_source_rejected source-section-address 'See §12 for the rule.'
check_source_rejected source-old-page 'See concepts/model.md for the object model.'
check_source_rejected source-retired-path 'See docs/decisions/network.md for the rule.'

ignored="$tmp/ignored-fixture"
mkdir -p "$ignored/docs" "$ignored/internal"
printf '%s\n' 'docs/' > "$ignored/.ignore"
printf '%s\n' 'See §12 for the rule.' > "$ignored/docs/page.md"
if "$script_dir/check-retired-references.sh" "$ignored" >/dev/null 2>&1; then
  echo "check-retired-references accepted a prohibited fixture hidden by .ignore" >&2
  exit 1
fi

clean="$tmp/clean"
mkdir -p "$clean/docs" "$clean/internal"
printf '%s\n' '# Object model' '```sh' '# 1. This is a shell comment.' '```' \
  'See [Objects](objects.md).' > "$clean/docs/page.md"
printf '%s\n' 'package p' '// The model package has no I/O dependencies.' > "$clean/internal/example.go"
"$script_dir/check-retired-references.sh" "$clean" >/dev/null

echo "check-retired-references rejected every prohibited fixture"
