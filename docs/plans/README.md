# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`). Each names the **capability**
> it delivers, not the technique it uses. Plans are forward-looking and
> **expire as shipped** — once a milestone lands and passes its review gate, its
> plan is deleted, not archived: the living truth of "what the engine is" moves
> to `docs/spec/`, and the milestone survives only as this one-line ladder entry.
>
> Discipline (one at a time, review gate, no pulling work forward): `CLAUDE.md` →
> Milestone discipline.

## Delivered

**M1–M6 shipped**, plans expired. What they built is the converged architecture —
read `docs/spec/`, not a plan. Acceptance lives in the substrate tests
(`test/*_substrate_test.go` and `internal/*/*_substrate_test.go`), each assertion
stating the property it proves inline.

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** | Single sandbox `create / pause / resume / destroy` end-to-end | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** | Persistent store + the three §4.4 invariants: atomic commit + outbox, true-concurrency, crash-reconciliation (backend = ADR-0004) | §4.4 · §5.A |
| **M3** | Transport depth: PTY, multi-connection fan-out, `Connect` reconnect, bounded ring-replay, external data-plane projection | §5.E · §3.2 · §6 |
| **M4** | Reaper + health classification + live periodic reconcile: `auto-pause` / `evict` / `crash-pause` | §4.3 · §4.4-A |
| **M5** | Templates as first-class rows: `parent` lineage, existence-as-state, derived refcount, reference-reclaim atomicity, GC, boot seed, Promote | §3.1 · §4.3 · §5.B · §6.1 |
| **M6** | Resource governance: §3.4's three axes enforced (cgroup CPU · btrfs squota disk · host memcg RAM), no-oversubscribe admission, `PATCH` as the Binding write point, `internal/btrfs` | §2.3 · §3.4 · §4.1 · §4.3 · §5.A · §5.B · §6.1 |

## Forward

| # | Plan | Capability delivered | Anchors |
| --- | --- | --- | --- |
| **M7** | *(planned)* | Volume: independent subsystem + own `volume.Backend` seam (local = btrfs subvolume; remote-FUSE deferred), mount table, exclusive lease (= refcount mount row), access modes, direct-write regime, btrfs snapshot/clone. Reuses M6's `internal/btrfs` package unchanged. | §5.C · volume D1–D8 |
| **M8** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M9** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order, forward.** M7 is the largest remaining rung — new subsystem +
new seam + new store table + a new uniqueness invariant (exclusive lease) +
mediator hooks + snapshot/clone + §6 endpoints. It inherits the M5 object model
and imports `internal/btrfs` unchanged, so no new storage primitive lands with
it. M8 is security-critical and lands only on top of a solid lifecycle +
storage + transport foundation.
