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
| **M4** ✅ | Reaper + health classification (two-tier liveness × guest-side connection activity) + live periodic reconcile: `auto-pause` / `evict` / live `crash-pause` | §4.3 · §4.4-A |
| **M5** ✅ | Uniform object model on Template lineage: first-class Template rows (opaque ID + `parent` lineage, **existence-as-state**, **derived** refcount) + FK-enforced reference-reclaim atomicity + safe delete/GC + boot seed + orphan-layer sweep + Promote (multi-lowerdir freeze, bytes-first) | §3.1 · §4.3 · §5.B · §6.1 |

Acceptance for the shipped milestones lives in the substrate tests
(`test/*_substrate_test.go`), which state each assertion's property inline.

## Forward

| # | Plan | Capability delivered | Anchors |
| --- | --- | --- | --- |
| **M6** | [`M6-resource-governance.md`](./M6-resource-governance.md) | Resource governance — §3.4's limits become real enforcement. Three dimensions, three mechanisms: CPU/IO rate → cgroup; disk capacity → btrfs subvolume + squota on the Overlay upper+work; RAM → host memcg + `oom.group=1` (a **safety** limit — exceeding it terminates the sandbox, so it also needs the `resource-terminated` disposition). Plus the no-oversubscribe admission gate. Builds `internal/btrfs` as a leaf package, first consumer being the Overlay upper. | §2.3 · §3.4 · §4.3 · §5.A · §5.B |
| **M7** | *(planned)* | Volume: independent subsystem + own `volume.Backend` seam (local = btrfs subvolume; remote-FUSE deferred), mount table, exclusive lease (= refcount mount row), access modes, direct-write regime, btrfs snapshot/clone. Reuses M6's `internal/btrfs` package unchanged. | §5.C · volume D1–D8 |
| **M8** | *(planned)* | Network egress data plane + layered deny-set + credential injection (**security-critical**) | §5.D · §2.4 |
| **M9** | *(planned)* | Credential store seam | §5.D |

The §6 API / CLI contract grows alongside from M1 — it is not a separate
milestone.

**Why this order.** M1 proved the five hardest joints coexist on the real
substrate before anything layered on; M2 hardened the correctness invariants M1
stubbed (in-memory store); M3 made the headline "multi-connection concurrent,
shared FS/network" real. M4's reaper had to follow transport: the §4.3 `idle`
state and TTL-evict are defined against a **connection-activity signal** ("all
connections idle — no process activity, no pty I/O"), which only exists once the
transport surface is real (PTY, `Connect`, multi-connection fan-out) — so the
reaper landed on real signals and folded in the periodic reconcile from M2.

**Templates (M5) came before Volume** because Templates faced the hardest
object-model case directly: `create-from-Template` was an *unprotected reference*
(Template was a bare directory — no row / refcount / lineage), so M5 poured the
**uniform object model + reference-reclaim atomicity** foundation (§4.3) that
Volume (M7) reuses — its exclusive lease *is* a refcount mount row (§5.C).

**Resource governance (M6) comes before Volume** for three reasons:

- **§2.3 asserts an invariant nothing currently enforces.** "A guest's `dd` only
  makes its own `write()` fail, it cannot fill the host disk" holds only with a
  capacity cap, and §2.3 itself notes this dimension is *not* guarded by pillar ①
  (syscall interception virtualizes syscalls, not resource usage) — so one legal
  `write()` breaches it today. By contrast §5.D's gap is not a hole: the netns has
  one /30 veth pair and no default route, so egress is already deny-all, and the
  network milestone *opens* a controlled path rather than closing a leak.
- **The dependency runs this way.** Volume needs subvolumes (for cheap snapshots
  and its own cap), but subvolume+quota does not need Volume — and the Overlay
  upper is already a consumer, one per sandbox. So the shared primitive
  (`internal/btrfs`) lands with its first consumer here, and Volume imports it
  unchanged.
- **Risk separation.** Volume is the largest rung (new subsystem + new seam + new
  store table + new uniqueness invariant + six mediator hooks + snapshot/clone +
  §6 endpoints). Proving the capacity primitive against the simpler consumer
  first — single writer, no shared-rw, no snapshots — keeps two independent risk
  sources off one review gate.

The security-critical egress work (M8) still lands only after the lifecycle +
storage + transport foundation is solid.
