# Open Decisions register

> The spec describes only the converged architecture; choices not yet settled, design points still
> to be refined, and assumptions to verify before landing are registered in this document.

> **Numbering convention**: a bare `§X.Y` always refers to a spec global section, located via
> SPEC.md's "document map" at the top; `Dx` refers to a section of the corresponding deep-dive
> document; this document's own sections are written as "this document's §N".
>
> **This document's §1** blocks the relevant subsystem's spike · **§2** architecture settled,
> internal mechanism not yet developed · **§3** has candidates but no conclusion · **§4**
> implementation gap (design settled, code not yet delivered). Each item is annotated with what it
> belongs to, its current leaning, and who consumes it; items already detailed in a deep-dive get
> only a one-line summary plus a pointer, **not a copy of the body**.

---

## 1. Verification needed before landing (spike, blocking the relevant subsystem)

> None.

---

## 2. Design points still to be refined (architecture settled, internal mechanism not yet developed)

| Belongs to (spec §) | What's still to be refined | Notes / current leaning |
| --- | --- | --- |
| **§5.C Volume (outside the contract boundary)** | The contract is in §5.C; the boundary items still to be refined: admitting a direct mutation vs a live-ro reader / a snapshot's **consistency label** landing in §6 / the remote backend's materialization shape | **See [`deep-dives/volume.md`](./deep-dives/volume.md) D8** for detail, not duplicated in this table. Mostly product-layer policy, or settled once it lands in §6 |
| **The size of the replayable event-stream window (§4.4-C)** | The event stream's outward shape is settled (pull + a `seq` cursor, §4.4-C / §6.1). **The only thing still open is one item: how large the replayable window should be** — i.e. when to start trimming the outbox. **See §4.4-C's closing paragraph for detail** (why this is deferred rather than answered, plus the associated at-rest KNOWN LIMITATION), not duplicated here | Who consumes it: a future external subscriber — the value must be argued from "the reconnect duration promised to it", and no such subscriber exists today |
| **Whether §5.D's fail-closed emits a §4.1 audit event** | Leaning toward yes. Every other item of §5.D is already settled, **see [`deep-dives/network.md`](./deep-dives/network.md) D8** for the list, not duplicated here. **Capacity-pressure events are not in this list** — the soft threshold is already settled as not emitting one (§4.4-C) | Who consumes it: a future subscriber to the §4.1 audit stream |
| **Whether §5.D's internal deny-safe tier is widened beyond RFC1918** | **The membership list is already settled into §2.4** (the three RFC1918 ranges plus mechanical v6 derivation), so this row does not ask "which set to take" — only whether to widen it in the future. Private-use ranges falling outside it (a resolver on the local machine inside `0.0.0.0/8`, CGNAT `100.64/10`, etc.) are treated as public internet today. **The cost of widening has already been measured, so this is not a new investigation**: the wider candidate is a pinned snapshot of the IANA special-purpose registry's Globally-Reachable=False column, which would need five GR=True reverse allowances and would also collide with D5 ⑱'s nested-member problem — **the exact shape of both costs is in §2.4**, not duplicated here. **The shape is purely additive** (adding a line to a static list, D7 litmus), so no mechanism is reserved for it | Who consumes it: a future deployer — the value must be argued from "what this deployment's internal network actually contains", not from the list's completeness |
| **The §5.A provider interface (the remote form's input shape)** | **What's still to be refined is the remote form's input shape**: `RootfsSource` (a local `MergedPath` vs a remote handle), `SteeringSpec`'s **exact fields** (the reachability requirements §5.D derives; no host primitive may appear in the signature, per §5.A's ❌ list), and `ResourceLimits`'s equivalent on a non-cgroup scheduling layer — worked out on paper against "an isolation unit scheduled remotely" as the reference, **implementing no remote provider** | Who consumes it: a future remote provider |
| **§5.B Overlay materialization** | Still to be refined: a content-addressed diff layer, multi-node addressing across nodes via the rootfs provider (**an application-layer collaboration safety point is NOT in this list — already rejected**, see [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4) | **See [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5** for detail, not duplicated here |
| **Which way §3.4's second term should err when a quota read fails** | **This is not "whether to fall back" — it is that the fallback's direction has never been argued.** `LocalBtrfs.UncappedBytes` falls back to `allocbytes.Tree` when a unit's quota row cannot be read, and the comment's rationale is "undercounting real bytes lets the node overcommit, so the fallback is the conservative direction" — but that statement treats the error direction as one-sided, and measured, it goes both ways: a directory walk **counts only regular files**, while the quota row **also counts metadata extents** (an empty unit measures 16384 B via the quota row vs 0 via the walk), so the walk **undercounts** units that are sparse or have many small files (the overcommit direction); at the same time it counts an extent **physically shared** with its source in full, while the quota row counts it only once, so the walk **overcounts** units heavy with snapshots/clones (the wrongful-refusal direction). **Which side dominates depends on that node's composition, not on the code.** The trigger condition is not rare: a concurrent delete **anywhere** on that filesystem makes the quota row's read fail wholesale, and the substrate's cleaner is asynchronous, so the caller has no way to serialize against it (the cause, its measured frequency, and the attached trap of mistaking an empty output for "the row is gone" are in [`deep-dives/volume.md`](./deep-dives/volume.md) D7.1, not duplicated here). **The per-volume half is already settled** (`Backend.Usage` returns an unreadable row as an error rather than `(walk, 0, nil)`); this row asks only what the **aggregate term** should do — `UncappedBytes` has no per-unit error exit, so one unit's failed read either pollutes the whole node's number or fails the entire admission read | Leaning: measure both error directions until it is clear whether either is large enough to actually flip an admission verdict, before settling a shape; reserve no mechanism yet. Who consumes it: §3.4's admission gate — the value must be argued from "what volumes on this node actually look like" |
| **§5.C's per-volume capacity cap materialized on a remote backend** | The local arm is subvolume plus squota (a primitive §5.B/§5.C share). **What's still to be refined is the remote backend**: by what means an object-store / network-FS backend delivers "this volume may grow no larger than X" (a backend quota API, or client-side accounting), and its relationship to `Capabilities()` | Sealed inside that backend (volume.md D1/D7), not entering the core; who consumes it: a future remote volume backend |
| **Whether the §6.1 sandbox read surface reports `used_byte`** | The volume side's `GET /v1/volumes/{id}` already has it, on the grounds that "the caller needs it to pick a new cap **strictly greater than** it"; `PATCH /v1/sandboxes/{id}` now carries that same strict inequality too (§3.4), so the same rationale holds for a sandbox. **The mechanism is already in place, no addition needed**: `rootfs.Provider.Usage` already reports this number, so this is purely a question of whether the read surface adds one more key. **Leaning toward yes**, following the same omission rule as the volume side (**the rule's exact text is in §6.1's `GET /v1/volumes/{id}` note**, not duplicated here). Today's substitute is carrying the current usage in the 400's message text, which works but requires the caller to parse an error string | Who consumes it: a caller changing a cap — the value must be argued from "should the read surface pay one more backend round trip per single read" (the concern about a list multiplying that cost by the sandbox count is word for word the same as on the volume side) |
| **§5.A / the Template (content catalog)** | **The built-in Template's content list**: a bare base (uv/pip/git/build-essential; sudo optional, purely a guest-side convenience, not engine semantics), with each agent stack (claude-code/opencode/pi) layered on top of the bare base. **This is a product/template-layer content choice**, still open (the Template's first-class row-ification / boot seed / multi-lowerdir layering mechanism is §4.3 / §5.B's already-settled architecture, not part of this row) | guest defaults to root (§2.2), and installing packages is a one-shot exec with no init dependency introduced, see [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |
| **Whether `GET …/network` reports a per-rule hit counter** | Every `allow[]` / `via[]` entry already carries an nftables counter — the engine reads it internally as the only signal for "is this rule live" (network.md D4.2's `iifname` trap, D4.1's silent-pass-through case) and has twice shaped a design decision around it. **The mechanism is already in place, no addition needed**: this is purely a question of whether the read surface exposes one more key per entry, plus one aggregate "hit the default-deny catch-all" count. **Leaning toward yes**, following the same shape as the `used_byte` row above (report only what a live placement can read; a `paused` sandbox has no rule set installed and so has no counter to report, the same omission rule). **Two things any shape must state, not silently assume**: counters are placement-local state, so they reset to zero across pause/resume and across a control-plane restart's rule reinstall (network.md D5 ㉒'s atomic-reinstall measurement) — a caller reading "0" must not infer "never hit"; and a `via[]`-scoped refusal happens one hop past the engine (see network.md D4.3's closing paragraph), so this counter tells a caller a `via[]` entry was *matched*, never whether the connection was subsequently allowed or refused by that external layer | Who consumes it: the platform layer's allow-list rollout — without a per-entry deny signal, a first-cut allow-list that is inevitably incomplete has no way to attribute a guest-side failure to the network declaration, and today's substitute (no observability at all) forces every miss onto a user-reported ticket |

---

## 3. Choices awaiting a decision (candidates exist, no conclusion)

> None.

---

## 4. Implementation gaps (design settled, code not yet landed / still to be closed out)

> None.

---

> **Discipline:** once an item is decided, first update the corresponding deep-dive document or
> spec § section, **then remove it from this register** — this keeps "settled" in the spec and
> "unsettled" in this document, with the two never overlapping. The same holds for this document's
> §4 gaps: once the code lands, remove the row.
>
> **A row in this table is cited by its bold title, never by position** (the reason is word for
> word the same as the one at the top of network.md D5 that refuses citation by position).
