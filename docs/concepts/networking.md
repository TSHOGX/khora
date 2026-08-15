# Network declaration

Every Sandbox stores one complete network declaration. Omitting it at creation
normalizes to a fail-closed declaration with default-deny egress and empty lists.
Supplying a declaration without `egress.default` is invalid.

## Declaration shape

The declaration has four collections or policy fields:

- `egress.default`: `allow` or `deny` for traffic unmatched by another rule;
- `egress.allow[]`: destination CIDR, optional port, and explicit `tcp` or `udp`;
- `egress.via[]`: TCP matches steered to a host-local second-hop endpoint;
- `ingress.publish[]`: guest TCP or UDP ports exposed through allocated host
  endpoints.

Domain names are not accepted. Resolve names outside the engine and submit IP
prefixes. `via[]` steers only TCP; it does not run, authenticate, or observe the
second-hop service. The exact record types and normalization are in
[internal/model/model.go](../../internal/model/model.go); validation is in
[internal/network/validate.go](../../internal/network/validate.go).

## Enforcement

The network package converts a declaration into placement-free abstract rules.
The runtime provider fills in Placement facts and installs nftables rules before
the isolated unit is allowed to run. Rules bind to the host-side veth interface,
not a guest-controlled source address. Both egress hops are restricted: the
per-sandbox table selects and steers traffic, while an engine-level table limits
traffic to declared second-hop endpoints.

Pause removes the Placement and its rules. Resume and startup adoption rebuild
rules from the stored declaration. Structural rule-derivation tests live in
[internal/network/generate_test.go](../../internal/network/generate_test.go), and
real isolation behavior is exercised by
[test/network_substrate_test.go](../../test/network_substrate_test.go).

## Updates

Egress is replaced whole while paused; there is no field-level merge. Publishing
a port is additive and idempotent. It can install immediately for a running
Sandbox or persist for the next resume when paused. See
[Networking guide](../guides/networking.md).
