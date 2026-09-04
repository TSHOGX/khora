# Runtime internals

## Provider contract

`runtime.Provider` materializes and manages a live Placement from portable policy:
resource limits, a rootfs source handle, mounted Volume handles, and abstract network
rules. It returns a Placement handle, endpoint, health, and abstract death cause.
The seam does not expose cgroup paths, namespace descriptors, or general host paths.

The interface and local handle are in
[internal/runtime/provider.go](../../internal/runtime/provider.go). Structural seam
tests inject plausible host-shaped fields and signatures to prove the guard can fail.

The shipped assembly is single-node and uses the local runsc provider. The seam
remains internal; no remote transport or compatibility layer is part of this
runtime contract.

## Local runsc Placement

The local provider creates namespaces, cgroup controls, veth topology, OCI config,
and runsc/gofer processes. Network rules are installed after Placement-specific
names are known and before guest execution is admitted. Adopt reconstructs handles
for surviving runsc units after orchestrator restart.

Launch failures must remove every artifact created before the failure. Unit tests
exercise command construction and handle cleanup; real ordering is exercised by
[internal/runtime/launchfail_substrate_test.go](../../internal/runtime/launchfail_substrate_test.go)
and `local_runsc_substrate_test.go`.

## nftables constraints

Per-Sandbox base chains use accept policy plus explicit narrowed terminal rules.
`policy drop` is forbidden because base chains on one hook are not isolated: a drop
verdict in one table terminates traversal and can kill another Sandbox's traffic.

Interface matching uses `iifname`/`oifname`, not a frozen interface index. A same-name
veth rebuild otherwise leaves the old index in the loaded rule while listing may
still render a misleading name. Rules must be narrowed; an unqualified accept or
drop affects traffic owned by other tables.

[internal/runtime/nftables_test.go](../../internal/runtime/nftables_test.go) contains
negative fixtures for all three shapes, and
[test/network_substrate_test.go](../../test/network_substrate_test.go) carries the
corresponding cross-Sandbox counterexamples.
