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
| **M2** ✅ | [`M2-persistent-store-invariants.md`](./M2-persistent-store-invariants.md) | Persistent store + the three §4.4 invariants in **depth**: atomic commit + outbox, true concurrency stress, startup crash-reconciliation (backend = ADR-0004; periodic reconciliation deferred to M4) | §4.4 |
| **M3** ✅ | [`M3-transport-depth.md`](./M3-transport-depth.md) | Transport depth (§5.E): PTY + multi-connection fan-out + `Connect` reconnect + bounded ring-replay + the two D4.3 fan-out invariants; external data-plane Connect projection (D6). fs breadth (list/stat/mkdir/…, multipart) is a small follow-on | §5.E · §3.2 · §6 |
| **M4** | *(next)* | Reaper + health quadrant (two-tier liveness × connection activity) + periodic reconcile | §4.3 · §4.4-A |
| **M5** | *(planned)* | Volume: mount table, exclusive lease, direct-write regime | §5.C |
| **M6** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M7** | *(planned)* | Promote + layered Templates + refcount GC | §5.B · §4.3 |
| **M8** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order.** M1 proves the five hardest joints coexist on the real
substrate before anything is layered on. M2 hardens the correctness invariants
M1 stubbed (in-memory store).

**Transport (M3) precedes reaper (M4)** because the health model consumes
transport's signals. The §4.3 `idle` quadrant and TTL-evict are defined in terms
of a **connection-activity signal** ("all connections idle — no process activity,
no pty I/O"), which only exists once the transport surface is real: PTY,
`Connect`, and multi-connection fan-out. Transport depth is also the still-stubbed
**product atom** and carries its own risk-first concurrency invariants (D4.3
fan-out: source never blocks on a slow subscriber, bounded ring replay), and it
makes §1.2/§3.2's headline "multi-connection concurrent, shared FS/network"
demonstrable. So M4 reaper lands on real signals, and folds in the periodic
reconcile deferred from M2.

M5–M8 each add one subsystem/capability on already-verified seams. The
security-critical egress work (M6) lands only after the lifecycle + storage +
transport foundation is solid.
