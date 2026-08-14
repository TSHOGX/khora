#!/usr/bin/env bash
# Assert that a recorded substrate run COVERS HEAD.
#
# This replaces a `substrate` CI job that targeted [self-hosted, linux, btrfs, runsc]
# with no registered runner, so it sat `queued` forever and never reported. Per
# scripts/check-asks.sh: a gate that cannot fail is worse than no gate, because it is
# believed.
#
# A hosted runner cannot run the suite — no root for --network=sandbox, no runsc, no
# btrfs. What it CAN do is verify that a covering run exists, which is a real
# assertion with a real failure mode. Two conditions, both mechanical:
#
#   ① The recorded SHA is an ANCESTOR of HEAD. A record from a sibling branch says
#      nothing about this tree.
#   ② `substrategate` reports the diff from that SHA to HEAD as NOT needing the
#      substrate. That is the same tool, and the same equivalence (ADR-0005), that
#      licenses skipping the gate locally.
#
# Together: either the substrate ran on this tree, or every change since is one that
# provably cannot affect a running sandbox.
#
# This does NOT re-run the suite and cannot. It is a freshness check on evidence
# produced elsewhere — `scripts/record-substrate-run.sh` produces it.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2

DIR="${SUBSTRATE_RUNS_DIR:-docs/substrate-runs}"
HEAD_SHA="$(git rev-parse HEAD)"

if [[ ! -d "$DIR" ]]; then
  echo "check-substrate-coverage: FAIL — no $DIR/ in the tree."
  echo "  The README claims the engine is verified against the real substrate. Record a"
  echo "  run: scripts/record-substrate-run.sh"
  exit 1
fi

# Every record names its commit in a `| commit | \`<sha>\` |` row. Read the SHA from
# the CONTENT rather than the filename: a copied or renamed file would otherwise
# claim coverage it does not have.
mapfile -t recorded < <(grep -rhoE '^\| commit \| `[0-9a-f]{40}` \|' "$DIR" 2>/dev/null |
  grep -oE '[0-9a-f]{40}' | sort -u)

if (( ${#recorded[@]} == 0 )); then
  echo "check-substrate-coverage: FAIL — $DIR/ holds no record with a commit SHA."
  echo "  A record without a SHA cannot be checked for freshness, which is how the"
  echo "  previous one rotted into a false claim."
  exit 1
fi

# A record is only evidence if its run PASSED. `exit | 0` in the same file.
covering=""
for sha in "${recorded[@]}"; do
  file="$(grep -rlE "^\| commit \| \`$sha\` \|" "$DIR" | head -1)"
  [[ -n "$file" ]] || continue
  if ! grep -qE '^\| exit \| 0 \|' "$file"; then
    echo "check-substrate-coverage: skipping $file — the run it records did not exit 0"
    continue
  fi
  # ① ancestor. A missing object is not a pass: in a shallow checkout merge-base
  # cannot answer, and treating "cannot tell" as covered is the failure this exists
  # to prevent. The workflow checks out with fetch-depth: 0.
  if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then
    echo "check-substrate-coverage: skipping $sha — not an object in this checkout"
    continue
  fi
  if ! git merge-base --is-ancestor "$sha" "$HEAD_SHA" 2>/dev/null; then
    echo "check-substrate-coverage: skipping ${sha:0:12} — not an ancestor of HEAD"
    continue
  fi
  if [[ "$sha" == "$HEAD_SHA" ]]; then
    echo "check-substrate-coverage: PASS — $file records a passing run on HEAD itself."
    exit 0
  fi
  # ② the intervening diff must not need the substrate. substrategate compares one
  # rev against its PARENT, so it cannot answer for a range; ask it per-commit and
  # require every commit since the record to be inert.
  needs=""
  while read -r c; do
    [[ -n "$c" ]] || continue
    # A MERGE commit is skipped when both its parents are already accounted for, and
    # this is a correctness fix rather than a convenience. substrategate diffs a rev
    # against its FIRST parent, which for a merge is the base branch — so it replays
    # the whole branch as ONE diff and reports every .go file in it, including files
    # the recorded run already certified commit by commit. Measured: the v0.0.1 merge
    # was flagged as needing the substrate while every commit inside it was either
    # covered by the record or individually inert.
    #
    # Safe only under the condition checked here: each parent must be reachable from
    # HEAD and either already covered by the record or itself in the walked range, so
    # `rev-list` visits the branch's own commits and classifies each on its own. A
    # merge bringing in a parent from OUTSIDE that set is NOT skipped — those commits
    # were never walked, so nothing else would ever look at them.
    # read -ra, not `for p in $parents`: this must not depend on the shell's
    # word-splitting. Under zsh an unquoted expansion does NOT split on spaces, so the
    # loop saw both parent SHAs as one string, every ancestry test failed, and the
    # merge was reported as needing the substrate. The shebang is bash, so CI was
    # correct while local verification was not — the worse direction to be wrong in,
    # since it hides in exactly the place a person checks.
    read -ra parents <<<"$(git rev-list --parents -n1 "$c" | cut -d' ' -f2-)"
    if (( ${#parents[@]} > 1 )); then
      unwalked=0
      for p in "${parents[@]}"; do
        if ! git merge-base --is-ancestor "$p" "$HEAD_SHA" 2>/dev/null; then
          unwalked=1; break
        fi
        git merge-base --is-ancestor "$p" "$sha" 2>/dev/null && continue
        if ! git rev-list "$sha..$HEAD_SHA" | grep -qF "$p"; then
          unwalked=1; break
        fi
      done
      if (( ! unwalked )); then
        continue
      fi
    fi
    if ! go run ./test/preflight/substrategate "$c" >/dev/null 2>&1; then
      needs="$needs ${c:0:12}"
    fi
  done < <(git rev-list --reverse "$sha..$HEAD_SHA")
  if [[ -z "$needs" ]]; then
    echo "check-substrate-coverage: PASS — $file records a passing run on ${sha:0:12},"
    echo "  and every commit since is one substrategate reports as not needing the substrate."
    exit 0
  fi
  covering="$covering
  ${sha:0:12} ($file): these commits since it DO need the substrate:$needs"
done

echo "check-substrate-coverage: FAIL — no recorded run covers HEAD ($HEAD_SHA)."
echo "  Records considered:$covering"
echo
echo "  Run the suite on the substrate and record it:"
echo "    scripts/record-substrate-run.sh"
echo "  A green cheap gate is not an answer here: it cannot speak for anything on the"
echo "  substrate path."
exit 1
