# Network internals

## Pure derivation and provider completion

The network package validates a stored declaration and derives placement-free rules
from it. Runtime supplies veth names, guest addresses, allocated host ports, and
second-hop ownership, then installs one nftables transaction before guest execution.
This split keeps network and runtime from importing each other; shared rule vocabulary
lives in the leaf package `internal/ruleset`.

Validation classes are in
[internal/network/validate.go](../../internal/network/validate.go). Rule ordering and
counter attribution are in `generate.go`, with negative and structural tests beside
both files.

## Cross-Sandbox floor

Guest root can add a source address, so source-IP matching is not an isolation
boundary. Rules bind to the host-side veth name. The floor rejects loopback,
link-local, sibling Sandbox addresses, and other protected host surfaces before a
declaration allow can widen them.

Per-Sandbox base chains cannot use drop policy or unqualified verdicts. nftables
evaluates all base chains at a hook, and a terminal verdict can affect traffic associated
with another table. Unit guards are in
[internal/runtime/nftables_test.go](../../internal/runtime/nftables_test.go); the
counterexamples run on real namespaces in
[test/network_substrate_test.go](../../test/network_substrate_test.go).

## Reject versus drop

For guest-initiated TCP to a forbidden address, the floor uses reject so the caller
fails promptly. The runsc substrate reproduced roughly 2 ms for reject while drop
remained pending for tens of seconds. Both prevent delivery; only reject meets the
observable fail-fast behavior. External unauthenticated ingress still uses drop.

## Second-hop enforcement

`via[]` DNAT occurs in the Sandbox table. A separate engine-level output rule limits
the second-hop process, identified by configured UID, to declared destinations. The
second hop does not traverse the original forward hook, so first-hop rules alone
cannot constrain it. Assembly refuses a second-hop UID equal to the engine process or
rules that would cut off engine-to-guest transport.

The deny-safe tier is settled: `ruleset.TierV4` contains RFC1918's three IPv4
blocks, and `ruleset.TierV6` contains `fc00::/7` plus mechanically derived NAT64
images of those IPv4 blocks. The ruleset tests pin those members and reject
nesting with the unliftable floor.

## Atomic reinstall and counters

Hot publish and adoption replace a table atomically. Delete-then-install was measured
to kill live flows; atomic replacement preserved them. Reinstall resets counters.
Nonzero proves a rule acted in the current Placement, while zero does not prove it
never acted across earlier Placements. A `via` match counter records steering, not
acceptance by the external endpoint.
