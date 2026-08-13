#!/usr/bin/env bash
# Spec/test drift gate.
#
# WHY THIS EXISTS, and why it did not before: through M1–M8 the spec legitimately ran
# ahead of the code — `docs/plans/` tracked "built vs not-yet-built" and a § describing
# an intended shape was normal. That phase is over. The engine is substantially built,
# so **a spec change now almost always implies a code change**, and "settled, not yet
# built" has turned from a normal state into a debt that reads as done from outside.
#
# The assertion: **a change under `docs/spec/` with no test touched requires an explicit
# declaration.** Deliberately coarse — it does not try to infer WHICH § a diff hunk
# belongs to, because a diff also contains cross-references to §§ it merely cites, and a
# gate that guesses either over-fires until it is disabled or under-fires silently. This
# one can only over-fire, and the escape is one line.
#
# The escape, which must be WRITTEN rather than assumed:
#
#   Spec-only: <reason>
#
# in any commit message in the pushed range, or `SPEC_ONLY=<reason>` in the environment.
# A wording fix, a corrected cross-reference, a criterion restated — all legitimate, and
# all now cost one sentence that survives in the history. That is the point: "I was only
# rewording" stops being a default and becomes a claim.
#
# What this would have caught: a change that rewrote §4.3's reclamation predicate, §5.B,
# §3.4.2 and a new deep-dive section — granting a capability the code does not have —
# across three commits touching zero test files, while the register asserted no
# implementation gaps.
#
# NOT installed in the spec-only sibling repos: with no code at all, every commit there
# is spec-only, and a gate that fires every time is one that gets switched off.
set -uo pipefail

BASE="${DRIFT_BASE:-origin/main}"

git rev-parse --verify -q "$BASE" >/dev/null 2>&1 || { echo "check-spec-drift: no $BASE, skipping"; exit 0; }

range="$BASE...HEAD"
spec_changed=$(git diff --name-only "$range" -- 'docs/spec/**.md' | grep -c . || true)
[[ "$spec_changed" -eq 0 ]] && { echo "check-spec-drift: ok (no spec change)"; exit 0; }

test_changed=$(git diff --name-only "$range" -- '**_test.go' | grep -c . || true)
[[ "$test_changed" -gt 0 ]] && { echo "check-spec-drift: ok ($spec_changed spec file(s), $test_changed test file(s))"; exit 0; }

reason="${SPEC_ONLY:-}"
[[ -z "$reason" ]] && reason=$(git log "$range" --format=%B | grep -m1 -oP '^\s*Spec-only:\s*\K.+' || true)

if [[ -n "$reason" ]]; then
  echo "check-spec-drift: ok — declared spec-only: $reason"
  exit 0
fi

cat <<MSG
check-spec-drift: FAIL — $spec_changed spec file(s) changed, no test file did.

$(git diff --name-only "$range" -- 'docs/spec/**.md' | sed 's/^/    /')

  The engine is substantially built, so a spec change usually implies a code change.
  Three ways forward, and the first two are the common ones:

    1. The code needs to change too   -> change it, with a test that cites the §.
    2. This is genuinely spec-only    -> say so, in the commit message:
                                            Spec-only: corrected a cross-reference
    3. Settled but deliberately not built
                                      -> register it under "Implementation gaps" in
                                         open-decisions.md, and declare spec-only.

  Option 3 is a debt, not a resting place: from outside this repo a § reads as done.
MSG
exit 1
