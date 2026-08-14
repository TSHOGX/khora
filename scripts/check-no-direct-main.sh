#!/usr/bin/env bash
# Refuse a direct push to main. Reads the push plan from stdin, as pre-push hooks do.
#
# THE ONE INVARIANT: **main moves only through a reviewable unit.** Not "one PR per
# issue" — an issue and a change are many-to-many, and a ruling that changes nothing in
# the tree needs no PR at all. What this forbids is narrower and is the only part that
# cannot be recovered after the fact: a change reaching main with nothing to look at.
#
# This is a local safeguard. It is bypassable with --no-verify and cannot determine
# whether a remote PR exists or was reviewed.
#
# Escape, for the cases that genuinely are not reviewable work:
#   ALLOW_MAIN_PUSH=<reason> git push origin main
# The reason is printed and appears in the shell history rather than vanishing.
#
# NOTE ON SCOPE: this deliberately does not check that the pushed branch has a PR, or
# that the PR was reviewed. A hook cannot know either at push time, and pretending
# otherwise would build the kind of gate that reports green while checking nothing —
# five of those turned up in one week here. It checks the one thing it can see.
set -uo pipefail

reason="${ALLOW_MAIN_PUSH:-}"
protected="${PROTECTED_REF:-refs/heads/main}"
blocked=0

while read -r _local_ref _local_sha remote_ref _remote_sha; do
  [[ -z "${remote_ref:-}" ]] && continue
  [[ "$remote_ref" != "$protected" ]] && continue
  if [[ -n "$reason" ]]; then
    echo "check-no-direct-main: allowed by declaration — $reason"
    continue
  fi
  blocked=1
done

if [[ "$blocked" -eq 1 ]]; then
  cat <<'MSG'
check-no-direct-main: FAIL — direct push to main.

  main moves only through a reviewable PR. Instead:

    git switch -c <topic>
    git push -u origin <topic>
    gh pr create --fill        # the template asks for issue and verification details

  A PR may close several issues, or none. What it may not do is skip existing.

  If this genuinely is not reviewable work — a typo, a local-only fixup — say so:

    ALLOW_MAIN_PUSH="fixing a broken link" git push origin main
MSG
  exit 1
fi

echo "check-no-direct-main: ok"
exit 0
