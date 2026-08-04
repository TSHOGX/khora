# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`). Each names the **capability**
> it delivers, not the technique it uses. Plans are forward-looking and
> **expire as shipped** — once a milestone lands and passes its review gate, the
> living truth of "what the engine is" moves to `docs/spec/` and the milestone
> survives only as its one-line ladder entry here.
>
> Discipline (one at a time, review gate, no pulling work forward): `CLAUDE.md` →
> Milestone discipline.

## Delivered

**M1–M6 shipped**, plans expired. What they built is the converged architecture —
read `docs/spec/`, not a plan. Acceptance lives in the substrate tests
(`test/*_substrate_test.go` and `internal/*/*_substrate_test.go`), each assertion
stating the property it proves inline — one of them, `pause_drain`, additionally
needs the `drainrace` tag, so the routine gate does not cover it (see `README.md`).

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** | Single sandbox `create / pause / resume / destroy` end-to-end | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** | Persistent store + the three §4.4 invariants: atomic commit + outbox, true-concurrency, crash-reconciliation (backend = ADR-0004) | §4.4 · §5.A |
| **M3** | Transport depth: PTY, multi-connection fan-out, `Connect` reconnect, bounded ring-replay, external data-plane projection | §5.E · §3.2 · §6 |
| **M4** | Reaper + health classification + live periodic reconcile: `auto-pause` / `evict` / `crash-pause` | §4.3 · §4.4-A |
| **M5** | Templates as first-class rows: `parent` lineage, existence-as-state, derived refcount, reference-reclaim atomicity, GC, boot seed, Promote | §3.1 · §4.3 · §5.B · §6.1 |
| **M6** | Resource governance: §3.4's three axes enforced (cgroup CPU · btrfs squota disk · host memcg RAM), no-oversubscribe admission, `PATCH` as the Binding write point, `internal/btrfs` | §2.3 · §3.4 · §4.1 · §4.3 · §5.A · §5.B · §6.1 |
| **—** | *Post-M6 hardening (no rung, shipped):* the complete §5.E fs surface + streaming large-file bypass, the §4.2 pause drain barrier, `cpu.weight` beside `cpu.max`, two structural guards (`internal/arch` — subsystems never import each other — with `internal/guestport` as the shared leaf that keeps its allowlist empty), and three closures: the §4.4-B transition-lock refcount, Promote through the §3.4 admission inequality, and PID 1's single-waiter reap table + namespace-wide signal broadcast | §3.4 · §4.2 · §4.4-B · §5.A · §5.E · §6.1 |
| **—** | *Pre-M7 hardening (no rung, shipped):* the event stream's outward face — the `AUTOINCREMENT` `seq` cursor with the store's total-order + no-visibility-gap contract (adversarial contract case included), `store.Publisher`'s poison-envelope exit + capped jittered backoff + off-loop heartbeat metrics, and `GET /v1/events` (pull only, hard `limit` cap, 410 as reserved semantics); plus `death_cause` on the §6.1 read surface, `SendSignal` refusing non-directed targets, the barrier's reclaim invariant pinned by test, and disk MAJ:MIN settled as provider-derived under a seam-token guard | §4.3 · §4.4-A · §4.4-C · §5.A · §6.1 |

## Forward

| # | Plan | Capability to deliver | Anchors |
| --- | --- | --- | --- |
| **M7** | `M7-volume.md` | Volume: independent subsystem + own `volume.Backend` seam (local = btrfs subvolume; remote-FUSE deferred), mount table, exclusive lease (= refcount mount row), access modes, direct-write regime, btrfs snapshot/clone. Reuses `internal/btrfs` unchanged. | §5.C · volume D1–D8 |
| **M8** | `M8-network.md` | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M9** | `M9-credential-store.md` | Credential store seam | §5.D |

(No plan file is written yet — the name is the one the plan will take.)

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order, forward.** M7 is the largest remaining rung — new subsystem + new
seam + new store table + a new uniqueness invariant (exclusive lease) + mediator hooks
+ snapshot/clone + §6 endpoints — yet it needs no new storage primitive. M8 is
security-critical, so it lands only on a settled lifecycle + storage + transport
foundation.
