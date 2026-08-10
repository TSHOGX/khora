# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`), named for the **capability** it
> delivers, not the technique it uses. Plans are forward-looking and **expire as
> shipped**: once a milestone passes its review gate, the living truth of "what the
> engine is" moves to `docs/spec/`, and the milestone survives here as one line.
>
> Discipline (one at a time, review gate, no pulling work forward): `CLAUDE.md` →
> Milestone discipline.

## Delivered

**M1–M8 shipped.** What they built is the converged architecture — read
`docs/spec/`, not a plan. Acceptance lives in the substrate tests
(`test/*_substrate_test.go`, `internal/*/*_substrate_test.go`), each assertion
stating inline the property it proves; invocations in [`README.md`](../../README.md).

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** | Single sandbox `create / pause / resume / destroy` end-to-end | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** | Persistent store + the three §4.4 invariants: atomic commit + outbox, true-concurrency, crash-reconciliation | §4.4 · §5.A · ADR-0004 |
| **M3** | Transport depth: PTY, multi-connection fan-out, `Connect` reconnect, bounded ring-replay, the §6 outer-edge projection (control-plane REST + Connect-RPC) | §3.2 · §5.E · §6 |
| **M4** | Reaper + health classification + live periodic reconcile: `auto-pause` / `evict` / `crash-pause` | §4.3 · §4.4-A |
| **M5** | Templates as first-class rows: `parent` lineage, existence-as-state, derived refcount, reference-reclaim atomicity, GC, boot seed, Promote | §3.1 · §4.3 · §5.B · §6.1 |
| **M6** | Resource governance: §3.4's three axes enforced (cgroup CPU · btrfs squota disk · host memcg RAM), no-oversubscribe admission, `PATCH` as the Binding write point | §2.3 · §3.4 · §4.1 · §4.3 · §5.A · §5.B · §6.1 |
| **M7** | Volume as a first-class object: the §5.C subsystem + its own `volume.Backend` seam, the mount table carrying the exclusive lease, three access modes, the volume half of the admission gate, the cap write point, snapshot/clone with `parent` lineage, direct `import`/`export`, and two reclaim faces | §3.4 · §4.3 · §5.C · §6.1 · [`volume.md`](../spec/deep-dives/volume.md) |
| **M8** | Reachability that is **enforced**: the §5.D subsystem holding the four-field declaration and generating the rule set, the §5.A rendering + install atomic with placement, the L0 floor and the deny-safe tier, `via[]` steering with its second hop pinned on `output` by a deployment-supplied uid, `publish` as the explicit inbound exception, the rule set as crash-surviving host state in reconciliation, and the declaration 400 classes | §2.1 ③④ · §2.4 · §5.D · §6.1 · §6.2 + [`network.md`](../spec/deep-dives/network.md) |

## Forward

> **None.** M8 was the last rung and every milestone has shipped, so the living
> truth of "what the engine is" is `docs/spec/` in full, with no forward plan
> beside it. The one open implementation gap is registered in
> [`open-decisions.md`](../spec/open-decisions.md) §4, not here.

Nothing follows because the remaining candidates are non-goals, not unbuilt rungs
— credentials being the one that looks like a milestone and is not (§1.4 states
why, and what the engine owes a credential proxy instead). The §6 API / CLI
contract was never a milestone of its own: it grew alongside from M1.
