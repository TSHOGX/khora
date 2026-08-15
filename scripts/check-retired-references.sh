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

# Technical sources must state the constraint where it is enforced. They may not
# point at a section, ADR or retired documentation path that can move independently.
if scan_files technical \
  'ADR-[0-9]+|§[0-9]+|[①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳㉑㉒㉓㉔㉕㉖㉗㉘㉙㉚㉛㉜㉝㉞㉟㊱㊲㊳㊴㊵㊶㊷㊸㊹㊺㊻㊼㊽㊾㊿⓪]|(^|[^[:alnum:]_])D[0-9]+([.][0-9A-Za-z]+)*([^[:alnum:]_]|$)|(^|[/`(])(architecture|model|subsystems|contract|reaper-health|rootfs-overlay)\.md([^[:alnum:]_]|$)|docs/(spec|decisions)/[^[:space:]`\)]+'; then
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
