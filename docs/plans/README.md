# Milestones — the ladder

> One plan per milestone (`M<N>-<capability>.md`). Each names the **capability**
> it delivers, not the technique it uses. Plans are forward-looking and
> **expire as shipped** — once a milestone lands and passes its review gate, its
> plan is deleted, not archived: the living truth of "what the engine is" moves
> to `docs/spec/`, and the milestone survives only as this one-line ladder entry.
>
> Rule of thumb: **build one, gate it (review), then write the next.** Don't pull
> later-milestone work forward (see `CLAUDE.md` → Milestone discipline).

## Delivered

**M1–M3 shipped** — their plans have expired (per expire-as-shipped). What they
built is now part of the converged architecture; read `docs/spec/`, not a plan:

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** ✅ | Single sandbox `create / pause / resume / destroy` end-to-end; CAS + transition-lock shape, cold-start failure unwind | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** ✅ | Persistent store + the three §4.4 invariants in depth: atomic commit + outbox, true-concurrency stress, startup crash-reconciliation (backend = ADR-0004) | §4.4 · §5.A |
| **M3** ✅ | Transport depth (§5.E): PTY + multi-connection fan-out + `Connect` reconnect + bounded ring-replay + the two D4.3 fan-out invariants; external data-plane Connect projection (D6) | §5.E · §3.2 · §6 |

Acceptance for the shipped milestones lives in the substrate tests
(`test/*_substrate_test.go`), which state each assertion's property inline.

## Forward

| # | Plan | Capability delivered | Anchors |
| --- | --- | --- | --- |
| **M4** | *(next)* | Reaper + health quadrant (two-tier liveness × connection activity) + periodic reconcile | §4.3 · §4.4-A |
| **M5** | *(planned)* | Volume: mount table, exclusive lease, direct-write regime | §5.C |
| **M6** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M7** | *(planned)* | Promote + layered Templates + refcount GC | §5.B · §4.3 |
| **M8** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order.** M1 proved the five hardest joints coexist on the real
substrate before anything layered on; M2 hardened the correctness invariants M1
stubbed (in-memory store); M3 made the headline "multi-connection concurrent,
shared FS/network" real.

**Transport (M3) preceded reaper (M4)** because the health model consumes
transport's signals: the §4.3 `idle` quadrant and TTL-evict are defined in terms
of a **connection-activity signal** ("all connections idle — no process activity,
no pty I/O"), which only exists once the transport surface is real (PTY,
`Connect`, multi-connection fan-out). So M4's reaper lands on real signals, and
folds in the periodic reconcile deferred from M2.

M5–M8 each add one subsystem/capability on already-verified seams. The
security-critical egress work (M6) lands only after the lifecycle + storage +
transport foundation is solid.
