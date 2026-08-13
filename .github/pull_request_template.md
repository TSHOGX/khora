<!--
The PR is the atomic unit: spec §§ + code + tests + the ruling that closes the issue.

Why this exists (ADR-0009 / ADR-0011): through M1–M8 the spec ran ahead of the code by
design. The engine is substantially built now, so a spec change almost always implies a
code change — and a ruling that lands as spec alone reads as done from the two repos
that consume this one. One PR per issue also makes the change's size follow the issue's
size, rather than whatever felt like enough at the time.

Branch protection is unavailable on a private repo on this plan, so nothing here is
mechanically required. The pre-push hook is the enforcement; these three fields are what
a reviewer — or the next round, months later — cannot reconstruct from the diff.
-->

Closes #

## §§ touched

<!--
List them, e.g. §4.3, §5.B, D5.3. This is the dual of an ask's `Hangs on:` line: it is
what lets a later round find the argument from the spec side. Write "none" for a
code-only change.
-->

## Substrate run

<!--
Ran / not run, and if not, why. `goal.md`: "A green build proves nothing… Acceptance is
on the real substrate." The cheap gate cannot speak for anything on the substrate path,
and the only recorded run in the tree predates M10/M11 entirely — so "CI is green" is
not an answer to this field.
-->

## Ruling

<!--
For a PR closing an inbound ask: the ruling, in the form the consumer needs — granted /
refused / duplicate, the reason, and what they must now change or must NOT change yet.
This text becomes the receipt. A bare "done" leaves them nothing to verify.

If the ruling is granted-but-not-built, say so here AND register it under Implementation
gaps, because from outside this repo a § reads as done.
-->
