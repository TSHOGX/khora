# Sandbox Engine — the model layer (§3 sandbox model · §4 lifecycle)

> Belongs to [SPEC.md](./SPEC.md); § numbers run continuously across files, and the
> navigation is the "Document map" at the top of SPEC.md.

---

## 3. Sandbox model

### 3.1 Three states: mutability cut along layers

```text
Template   an immutable, content-addressed read-only base (shareable/forkable by many sandboxes)
  └─ Live   = Template (read-only lower) + a writable Overlay + running memory   ← transient, mutable
       └─ Paused = the Overlay quiesced on disk, the container stopped, RAM/CPU released
            │                                                                     ← occupies storage only
            └─ (optional) Promote = freeze the current Overlay into a new Template
```

- **Template**: immutable, shared, forkable, with **an opaque ID as its identity**. Either
  platform-provided (a clean base, a stack carrying some agent …) or built by the user via
  Promote. Immutability is delivered by **a read-only lower layer + refcount protection**,
  not by a content hash — a hash can only *detect* tampering, not *prevent* it (prevention
  comes from the read-only mechanism); **a content digest is an optional evolution anchor
  for deduplication/integrity** (§5.B / rootfs-overlay D3), and identity versus
  deduplication/caching are two cleanly separated things.
- **Overlay**: this sandbox's **writable delta layer** relative to its Template (apt/pip and
  `$HOME` all write into it), stacked on top of the Template to form Live's writable rootfs.
  **The Overlay is a first-class, ID-addressed persistent artifact decoupled from the
  Template** (see 3.1.1), not an appendage of it.
- **Live**: `Template (read-only lower) + Overlay (writable layer) + running memory`. The
  running memory is transient (discarded on pause, §4.2); the Overlay persists on the
  storage backend.
- **Paused**: after quiescing, the runsc container is stopped, the Overlay stays on the
  backend, and RAM/CPU are released. **No memory snapshot is taken** (§4.2).
- **Promote**: freeze the current Overlay into the new lower layer of a named Template (like
  a layered image), for later forks. **It is initiated against a logically running sandbox,
  but the freeze reads at-rest bytes** — the engine internally quiesces/flushes it to a
  consistent point (equivalent to one pause) and then reads the frozen Overlay to freeze it;
  **file content is never read while the container runs** (§5.B "reading Overlay content
  happens only at rest").
  - **One deliberate exception**: the cost forecast (`PromoteCostBytes`, used to pass the
    §3.4 admission gate) walks the **still-running** upper. Four properties:
    - **Read-only metadata**: it only `stat`s and opens no file — the same exemption as
      §5.B's "metering may be asked of the backend at any time".
    - **Projected as `max(allocated, apparent size)` rather than a pure allocation
      statement**: freezing is a copy that does not punch holes, so counting a sparse file by
      allocation would underestimate, and underestimation is the one direction the admission
      gate cannot tolerate.
    - **The gate comes before the quiesce**, in the order forecast → quiesce → freeze: the
      quiesce is itself a real pause and emits a `paused` event, so putting the forecast after
      it would let a refusal for insufficient capacity leave behind a sandbox that has
      **already been stopped** (rootfs-overlay D6.5).
    - **It uses the same exclusion set as the freeze**: otherwise it would refuse a Promote
      over bytes destined to be pruned. The direction is still a safe upper bound — hard links
      are counted per link, and the freeze does indeed expand them into separate files.

#### 3.1.1 The Overlay is a decoupled artifact, bound to neither the Template nor a host path

This is the precondition for multi-node being possible at all: the Overlay's materialized
location must not degenerate into "the upperdir is always on a local disk".

- **The logical model**: the Overlay is **a frozen layer addressed through the rootfs
  provider** (carrying its own ID and properties). It **references** a Template as its lower
  layer, but **where it exists is independent of the Template** — it can be on the same disk
  as the Template, or on another network store or object store, and its properties (size, the
  Template it references, refcount, lifecycle state) can even be recorded in a separate table.
- **Materialization is the rootfs provider's business, and the model assumes no host-local
  path**: single-node lands it as a local overlayfs upperdir, multi-node as a layer
  addressable across nodes — the two materializations are in §5.B. What matters is that at
  the model layer an Overlay is always "an ID-addressed frozen layer referencing some
  Template"; **the moment a host-local path is assumed, any non-local provider breaks the
  instant it is plugged in**, and that is precisely the precondition for §3.3's "identity does
  not encode placement" and for §4.2's pause/resume holding under multi-node.
- **ID addressing applies internally too**: §6.2's requirement to "expose no host path
  outwardly" is **carried through to the engine's own Rootfs subsystem** — an Overlay exists in
  the control plane as an ID with properties, and a host path is merely some provider's
  implementation detail, never entering the model.

### 3.2 A sandbox is the persistent unit; a connection is a multiplexed concurrent process

```text
Sandbox (the persistent unit: the Overlay artifact, mounted Volumes, the network declaration,
         the resource ceilings)                                        ← stable, opaque ID
  ├── shell        (process + PTY)
  ├── agent        (process, e.g. Claude)
  └── agent        (process, e.g. Pi)     ← concurrent, sharing this sandbox's FS / network
```

A shell is "a kind of connection available on every sandbox at any time", not a product mode.
Several connections writing the same FS concurrently = several people ssh'd into one Linux
box; that is the Unix model itself. **Every connection = one `Start` on the `process`
service** (a shell being `exec /bin/bash` with a PTY), addressed by pid and reconnectable via
`Connect`; the engine maintains no registry of connections (§5.E).

#### 3.2.1 Swarms / many agents: the engine does not know about "agents", let alone address them

One sandbox may need to run several agents, of different kinds, and even **agents spawning
further agents** (a swarm). **The engine provides only the two general mechanisms process/fs
and never knows the concept "agent".** Three layers, each in its place:

- **The engine layer**: a sandbox is just a machine, exposing only `process` (start/attach/
  control a process) and `fs` (read/write files). An agent spawning an agent = processes on
  one Linux box talking to each other over localhost, which the engine **does not track and
  should not track** (consistent with the decoupling premise). On the engine side there are
  **no** primitives such as named endpoints, tags, or a session registry.
- **Addressing uses guest-native mechanisms; the engine builds no namespace of its own**: to
  talk to some agent (a spawned one included), use its pid / unix socket / port and connect
  through the `process` capability — **Linux already has a process namespace and the engine
  does not invent a second one**. If a product wants human-readable names, that is **data**
  (an agent writes "I am the planner" into its own log), the product layer reads the log to
  get the mapping, and the engine never parses a name.
- **The product layer (above the engine, not inside it)**: "sessions", swarm topology (who
  spawned whom, the DAG), which agent belongs to which user/tenant — all of it lives here,
  built on the engine's ID-addressed atoms (sandbox / process / fs) (§1.4). Agent session
  semantics are themselves an adapter addition outside the engine (§5.E).

### 3.3 Identity: reference, not belonging

**Identity here means "which one is referred to"** — that is, who "this sandbox" or "this
connection" points at, and whether it is still the same one after it changes. **It is not "who
possesses it"**: user identity and operation permissions are not discussed here (and are not
inside the engine either, see §1.4). Of the three layers below, the first two are **stable
references** (staying the same across change), while Placement is **physical resources rebound
every time** — it is listed in the table precisely because it is the thing most easily mistaken
for identity:

| Layer | What it is | Across pause/resume | Notes |
| --- | --- | --- | --- |
| **Sandbox** | This persistent sandbox (the Overlay artifact / mounted Volumes) | **Stable, opaque** | Still the same sandbox after resume |
| **Process** | One connection (a shell / some agent) | Transient, concurrent | Addressed by pid, allocated by the guest when the connection is established, reconnectable via `Connect` |
| *(not an identity)* **Placement** | netns/IP / container / cgroup / which node it lands on | **Rebound every time** | Allocated from scratch on resume |

The iron rule: **a Sandbox's identity does not encode its placement.** After a resume the IP /
container / cgroup / node may all differ, but the Overlay artifact has not changed (session
history and the like are just bytes inside the Overlay and do not count separately as
identity), so it is still the same sandbox. This is the precondition for §4's pause/resume and
a multi-node provider (§5.A) holding at the same time.

### 3.4 Source of truth: two kinds

Cut by **resume behavior**, there are only two:

- **Frozen (read the bytes)**: the SoT is the bytes on the backend — the sandbox's **Overlay
  artifact** (addressed through the rootfs provider) and data Volume contents (each on its own
  backend). Resume reads them straight back.
- **Binding (rebuilt from the control-plane record)**: the SoT is the sandbox's own
  control-plane record — which Template it references, which Volumes are mounted, the
  **resource ceilings (CPU/RAM/disk)**, the **lifecycle TTLs (`idle_timeout`/`retention`,
  §4.3)**, and the network declaration. Resume rebuilds from the **current record** and does
  not read frozen bytes. Their values stay stable across resume, which is what makes them all
  Bindings — a placement is rebound each time and is not a SoT (§3.3). **But their
  materialization points differ**: resource ceilings materialize at every placement (§5.A/§5.B),
  whereas **TTLs have no placement-local materialization at all** — their consumer is the reaper,
  which reads the record directly on every sweep. A connection (process) is **not a Binding**:
  the engine stores no connection records, and after a resume the client initiates them again
  (§5.E).

> Note that the Overlay artifact **straddles both kinds**: its **bytes** are Frozen (read
> straight back), but **which Template it references** is a Binding (a control-plane record).
> Resume = read the Overlay bytes back + stack them onto the current Template per the record,
> and neither half can be missing.

