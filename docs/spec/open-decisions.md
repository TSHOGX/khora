# Open Decisions register

> **One job: let a reader of a spec § discover that it is not settled.** Nothing here is decided —
> do not cite an entry as a conclusion.
>
> **Entries are keyed by the § they hang on.** No invented numbering: where a stable address is
> genuinely needed, the linked issue supplies one (ADR-0009). A bare `§X.Y` is a spec global
> section, located via SPEC.md's document map; `Dx` is a deep-dive section.
>
> **Three lines: what is open · the settlement criterion · what moves with it.** The argument is
> **not** here — it lives in the deep-dive that owns the topic, or in the linked issue. The
> criterion is the load-bearing line: it is what stops the next round re-deriving the question.
>
> **Once decided:** the deep-dive or spec § is written **first**, the entry is deleted **second**.
> Settled lives in the spec, unsettled here, and the two never overlap.

---

## Verification needed before landing (spike, blocking a subsystem)

> None.

---

## Design points still to be refined

### §5.C · When and in what shape a remote volume backend lands

- **What is open.** The boundary item left by §5.C's contract: the arrival shape of an S3 / NFS
  backend.
- **Settlement criterion.** A future remote volume backend actually being built — the contract is
  settled, so nothing is blocked meanwhile.
- **Argument.** [`deep-dives/volume.md`](./deep-dives/volume.md) D8.

### §4.4-C · The size of the replayable event-stream window

- **What is open.** **Only the VALUE.** The outward shape (pull + a `seq` cursor) and the trimming
  mechanism's five properties are settled in §4.4-C / §6.1.
- **Settlement criterion.** A deployment's choice, argued from **the reconnect duration promised
  to an external subscriber** — and no such subscriber exists today. The engine's default is
  **off**, following the node-budget rule that it must not invent a number; §4.4-C states that
  cost as a known limitation.
- **Argument.** #5.

### §5.D · Whether the internal deny-safe tier is widened beyond RFC1918

- **What is open.** Only whether a future deployment needs more than the three RFC1918 ranges. The
  membership list and the reason for it are **settled in §2.4**; `169.254.0.0/16` and every
  metadata endpoint are in the unliftable L0 floor, not this tier.
- **Settlement criterion.** A deployer arguing from **what their internal network actually
  contains** — never from the list's completeness. **The additivity is verified; do not
  re-investigate it.**
- **Argument, and how to widen it safely.** #6.

### §5.A · The provider interface, remote form's input shape

- **What is open.** Which obligations a non-nft enforcer cannot carry (`Reject` vs `Drop`, rule
  order, `via[]` steering); whether `DeathCause`'s three values survive a remote scheduler's
  termination model; whether `Stop` should state that memory is discarded. **`ResourceLimits` and
  `SecondHopUID` have left this list — both settled.**
- **Settlement criterion.** A remote provider actually being registered. Nothing here is breaking,
  so it stays a pure registration.
- **Argument.** #7.

### §5.B · A content-addressed diff layer (dedup, not capability)

- **What is open.** Whether to dedup identical bytes across layers, and at what granularity
  (whole-file vs chunked), plus the digest that comes with it. **The capability case is closed** —
  pause/resume, layered Templates, fork and cross-host portability are all delivered today without
  it.
- **Settlement criterion.** Argued from **what a node's layer library actually costs**, and there
  is no reporting surface for that yet — the honest reason this cannot be settled now.
- **Argument.** [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D2/D3/D5.1, whose
  ten-item list is what keeps the arm additive.

### §5.B / §4.3 · Whether the Overlay becomes a first-class object, independent of the sandbox row

- **What is open.** Whether to remove the special case whereby an Overlay is referenced by exactly
  one sandbox row. **Not flatten and not re-base** — a first-class object *dissolves* the
  lineage-depth problem where those only *manage* it. **Direction leans yes**: it deletes a
  special case and is isomorphic to what Volumes already have.
- **Settlement criterion.** **Must be settled together with §3.4's admission accounting, not in
  sequence** — a detached Overlay's bytes occupy the node's disk budget while no sandbox row
  carries its cap, which lands it on the uncapped-bytes problem. Exclusivity also becomes a lease
  problem: at most one sandbox may hold a writable upper.
- **Argument, and the fallback in place (Promote).** #1 — filed as an ask by the platform, which is
  where the question was raised from.

### §3.4 · A per-unit unknown channel when a quota read fails

- **What is open.** **Only the aggregate's SHAPE**: `UncappedBytes` has no per-unit error exit, so
  one unreadable unit fails the whole node's admission read.
- **Settlement criterion.** The **post-retry** residual failure rate × the unit count. **The retry
  already landed and changed the input** — `btrfs.Client.Usage` retries 3×, turning 8 first-attempt
  failures into 0 final failures over 300 reads under three-way churn, so the 7.3% figure once
  cited here is per-attempt and pre-retry.
- **Also settled and no longer open: do not synthesize a number.** An unreadable row produces an
  error, never a substituted measurement.
- **Argument.** #8. **Moves with §5.B's `UncappedBytes`**, which has the byte-identical shape.

---

## Choices awaiting a decision (candidates exist, no conclusion)

### §3.3 / §6.1 · Resolving a placement address back to a sandbox ID

- **What is open.** Whether the engine offers the lookup at all, and in which of two shapes: a
  point-in-time query that refuses rather than guessing, or **a fact on the event stream** (the
  leaning). **Not** exposing the placement outward — §6.2 stays ID-addressed.
- **Settlement criterion.** Whether the consumer needs to resolve a **live** observation (a query
  suffices) or a **past** one (only a stream fact does). **Those are different features**, and the
  requirement is under-specified in exactly this respect. **Guest IPs are reused** and resume
  deliberately takes a new one, so an answer without a time can confidently name a different,
  innocent sandbox.
- **Argument.** #4. **Weigh with #3** — same substrate; granting both separately buys one
  capability twice.

### §6.1 · Whether the engine emits traces, and what a span covers

- **What is open.** Traces, and if so the span boundary. **Per-sandbox metric series are settled
  as refused** (label cardinality) and that is not reopened — but that argument does not reach
  traces, whose cost scales with sampled operations rather than object count.
- **Settlement criterion.** The requirement is *after-the-fact investigation of a per-sandbox
  anomaly*, which a polled read surface cannot serve. **Three accumulated cases from this repo's
  own debugging each want a per-object timestamped fact and none wants a request-scoped span
  tree** — the closest thing to a decision this entry holds.
- **Argument.** #3. **Weigh with #4.**

---

## Implementation gaps (design settled, code not yet landed)

> None.
