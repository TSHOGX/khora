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
  to an external subscriber** — and **no subscriber has promised one yet.** *Corrected: this
  entry used to read "no such subscriber exists", which is false — the platform claims the role
  by name ("the platform is the subscriber that decides it") and has built the surrounding
  machinery, including an expired-cursor error state. What is missing is the NUMBER: it nowhere
  states the longest disconnect-recovery time it permits, and its adjacent figures are marked as
  having no precedent behind them. The distinction matters because the old wording would send a
  later round looking for a subscriber instead of asking for a duration.* The engine's default is
  **off**, following the node-budget rule that it must not invent a number; §4.4-C states that
  cost as a known limitation.
- **Argument.** #5. **The trimming mechanism has landed** and is not an implementation gap —
  `Store.TrimOutbox`, where 0 means never trim and is the default.

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