> There is no third kind (revocable external authority). The engine has no authorization
> subject, so there is no "externally revocable authority" either; every policy's authority
> lives in the sandbox's own control-plane record and belongs to Binding.

#### 3.4.1 The three axes of resource ceilings: each mechanism, each consequence of exceeding it

**"Resource ceiling" is three dimensions, each materialized by the mechanism appropriate to
it, so the consequence of exceeding each differs too.** All three are per-sandbox Bindings and
all three materialize at placement time. **A Volume's capacity cap is not one of these axes** —
it is a per-volume property, recorded on the volume record and materialized by §5.C; but it
still enters the first term of the node accounting inequality (§3.4.2):

| Dimension | Mechanism | Consequence of exceeding | Role |
| --- | --- | --- | --- |
| **CPU** (a rate; disk I/O is the same axis and joins in the same shape once it has a consumer) | §5.A lands it on cgroup: **both a ceiling and a share are written** (`cpu.max` caps the peak, `cpu.weight` sets the relative share under contention) | The guest slows down, with no error | Resource ceiling: fair allocation |
| **disk capacity** (at-rest bytes) | A filesystem-level quota, applied by §5.B to the Overlay upper+work | `write()` returns `ENOSPC` and **the sandbox lives on as usual** | Resource ceiling: fair allocation |
| **RAM** | The host memcg (+ `oom.group=1`) | **The sandbox is terminated** | **A security limit: bounding the blast radius** |

**Disk capacity and disk I/O are two orthogonal axes** — do not conflate them: cgroup cannot
govern how many bytes are occupied at rest, and a quota cannot govern read/write rate. The
capacity axis is the enforcement point for §2.3's blast radius on the disk dimension.

**Writing both a ceiling and a share on the CPU axis is a necessary condition for the "fair
allocation" role.** The contract still has only **one** knob (`cpu_millis`), from which both
the ceiling and the share are derived (§1.3: no field added, seam shape untouched). Writing
only the ceiling is not enough: cgroup's default weight is the same for every leaf, so when
the node is saturated a 4000-millis and a 250-millis sandbox **split the CPU evenly** — neither
cap is broken, but the ratio is nothing like what they were promised. The share is exactly what
makes "no admission gate on CPU, oversubscription allowed" (next paragraph) degrade
**proportionally** rather than uniformly under congestion; it takes effect only when the node
is saturated and both parties are runnable, and changes nothing under low load.

**CPU gets no node budget (unlike RAM / capacity).** The criterion is the consequence of
exceeding: oversubscribing CPU only slows the guest down with no error, and the admission gate
guards only the axes whose consequence of exceeding is ungraceful (§3.4.2). Corroborating
evidence: mature implementations deliberately compress CPU scheduling requests to 1/10 of the
ceiling to improve packing density, treating CPU as an oversubscribable dimension rather than
a quota dimension.

**Three dimensions are all of them, and `pids` is permanently excluded.** Measured: cgroup's
`pids` cap limits **host tasks** (about 2.3 per guest process under systrap), and setting it
low makes the sentry panic outright (`failed to get clone`) and the entire sandbox disappear —
it is **an availability landmine rather than a resource cap**, fundamentally unlike this
section's three dimensions whose consequences of exceeding are each well-defined (exceeding a
pids cap produces neither a slowdown, nor an `ENOSPC`, nor an explicable termination, but a
sandbox vanishing into thin air). So it does not enter `ResourceLimits` and does not enter §6.1.

#### 3.4.2 No node-level oversubscription: two independent budgets

**Two independent budgets, each metering "what the engine has promised".** A per-sandbox cap
only downgrades "fill the whole machine" to "fill your own share"; whether the **sum** of the
caps can be delivered is a separate matter, computed by the **admission gate** before create /
resume (§4). The two budgets are not interchangeable (memory and disk do not substitute for each
other), and **only axes whose consequence of exceeding is ungraceful get a budget** — disk
capacity (an innocent sandbox taking `ENOSPC`) and RAM (a sandbox being terminated), so of
§3.4.1's three axes only these two are here.

- `Σ(declared disk caps) + Σ(measured bytes not covered by a cap) ≤ the configured disk budget`
  (why the second term must exist: rootfs-overlay D6.2; who computes it when, and how the
  Promote path passes it: D6.5 in the same file)
  - **Both terms sum across §5.B and §5.C, not over sandboxes alone.** The first term =
    `Σ(sandbox disk caps) + Σ(volume caps)`; the second = `Σ(§5.B uncapped bytes) + Σ(§5.C
    uncapped bytes)`, the latter reported by the §5.C backend through a seam method of the same
    shape as §5.B's. **No third term is added**: volume caps and sandbox caps are like terms on
    the same left-hand side (both are "upper bounds the engine has declared"), even though a
    volume cap is recorded on the volume record rather than in some sandbox's Binding (§5.C).
    Omitting them oversubscribes on the spot: a node could be filled by a pile of uncapped
    volumes without the admission gate ever refusing once.
  - **The axis being metered ⟹ volumes must have caps too.** §3.4.3's reasoning that "every
    sandbox on a metered axis must have a cap" holds word for word for volumes, so when disk is
    metered a volume's capacity cap is **conditionally required** as well and missing it is a
    refusal — exactly which endpoints take `size`, and which status code the refusal is, belong
    to §6.1/§6.2 (whose "per-axis conditional-required" is the same rule). They are counted by
    their **declared cap** rather than by metadata-level actual bytes — "take the strongest
    available statement about each object" already covers this.
  - **But "counted by the declared cap" is a requirement on the engine, not a description of
    the substrate.** Measured: a snapshot yields **a brand-new, uncapped** quota row (in all
    four shapes), and the substrate offers no inheritable cap. So this accounting is a true
    statement about this node only when **the engine goes and writes that cap itself**, and
    failing to write it is silent and undetectable after the fact — the mechanism, the crash
    window, and why it cannot be reconciled after the fact are in
    [`deep-dives/volume.md`](./deep-dives/volume.md) D7.3.
- `Σ(declared RAM caps) ≤ the configured RAM budget`

**The two terms are mutually exclusive, and the same physical byte is counted once.** One term
is **declared caps** (an upper bound on future growth), the other is **occupied bytes not
covered by a cap** (the only available true statement): a capped Overlay upper **counts 0** in
the measured term — adding its current usage would charge a second time on top of an existing
promise, and that usage is a prediction rather than an occupancy. The criterion is uniformly
**take the strongest available statement about each object**.

**Both budgets count only "bytes the engine can name".** Layer bytes are itemized separately
because **a Template is a first-class engine object** (§4.3 refcount / §5.B) and the bytes it
occupies are covered by no sandbox cap, so the engine must account for them. Whereas **the
substrate's own per-sandbox overhead — runsc state on disk, the sentry/gofer footprint in RAM —
is outside the budgets**: that is the provider's implementation constant (§5.A), and counting it
into the inequality would weld it into the mediator's ledger. So a budget means **the bytes the
engine may promise a guest**, not the host's physical headroom; the operator configures the
budget below the physical value, and that margin is what is left for the substrate's own
overhead.

> **One honest residual**: the uncounted RAM overhead is proportional to the number of
> sandboxes, and its proportion grows as caps shrink, so "configure the budget as some
> percentage of physical RAM" does not hold for all cap sizes. If it bites, the way to settle it
> is to configure a **minimum RAM cap floor** (bounding the ratio's upper limit, requiring no
> engine-named overhead and no change to the §5.A seam), not to make overhead a member of the
> contract.

#### 3.4.3 Rules for writing a cap: a cap is mandatory, raising re-runs the gate, two directions refused

**Once an axis is metered, every sandbox on that axis must have a cap.** An uncapped sandbox
(value 0 = no ceiling) contributes 0 to the inequality's left side while being able to occupy
all of the right — that is not "undercounting a little", it makes the whole inequality **stop
describing that disk**. So an uncapped create / resume on a metered axis is **refused** at
admission (400, a different thing from "the node is full", 507), and is **not** silently filled
in with a default: a refusal is visible and a default is not — the reason §2.3 can call `ENOSPC`
an "**expected** outcome inside the data plane" is that the boundary was **declared explicitly**.
`0 = no ceiling` is legal only when the axis is **not metered**. **The RAM axis most of all**:
uncapped means exceeding is no longer confined to this sandbox, and there is no memcg to read, so
the cause of death can only be `Unknown` (§4.3). CPU is not in the accounting, so `cpu_millis: 0`
is always legal.

**Raising a ceiling must re-run the admission gate.** The gate **sums from the store** (`paused`
sandboxes included), so any write to a cap column changes Σ **on the spot** rather than at the
next resume; a raise must therefore re-run admission before committing and **may be refused**,
while a lowering need not. Likewise, any control-plane action that **increases uncapped
bytes** (such as Promote freezing a new layer) must pass the same inequality.

