#!/usr/bin/env bash
set -euo pipefail

root=${1:-$(git rev-parse --show-toplevel)}
fail=0

maintained_files() {
  if [[ -e "$root/.git" ]]; then
    git -C "$root" ls-files --cached --others --exclude-standard -z
  else
    find "$root" -type f -print0
  fi
}

scan_files() {
  local scope=$1 pattern=$2 file rel base matched=1
  while IFS= read -r -d '' file; do
    [[ "$file" = /* ]] || file="$root/$file"
    [[ -f "$file" ]] || continue
    rel=${file#"$root"/}
    base=${rel##*/}
    case "$rel" in
      scripts/check-retired-references.sh|scripts/test-check-retired-references.sh) continue ;;
    esac
    case "$scope" in
      technical) [[ "$rel" == *.md ]] && continue ;;
      documentation)
        [[ "$rel" == *.md ]] || continue
        [[ "$base" == CLAUDE.md || "$base" == CHANGELOG.md ]] && continue
        ;;
    esac
    if grep -IEn -- "$pattern" "$file"; then
      matched=0
    fi
  done < <(maintained_files)
  return "$matched"
}

if [[ -e "$root/docs/spec" || -e "$root/docs/decisions" ]]; then
  echo "retired documentation tree is present" >&2
  fail=1
fi

if scan_files all 'docs/spec|docs/decisions'; then
  echo "retired documentation path is referenced" >&2
  fail=1
fi

# Technical sources describe the code they enforce; they must not depend on a
# Markdown page whose name or contents can change independently. These exact
# lines use Markdown paths as generator output or test data, not as citations.
mechanical_markdown_path() {
  local rel=$1 line=$2
  case "$rel" in
    cmd/khora/docs.go)
      [[ "$line" =~ ^[[:space:]]*const[[:space:]]+cliDocPath[[:space:]]*=[[:space:]]*\"docs/api/cli\.md\"[[:space:]]*$ ]]
      ;;
    cmd/orchestrator/docs.go)
      [[ "$line" =~ ^[[:space:]]*const[[:space:]]+orchestratorDocPath[[:space:]]*=[[:space:]]*\"docs/configuration/orchestrator\.md\"[[:space:]]*$ ]]
      ;;
    scripts/generate-docs.sh)
      [[ "$line" == "go run ./cmd/orchestrator __generate-docs docs/configuration/orchestrator.md" ||
        "$line" == "go run ./cmd/khora __generate-docs docs/api/cli.md" ]]
      ;;
    test/preflight/substrategate/classify_test.go)
      [[ "$line" =~ ^[[:space:]]*\"docs/operations/startup-shutdown\.md\":[[:space:]]+true,[[:space:]]*$ ]]
      ;;
    test/preflight/substrategate/main_test.go)
      [[ "$line" == $'\twriteTestFile(t, "notes.md", "uncommitted\\n")' ||
        "$line" == $'\tif got["notes.md"] != "A" {' ||
        "$line" == $'\t\tt.Errorf("untracked working-tree change status = %q, want A; all changes: %v", got["notes.md"], got)' ]]
      ;;
    *) return 1 ;;
  esac
}

technical_md=0
markdown_path='(^|[^[:alnum:]_.-])([[:alnum:]_.-]+/)*[[:alnum:]_.-]+\.md([^[:alnum:]_]|$)'
while IFS= read -r -d '' file; do
  [[ "$file" = /* ]] || file="$root/$file"
  [[ -f "$file" && "$file" != *.md ]] || continue
  rel=${file#"$root"/}
  case "$rel" in
    scripts/check-retired-references.sh|scripts/test-check-retired-references.sh) continue ;;
  esac
  while IFS= read -r match; do
    [[ -n "$match" ]] || continue
    line=${match#*:}
    mechanical_markdown_path "$rel" "$line" && continue
    printf '%s:%s\n' "$file" "$match"
    technical_md=1
  done < <(grep -IEn -- "$markdown_path" "$file" || true)
done < <(maintained_files)
if [[ "$technical_md" -ne 0 ]]; then
  echo "technical source references a Markdown document" >&2
  fail=1
fi

# Technical sources must state the constraint where it is enforced. They may not
# point at a section, ADR or retired documentation path that can move independently.
if scan_files technical \
  'ADR-[0-9]+|§[0-9]+|[①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳㉑㉒㉓㉔㉕㉖㉗㉘㉙㉚㉛㉜㉝㉞㉟㊱㊲㊳㊴㊵㊶㊷㊸㊹㊺㊻㊼㊽㊾㊿⓪]|(^|[^[:alnum:]_])D[0-9]+([.][0-9A-Za-z]+)*([^[:alnum:]_]|$)|docs/(spec|decisions)/[^[:space:]`\)]+'; then
  echo "technical source contains a section, ADR or retired documentation address" >&2
  fail=1
fi

# Maintained documentation uses descriptive headings and named concepts. D-prefixed
# spec addresses, numbered headings and the mechanically migrated page names would
# otherwise recreate the retired section map under new paths.
if scan_files documentation \
  'ADR-[0-9]+|§[0-9]+|[①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳㉑㉒㉓㉔㉕㉖㉗㉘㉙㉚㉛㉜㉝㉞㉟㊱㊲㊳㊴㊵㊶㊷㊸㊹㊺㊻㊼㊽㊾㊿⓪]|(^|[^[:alnum:]_])D[0-9]+([.][0-9A-Za-z]+)*([^[:alnum:]_]|$)|(^|[/`(])(architecture|model|subsystems|contract|reaper-health|rootfs-overlay)\.md([^[:alnum:]_]|$)'; then
  echo "maintained documentation contains a retired section or page address" >&2
  fail=1
fi

# Shell comments inside fenced examples may begin with "# 1." without being a
# Markdown heading. Parse fences so the guard rejects only numbered headings.
numbered=0
while IFS= read -r -d '' file; do
  [[ "$file" = /* ]] || file="$root/$file"
  [[ -f "$file" && "$file" == *.md ]] || continue
  case "${file##*/}" in
    CLAUDE.md|CHANGELOG.md) continue ;;
  esac
  if awk '
    /^```/ { fenced = !fenced; next }
    !fenced && /^#{1,6}[[:space:]]+[0-9]+([.][0-9A-Za-z]+)*[.)]?[[:space:]]/ {
      print FILENAME ":" FNR ":" $0
      found = 1
    }
    END { exit !found }
  ' "$file"; then
    numbered=1
  fi
done < <(maintained_files)
if [[ "$numbered" -ne 0 ]]; then
  echo "maintained documentation contains a numbered heading" >&2
  fail=1
fi

exit "$fail"
