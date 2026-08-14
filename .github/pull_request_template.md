<!--
The PR is the atomic unit: spec §§ + code + tests + the ruling that closes the issue.

Why this exists (ADR-0009 / ADR-0011): through M1–M8 the spec ran ahead of the code by
design. The engine is substantially built now, so a spec change almost always implies a
code change — and a ruling that lands as spec alone reads as done from the two repos
that consume this one.

A PR may close several issues, or none. The invariant is only that main moves through
something reviewable.

Branch protection is unavailable on a private repo on this plan, so nothing here is
mechanically required. The pre-push hook is the enforcement; these three fields are what
a reviewer — or the next round, months later — cannot reconstruct from the diff.
-->

Closes #
<!-- Zero or more. Several related issues in one PR is fine and often right:
     two asks landing on the same substrate should be ruled together, not twice. -->

## §§ touched

<!--
List them, e.g. §4.3, §5.B, D5.3. This is the dual of an ask's `Hangs on:` line: it is
what lets a later round find the argument from the spec side. Write "none" for a
code-only change.
-->

## Substrate run

<!--
The SHA of the recorded run in docs/substrate-runs/ that covers this branch — not
"ran" or "green". A SHA is checkable and a claim is not, which is how the previous
record rotted into a false claim while looking like evidence.

Record one with scripts/record-substrate-run.sh. If the covering run is an ancestor
rather than this exact tree, say so: the CI job accepts that only when substrategate
reports every commit since as not needing the substrate (ADR-0005).

"CI is green" is not an answer. The cheap gate cannot speak for anything on the
substrate path, and substrate-coverage only checks that evidence exists.
-->

## Ruling

<!--
For a PR closing an inbound ask: the ruling, in the form the consumer needs — granted /
refused / duplicate, the reason, and what they must now change or must NOT change yet.
This text becomes the receipt. A bare "done" leaves them nothing to verify.

If the ruling is granted-but-not-built, say so here AND register it under Implementation
gaps, because from outside this repo a § reads as done.
-->
