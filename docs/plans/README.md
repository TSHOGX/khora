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

**M1–M4 shipped** — their plans have expired (per expire-as-shipped). What they
built is now part of the converged architecture; read `docs/spec/`, not a plan:

| # | Capability delivered | Where it lives now |
| --- | --- | --- |
| **M1** ✅ | Single sandbox `create / pause / resume / destroy` end-to-end; CAS + transition-lock shape, cold-start failure unwind | §3.3 · §4 · §5.A · §5.B · §5.E |
| **M2** ✅ | Persistent store + the three §4.4 invariants in depth: atomic commit + outbox, true-concurrency stress, startup crash-reconciliation (backend = ADR-0004) | §4.4 · §5.A |
| **M3** ✅ | Transport depth (§5.E): PTY + multi-connection fan-out + `Connect` reconnect + bounded ring-replay + the two D4.3 fan-out invariants; external data-plane Connect projection (D6) | §5.E · §3.2 · §6 |
| **M4** ✅ | Reaper + health quadrant (two-tier liveness × guest-side connection activity) + live periodic reconcile: `auto-pause` / `evict` / live `crash-pause` | §4.3 · §4.4-A |

Acceptance for the shipped milestones lives in the substrate tests
(`test/*_substrate_test.go`), which state each assertion's property inline.

## Forward

| # | Plan | Capability delivered | Anchors |
| --- | --- | --- | --- |
| **M5** | *(planned)* | Templates: first-class Template rows (lifecycle + refcount + lineage) + reference-reclaim atomicity + Promote + refcount GC | §5.B · §4.3 |
| **M6** | *(planned)* | Volume: mount table, exclusive lease (= refcount mount row), direct-write regime | §5.C |
| **M7** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M8** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order.** M1 proved the five hardest joints coexist on the real
substrate before anything layered on; M2 hardened the correctness invariants M1
stubbed (in-memory store); M3 made the headline "multi-connection concurrent,
shared FS/network" real. M4's reaper had to follow transport: the §4.3 `idle`
quadrant and TTL-evict are defined against a **connection-activity signal** ("all
connections idle — no process activity, no pty I/O"), which only exists once the
transport surface is real (PTY, `Connect`, multi-connection fan-out) — so the
reaper landed on real signals and folded in the periodic reconcile from M2.

**Templates (M5) come before Volume** because Templates face the hardest
object-model case directly: `create-from-Template` is an *unprotected reference*
today (Template is a bare directory — no row / refcount / lineage), so M5 pours
the **uniform object model + reference-reclaim atomicity** foundation (§4.3) that
Volume (M6) then reuses — its exclusive lease *is* a refcount mount row (§5.C).
The security-critical egress work (M7) lands only after the lifecycle + storage +
transport foundation is solid.
