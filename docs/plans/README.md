# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`), named for the **capability** it
> delivers, not the technique it uses. Plans are forward-looking and **expire as
> shipped**: once a milestone passes its review gate, the living truth of "what the
> engine is" moves to `docs/spec/`, and the milestone survives here as one line.
>
> Discipline (one at a time, review gate, no pulling work forward): `CLAUDE.md` →
> Milestone discipline.

## Delivered

**M1–M7 shipped.** What they built is the converged architecture — read
`docs/spec/`, not a plan. Acceptance lives in the substrate tests
(`test/*_substrate_test.go`, `internal/*/*_substrate_test.go`), each assertion
stating inline the property it proves; invocations in [`README.md`](../../README.md).

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** | Single sandbox `create / pause / resume / destroy` end-to-end | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** | Persistent store + the three §4.4 invariants: atomic commit + outbox, true-concurrency, crash-reconciliation | §4.4 · §5.A · ADR-0004 |
| **M3** | Transport depth: PTY, multi-connection fan-out, `Connect` reconnect, bounded ring-replay, external data-plane projection | §3.2 · §5.E · §6 |
| **M4** | Reaper + health classification + live periodic reconcile: `auto-pause` / `evict` / `crash-pause` | §4.3 · §4.4-A |
| **M5** | Templates as first-class rows: `parent` lineage, existence-as-state, derived refcount, reference-reclaim atomicity, GC, boot seed, Promote | §3.1 · §4.3 · §5.B · §6.1 |
| **M6** | Resource governance: §3.4's three axes enforced (cgroup CPU · btrfs squota disk · host memcg RAM), no-oversubscribe admission, `PATCH` as the Binding write point | §2.3 · §3.4 · §4.1 · §4.3 · §5.A · §5.B · §6.1 |
| **M7** | Volume as a first-class object: the §5.C subsystem + its own `volume.Backend` seam, the mount table carrying the exclusive lease, three access modes, the volume half of the admission gate, the cap write point, snapshot/clone with `parent` lineage, direct `import`/`export`, and two reclaim faces | §3.4 · §4.3 · §5.C · §6.1 · [`volume.md`](../spec/deep-dives/volume.md) |

## Forward

| # | Plan | Capability to deliver | Anchors |
| --- | --- | --- | --- |
| **M8** | `M8-network.md` | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §2.4 · §5.D |
| **M9** | `M9-credential-store.md` | Credential store seam | §5.D |

Neither plan file exists yet — the name is the one the plan will take.

**M8 cannot be written yet.** Two design decisions are open (how the L0 floor is
earned, where the placement-derived deny-set lives), and four measured facts must be
inherited rather than rediscovered. Both live in
[`open-decisions.md`](../spec/open-decisions.md) — §2 for the decisions, §4 for the
enforcement gap and the facts. Read it first: today's cross-sandbox isolation rests
on a missing route the guest is authorized to add, not on enforcement.

**Why M8 lands last.** It is the security-critical face, so it goes on top of a
settled lifecycle + storage + transport foundation rather than beside one.

The §6 API / CLI contract grows alongside from M1 — it is not a separate milestone.