> **"A lowering need not be re-checked" does not mean "lowering is the safe direction".** The Σ
> argument is right: lowering only shrinks the left side. But **lowering below current usage
> wedges that storage unit, irreversibly** — the substrate accepts the lowering with zero
> validation, after which every byte-growing operation returns `ENOSPC`, **including removing or
> raising the cap itself**, and the only way out is deleting the guest's bytes (the measured
> detail, the boundary case `limit == usage`, and three derived constraints are in
> [`deep-dives/volume.md`](./deep-dives/volume.md) D7.3, not duplicated here).
> So the engine **must not accept such a lowering**, and the criterion is a strict inequality; it
> is also the converse of "the boundary must be declared explicitly" above — a silently accepted
> shrink declares nothing to a running workload. **Each side has its own write point**: the volume
> side at `PATCH /v1/volumes/{id}`, the sandbox disk side at `PATCH /v1/sandboxes/{id}` (400). The
> sandbox side decides through §5.B's sandbox-level usage query seam (why the "uncapped bytes"
> term cannot answer this question is §3.4.2's mutual exclusion).
> **The consequence is worse on the sandbox side than the volume side**: the cap is replayed at
> **every** placement, so a wedged unit cannot even get through a resume — the same consequence as
> the refused `0 → N` write below. Three measurements that hold only on the sandbox side
> (including `limit == usage` wedging equally, and a constraint it imposes on `Mount`'s "new
> workdir" step) are in
> [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6.6.

> **On the sandbox disk axis, `0 → N` (uncapped becoming capped) is a refused write (400), not a
> raise.** The criterion is "can the materialization layer deliver what this write promises?":
> a per-sandbox capacity cap materializes as a quota-bearing storage unit, and **that unit is
> built as a quota unit only at create, and only if it had a cap at that time**; an uncapped
> sandbox's upper is an ordinary directory tree with no writable quota row on it. So accepting
> this PATCH would make the record declare a cap that **no future placement can ever deliver** —
> and §5.B's discipline is "if the cap cannot be delivered, refuse this placement, never silently
> degrade", so that sandbox **can never resume again**: one 200 PATCH turns the sandbox into a
> brick. **The refusal must land at the write point rather than at placement**: the former is
> visible on the spot and changes nothing, the latter is a failure that detonates later, on the
> recovery path.
>
> **Why not "convert it into a quota unit at the next placement while we are there"**: that would
> move bytes on the resume hot path (build a new unit → copy → swap), adding a data-movement step
> with a crash window to the most load-bearing chain, all for a result the caller **can get by
> recreating the sandbox**. **Nor "just build every sandbox as a quota unit"**: that makes every
> sandbox pay the overhead and requires `StateDir` to be btrfs — an uncapped sandbox should run on
> any FS. **Directionality**: `N → 0` (removing the cap) is legal, it merely clears the cap; `N → M`
> (changing it) is legal too. Only the `0 → N` direction is refused.

#### 3.4.4 A `paused` sandbox still occupies both budgets

**A `paused` sandbox still occupies both budgets**, even though it has physically released
RAM/CPU (§4.2). The reason is not accounting convenience but that `paused` must be **a
reversible gate** (§1.1): were it not counted, a new create would consume the resources its
resume needs, the resume would hit the admission gate and stay `paused` (§4.4-A), and since
`UpdatedAt` does not advance ⇒ `retention` runs on ⇒ the sandbox is evicted and **the Overlay
bytes are destroyed** (§4.3). The very reason the admission gate exists is "do not let one
sandbox fail because of **another** sandbox's usage"; being **destroyed** because of someone
else's usage is worse than having a write fail because of it. **The cost, stated plainly**: a
node where most sandboxes are `paused` will refuse creates while facing physically idle RAM, and
auto-pause (§4.3) is the engine's own move that manufactures that situation. This is the price of
"a pause guarantees recoverability", and it is accepted.

#### 3.4.5 Bindings are writable: one write point, effective points in three classes by consumer

**Bindings are writable, the write point is unique, and effective points fall into three classes
by consumer.** A Binding's SoT is the control-plane record, so "editing a Binding" is editing
that record — §6.1's `PATCH /v1/sandboxes/{id}` is the **only** general write point besides
create, sharing the same transition lock and conditional-write commit as the other verbs (§4.4-B
/ §4.4-C). **There is exactly one other write point: `POST /v1/sandboxes/{id}/publish`, which
writes the single field `ingress.publish[]`**, and it too persists in order to survive resume
(the mechanism and reasoning are in §5.D). The effective point depends on **who consumes the
field**:

- **Consumed by the provider / rootfs provider (the three resource-ceiling axes)**: materialized
  at the **next placement**, that is, the next resume. So these fields are **editable only in
  `paused`** — accepting, on a `running` sandbox, an edit that takes effect only at some future
  resume would leave the record inconsistent with the live isolation unit for a long time, a
  false invariant of the "the record says one thing, the substrate does another" kind. The
  operational path is `pause → PATCH → resume`, the same road as §5.C's mount-table edits and
  §5.D's network-declaration edits.
- **Consumed by the reaper (`idle_timeout` / `retention`)**: the reaper rereads the record
  directly on every sweep and **does not go through a placement**, so these take effect
  **immediately** and are editable in both `running` and `paused`.
- **Consumed by the rule set (`ingress.publish[]`)**: purely an addition of host-side rules,
  which **reinstalls the whole rule set in place on a `running` sandbox and takes effect
  immediately**, so it is not subject to the "pause first" constraint above (on a `paused`
  sandbox it only persists, and the rule appears at the next placement — which is not a gap in
  the invariant: a sandbox with no placement has nothing to bypass in the first place). **It is
  the only Binding field that changes host state without going through a placement**, so the
  order is "persist first, then install the rules": the reverse would leave a hole open that the
  engine itself does not know about.

**Every rematerialization of a Binding must re-evaluate its admission criteria, not merely
re-render its content.** The criteria on which a write point (`PATCH` / create / `publish`)
refuses a binding must be walked again at **the next placement, at a crash reconciliation's
reinstall, and at any repair rescan** — those paths **rebuild the same host state**, so they
carry the same invariant the write point does and are not a downstream of "it has been checked
already".
**Why this must be written out**: a binding is durable while what its criteria must read is
**not** — the criteria may depend on deployment-time inputs (per process), on a storage unit's
**current** usage, or on a placement's products. So "persist → the environment changes →
rematerialize" is **a legal path that bypasses that refusal, with no write in between**: a
binding correctly refused at the write point comes alive after one restart under a different
deployment. And that path is **completely insensitive to every assertion on the write-point
side** — those test the write point, so it passes silently all the way through. This is not
defensive programming: it is a recurring cause of the same shape (a reinstall path losing the
criteria's inputs, a repair scan losing usage, a rebind deduplicating by the wrong key), and the
symptom happens to be "everything looks fine".

**Three incidental disciplines on writing.** ① `PATCH` replaces the whole binding only: fields
not given are untouched, and there are no "pending/unresolved" values (§6.2). ② It **does not
touch `UpdatedAt`** — `retention` counts from that column (§4.3), and refreshing it in passing
would quietly extend the eviction deadline as a side effect of changing a cap. ③ It is **not a
§4.1 transition** and so emits no event — the same for Promote (except that Promote **itself** is
not a transition; the quiesce it performs internally on a `running` source is an ordinary pause
and emits `paused` as usual).

**The provider is not required to change a cap in place**: there is no "set this unit's cap to N,
now" call — that would be a request shaped like a host primitive, and a scheduled remote isolation
unit generally cannot deliver it (§5.A's ❌ list). Changing a cap goes through "edit the record +
rematerialize at the next placement".

**The RAM ceiling is not a resource ceiling; it is a security limit.** It bounds the blast radius
and provides no fair allocation. The consequence of exceeding it is **a lifecycle event (the
sandbox is terminated)** rather than one failed operation — because on this substrate there is no
path where "a process inside the guest gets `ENOMEM` and the sandbox survives": gVisor has no
in-guest OOM killer, the guest's `MemTotal` is a decorative value read once at boot, and the only
point of leverage is the host memcg. So exceeding RAM is **a state transition the control plane
must know about** (§4.3 has its disposition class), whereas exceeding CPU/disk is an event inside
the data plane (the guest itself handles a slowdown or an errno) that the control plane does not
get involved in.

---

## 4. Lifecycle model

The lifecycle is **a mediator standing above the five subsystems (§5)**, not a sixth peer
subsystem and not some subsystem's internal feature. The reason: `resume` **is one action that
spans four subsystems, A–D** — stop/start processes (A), remount the Overlay (B), remount Volumes
per the mount table (C), reinstall the rule set per the network declaration (D); E (transport) is
stateless, the engine does not participate, and the client reconnects on its own.

From this follows the assignment of responsibilities that prevents cycles:

> **Verbs belong to §4, mechanisms to §5.** The lifecycle (§4) holds the state machine and
> **calls** the capabilities each subsystem exposes according to it; subsystems provide
> mechanisms only (Rootfs provides "mount/unmount an Overlay", Network provides "generate a rule
> set" …), and **subsystems never call each other**. A cross-subsystem call such as "Rootfs pokes
> Network during resume" is forbidden — that is exactly where a reverse-dependency cycle starts.

### 4.1 State machine

Four lifecycle states: `creating` / `running` / `paused` / `destroyed`. (Their relation to §3.1's
three states: `running`↔Live, `paused`↔Paused; a Template is not a sandbox state, and
`creating`/`destroyed` have no corresponding FS state. §3.1's three states are about rootfs
mutability, this section's four are the lifecycle machine.)

```text
creating ──▶ running ──pause / auto-pause / crash──▶ paused ──▶ destroyed
                │   ▲                                 │   │         ▲
                │   └──────────── resume ─────────────┘   │         │
                └──────── destroy ────────────────────────┴─────────┘
        (destroy may be initiated from running or paused; evict = the reaper's automatic
         destroy of a paused sandbox)
```

- **Explicit transitions** (API verbs): `create` / `pause` / `resume` / `destroy`; `destroy` may
  be initiated from either `running` or `paused`, with the terminal state `destroyed` (deleting
  the Overlay artifact).
- **Automatic transitions** (the reaper daemon), **three in total**. The involuntary demotion in
  the next bullet is not counted among them, even though it too is driven by the same reaper
  round. Two go in the "release" direction: a `running` sandbox idle past its threshold →
  `auto-pause`; a `paused` sandbox past its retention period → `evict` (**`evict` IS the
  `destroy` the reaper triggers on a `paused` sandbox** — the same `→ destroyed` terminal state,
  only with the reaper rather than the user as trigger). The third is the only one that can bring
  a sandbox back to `running`: the escalation after transport-down exceeds the restart budget
  continuously (§4.3 / reaper-health D4b). It is **not one `paused → running` jump but two
  steps** — first a crash-pause down to `paused` (emitting `crash-paused` as usual, carrying no
  cause of death), then the reaper immediately initiating **an ordinary resume**, cold-starting
  the whole chain on a new IP; each step passes §4.4-B's lock and CAS separately, and if the
  resume fails it stays `paused`. **A death that L1 cannot detect is not among these** — that
  requires an explicit verb to recover (§4.3).
- **Involuntary transitions** (probe-driven): when a reconciliation or a reaper sweep finds that
  a `running` sandbox's isolation unit is dead, a CAS demotes it to `paused` (§4.2's crash
  variant / §4.3 / §4.4-A) — this is **the third road into `paused`**, alongside a user `pause`
  and the reaper's `auto-pause`, with event Kind `crash-paused` (see the next bullet). **The
  cause of death is persisted in the same commit as this CAS** (§4.3): it decides whether that
  sandbox may take a resume afterwards, and the signal is unrecoverable once the commit is done.
- **Transitions are published outward as events**: every state-machine transition is published to
  an **event stream**, with exactly six Kinds — `created` / `paused` / `auto-paused` /
  `crash-paused` / `resumed` / `destroyed` (**there is no `evict` Kind**: an evict emits
  `destroyed`). Subscribers such as webhooks, metrics, and billing consume it **outside the
  engine**, and **the engine neither knows nor depends on who is subscribed**. This is how
  observability / billing attach — one-way and additive (§1.3: add a subscriber) — not the engine
  calling back into external systems. **All three pause-class events have state `paused`, but the
  Kind carries the reason**: `paused` = a user pause, `auto-paused` = the reaper's idle pause,
  `crash-paused` = an **involuntary** demotion (§4.2's crash variant); the state is identical and
  the Kind lets a subscriber tell "why it was paused". **`crash-paused` covers two involuntary
  paths, not just "the unit is dead"**: ① a probe/reconciliation finding the isolation unit dead
  (with a cause of death persisted); ② the escalation after transport-down exceeds the restart
  budget continuously (§4.3 / reaper-health D4b) — in that one **L1 is still alive**, so it
  **carries no cause of death** (there is no explicable death, and §2.4 says do not guess). Their
  sharing one Kind is D4b's deliberate choice (the escalation only changes which health state is
  acted on, inventing no new state or verb); a subscriber wanting to distinguish them looks at
  **whether a cause of death is present**.
  **How the state commit and the event publication manage to lose and mis-order nothing
  (billing-grade) is in §4.4.**
  > **Terminal states must pair up: a sandbox never seen as `created` emits no `destroyed`.** A
  > `creating` record has never been externally visible (`created` is emitted only after the
  > create chain commits), so deleting it **emits no event** — whether that deletion is a
  > reconciliation cleaning up crash residue or an operator manually destroying a stuck record
  > afterwards (the reconciliation's own deletion after a crash can also fail, so that record is
  > genuinely reachable). One rule for both paths: otherwise a billing-class subscriber would
  > receive an invoice that "ended but never began".

### 4.2 pause / resume: FS-first, no memory snapshot

This is this engine's fundamental divergence from "heavyweight snapshot systems":

- **pause** = quiesce the sandbox to a consistent point → cleanly stop the runsc container. The
  Overlay artifact **is already on the backend**, so pause **moves and captures no bytes** and
  need only flush to a consistent point (§5.B); stopping the container releases RAM/CPU.
- **resume** (engine side) = **cold-start a new container** on the same Overlay artifact +
  remount Volumes per the Binding / reinstall the rule set per the network declaration. The
  engine's resume **stops there** — it restores the sandbox's substrate and does not touch
  sessions.
- **Session continuation is not inside the engine's resume**: an agent wanting to "pick up the
  conversation" is the client/adapter calling the driver's native `--resume` itself after
  reconnecting (reading the history on disk inside the Overlay); the engine neither perceives nor
  participates in that (§5.E).
- **"pause/resume of the FS" is not a separate mechanism** — the Overlay artifact stays on the
  backend, so there is zero movement on the pause side; the hard part, memory, is simply not done.

**The quiescent point belongs to pause (§4) and is not a first-class property of any subsystem.**
"Quiesce the sandbox to a consistent point" is one action orchestrated by the pause verb, spanning
three subsystems' mechanisms and isomorphic to resume: **§5.A supplies the freeze mechanism**
(stop/freeze the runsc container), **§5.E supplies the gated RPC edge** (the mutating fs calls the
engine itself issues), and **§5.B supplies the flush consistency point** (landing in-flight writes
into the Overlay upper before the container stops, guaranteeing consistent at-rest bytes, §5.B
"read at rest only"). The lifecycle holds "when to quiesce and to what point", and subsystems
provide mechanisms only — the quiescent point is not built into §5.E's process (that would
introduce process semantics into a stateless, payload-agnostic transport layer). The quiescent
point **splits into two primitives by process behavior, not by agent/turn semantics**:

- **Mutating writes the engine itself issues**: they first pass a **host-side drain barrier** —
  stop accepting new mutating fs writes, wait for in-flight ones to drain, then flush, then
  freeze. It is **deterministic**, because the host has complete command of the RPCs it issues
  itself: no guest cooperation is needed and no ack channel is introduced. What it closes is a
  concrete window — `fs.Write` is truncate-then-write, and a preemption landing between the two
  steps would leave the file empty. **It blocks writes only, not reads**: blocking reads would
  amount to switching off the observability surface during a pause. **A drain timeout degrades to
  preemption and never lets the pause fail** — one stuck RPC must not make a sandbox un-pausable,
  and where the timeout lands is exactly where a crash lands ("a crash = an involuntary pause",
  below, has already accepted that). A crash-pause does not drain: the container is already dead
  and there is no in-flight work to wait for.
- **Workloads inside the guest (a bare pty, a long-running process)**: handled **preemptively** —
  frozen outright, with no cooperative handshake. **An application-layer safe point does not
  enter the engine; that is rejected**: the engine does not know about agents (§3.2.1), so it can
  neither require a workload to stop at a safe point on a signal nor verify that it stopped at
  the right one; the substrate flush (§5.B) already guarantees what is needed, so this layer of
  cooperation is not a correctness precondition (§4.4). The falsification and corroborating
  evidence from two mature implementations are in
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4.

Why this choice is right:

1. **Auto-pause triggers at precisely the moment memory is least useful.** Idle = every
   connection is idle (no in-flight agent activity, no pty I/O), and an idle sandbox has no
   valuable in-flight memory state to preserve.
2. **Correctness should not depend on memory state in the first place** — session continuation
   rests on driver/home state on disk (read by an adapter), and rebuilding on a cold start is not
   a degradation but the contract.
3. **The cost**: resume is a seconds-scale cold start, not sub-second. Resume happens after an
   idle period rather than on a hot path, which is acceptable.

A by-product: a sandbox's persistent state is a **plain-file Overlay artifact**, inherently
movable across host / CPU / runsc version (§1.5#3 / §5.B).

**A crash = an involuntary pause, recovered through the same resume path.** The FS-first model
covers crash recovery naturally, with no new state: the persistent state is entirely in the
Overlay artifact on the backend, so the substrate dying unexpectedly (a sentry crash, node power
loss) ⟺ RAM/CPU released ⟺ **exactly equivalent on disk to a pause**. Recovering a crashed
sandbox is therefore one resume, not a special path. This does impose a requirement on the reaper
— it must separate "the substrate is dead but recoverable" from "healthy and idle", since a
crashed sandbox must take a resume rather than auto-pause's quiesce (the health classes and their
criteria are in §4.3).

> **Note the scope: "crash" here means specifically the death of the substrate (the sentry/
> container dying whole)** — that is, an L1 probe failure in §4.3.
> A transport-mode `sandboxd` crashing on its own is **not** on this path: PID 1 restarts it in
> place and it recovers on the same IP (§5.E.1), making it an **in-process recoverable**
> transport-down transient (§4.3) that does not take a resume. The two are distinguished by the
> two probe layers.
> **The control plane (orchestrator) crashing itself is a third axis** and is not in this section:
> it does not damage persistent state and can only leave half-built placements, which §4.4-A's
> crash variant (startup reconciliation) catches, without taking a resume.

### 4.3 Health and reclamation model

Reclamation is not a separate mechanism but **a consumer of the health model**: the reaper first
decides a sandbox's current health state, then acts according to it. Staleness detection is caught
explicitly by this section's health model + placement/IP-as-epoch (below), without resting on a
counter field.

**Probing has two layers, not one.** PID 1 (`sandboxd` in init mode) **supervises and restarts in
place** the transport-mode child process (§5.E.1), so "the substrate is alive" and "the transport
port is listening" are **two independent facts**, and the control plane must **decide both** to
classify correctly:

- **L1 — the substrate probe**: is the runsc/sentry container alive (§5.A).
- **L2 — the transport probe**: does a `Health` RPC to the netns IP:port succeed (§5.E, see
  transport.md D4.5).

> **These two `L`s are probe-layer numbers and have nothing to do with OSI layer numbers.** The
> `L2`/`L3` in network.md's discussion of packet injection are OSI's link / network layer (and the
> `L0` there is yet another thing: the nft floor), while this section's `L1`/`L2` refer only to the
> two probes above. When reading across documents, read each by its own definition.

> **"Decide both" does not mean "two round trips": L2 reachable ⟹ L1 alive, so the L1 probe is
> skipped when L2 succeeds.** `sandboxd` answers from **inside** the isolation unit, so its being
> able to answer proves the container is alive — this is not omitting a check, it is settling both
> layers at once with a stronger observation. **Only when L2 is unreachable do the two layers
> genuinely diverge** (a transport-down transient vs dead-recoverable), and only then must L1 be
> asked separately. The classifier therefore never reads idle on the premise of "L1 unknown": idle
> is read only on the branch where L2 is reachable, and that branch already implies L1 is alive.

**Health classification (two probe layers first, then idle — delivering §4.2's requirement).** The
dispositions are **all different**:

**The classifier's domain is `running` records.** `paused` goes only through TTL/evict, and
`creating` / `destroyed` are not the classifier's objects; taking `unknown` for them means "not in
this table" rather than a health judgment.

| Health state | Criteria (L1 substrate / L2 transport) | Disposition |
| --- | --- | --- |
| **healthy** | L1 alive + L2 reachable, and **the idle threshold has not been crossed** — some connection is active (recent I/O: process output / pty reads and writes), or all are idle but for less than `idle_timeout` (`idle_timeout: 0` = no deadline included) | Stay `running`, do nothing |
| **idle** | L1 alive + L2 reachable, **all** connections idle (no I/O recently) **and the idle duration has reached `idle_timeout`** — the threshold is part of the classification predicate itself rather than a condition on the disposition, so there is no such state as "classified idle but under the threshold" | **auto-pause**, quiescing to a consistent point (§4.2; the flush is guaranteed by the substrate and depends on no cooperative handshake) |
| **transport-down (transient)** | **L1 alive, L2 unreachable** — the transport child process crashed and PID 1 is restarting it in place (§5.E.1) | **Wait for the restart**; the port comes back on **the same IP**, so this is **not** dead-recoverable and does **not** take a resume; **transport-down continuously exceeding the restart budget (measured as continuous duration, not as a count of PID 1 restarts) → escalate to dead-recoverable and take a resume** (guarding against being stuck in a crash loop; mechanism in reaper-health D4b) |
| **dead-recoverable** | **L1 undetectable** (the container died whole), the persistent state (the Overlay artifact) intact on the backend, **and the cause of death is not hitting a resource cap** | **crash-pause down to `paused`** (§4.2's involuntary pause), with the cause of death persisted in the same commit; the recovery path is a **resume** (a cold start on a new IP) **initiated explicitly by the caller**, not auto-pause's quiesce |
| **resource-terminated** | **L1 undetectable, and the cause of death is hitting the RAM cap** (§3.4's security limit: the sandbox is terminated as a whole group) | **Do not resume** and do not auto-pause — stay `paused`. A resume would let the same workload trigger termination again, that is, a crash loop. **There is exactly one way out: change the cap** — raise `memory_byte` through §6.1's `PATCH /v1/sandboxes/{id}` (it must be **strictly** greater than the old value, since writing an equal value does not clear the cause of death — **this constrains only the clearing of the cause of death and does not change §3.4.3's directionality**; it must re-run the admission gate and is refused if the node has no capacity), and **that same commit clears the cause of death**, after which a resume is allowed; a successful resume clears it too. **It is not exempt from eviction** (see below) |
| **unknown** | **Three sources, and only the third goes through L2**: ① the record is not `running` (not in this table, see above); ② the live-map holds **no live handle for this sandbox** (the store says `running` but this process is not managing it) — the classifier returns unknown on the spot and **sends no probe at all**, that being the business of §4.4-A's takeover half; ③ L2 unreachable and L1 undecidable (the provider errored) | **Do nothing, retry next round** — be conservative about uncertainty (§4.4-A): neither demote nor resume. It is **never a disposition**, only the classifier's safe default |

> **resource-terminated stands alongside dead-recoverable and is not a subclass of it.** In both,
> L1 is undetectable and the persistent state is intact, and they present identically; they can be
> distinguished only by **the cause-of-death signal**, while their dispositions are **opposite**
> (one must resume, the other must not). Across the §5.A seam the cause of death is an **abstract
> enumeration** (`{Unknown, Gone, ResourceExceeded}`) rather than an errno / cgroup file:
> `local_runsc` concludes it from its own memcg, a remote provider concludes the same from its
> scheduling layer's termination reason, and **the mediator sees only the enumeration**. That is
> what makes this classification hold across providers rather than being a host-local hack.
>
> **The cause of death must be persisted at the instant it is observed, in the same commit as the
> demoting CAS.** The cause-of-death signal is **volatile** (in the local shape it lives in the
> provider's placement-local resources, disappearing when the placement is torn down and not
> surviving a host restart), so "ask again later" is not a road that exists: after the demotion
> commits, the evidence is unrecoverable. So `crash-paused`'s CAS and the cause-of-death write are
> **one atomic commit** (the same shape as §4.4-C).
>
> **`Unknown` is a first-class legal value, not an error.** When the cause of death cannot be read,
> the disposition is `dead-recoverable` (go resume).
> **Self-correction is structural and rests on no counter**: if it really was a cap being hit, the
> same workload will trigger termination again after the next resume, and this time it happens in
> front of the control plane with the placement intact, so it is observed, persisted along with the
> demoting CAS, and **subsequent resumes are refused by that persisted fact**. **There is no upper
> bound on resume attempts, and none is needed**: the engine never resumes on its own **because L1
> is undetectable** (of §4.1's three automatic transitions, the only one heading toward `running`
> is triggered by L1 being **alive** while L2 fails to converge, not by an observation of death),
> so a retry is a choice the caller pays for explicitly each time rather than a loop of the
> engine's. Wanting "automatically bring it back after a crash" is an **out-of-engine subscriber**
> consuming `crash-paused` events (§1.3 Consumption). The full falsification (a count has no
> derivable reset point; refusing by count differs in nature from refusing by a persisted fact; and
> it is equivalent to the counter §1.1 bars) is in
> [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D4c.
>
> **One honest residual**: structural self-correction holds only for exhaustion that is **capped
> and reproducible**. If a capped sandbox has its placement already torn down at each death (racing
> teardown, or a host restart), the cause of death can be read as `Unknown` repeatedly and
> **convergence to `resource-terminated` is not guaranteed**. What stops an infinite loop is not any
> engine-side bound but: every retry costing the caller an explicit verb, §2.4's deterministic
> whole-group termination having already eradicated the host-burning "no killable victim → retry
> forever" path, and the capacity/RAM caps themselves.

> **The connection-activity signal is measured on the guest side, not the host side.** `sandboxd`
> stamps a **sandbox-global "last I/O" timestamp** on the output/input paths, and the `Health` probe
> carries the idle duration back (one thing serving three purposes, §5.E / transport D4.5). **It is
> not measured at a host-side data-plane proxy**: under the detached process model (§3.2) a process
> keeps producing into the ring buffer while no client is attached, the host side cannot see those
> bytes, and a working sandbox would be misclassified as idle. The criterion is **I/O activity**
> rather than CPU occupancy (a purely CPU-bound process with zero I/O does not count, which is the
> definition of idle itself). The full reasoning and the implementation of the classifier /
> single-flight / periodic reconciliation are in
> [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md).

Three key distinctions, each decided by crossing all three of "two probe layers + connection
activity"; drop one and it misclassifies:

- **dead-recoverable and idle present identically** (both have "no active connection"), and must be
  separated by L1 rather than by connection activity — a crashed sandbox must never be treated as
  idle and taken through auto-pause's quiesce (which assumes the substrate is alive and can be
  stopped at a consistent point); it must take a resume.
- **transport-down and dead-recoverable present identically** (L2 unreachable in both), and must be
  separated by L1 — a transport crash is **in-process recoverable** (PID 1 restarts it in place on
  the same IP, at far lower cost than a resume), and misclassifying it as dead-recoverable would
  needlessly trigger a whole-chain cold start on a new IP. A transport crash ≠ a container crash,
  which is a direct consequence of §5.E.1's "init and transport are separate processes".
- A process being alive does not itself mean active; both probe layers being reachable does not
  itself mean healthy (connection activity must also be considered).

**Classification is suspended while a resume is in flight.** During a resume the guest is not yet
listening, so L2 is necessarily unreachable; health classification is itself **only a read-only
probe that changes no state**, but running it anyway would misclassify as dead-recoverable, spend a
resume attempt certain to fail, and emit a false alarm. So health classification for that sandbox is
suspended while the resume is held, until the first successful L2 probe flips it to `running` (the
timing is in [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5). **This guard lands
on the existing transition lock and is not a second mechanism standing alongside it**: two sentences
are all of it — a resume **holds that lock throughout** (§4.4-B), and the reaper's classification
disposition **skips the round when its try-lock fails** on a locked sandbox (§4.4-B), so no
classification happens during the in-flight period by construction and there is no new object to
guard. It is **load-bearing only for efficiency and observational cleanliness (no wasted attempt, no
false alarm); the correctness of concurrent verbs fighting is backstopped by that lock held
throughout, not here**. It also **adds no §4.1 state** (still four).

**Staleness detection: the placement/IP IS the epoch.** A sandbox cold-starts onto a new netns IP
and the old IP is void (§5.E), so **a placement/IP change is itself that epoch signal** — speaking
with an old IP / old placement handle is judged stale, and the current placement is re-resolved
before reconnecting (§3.3's "identity does not encode placement"). No separate counter field is
added; every rebinding of a placement inherently carries the semantics "this is a new instance".

**TTLs / eviction**: the reaper advances §4.1's two "release"-direction automatic transitions by
TTL. **A TTL is a per-sandbox Binding** (§3.4): the two thresholds — `idle_timeout` (a `running`
sandbox idle past it → auto-pause) and `retention` (a `paused` sandbox past it → evict) — are fields
of the sandbox's control-plane record, given at create or falling back to the engine-level default
(§1.1's single-binary configuration), so the record always holds a concrete duration the reaper can
read directly. **A value of 0 = that threshold has no deadline** (`idle_timeout: 0` never
auto-pauses, `retention: 0` never evicts). The default values are pure tuning (see
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D5).
> **"Waiting for a human" has a deadline; it is not an indefinite suspension.** `resource-terminated`
> is **not** an exception to reclamation: `retention`'s clock starts at **the instant of death** (the
> `UpdatedAt` written by the demoting CAS), and past the deadline it is `evict`ed as usual, Overlay
> artifact and all. Three reasons: ① the reclamation criteria are refcount and TTL only (next
> bullet), and adding **the cause of death** — a diagnostic field describing a **past** event — to
> the eviction predicate would widen the reaper contract for a scenario an existing knob already
> covers; ② under a non-oversubscribing admission gate (§3.4) an exemption would manufacture a class
> of sandbox that "permanently occupies Overlay bytes **and admission-gate allowance** while never
> running again", which is worse than a leak — the gate treats it as **a live promise**; ③ a scenario
> genuinely needing indefinite retention needs no engine exception, since `retention = 0` (no
> deadline) is exactly that existing knob, and `PATCH` (§3.4) makes it **still settable after the
> fact**. So the disposition is: stay `paused`, giving a human a window as long as `retention` to
> change the cap / Promote / destroy; if nobody intervenes within the window, evict.

**Auto-pause's in-lock activity re-read (a race backstop)**: a sweep's "classify → act" runs
**within one lock hold** (§4.4-B's lock), so there is no window on the lifecycle-state side. But
**that lock guards engine-side transitions, not the guest's I/O**: activity is a fact inside the
guest rather than lifecycle-state, a CAS cannot compare against it, and between the classifying
probe and the pre-disposition probe lies **one RPC round trip** in which the sandbox can become
active — and auto-pause's CAS would still pass. So as not to wrongly freeze a working sandbox,
`auto-pause` **re-reads the activity signal once more** inside the lock in addition to the CAS (the
current `idle_millis`), freezing only if it is still past the threshold and abandoning the round
otherwise. The disposition therefore rests on the freshest in-lock snapshot of both state and
activity (reasoning in [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D3).
- **Object reclamation (the uniform object model invariant)**: every first-class persistent object
  (sandbox / Overlay artifact / Volume / Template / mount row) carries **decidable reclamation
  semantics** (not the reaper's private implementation). Cut into two halves, **universal** and
  **mutable objects only**:
  - **refcount is universal and derived**: reclamation looks at "is there a live row referencing
    it", and **no counter field is materialized** (it is derived by querying whether reference rows
    exist — equivalent to a materialized count under a single-node authoritative store, see the
    next bullet and §4.4-C). There is no owner column — only refcount/TTL matter, never "who
    possesses it" (§1.4). A connection (process) is not a persistent object and does not
    participate in reclamation.
    **The Overlay artifact is a degenerate case**: it happens to be referenced by exactly one
    sandbox row, so its reclamation predicate is simply "no corresponding sandbox row", caught by
    the orphan-unit sweep (§4.4-A), and needs no count.
  - **An explicit lifecycle state machine belongs only to mutable objects that genuinely transition
    (the sandbox, §4.1's four states)**. **Immutable artifacts (Templates, and mount rows later) use
    "existence is the state"**: the row existing = ready, deleting the row = destroyed, with no
    intermediate state, so they have no state column. **The cost and the hard constraint**:
    existence-as-state has no not-ready escape hatch, so it **mandates bytes-first
    materialization** (see §5.B); only objects with a state column (the sandbox's `creating`) may
    go record-first and be backstopped by reconcile.
    **An in-progress / failable build is a separate build entity** (a build table added additively
    later, carrying building/ready/failed) and is not stuffed into an immutable artifact's
    existence.
  Template lineage is the sharpest case: while referenced by some sandbox as its read-only lower
  layer (create-from-Template / a Promote stacked layer / forking several instances are all this one
  kind of reference), refcount > 0 and it **must never be deleted**. **A mount row's refcount
  directly carries §5.C's Volume exclusive lease** (the mechanism belongs to §5.C). **Volumes have
  lineage too** (the source of a snapshot / clone, §5.C): it is the same shape as Template lineage —
  references point upward and the referenced cannot be deleted — but it holds one more thing:
  deleting a source while live children exist would **leak a backend quota row**, so that RESTRICT
  is not merely policy, it is what makes "delete the sharers first, then the source" the only
  reachable order (see [`deep-dives/volume.md`](./deep-dives/volume.md) D6.1a / D7.1).
- **Reference-reclamation atomicity (one dynamic invariant)**: "refcount > 0 must never be deleted"
  above is only a static criterion, and one more invariant is needed to close the
  **reclaim-vs-reference race** — otherwise there is the classic TOCTOU window: GC reads some
  Template layer's refcount as 0 and is about to reclaim it, while at that same moment a new sandbox
  references that layer (every create-from-Template references a Template layer, and a fork merely
  has the referenced layer be one Promote produced). The invariant: **"creating a reference"
  (create/fork/Promote referencing a Template layer, building a mount row referencing a Volume) and
  GC's "no references → reclaim" decide-and-delete must be serialized by the same authoritative
  store** — reusing §4.4-C's store atomic-commit contract directly, inventing no new mechanism.
  Under a derived refcount this degenerates into **a foreign-key constraint**: a reference is a row
  pointing at the target, and reclamation is a `DELETE` on the target. So there is no intermediate
  window: if the reference row lands first, the `DELETE` hits "referenced" and is refused (the
  reference lands on a live layer); if the `DELETE` commits first, the referencing insert hits "the
  target does not exist" and fails. **An authoritative single-node store (§1.1) is the premise**:
  both reference and reclamation are row operations on it, and a single serialized writer keeps them
  from interleaving, so **no explicit CAS is needed** — the race falls out of the schema. And
  Template lineage is **acyclic**, so store-authoritative existence checks are used uniformly, with
  **no separate mark-and-sweep collector and no sequence-numbered GC barrier** (the latter being
  compensation logic for a transactionless store, whereas the atomic-commit contract has already
  closed the race). **"Referenced means refuse the delete" is a contractual promise of the store
  interface** (a seam, §1.3): SQLite delivers it with FK RESTRICT, Memory with a Go check,
  isomorphic to §4.4-C's atomic commit — backend differences do not leak into correctness.

### 4.4 Concurrency and failure semantics of transitions

§4.1's state machine is **imperative**, and an imperative state machine has three classic fissures:
"a transition fails halfway and leaves dirty state", "two verbs fight concurrently", and "the state
changed but the event was not published (or vice versa)". This engine uses a single-node embedded
store (§1.1) with the placement/IP as the epoch (§3.3), and this section catches those three
fissures with three invariants — introducing no `failed` intermediate state and no counter.

**The four states (§4.1) get no failure state, and that is a conclusion of the model rather than an
omission.** Transition by transition, not one of them can get stuck in a "cross-subsystem dirty
intermediate state":

- **A resume / create cold start failing**: the canonical cold-start chain (§5.E.1, see
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5) materializes
  Rootfs/Volume/Network first (①②③) and then brings runsc up (④) — and **mounts are taken from the
  OCI spec when runsc starts and cannot be changed live** (§2.5), so a Volume is not "mounted after
  the process starts" but a component of the bring-up action, and **there is no intermediate state
  of "the process is up but the Volume is not mounted"**. Failure lands in only two places: ①②③
  failing to materialize (runsc has not been touched and the persistent state is untouched), and
  ④/⑦ failing to bring up or to probe. Both **roll back to the source state** (resume→`paused`,
  create→delete the record), leaving no `failed`.
- **A pause failing to quiesce**: the drain barrier is an optimization rather than a correctness
  precondition, so each of its degradation paths (drain timeout → preemptive freeze → a hard kill in
  the worst case) lands on **a legal `paused`** (§4.2). So pause has no dirty intermediate state.
- **A destroy failing to delete bytes**: the state transition (marking `destroyed`) and byte
  reclamation (deleting the Overlay/Volume) are **decoupled** — the transition is a control-plane
  action, and after a byte deletion fails the **orphan-unit sweep** (§4.4-A's reverse
  reconciliation, which is not the same thing as refcount GC) retries asynchronously. A failed byte
  deletion neither blocks nor rolls back the state transition, and destroy is inherently idempotent.

From this follow three invariants, catching the three fissures above (A catches dirty state after
failure, B catches concurrent fights, C catches state-versus-event inconsistency):

#### 4.4-A Invariant A — a failed transition rolls back atomically to the source state, half-built placements are reclaimed, and no compensating transaction is introduced

The persistent state lives only in the Overlay artifact and data Volumes (§4.2), and **no transition
failure damages it**. The only thing to clean up on failure is **the placement this transition
half-built** (netns/IP, the nft rule set, the overlay mount) — and that is already part of §3.3's
"a placement is rebound at every resume and the old one is void", not added compensation logic. So
the failure disposition is **reclaim the half-built placement + return the control-plane state to
the source state**, not a cross-subsystem saga/rollback.

**A's crash variant — the orchestrator itself crashing mid-transition, with startup reconciliation
performing the same reclamation afterwards, and no intent/phase log written.** The reclamation above
is `defer`-style within the process; when the orchestrator crashes halfway (say a resume has already
built netns/IP/nft/overlay-mount while the store still sits at `paused`), the defers cannot run and
an **orphan placement** is left behind. **The store's state is not ambiguous** at that point —
§4.4-C's atomic commit guarantees it sits only at a clean source or target state; what is missing is
merely that batch of half-built placement-local resources on the host (non-persistent objects, which
neither §4.3's refcount GC nor §4.4-C's outbox scans).

**The way this is caught is reverse reconciliation, not writing a forward "intent + phase" log and
replaying it** (the latter being exactly the durable multi-phase transaction rejected at the top of
this section). The criterion: a placement is derived from the store's authoritative state and is
discardable, so enumerate this host's placement resources, and any that **does not support a sandbox
that should hold a placement according to the store** is an orphan, reclaimed per this invariant. An
authoritative single-node store (§1.1) degenerates this reconciliation into **one local scan diffed
against one local table**, with no phase records, orthogonal to placement/IP-as-epoch (§4.3). A
half-built workdir/upper orphan is one special case of it (rootfs-overlay D5). Two ways to run it:

- **Startup reconciliation**: the process has just crashed, every lock is gone, and every in-progress
  resume is already dead, so identification needs no lock.
- **Periodic reconciliation** (run while the orchestrator is alive) = **the same** reverse
  reconciliation plus exactly two things: ① `TryLock`-skip **locked** sandboxes — an in-flight resume
  **holds the transition lock throughout** (§4.4-B), holding the lock means building is under way,
  and not skipping would wrongly reclaim a live placement being built; ② protect the live placements
  this process built with its own hands in the live-map from being taken for orphans.

The live placements of surviving `running` sandboxes are never orphans; they belong to the takeover
half below. The trigger timing, lock coordination, host resource enumeration, and batching strategy
are in [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D4.

**Reclamation ordering: the live path uses strict reverse dependency order, startup reconciliation
puts a forced teardown first.** A live `destroy`/`pause` holds in-process placement handles and tears
down cleanly in strict reverse dependency order; **startup reconciliation has no in-process handles**,
only a local table, so it force-tears orphan mounts (without blocking on a busy mount), and **the
upper is not touched at all** (§4.4-A does not touch persistent state). The two paths' orderings
differ deliberately — the live path aims for cleanliness, startup reconciliation for "tear it all
down in one scan". **The precise reverse dependency order belongs to §5.A** (it holds the
placement-local resources and therefore their construction order), and this section does not
duplicate that ordering; the model layer needs only two properties: every step is idempotent and
best-effort, so even a half-built placement can be torn down cleanly; and **the overlay's umount is
not on that chain**, belonging to the mediator's side (§4.4-A does not touch persistent state). The
reference to reaper-health D4 above is only for **trigger timing and lock coordination** (when
periodic reconciliation runs, how `TryLock` skips locked sandboxes); the ordering is not there.

**The other half of A's crash variant — retaking handles on surviving `running` placements, which is
not reclamation.** What is missing after a crash is not only orphans: the orchestrator's in-process
placement handles (endpoints, provider-private handles) evaporate with the process while the
underlying isolation unit is still alive. **If only reclamation is done and takeover is not, the
consequence is not dirty state but loss of contact**: the store says `running` and the container is
still running, but the engine cannot get a handle — after which a destroy would fail to tear down a
live container (a leak) and the transport proxy would fail with "no live placement". So
reconciliation is **dual**, in two halves:

- **Half-built placements that are not `running` → reclaim** (the previous paragraphs).
- **Surviving placements that are `running` → retake**: the provider re-recognizes its own resources
  by SandboxID, rebuilds the handle, and rediscovers the endpoint.
  **A takeover is not a resume** — the same placement, the same IP, and no new epoch (§3.3/§4.3),
  because the isolation unit never restarted.

Two orchestration semantics: ① **the liveness decision is aligned with cold start's ⑦ readiness
gate** — L1 alive but L2 not answering (a `created`-but-not-yet-listening unit, say) is not a
takeable live unit, and is demoted to `paused` per §4.2's "a crash = an involuntary pause"
(recoverable through a cold start at the next resume), rather than filling the live-map with a dead
handle; ② **the failure disposition is skip-and-log + be conservative about an uncertain surviving
`running`**, that is, protect its placement from reclamation — failing to tear down a live container
is irreversible, whereas a leaked placement is collected at the next startup, and reconciliation is
idempotent so a skipped item comes round again. Re-recognizing survivors requires the provider to be
able to enumerate its own placement-local resources by SandboxID, and that mechanism (the
`Adopt`/`ReclaimOrphans` signatures, why annotating with the SandboxID is a permitted reverse
direction under §3.3, the two-layer liveness decision) lands in §5.A and is not restated here. The
single readiness probe during takeover, the **continuous health classification** while the
orchestrator is alive (see §4.3, distinguishing the transport-down transient), and the
implementation of **periodic reconciliation** (with §4.4-B's lock coordination) are in
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D3/D4.

#### 4.4-B Invariant B — one transition lock per sandbox: verbs serialize, with a CAS precondition check

Concurrency exists even under a single-node embedded store: a user `pause` and `resume` arriving at
once, a user `resume` racing the reaper's `evict`, two `pause`s stacking. The arbitration mechanism:

- **One transition lock per sandbox**, acquired serially by every lifecycle verb
  (`create`/`pause`/`resume`/`destroy` + the reaper's `auto-pause`/`evict`) for **the same sandbox**,
  so only one verb advances it at a time.
  **The two non-transition write points are under the same lock**: `PATCH` (editing a Binding) and
  `Promote` (freezing a new layer) are not §4.1 transitions and emit no event (§3.4.5), but they hold
  that sandbox's lock throughout and are therefore **mutually exclusive** with
  `destroy`/`resume`/`evict` — a cap change cannot interleave with a resume in progress, and cannot
  commit while the sandbox is being deleted.
- **A state precondition check (CAS) comes first inside the lock**: every verb declares its legal
  source state (`resume` requires `paused`, `pause` requires `running`), and if the current
  `lifecycle-state` ≠ the expected source state it is **refused** and not executed. **Two verbs are
  deliberate exceptions rather than omissions**: `create`'s ID is newly minted and there is no source
  state to compare against; `destroy` is legal and idempotent from all three of `running` / `paused` /
  `creating`, so there is no single "expected source state" to compare-and-set — its serialization
  rests **entirely on that lock**, not on a CAS. So "pause and resume arriving together" = whoever
  gets the lock second faces a changed state and fails the CAS. "resume racing evict" = whoever takes
  the lock first wins, and **how the loser waits depends on who issued it**:
  - **User verbs** (`create`/`pause`/`resume`/`destroy`) **block on the lock**, take it once the
    winner's whole transition has settled, and redo the CAS against **the changed state** (if resume
    won, the state is already `running`; `destroy` has no source-state CAS, see above).
  - **Verbs the reaper issues** (`auto-pause`/`evict`, and periodic reconciliation) **do not block**:
    they **try the lock** (`TryLock`) and on failure **skip the round**, leaving it for the next one
    (reaper-health D3). The reason is that the reaper is periodic with no caller waiting on it, while
    blocking on the lock would let one long resume stall the entire sweep; a sandbox it skips is
    naturally reclassified next round, and by then the state is current.
- **The lock's hold interval = the whole transition, not just the CAS instant.** For a long
  cold-start verb like resume/create, the lock is held from the CAS until ⑦ flips it to `running` (or
  a timeout unwinds it to the source state) — **it is not released early, before the guest is
  listening**. So "who is materializing the placement / mounting the Overlay" and "who may change
  this sandbox's state" fall under the same lock: while a resume is materializing, a concurrent
  `destroy`/`evict` cannot take the lock and cannot delete an Overlay artifact that is being
  mounted/cold-started. This closes a data race that would otherwise be wide open (a destroy is legal
  from `paused` and its CAS would pass, so with the lock already released it would delete the bytes
  under an in-flight resume's feet — and refcount does not catch it, because an in-flight mount is
  not a persistent reference row).
- **Ownership**: the lock and the CAS are the §4 mediator's orchestration obligations, and the store
  only provides the **conditional-write (compare-and-set)** mechanism on `lifecycle-state` — which
  does not violate "subsystems do not call each other" (top of §4), because the serialization happens
  at the mediator layer rather than by having one subsystem call another.
- **The lock, held throughout, already gives exclusive possession of the entire resume, so the
  correctness of concurrent verbs fighting lives entirely here**; §4.3's "classification is suspended
  during the in-flight period" is another consequence of this same lock (the reaper's try-lock skip)
  rather than a second guard, and it is load-bearing only for efficiency and observational
  cleanliness.

#### 4.4-C Invariant C — the state commit and the event publication are one atomic commit; one algorithm, with backend differences settled behind the store contract

Every §4.1 transition must publish an event to the event stream, billing-class subscribers keep
accounts from it, and **losing one loses an invoice**. The dangerous approach is letting the judgment
"does this backend support transactions" leak into the publishing algorithm and split it into N
correctness regimes. The lawful approach is a standard application of §1.3's seam/invariant duality:

- **The algorithm is written exactly once**: a transactional outbox + at-least-once delivery +
  idempotent deduplication by the subscriber. The state transition and the "event to be published"
  record **land in the same commit**; a publisher reads the outbox asynchronously, sends, and marks
  it sent; the event carries **a unique transition ID** by which subscribers deduplicate. Better to
  resend (at-least-once) than to lose one.
- **The store interface promises it, the backend delivers it**: the store is a Behavior seam (§1.3),
  and it **settles the minimum guarantee the algorithm needs into one contract** — "a
  `lifecycle-state` change + an outbox record appended = one atomic commit". **The interface method
  itself promises that atomicity and the backend must deliver it**: a backend with transactions uses
  a local transaction, one without multi-key atomicity falls back to "write the state and the event
  into the same document/single key". **It must never be designed as "the backend reports whether it
  supports transactions and the algorithm branches on that"** — that would leak backend differences
  into the algorithm's logic and split correctness. Swapping backends must not touch this one
  algorithm, and that is exactly what the seam is for (register an implementation, do not change
  existing structure).
- **The engine does not track subscribers**: it only guarantees that "the event is durably written to
  a replayable stream"; resending after a disconnect and tracking a consumption position are the
  subscriber's own business (§1.3 add a subscriber).

**The sequence-number contract: a total order + no visibility gap, both promised by the store
interface.** Every outbox row carries a **monotonic integer `seq`**, and that is the ordering key and
the cursor — `occurred_at` (unix nanoseconds) **is not a total order**: same-nanosecond values have
no tie-break, and the wall clock can be stepped backwards by NTP, so it cannot serve as a resumable
cursor. Two promises:

- **Total order**: the `seq` obtained by each append is unique and strictly increasing, and reads
  return in `seq` order.
- **No visibility gap**: once `seq = N` is visible to a reader, no row with `seq < N` appears
  **afterwards**. This is the **entire basis** for "a subscriber that has consumed up to N resumes
  from `after=N` without loss".

The second is the **non-obvious** one, and it is a real defect class rather than a hypothetical one:
if the number is assigned at INSERT while the row becomes visible only at COMMIT, two concurrent
producers interleave — tx1 takes 5, tx2 takes 6, tx2 commits first, the reader consumes 6 and advances
its cursor, and 5 then appears **after an already-delivered later event**, with no cursor ever looking
back at it. **Note what does not save it: serializing the readers.** What breaks is the producing
side, so locking on the reading side would only observe a stream that is "perfectly ordered and
already missing a row".

Per invariant C's lawful approach, **this is written into the `store.Store` contract alongside the
atomic commit** and must not be some backend's incidental property: SQLite delivers it through
**serialized write transactions** (with another write transaction open, a second INSERT gets
`SQLITE_BUSY` outright, so "whoever took the lower number commits later" cannot arise), and Memory
delivers it by **both allocating the number and making the row visible inside the same critical
section**. A backend that permits genuinely concurrent write transactions (Postgres with a
`BIGSERIAL` default, say) **must add a watermark gate** — visibility released only once all lower
numbers have committed — or it does not satisfy this interface; the contract tests carry an
adversarial case pinning exactly this, so backend differences **break in the contract test rather
than during a billing reconciliation**.

**Outward = pull only (a cursor-paginated GET), in the control plane.** The shape and status codes
are in §6.1's `GET /v1/events`. The criterion is that **the engine must not hold per-subscriber
state** (§1.3): a pull surface by construction has no subscriber table, no eviction policy, and no
cleanup path, whereas a genuinely fan-out push needs all three immediately. If push is wanted later,
it is **a presentation-layer adapter that server-polls the same pull query**, sharing the same `seq`,
and so costs nothing structurally — **"push as an adapter" holds no state, "push as genuine fan-out"
does, and that is the dividing line**.

**Head-of-line blocking: kept, and this is the correct side.** One undeliverable event blocks
everything behind it. Because this section's opening "losing one loses an invoice" means **order
carries semantics** ("pause then resume" and the reverse are two different invoices), skipping the
failing one trades **a visible stall** for **a silent mis-accounting**. The converse also explains why
we cannot copy someone else's skip-style design: skipping is tolerable only when the sink is
**idempotent by construction and versioned by logical time** (a late duplicate loses at merge time
and does not resurrect an old state), and our sink has no such cushion.

**But "a stall" is a trade-off, while "an unbounded, unalarmed, invisible stall" is a defect**, so
three operability properties are hard requirements (the mechanisms all land in `store.Publisher`, and
this section only states the requirements): ① **a poison-envelope exit** — retries have a count
bound, past which retrying stops and a signal that must be noticed is latched, and clearing it is an
operator action that is **not automatically released**: automatic release would turn the bound back
into the infinite retry it exists to replace; ② **backoff has both a ceiling and jitter** — the two
fix different faults: unbounded exponential backoff schedules retries into the far future, where the
event is neither delivered nor alarmed, while without jitter several drainers collide on the same row
at the same expiry instant; ③ **observability does not share a source with the loop being observed**
— sampling inside the drain loop itself means the numbers stop updating at precisely the moment it
wedges, so "a wedged worker" and "idle" export the same stale value and are indistinguishable;
therefore the sampling point is independent of that loop, and **stuck in the loop** versus **stuck in
the sink's delivery call** must be separately visible.

**Capacity-pressure events (a soft threshold): not emitted, and no event kind is added.** §2.3 /
rootfs-overlay D6.5's hard-threshold behavior is unchanged (the guest sees an ordinary `ENOSPC` and
the sandbox survives). A soft threshold is **not emitted as a §4.1 event**: capacity pressure is **a
sample of a continuous quantity**, whereas the §4.1 event stream is a stream of **discrete state
transitions** — stuffing the former into the latter would either fix an unarguable threshold to
binarize a continuous quantity, or let event frequency jitter with load, and both would contaminate
the "every event = one transition" property that lets subscribers reconcile. To monitor it, attach
externally as a gauge / by sampling (§1.3 add a subscriber, the Consumption seam), not as an engine
primitive. External evidence points the same way: both reference implementations expose only gauges /
samples, and nobody emits pressure as an event.

**The retention window (trimming)**: **no trimming today; rows are only marked, never deleted.** The
cost, stated plainly: the outbox's at-rest size grows without bound, which is **a known limitation**
rather than a virtue of this decision. **A consequence**: `earliest` is always the smallest sequence
number, so §6.1's 410 is unreachable today — the response still carries `{earliest, latest}` and the
410 is written as reserved semantics, so that when trimming is genuinely added the caller contract
needs no change.

**The mechanism is settled; only its value is not, and the two must not be conflated.** The window's
value must be argued from "the reconnect duration promised to external subscribers", and the engine's
own sink is a logging reference implementation whose "delivered" is very nearly always true, so **there
is no arguable value** — picking a number out of the air is the speculative reservation §1.3 refuses,
and it stays open (the event-stream row in [`open-decisions.md`](./open-decisions.md) §2). But **an
absent mechanism is itself a decision, and it is the one that cannot be revisited at deploy time**, so
the mechanism's shape is fixed here. Five properties, each of which has a wrong answer available:

- **Trim by the age of DELIVERY, never of occurrence.** The poison-envelope exit above does not release
  itself, so a halted stream can be resumed days later and deliver its backlog with fresh delivery
  timestamps but ancient `occurred_at`. Ageing on the latter would make that backlog **instantly
  trimmable at the moment of first delivery** — deleting events no subscriber ever had the chance to
  fetch, which is the one failure at-least-once exists to prevent.
- **Delivered rows only — and that floor is STRUCTURAL rather than a check to remember.** The publisher
  drains in `seq` order and stops at the first failure, so the delivered set is always a **prefix** of
  the sequence; adding the delivery predicate makes the trimmable set a prefix of a prefix. Trim
  therefore **cannot** outrun the publisher, and it must **not** consult the publisher's halt latch to
  achieve that: the halted head is by definition undelivered, so the natural predicate already stops
  below it, while a latch check would let a long halt silently void the configured bound and would put
  a runtime dependency on the publisher where no invariant needs one.
- **Never trim the newest row.** Trimming a table *empty* makes `earliest` and `latest` both read 0 —
  **indistinguishable from "nothing ever happened"** — so a stale cursor is neither served nor refused,
  and the caller reads "I am caught up" and silently skips everything trimmed. That is precisely the
  silent short page §6.1's 410 exists to prevent, arriving through the mechanism meant to activate it.
  Retaining one row keeps the window's boundaries truthful at negligible cost, and is what a
  segment-based log does structurally by never deleting its active segment.
- **`after=0` must never 410.** Zero is contractually "from the start of the retention window", i.e. an
  explicit request for whatever is retained; a naive `after < earliest` test fires on it for every
  trimmed outbox and breaks the documented cold-start cursor.
- **`seq` must not go backwards, and emptying the table is the case that tests it.** The sequence's
  high-water mark must survive a table emptied by trimming, or a subscriber's cursor is pointed past
  freshly-issued numbers (the schema already defends this deliberately; the defence now has a reachable
  case and must be asserted, not assumed).

**The default is OFF, and that follows an existing house rule rather than timidity**: the same refusal
as the node capacity budgets, which are deliberately not defaulted because the engine must not invent a
number and start acting on a guess. Stated without softening, the choice is between two failures — off
ships the unbounded at-rest growth above, while a finite default ships **deletion of billing-relevant
history on a number nobody chose**. The first is visible, bounded and recoverable; the second is none of
those. External precedent splits the same way: the systems with an opinionated time default are
general-purpose logs whose disk is the operator's problem, while a replication mechanism whose consumer
matters ships *unlimited* retention and makes bounding it opt-in.
