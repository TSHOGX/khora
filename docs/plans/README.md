# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`). Each names the **capability**
> it delivers, not the technique it uses. Plans are forward-looking and
> **expire as shipped** — a finished milestone's plan becomes a historical
> record; the living truth of "what the engine is" is `docs/spec/`.
>
> Rule of thumb: **build one, gate it (review), then write the next.** Don't pull
> later-milestone work forward (see `CLAUDE.md` → Milestone discipline).

| # | Plan | Capability delivered | Anchors |
| --- | --- | --- | --- |
| **M1** ✅ | [`M1-sandbox-lifecycle.md`](./M1-sandbox-lifecycle.md) | Single sandbox `create / pause / resume / destroy` end-to-end (walking-skeleton technique); CAS + transition-lock **shape** and cold-start failure **unwind** proven, not deep-tested | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** ✅ | [`M2-persistent-store-invariants.md`](./M2-persistent-store-invariants.md) | Persistent store + the three §4.4 invariants in **depth**: atomic commit + outbox, true concurrency stress, startup crash-reconciliation (backend = ADR-0004; periodic reconciliation deferred to M3) | §4.4 |
| **M3** | *(next)* | Reaper + health quadrant (two-tier liveness × connection activity) | §4.3 |
| **M4** | *(planned)* | Volume: mount table, exclusive lease, direct-write regime | §5.C |
| **M5** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M6** | *(planned)* | Promote + layered Templates + refcount GC | §5.B · §4.3 |
| **M7** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order.** M1 proves the five hardest joints coexist on the real
substrate before anything is layered on. M2 hardens the correctness invariants
that M1 stubbed (in-memory store). M3–M7 each add one subsystem/capability on
already-verified seams. The security-critical egress work (M5) lands only after
the lifecycle + storage foundation is solid.
