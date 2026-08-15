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

# Technical sources must state the constraint where it is enforced. They may not
# point at a section, ADR or retired documentation path that can move independently.
if rg -n --hidden --glob '!.git/**' --glob '!*.md' \
  --glob '!scripts/check-retired-references.sh' \
  --glob '!scripts/test-check-retired-references.sh' \
  'ADR-[0-9]+|§[0-9]+|[①-⑳㉑-㉟㊱-㊿⓪]|(^|[^[:alnum:]_])D[0-9]+([.][0-9A-Za-z]+)*([^[:alnum:]_]|$)|(^|[/`(])(architecture|model|subsystems|contract|reaper-health|rootfs-overlay)\.md([^[:alnum:]_]|$)|docs/(spec|decisions)/[^[:space:]`\)]+' "$root"; then
  echo "technical source contains a section, ADR or retired documentation address" >&2
  fail=1
fi

# Maintained documentation uses descriptive headings and named concepts. D-prefixed
# spec addresses, numbered headings and the mechanically migrated page names would
# otherwise recreate the retired section map under new paths.
if (cd "$root" && rg -n --glob '*.md' --glob '!CLAUDE.md' --glob '!CHANGELOG.md' \
  --glob '!scripts/check-retired-references.sh' --glob '!scripts/test-check-retired-references.sh' \
  'ADR-[0-9]+|§[0-9]+|[①-⑳㉑-㉟㊱-㊿⓪]|(^|[^[:alnum:]_])D[0-9]+([.][0-9A-Za-z]+)*([^[:alnum:]_]|$)|(^|[/`(])(architecture|model|subsystems|contract|reaper-health|rootfs-overlay)\.md([^[:alnum:]_]|$)' \
  .); then
  echo "maintained documentation contains a retired section or page address" >&2
  fail=1
fi

# Shell comments inside fenced examples may begin with "# 1." without being a
# Markdown heading. Parse fences so the guard rejects only numbered headings.
numbered=0
while IFS= read -r -d '' file; do
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
done < <(find "$root" -path "$root/.git" -prune -o -type f -name '*.md' \
  ! -name 'CLAUDE.md' ! -name 'CHANGELOG.md' -print0)
if [[ "$numbered" -ne 0 ]]; then
  echo "maintained documentation contains a numbered heading" >&2
  fail=1
fi

exit "$fail"
