#!/usr/bin/env bash
# Ask/§ consistency gate (ADR-0009).
#
# Two assertions, both mechanical — neither reads an argument, so neither can be
# argued with:
#
#   ① A register entry citing a CLOSED issue fails.
#      That state means a counterparty ruled and the register still asserts the
#      question is open — the exact drift the hand-carried channel produced, where
#      three rulings existed upstream and none had come back.
#
#   ② A push touching a § that an OPEN ask hangs on fails.
#      The moment a maintainer needs to know an ask is pending is when the § is
#      about to be edited, not when a session opens. An issue declares its § with a
#      machine-readable `Hangs on: §<n>` line; that line is this gate's input.
#
# Override for ② once the ruling has actually been read: ACKED_ASKS=12,15 git push
# There is deliberately no override for ①: a closed issue with a live register entry
# is never correct, only unfinished.
#
# Offline or unauthenticated, this SKIPS rather than fails: it must not become the
# reason a push cannot happen on a plane. CI runs it too, which is where the
# no-network case gets covered.
set -uo pipefail

REPO="${ASK_GATE_REPO:-TSHOGX/warren}"
REGISTER="${ASK_GATE_REGISTER:-docs/spec/open-decisions.md}"
BASE="${ASK_GATE_BASE:-origin/main}"

# Offline SKIPS locally and FAILS in CI. The asymmetry is the point: a hook must not be
# why a push cannot happen on a plane, but a CI job that skips reports green while
# checking nothing — a gate that cannot fail is worse than no gate, because it is
# believed. CI is exactly where nobody is watching, so there it must be loud.
skip_or_fail() {
  if [[ -n "${CI:-}" ]]; then
    echo "check-asks: FAIL — $1, and this is CI, where a skip would report green"
    echo "  Set GH_TOKEN to a token that can read the counterparty repos."
    exit 1
  fi
  echo "check-asks: $1, skipping (local)"
  exit 0
}

command -v gh >/dev/null 2>&1 || skip_or_fail "no gh"
# `gh auth status` ignores GH_TOKEN in some versions; probe the API instead, which is
# what the checks below actually depend on.
gh api /rate_limit >/dev/null 2>&1 || skip_or_fail "gh cannot reach the API"

fail=0

# ── ① register entries citing a closed issue ─────────────────────────────────
if [[ -f "$REGISTER" ]]; then
  # `#12` resolves against this repo; `warren#12` against that one. An ask is filed on
  # the RECEIVING repo, so a register citing one is nearly always cross-repo — resolving
  # every citation locally would check the wrong tracker and pass vacuously.
  cited=$(grep -oE '(\b[a-z][a-z0-9-]*)?#[0-9]+' "$REGISTER" 2>/dev/null | sort -u)
  for c in $cited; do
    n=${c##*#}
    owner=${c%%#*}
    if [[ -n "$owner" && "$owner" != "$c" ]]; then
      target="TSHOGX/$owner"
    else
      target="$REPO"
    fi
    state=$(gh issue view "$n" --repo "$target" --json state -q .state 2>/dev/null) || continue
    if [[ "$state" == "CLOSED" ]]; then
      echo "check-asks: FAIL — $REGISTER cites $target#$n, which is CLOSED."
      echo "  A ruling landed and the register still says the question is open."
      echo "  Read the ruling, move the § first, then delete the entry."
      fail=1
    fi
  done
fi

# ── ② a § with an open ask is being touched ──────────────────────────────────
changed=$(git diff --name-only "$BASE"...HEAD 2>/dev/null | grep -E '\.md$' || true)
if [[ -n "$changed" ]]; then
  # `Hangs on: §1.6` / `Hangs on: §6.2.6, §6.2.1`
  mapfile -t asks < <(gh issue list --repo "$REPO" --label ask:inbound --state open \
                        --json number,title,body \
                        -q '.[] | "\(.number)\t\(.title)\t\(.body | capture("Hangs on: (?<h>[^\n]*)") .h // "")"' 2>/dev/null)
  acked=",${ACKED_ASKS:-},"
  for row in "${asks[@]:-}"; do
    [[ -z "$row" ]] && continue
    n=${row%%$'\t'*}; rest=${row#*$'\t'}; title=${rest%%$'\t'*}; hangs=${rest##*$'\t'}
    [[ -z "$hangs" ]] && continue
    [[ "$acked" == *",$n,"* ]] && continue
    # The suffix is part of the address: §4.4-C is not §4.4, and matching the
    # shorter form would fire on every file mentioning the parent.
    for sec in $(echo "$hangs" | grep -oE '§[0-9]+(\.[0-9A-Za-z]+)*(-[0-9A-Za-z]+)?'); do
      if echo "$changed" | xargs -r grep -lF "$sec" 2>/dev/null | grep -q .; then
        echo "check-asks: FAIL — #$n is open and hangs on $sec, which this push touches."
        echo "  \"$title\""
        echo "  Read the ask before changing that §; then: ACKED_ASKS=$n git push"
        fail=1
        break
      fi
    done
  done
fi

[[ $fail -eq 0 ]] && echo "check-asks: ok"
exit $fail
