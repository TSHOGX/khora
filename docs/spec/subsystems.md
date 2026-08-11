# Sandbox Engine — subsystems (§5 target definitions)

> Belongs to [SPEC.md](./SPEC.md); § numbers run continuously across files, and the
> navigation is the "Document map" at the top of SPEC.md.
> The five subsystems are peers depending only on §3's shared model and not on each other —
> hence one document for all of them.

---

## 5. Subsystems (target definitions)

The five subsystems are **peers**, depending only on §3's shared model (sandbox identity + the
store) and **not on each other** (which prevents cycles). This section fixes their targets and
boundaries; internal mechanisms still to be refined are registered in
[`open-decisions.md`](./open-decisions.md).

- **5.A Runtime** — the runsc process lifecycle and materializing the three states (*how* a
  sandbox runs).
- **5.B Rootfs** — the Template (read-only lower) + the writable Overlay artifact; pause/resume
  and Promote both hang here (*what* a sandbox is).
- **5.C Volume** — decoupled, ID-addressed storage with an independent lifecycle (the additional
  "which independent data is mounted").
- **5.D Network** — the network reachable set each sandbox may declare, and the rule set that
  delivers it (where a sandbox *can reach*, and *who can reach it*).
- **5.E Transport** — the host↔sandbox channel: two general capability services (process / fs),
  plus a `Health` probe for the engine's own use.

> Rootfs and Volume are two subsystems: an **Overlay artifact**'s lifecycle **follows its
> sandbox**, it references some Template, and it is the frozen layer pause/resume works on; a
> **Volume**'s lifecycle is **independent**, surviving before and after any sandbox, with its own
> management API and backend.
> Both are ID-addressed, but they **do not share one materialization seam** — §5.B's
> `rootfs.Provider` materializes layered composition (multi-lowerdir + upper/work + Promote),
> while §5.C's `volume.Backend` materializes a self-sufficient storage unit (bound directly, with
> no composition); they share only the underlying storage substrate primitives (btrfs subvolume /
> squota / reflink). Reclamation differs too: an Overlay references a Template and is reclaimed
> with its sandbox, whereas a Volume references nothing and is reclaimed independently.

### 5.A Runtime (the execution substrate)

- **Target**: make a single sandbox persistent, mutable, root-capable, and cold-startable from a
  Template.
- Provides the **runsc process-level start/stop mechanism** (driven by §4's
  create/pause/resume/destroy verbs), materializing the three-state model (§3.1). Verbs belong to
  §4 and this subsystem provides only mechanism (top of §4).
- The guest defaults to **root**; the engine does not model guest users, and "run as some user" is
  native to the guest (`su`/`sudo`) rather than an engine primitive (§2.2 / §3.2.1).
- **Materializing resource ceilings (the rate axes + RAM)**: a sandbox's rate ceilings (CPU; disk
  I/O is the same axis and joins in the same shape once it has a consumer) and its **RAM** ceiling
  are Bindings (§3.4), materialized by this subsystem at every placement — the single-node provider
  lands them on cgroup (for CPU **both the ceiling and the share are written**: `cpu.max` +
  `cpu.weight`, both derived from `cpu_millis`, reasoning in §3.4; RAM lands on memcg), while
  another provider lands them as its scheduling layer's equivalent resource constraints (the share
  corresponding to its scheduling weight). **The disk capacity ceiling is not here** — that is a
  storage-layer quota, the per-sandbox one belonging to §5.B (the Overlay upper+work) and the
  per-volume one to §5.C.
  - **When the disk I/O rate axis is implemented, the whole-disk MAJ:MIN is derived by the provider
    itself** — the same shape as RAM's overhead: the contract carries only the rate intent, and a
    device number is a provider-internal detail. **When implemented**, `local_runsc` will look up
    the backing whole disk from its own state dir (`io.max` wants a whole disk, and giving it a
    partition number fails the launch outright), and that value never leaves the local provider; a
    remote provider expresses the rate cap through its scheduling layer's equivalent.
    **A "host topology configuration item" is rejected**: whether it lands in the Binding or in the
    provider's inputs, it lifts a host primitive onto the cross-provider contract surface (the ❌
    list below) — a device number is a thoroughly host-local fact, and a remote provider cannot
    produce a whole-disk device number at all.
    That prohibition is pinned structurally by the token guard in
    `internal/runtime/seam_test.go` (`major`/`minor`/`majmin`/`devno`).
    - **Which seam the axis lands on is itself open, and the assumption that it is this one should not
      be inherited silently.** The external shapes point the other way: a microVM's rate limiter
      (bandwidth and ops token buckets) hangs off the **drive** handle beside its host path, and cloud
      block storage attaches IOPS **per volume** — i.e. the cap may belong on §5.C's seam rather than
      on `ResourceLimits`. Nothing changes today (there is still no consumer, so adding the field
      either place is the reservation §1.3 refuses), and **the device-number prohibition above holds
      regardless of which seam wins** — but a rate limiter is a property of a storage attachment at
      least as naturally as of a scheduling layer, and one such shape cannot render it universally
      either (it must be omitted where the block backend enforces instead).
- **How the RAM cap is apportioned is a provider-internal detail**: the interface says only
  `MemoryByte = N` (§3.4 Binding), and the provider decides for itself how to materialize "N bytes
  of RAM". Internally `local_runsc` sets the guest-visible RAM to N and the host ceiling to
  `N + overhead` (the overhead being **its implementation constant** rather than a contract member
  — a remote provider's overhead is composed completely differently: possibly a VM balloon,
  possibly its scheduling layer's memory request). **Not splitting it this way is a spec bug**:
  setting both to N tells the guest N while terminating it below N. It therefore also **does not
  enter node capacity accounting** (§3.4: the inequality counts only bytes the engine can name).
  The **determinism** of termination (whole-group, not the kernel picking a victim) is likewise a
  provider obligation, see §2.4.
  - **Two hard constraints** (unheld, the cap is worthless): ① **the limit must be paired with
    swap** — only an OCI `swap` equal to the limit (⇒ v2 `memory.swap.max=0`) is a hard cap, so
    after raising the host ceiling to `N + overhead` **swap must be pinned back to 0**, or that
    headroom becomes swap-backed again; ② **do not use `memory.high`** — `swap.max=0` plus
    unreclaimable anon turns "throttling" into a hang, so only a hard limit + `oom.group=1` is
    used.
- **Startup**: a cold start from a Template (seconds-scale), on top of which off-the-shelf restore
  / startup optimizations may be layered. The container entrypoint is `sandboxd` (in init mode as
  PID 1, §5.E.1) — this subsystem is only responsible for "bringing it up as PID 1", and the
  in-guest init/reaper/re-exec topology belongs to §5.E.1 and is not developed here.
- **The runtime provider is this subsystem's core seam**: "where, and with what, to run an isolated
  process context" is expressed through a pluggable provider interface. The interface must be
  expressed in **policy + handles** rather than **host primitives** — otherwise it is bolted to a
  "host-local" assumption and no non-local provider could ever take it up. **This is the general
  rule for every storage/execution seam, and this is its authoritative statement** (§5.C's
  `volume.Backend` holds it too, inheriting the list verbatim):
  - ❌ Leaking host primitives: "give me a cgroup path / a netns fd / the Overlay's **internal**
    host absolute paths (upper/work)" — an isolation unit scheduled onto a remote node simply cannot
    produce those.
  - ✅ Contractual: `(rootfs source, reachability requirement, resource ceilings) → (placement
    handle, transport endpoint, lifecycle control stop/start)`. **The `rootfs source` is an opaque
    handle**: under `local_runsc` it happens to materialize as an already-merged rootfs path, and
    under a remote shape it would be a different handle — what is forbidden is only the **internal**
    upper/work path of the previous bullet, while an already-merged rootfs handle is a legal input.
    **The reachability requirement (contract member name `SteeringSpec`)** is the **abstract rule
    set** §5.D derives from the declaration: the allow set + the L0 floor + publish endpoint
    descriptors + default-deny, and **not the declaration itself** — the provider receives the
    derived result and holds no authority over the declaration. **Rendering to nft text, and binding
    `iifname` / the guest IP / the publish host endpoint, all happen inside the provider at
    placement time**: those three are products of the placement and are host primitives only the
    provider holds, so lifting them onto the cross-provider contract surface would be exactly the
    leak in the ❌ list above (the precise division is in §5.D).
  The sole implementation `local_runsc` renders and materializes the reachability requirement as nft
  + netns, and hands §3.4's resource ceilings to runsc through **OCI `linux.resources`** to land on
  cgroup (the cgroupfs driver). A provider is **the materializer of the rule set and the resource
  ceilings** and holds no authority over the declaration (§5.D).
  - **Installing the enforcement mechanism must be atomic with the placement, and this is an
    acceptance-testable provider obligation** (§2.4): the rules go on **after the netns/veth are
    built and before the isolation unit is brought up**; a half-built failure goes through §4.4-A's
    reclamation. The same obligation extends to `Adopt` / `ReclaimOrphans` (the reconciliation
    mechanism below) — the rule set is crash-surviving host state, so taking over a live placement
    must rebuild it along with everything else.
  - **The predicate must bind a fact the guest cannot change** (pillar ④): `local_runsc` lands it as
    **an ingress-interface predicate on the host-side veth**. Matching by source address **has been
    measured to be forgeable by the guest** (network.md D5 ③), so this is a security assertion
    rather than a stylistic choice; putting it inside the sandbox netns does not work either
    (measured in the same place: guest packets do not traverse those hooks). **That predicate must
    be written as `iifname` (by name), not `iif` (by ifindex)**: `iif` freezes the name into an
    integer at load time and **silently fails open** after a same-name veth rebuild — measured,
    packets are delivered while the drop counter reads 0 (D5 ⑫). The two look identical in
    `nft list`, so this one is held by a rendering guard rather than by review.
  - **The network placement mechanism**: runsc with `--network=sandbox` **only adopts an existing
    netns and never builds one**. So `local_runsc` first builds the netns + veth and **allocates the
    guest-side IP**, then hands the netns to runsc through OCI
    `linux.namespaces[type=network].path`; after joining, the sentry **absorbs the kernel-side L3
    address into its own netstack** at boot (after boot that address is not readable from the kernel
    side, while the veth link remains). Consequently step ⑥ of guest-runtime D5.1's cold-start chain
    is **not a post-boot probe** for local_runsc: the launch path uses the engine's own allocation
    directly, and the `Adopt` path reads the /30 the **host-side** veth still carries and adds 1
    (the allocation pins the guest at host+1), so the endpoint is rebuildable after a crash restart
    without asking the guest. Every resume **builds a new netns** (§3.3, a new IP), so there is no
    "reusing a netns requires reloading the address" problem.
  - **cgroup ownership: the engine builds the parent layer, the leaf belongs to runsc** — runsc
    creates only the last segment of `cgroupsPath` and requires the parent to exist already,
    **and it writes limits only on the leaf it created itself** (if the engine pre-creates the full
    path, the limits **silently fail to take effect while the launch succeeds as usual**); so the
    engine `mkdir`s the parent layer and opens its `subtree_control`, leaves the leaf for runsc to
    create and reclaim at `delete`, and **asserts after launch that the limits really did take
    effect**. The parent layer is a **flat naming container** rather than a kubelet-style cgroup
    driver hierarchy (no per-sandbox intermediate level is created on its behalf — recreating that
    layer is what §1.1 rejects). Consequently **an orphan CID-cgroup is itself a scannable
    orphan-placement signal**, backstopped by §4.4-A's reconciliation.
  - **The reconciliation mechanism (the provider face of §4.4-A's crash variant)**: besides
    `Launch`/`Stop`/`Destroy`, the provider exposes two more enumerate/take-over/reclaim mechanisms,
    driven by the §4 mediator's **startup and periodic reconciliation** (one mechanism, with the verbs
    still belonging to §4):
    - `Adopt(SandboxID) → (placement handle, alive, cause of death, err)` — **re-recognize** that
      sandbox's own placement-local resources by SandboxID, rebuild the **opaque** handle, and
      rediscover the transport endpoint (**the same endpoint, no new epoch**, because the isolation
      unit never restarted, §3.3). Three mutually exclusive outcomes: **alive with the endpoint
      recovered** → hand over the handle; **definitely dead / nonexistent** → `alive=false`, **handing
      out the cause of death along with it** — this is the last moment the placement is still intact,
      and why the signal is volatile and why an undecidable case can only report `Unknown` are in
      §4.3; **L1 alive but the endpoint unrecoverable → report an error, not a death** — a provider
      must not collapse "uncertain" into `alive=false`, or the mediator would demote and tear down an
      isolation unit that proved itself alive (§4.4-A: be conservative about uncertainty). The
      right-hand side is still **policy + handles**, passing the ❌ host-primitive list above — the
      cause of death is an enumeration, not an errno / cgroup file.
    - `ReclaimOrphans(keep []SandboxID)` — tear down every engine placement not in the keep set (in
      reverse dependency order: runsc kill/delete → the cgroup leaf → veth → netns; the overlay umount
      belongs to the mediator's side, since subsystems do not call each other). The keep set is
      computed by the mediator from the **authoritative store** and fed in (the provider does not call
      the store in reverse, top of §4); persistent state (the Overlay upper) is **never touched**
      (§4.4-A).

    Accordingly, **placement-local resources must carry a SandboxID label** (`local_runsc`:
    netns=`sbxns_<id>`, reversible; cid=`sbxc_<id>`; veth=`vh` + the SandboxID's sha256[:12], derived
    one-way) so that the provider can enumerate its own resources (scanning the netns prefix) and look
    them up by SandboxID — this is the **reverse** of §3.3's "identity does not encode placement"
    (annotating which SandboxID a placement serves is allowed; inferring identity back from a
    placement is forbidden). **The liveness decision has two layers**: the provider supplies L1
    (`local_runsc` = `runsc state` ∈ {running,created}) + the endpoint, and the mediator adds an L2
    `Health` probe before treating it as takeable (aligned with cold start's ⑦ readiness gate, §4.3),
    demoting to `paused` if the probe does not get through. The SandboxID naming is also what lets a
    takeover mark a survivor's IP as inUse in the allocator, so the allocator after a restart does not
    hand that IP to a new sandbox.
- **`local_runsc` pins the entire set of runsc flags it depends on and inherits no defaults** — each
  key answers to one established guarantee, `runsc`'s defaults drift across versions, and §5.B's
  **portability across runsc versions** cannot rest on "the default will not change":
  - `--root=<a pinned directory>` — the sole addressing root for all runsc state. Pin it wrongly and
    `state`/`list` are silently empty, so §4.4-A's reverse reconciliation would read every live
    placement as nonexistent; it is therefore a load-bearing key rather than a path preference.
  - `--platform=systrap` — delivers §1.1's no-hypervisor; the platform itself is pluggable (another
    provider may choose KVM, §1.1).
  - `--network=sandbox` (the userspace netstack) — `host`/hostinet lets the sentry use host sockets
    directly and passes syscalls straight through to the host kernel, **violating §2's substrate ①**
    (hence rejected, not "an alternative mode"); and it is the precondition for §5.D's enforcement
    (a per-sandbox netns + nft on the host-side veth) and §5.E's transport (netstack-terminated, with
    the host dialing through the veth IP). **The cost: it refuses `--rootless`**, so the
    orchestrator/launcher must be root (or hold CAP_NET_ADMIN+CAP_SYS_ADMIN) to materialize a
    placement — **the sentry itself remains unprivileged and pillar ② is not broken**; this is a
    deployment caveat (§1.1's "can run inside CI / a container" = that environment must grant these
    caps), not a concession on isolation.
  - `--overlay2=none --file-access=shared` — §5.B/§5.C's persistence layer is managed by the engine
    outside runsc, and the gofer only proxies host directories.
  - `--net-raw=false` — runsc's default already denies raw/AF_PACKET sockets (requesting
    `CAP_NET_RAW` in the OCI spec does not obtain it either; measured, that bit is clear in the
    guest's `CapEff`), so unless pinned, "the guest cannot get a raw socket" is **merely an
    undeclared default**: should it drift across versions, pillar ④ would face a guest able to forge
    layer-2 frames — and what the ingress-interface predicate rests on is precisely "the guest cannot
    change which interface it appears on", with no test that would ring. `CAP_NET_RAW` is therefore
    also **not in the request list** — requesting a cap that cannot possibly be obtained is a false
    invariant, making a reader believe it was granted deliberately.
  - `--systemd-cgroup=false` — following the cgroupfs driver above.
  The exact argv is a provider implementation detail and does not enter this section; this section
  fixes only "which keys are load-bearing, and which § guarantee each answers to".
- **Observing CPU usage requires summing over all of `cgroup.procs`**: systrap runs guest threads as
  **separate host thread groups**, so sampling only the sentry's `/proc/<pid>/stat` underestimates
  badly. This constrains how it is measured, not enforcement (throttling is delivered by `cpu.max`).
- **Not included**: what a rootfs is composed of and freezing it (→ B), Volume mount semantics (→ C),
  the network declaration itself (→ D), the host↔guest I/O channel (→ E).

### 5.B Rootfs (what a sandbox is composed of)

- **Target**: make "what a sandbox is" persistable, freezable, and freezable into a reusable base,
  with the persistence layer **decoupled from the placement**.
- **Structure**: rootfs = the Template (read-only lower) + this sandbox's writable **Overlay
  artifact** (§3.1.1), with the merged result bound into the container as its writable rootfs.
  `apt`/`pip` and `$HOME` all write into the Overlay.
- **One key supporting fact**: **not storing memory (§4.2) ⟹ the persistent state is just plain file
  layers on the host** — which yields for free portability across host / CPU / runsc version
  (§1.5#3), auditability and diffability (readable with host-side `tar`/`diff`), and Promote below.
- **Overlay content is read only at rest**: the Overlay's **file content** is read **only at pause /
  stop** (the container stopped, runsc having flushed) — which is exactly the path a Promote freeze
  takes — and **never** while the guest runs. So the class of live-consistency problems like "reading
  an inconsistent state through the gofer cache while running" is not on the path. After a clean
  stop the upper is an ordinary host directory tree, and a cold start remounts it byte-identical
  (§2.5).
  **This constrains content, not capacity metering**: the admission gate must compute node usage
  while other sandboxes are running (§3.4), and that reads the backend's **allocation metadata**
  (blocks already allocated), opens no file, and has nothing to do with the gofer cache, so it is not
  restricted by this rule. Keep the two reads apart: content must be at rest, metering may be asked
  of the backend at any time.
- **The carrier: an outer kernel overlayfs, with runsc not involved in the write layer** (§2.5;
  derivation in [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)): the engine
  assembles lower (the Template) / upper / work outside runsc and hands the merged result to runsc
  with `--overlay2=none --file-access=shared`. **Assembling and unmounting the merged result belongs
  to this subsystem** (runsc receives only an already-merged rootfs path, that is, the `rootfs
  source` input of §5.A's provider, and does not care how the rootfs was assembled); upper/work must
  land on the same overlayfs-capable FS.
- **An Overlay is addressed through the rootfs provider and assumes no host-local path**:
  - Single-node materialization: the Overlay lands as a host overlayfs upperdir and the Template as a
    lowerdir, on the same machine, with pause capturing nothing and resume remounting the same
    upperdir for a cold start (§4.2). The persistent unit is the resident upperdir; see
    [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D3.
  - Multi-node materialization: the Overlay lands as a layer addressable across nodes (a PV / object
    storage), and a resume remounts the same artifact on any node. At the model layer an Overlay is
    always "an ID-addressed frozen layer referencing some Template", and a host path is merely a
    provider implementation detail (§3.1.1).
- **Promote**: freeze the current Overlay into the new Template's lower layer — the writable layer is
  demoted to a read-only lower, which is what makes Templates naturally layered (like a layered
  image). All Promote needs is "read the upper bytes at rest and freeze them into a new read-only
  layer", and the outer overlayfs's upper is an ordinary host directory tree that can simply be read
  (§2.5). **What is read is the at-rest upper after a clean stop** (§4.2's quiescent point:
  the host-side drain barrier first, degrading to preemption on timeout; a cooperative
  application-layer safe point is not a correctness precondition, since the substrate flush already
  guarantees what is needed).
- **A layered Template = multi-lowerdir**: a stack of read-only layers (like an OCI image), onto
  which Promote stacks one more — the new Template's lower stack = `[the new layer, ...the parent
  Template's lower stack]`, expanded along the lineage (the `parent` reference). How layers are
  stored/referenced is decided by the **rootfs provider** and sits **above** runsc's overlay
  mechanism; the abstraction does not vary with storage shape (the optional shapes are in
  rootfs-overlay D3).
- **Bytes-first for immutable artifacts**: the reason is §4.3's "existence is the state" — a Template
  has no state column and the row existing means ready, so its bytes (the new read-only layer
  directory) must be **fully materialized on the backend before the control-plane row is inserted**.
  create-from-Template = referencing an existing layer (whose bytes were already there); Promote =
  write the layer bytes first, insert the Template row second — a crash in between leaves only
  **orphan layer bytes** (reclaimable) and never a broken ready state of "a row pointing at
  nonexistent bytes". **Two orphan sweeps of the same shape**, both reusing §4.4-A's reverse
  reconciliation (rather than deleting bytes via FK) and both protected by an **in-flight build
  keep-set** so that something under construction is not wrongly torn down (the same shape as the
  placement live-map): an **orphan layer** = a layer directory with no corresponding live Template
  row; an **orphan Overlay unit** = a storage unit with no corresponding live sandbox row — which
  catches the half §4.4 left behind with "a failed byte deletion does not block the state transition
  and the sweep retries asynchronously", where the record is already deleted while the bytes remain
  and only a sweep can reclaim them.
- **A Promote captures only the Overlay tree, and excludes what the engine injected**: freezing a
  sandbox's FS = freezing the Overlay tree, but **runtime files the engine itself injected into the
  guest do not enter the layer** (`sandboxd`'s portal / sentinel files; the list is in rootfs-overlay
  D5). Mounted-in Volumes are **not in this tree either** (they are on their own backends, §5.C) —
  the property to hold is **"Volume content does not enter the layer, and neither do pre-existing
  bytes shadowed by a mount point"**, not "the layer definitely contains an empty directory": a mount
  point (`/home/workspace`, say) in the frozen result is **either an empty placeholder directory or
  absent entirely**, both of which are correct, so do not read "an empty placeholder" as a promise
  covering both cases (which case yields which result is in the same D5). Cold start is unaffected —
  `runsc create` materializes a missing dest itself, so §5.B needs no "create an empty directory"
  step.
  - **The set of mount points must be passed in as a per-Promote input and must not be a static
    package-level list**: a Volume dest is a per-sandbox mount row, which a package-level list has no
    way to know. This is also what lets **§5.B never have to ask §5.C** (producing no cross-subsystem
    call) — the §4 mediator holds the mount table already and calls Promote already. **The cost
    forecast uses the same set.**
- **Incidental capabilities of the underlying storage (single-node)**: snapshot/clone are cheap on a
  CoW FS (reflink). This is an additional capability of the single-node provider, not the model
  depending on a host path.
- **The capacity quota is this subsystem's materialization responsibility (the disk capacity axis,
  §3.4)**: the per-sandbox disk capacity ceiling (a Binding) is applied by the rootfs provider at
  placement time to the **Overlay upper subtree + work** (**both landing in the same storage unit ⇒
  one quota, with no bypass path**). The local materialization = **a btrfs subvolume + an squota
  limit**; on exceeding it the guest gets an ordinary `ENOSPC` and **the sandbox lives on as usual**
  (§3.4). **If the cap cannot be delivered, refuse this placement, and never silently degrade to
  "run uncapped"** — otherwise every assertion in §2.3 is trusting a cap that does not exist; and a
  value of 0 (no ceiling) means any residual cap on the unit must be **cleared**, since the record is
  always the sole authority (§3.4). Why squota rather than full qgroups, and the "Template bytes are
  not covered by the cap" cost it brings, are in
  [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6.
  **A Template is read-only, is not written by a sandbox, and gets no per-sandbox cap** — its growth
  is governed by Promote + refcount GC (§4.3) and it serves only as an input to node capacity
  accounting, which is a global accounting problem rather than an isolation problem.
- **The provider reports "bytes not covered by a cap" (the second term of node accounting, §3.4)**:
  the seam reports the total bytes this subsystem holds that are **covered by no declared cap** —
  capped units count 0 (the mediator has counted their caps already), and physically shared bytes
  count at most once. The number must come from **the backend's own allocation statement** and
  **never from the guest** (the sentry fabricates `st_blocks`, and for a sparse file the guest reports
  a block count inferred from size while the host's true value can be 0), nor from apparent size
  (which overestimates without bound for sparse files, and at the admission gate that means
  **refusing a create the node could have accommodated**). The contract is stated in policy language
  and mentions neither subvolume, qgroup, nor directory traversal, so a rootfs provider backed by
  object storage or a network FS can deliver it just as well (each asking its own backend). §5.C is
  isomorphic — per-volume caps go into the first term and their uncapped bytes into the second, and
  **no third term is added**.
- **The provider must also report "how much this one sandbox has used" (the limit side of the cap,
  §3.4)**: the seam reports a **single** Overlay unit's used bytes and the cap on its storage, in the
  same shape as §5.C's `Backend.Usage`. It is not a convenience method but something the write point
  forces: changing a cap must refuse "lowering below current usage" (the criterion being a **strict**
  inequality), while the "uncapped bytes" term above **counts capped units as 0 by contract** and so
  precisely cannot see the unit the cap change targets. The `limit` half is **diagnostic only and
  never used for validation** (the substrate maps "never capped" and "deliberately uncapped" onto each
  other in both directions, and the record is the sole authority); **a quota row that cannot be read
  is an error, and an "uncapped" answer must never be synthesized**.
  **The same criterion also belongs to this subsystem for a second pass on the materialization side**
  (§3.4.5: the write point's criteria must be re-evaluated at the next placement / reinstall / repair
  rescan, and the current usage it reads is **not durable**), and if it cannot be delivered, **refuse
  this placement** — the same rule as "never silently degrade to running uncapped". Why the sandbox
  side is worse than the volume side, and why this side refuses while §5.C's side skips, are in
  [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6.6.
- **The built-in Template's contents are a deployment-replaceable choice carrying no engine
  semantics, and the repo holds its DEFINITION rather than the artifact** (`templates/bare/`:
  a digest-pinned base, the package list, config overlays, and the builder). The built tree
  measures ~730 MB, so committing it would put that in git permanently and store a second
  full copy on every re-pin — the definition is what is small, diffable and reviewable.
  Consequently `--templates` names where BUILT Templates live and not where their definitions
  do — and it defaults to an absolute path OUTSIDE any module tree, because a Template
  carries mode-0700 root-owned directories that make a module-tree package walk fail for a
  non-root user (the builder refuses such a destination for the same reason).
  semantics.** It is listed here because a deployment needs *some* answer, not because the engine
  depends on any of it: the engine's contract with a Template is only "an immutable ID-addressed
  read-only lower layer" (§3.1), so replacing the list breaks nothing above this subsystem, and
  nothing in §5.A/§5.B branches on what a Template contains. **One bare, general-purpose base, and
  nothing agent-shaped** — an agent stack is a product-layer concern that needs no engine concept at
  all: create from the base, install with a one-shot exec (which introduces no init dependency,
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1), and **Promote**, which yields
  an ordinary Template whose lower stack is `[the new layer, …the base's stack]`. That path already
  exists and is the whole reason the engine needs no agent vocabulary here.
  - **Four items are correctness rather than convenience** — without them a sandbox cannot do the
    things a sandbox is for, and their absence presents as a confusing failure rather than a missing
    tool: **a CA certificate bundle** (without it every HTTPS request fails at verification, so no
    `pip install`, no `npm install`, no `git clone` over HTTPS); **`/etc/nsswitch.conf`** (glibc's
    name-service switch consults it, so name resolution can fail even with a valid resolver
    configured); **coreutils** (package post-install hooks and build scripts shell out to
    `ls`/`install`/`mkdir`, so their absence breaks package installation rather than merely
    inconveniencing a human); and **a non-trivial `/etc/passwd` plus `/etc/group`** (a root-only
    single line breaks `getpwuid` for tools that look themselves up). Guest identity is still root
    and the engine still models no guest users (§2.2) — this is hygiene *inside* the guest, not an
    identity model.
  - **The rest is convenience, freely re-decidable**: `python3` + `pip` + `uv`, `git`,
    `build-essential`, `nodejs` + `npm`, `curl`, and optionally `sudo` (purely a guest-side
    convenience for a workload that assumes it, never engine semantics). Each is a one-shot exec
    away, so shipping them trades cold-start size for convenience and commits to nothing.
  - **Preinstall the common Volume mount points**, which is a measured design by-product rather than
    taste: a mount destination **already present in the lower does not trigger copy-up**, while a
    missing one dirties the upper — so preinstalling the common paths keeps Promoted layers clean and
    degrades the exclusion entries to a correctness backstop rather than a routine mechanism
    ([`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5). It cannot cover a caller's
    own choice of destination, which is why the exclusion mechanism still exists.
  - **DNS: the Template ships no `/etc/resolv.conf`, and the engine injects none.** This is the one
    genuine design decision in the list, and both alternatives are worse. **Baking one in** pins a
    build-machine resolver address into a supposedly portable artifact, and it is actively harmful
    rather than merely wrong elsewhere: a resolver address the deployment's `egress.allow[]` does not
    name produces the 10–20 second hang then `Could not resolve host` that network.md D5 ⑦ measured —
    the worst diagnostic shape in the system — **by default**, on every deployment that does not
    happen to match the build machine. **Engine injection** breaks §1.1 (the engine would hold a
    deployment-specific network fact, the same coupling that keeps domain names out of §5.D) and
    lands in the upper: `/etc/resolv.conf` is not under a tmpfs — unlike `sandboxd`'s portal files,
    which are *inert* exclusion entries precisely because `/tmp` is a tmpfs — so an injected file
    would be captured by every Promote unless a new permanent entry were added to the
    manually-synchronized exclusion list. **So the resolver is the deployment's to supply**, by
    writing the file through §5.E's `fs` service after create or by baking it into its own Promoted
    Template, alongside the `allow[]` entry it must write anyway.
    **State the consequence plainly rather than discovering it: out of the box a sandbox resolves no
    names, and under `default: deny` that is by design.** Resolution needs UDP/53 to reach a
    resolver, the resolver does not fall back to TCP on its own (measured: with only TCP/53 allowed
    the allow rule takes 0 packets while the UDP drop counter climbs), and the engine deliberately
    does **not** add that entry on the caller's behalf — doing so would require it to know what a
    "resolver" is and to hold an address, which is the same refusal as domain names (§5.D /
    network.md D5 ⑦). Whether the deployment's resolver is even declarable is a property of that
    deployment: a loopback resolver is unreachable for a gVisor guest, a link-local one sits in the
    unliftable floor, and the one stably-declarable host-local address is the sandbox's own `/30`
    gateway — which is exactly why the floor's sibling item is excluded from `FloorLiftableNever`
    (§6.2).
  - **Build it from a digest-pinned base image export rather than by copying the host.** The current
    builder is a **test fixture** and says so — it copies this machine's userland and ships a
    `/bin/sh` with no coreutils — and a host copy cannot be reproducible by construction, since its
    output is a function of whatever the build machine happens to have. A digest-pinned export
    requires no build-time package resolution, which a distro bootstrapper needs a pinned snapshot
    mirror to match; and §5.B's carrier is already shaped for it (a Template is a read-only lower
    directory, a layered one is already multi-lowerdir "like an OCI image"). Neither approach makes
    the *result* bit-identical without further work; both make the *inputs* deterministic, which is
    what a reproducible build actually requires.
- **Not included**: a Volume's lifecycle and mount policy (→ C), and which process context the
  Overlay runs in (→ A).

### 5.C Volume (decoupled, independent storage)

- **Target**: make "additional data that must survive independently" a first-class citizen, with a
  lifecycle decoupled from any sandbox.
- **Definition**: an ID-addressed persistent storage unit, created before a sandbox, outliving a
  sandbox, and present while unmounted; mountable at different paths in several sandboxes, **not part
  of a sandbox's persistent state and not captured by a Promote**, each on its own backend.
- **A separate subsystem with its own seam, `volume.Backend`** (not sharing §5.B's materialization
  seam, see the §5 preamble): it yields **exactly one thing: a host mount point** (which runsc writes
  into the OCI mount spec and the gofer proxies into the guest, isomorphic to the merged rootfs).
  It holds §5.A's **policy + handles, no host-primitive leakage** seam rule too (inheriting the ❌
  list verbatim, with the reasoning not restated; the structural guard and the exemption table are in
  [`deep-dives/volume.md`](./deep-dives/volume.md) D2).
- **Sharing/exclusivity policy**: `AccessMode ∈ {Exclusive, SharedReadOnly, SharedReadWrite}`
  (exclusive / read-only fan-out / shared read-write; the last guarantees POSIX semantics only among
  several connections (processes) within one sandbox, while **cross-sandbox rw coherence is undefined
  by the engine** — the gofer's and sentry's separate caches make shared mutable consistency across
  an isolation boundary inherently unreliable, so the engine does not promise it and the user
  documentation says so honestly).
  - **AccessMode is a property of the mount row, not of the volume** (its cardinality is per-mount,
    not per-volume): measured, the same volume can be shared-rw for sandbox A and shared-ro for
    sandbox C **at the same time**, so a per-volume field cannot honestly carry it. The volume record
    holds only `cap_byte` (the cap, called `size` on the wire) / `backend` / `parent` (lineage, see
    snapshots below); `access` appears only on the mount row (§4.3), and `Materialize` does not take
    it either — **ro is enforced at runtime** (a ro bind ⇒ the guest's write gets `EROFS`, a clean
    POSIX refusal rather than a silent success) and **exclusivity is enforced in the control plane**
    (the mount row), both adjudicated before the call happens, so neither requires the backend to
    know (the independent seam-side argument is in
    [`deep-dives/volume.md`](./deep-dives/volume.md) D2).
    - **"Enforced at runtime" holds only in the direction "a writable unit + a ro mount".** The
      reverse — **an immutable unit + a rw mount** — is not a runtime `EROFS` but a **hard failure of
      `runsc create` at the gofer mount** (the container is never created and the guest never ran). So
      this ro rule's directionality must be explicit: the engine must never let a read-only backend
      unit be paired with a rw mount spec, or an error that should have been a 4xx at admission
      degrades into an opaque mount failure at cold start. By design this cannot be hit (a snapshot
      produces **an ordinary writable volume**, below), but it is a fact about the substrate rather
      than an assumption about our code (measured in
      [`deep-dives/volume.md`](./deep-dives/volume.md) D1).
  - All three modes need **no special mechanism**, concurrent cases included (the measured list is in
    [`deep-dives/volume.md`](./deep-dives/volume.md) D1).
- **The backend has two arms, with zero difference on the gofer side**: **local (the default)** = a
  **btrfs subvolume** on the host (bringing its own quota cap + cheap atomic snapshots), with the
  gofer proxying an ordinary directory and POSIX coming free from the host kernel; **remote** (S3 /
  NFS / …) = a **host-side FUSE mount point** maintained by the backend, likewise proxied into the
  guest by the gofer. **FUSE lives on the host side only and never inside the guest** (in-gVisor FUSE
  support is limited), and it is sealed inside that remote backend rather than entering the core model
  (why not "FUSE for everything", and where FUSE belongs, are in
  [`deep-dives/volume.md`](./deep-dives/volume.md) D1).
- **Operable while unmounted**: create/delete/list/inspect/snapshot/clone + importing/exporting
  content directly. Outwardly **everything is addressed by Volume ID and no host path is exposed**.
  - **This is delivered by a root-confined content accessor on the seam** (`OpenRoot`), **not through
    §5.E's `fs`**: paths are relative to the volume root, escaping it is unreachable, and the local
    arm = `openat2(RESOLVE_IN_ROOT)`. Why it does not go through the guest, and the measured detail of
    the mechanism, are in [`deep-dives/volume.md`](./deep-dives/volume.md) D2 / D5.1.
  - **This promise also answers two otherwise unsolvable scenarios, which is why it is not
    optional**: **seeding a volume from nothing** (when no sandbox exists yet) and **reading a volume
    after every sandbox is gone**. Both are in the direct consequence set of "a Volume's lifecycle is
    decoupled from any sandbox's" (the opening of this section) — for an object with an independent
    lifecycle, if content can only enter and leave through a sandbox, its independence is merely at the
    record level.
- **A single write regime (a cross-cutting invariant against double-write tearing)**: a Volume has two
  write paths, mounts and the direct API, and the engine holds one invariant — **as long as a live-rw
  mount holds that volume, mutating operations on the direct API (`import`) are refused outright**;
  direct writes are allowed only when no live-rw mount exists. The gate is on a **live** writer, not on
  "present in the mount table": **a paused sandbox does not count as a live writer** (its container is
  stopped and there is no live gofer), so a direct write is safe then and the new content is naturally
  seen after a resume. No reverse proxy is introduced (the falsification is in
  [`deep-dives/volume.md`](./deep-dives/volume.md) D5).
  - **This invariant is symmetric in both directions, and both sides must be gated.** The sentence
    above states it from the direct-write side, but the danger it guards against (two writers writing
    the same backend bytes while the gofer's and sentry's caches disagree with host-side writes) **does
    not care who arrived first**. So the reverse holds too: **as long as a direct mutation is in
    flight, a cold start (create / resume) that would rw-mount that volume is refused too**, rolling
    back to the source state (§4.4-A: failure rolls back atomically, resume→`paused`). Gating only one
    side is the same as gating neither — an import allowed at T0 plus a resume completing at T1
    manufactures those two concurrent writers without passing through any `paused` state.
  - **Both sides are adjudicated inside the same critical section**: the set of live-rw holders and the
    in-flight marker for direct writes are **two halves of the same volume-keyed mutex table**, not two
    separately maintained checks (which would leave a human to keep the two consistent). The same shape
    as the existing in-flight build keep-set, and it **introduces no new lock class** — in particular no
    per-volume lock: D3 **derives from the lock order** that this closes into a cycle and deadlocks
    (derived, with no reproduction vehicle in the repo, unlike D3's first two findings).
  - **The cold-start side refuses rather than waits.** By contract, a direct write's marker is held for
    **the whole stream**, whose duration is unbounded in the payload; and a cold start holds that
    sandbox's transition lock throughout (§4.4-B), so waiting would jam destroy / pause / evict behind
    an unbounded upload. Refusing is retryable and explicable (the remedy: wait for this import to
    finish), and it lands on §4.4-A's existing "roll back to the source state" path, adding no failure
    semantics.
  - **The predicate for "live" comes from gofer liveness (the placement live-map), not from
    `sandboxes.state`.** Measured, that state column is an unfaithful proxy in **both** directions: a
    cold start records a live placement **before** the health gate and the CAS-to-`running` (so the
    record says `creating`/`paused` while the gofer is already live, for a window as long as
    `HealthTimeout`), while pause CASes to `paused` first and stops the container **afterwards** (so the
    record says `paused` while the container is still running). No lock or SQL statement can fix that —
    **the fact being tested does not live in the store**. So the adjudication reads the mediator's
    live-map rather than that state column (and it sits in the same mutex table as the direct-write
    in-flight marker from the previous bullet).
  - **A live read-only mount does not gate a direct mutation, and what its reader may observe is
    stated rather than left to inference.** The invariant above is about **tearing**, and tearing needs
    **two writers**; a reader is not one. So a `shared-ro` holder — live or not — never refuses an
    `import`, and symmetrically a cold start that would mount the volume **read-only** is never refused
    for a direct write in flight. This is not an exception carved out of the gate above: refusing here
    would be a *different* rule with no hazard behind it, and it would delete a capability the engine
    deliberately has (booting a sandbox that only **reads** a volume being seeded).
    **What the ro reader may see is bounded, and the bound is measured**: a holder that already has the
    file open keeps reading the **complete previous inode** (the gofer follows the inode — D7.2
    measured zero errors, with reads and writes through the old descriptor continuing); a **fresh**
    lookup landing mid-replacement gets a momentary **`ENOENT`** (not EIO, not ESTALE), because an
    import **replaces rather than overwrites in place** — it unlinks first, which §5.C already requires
    for an independent reason (a shorter payload written over a longer file would otherwise leave the
    old tail attached, producing a file that is neither the old nor the new content); and an open
    mid-stream gets a **prefix** of the new content, the same consistency class a snapshot already has.
    **It never sees a torn file mixing the two versions**, because the one mechanism that could produce
    that — an in-place overwrite — is already excluded.
    **Freshness beyond that is not promised, and that is this section's EXISTING coherence position
    rather than a new one**: the gofer and sentry cache separately, so cross-sandbox rw coherence is
    already undefined by the engine (above), and a direct write is simply a second writer on the host
    side of that same boundary. **No warning, version stamp, or refusal is added.** A warning has no
    channel — §6 is request/response, and §4.1's event kinds are lifecycle transitions whose closed set
    the schema asserts, so inventing an outward channel fails §1.3's litmus at the first step. A
    version stamp is barred twice: `generation` is the one §1.1 row barred **outright**, and §4.3
    already assigns staleness detection to the placement as the epoch — and it would have **no reader**,
    since the consumer is a guest and §4.2 rejects application-layer cooperation outright. A refusal
    would delete the capability named above, on **both** sides of a symmetric gate.
    (The industry position is the same and is reached by every comparable system: NFS states plainly
    that its protocol "is not designed to support true cluster file system cache coherence" and pushes
    the escape to the application; a FUSE-fronted object store documents that an out-of-band overwrite
    during a read yields "either the old data or the new data, but never partial or corrupt data".
    Where a validator exists at all it is used to **revalidate or fail the reader**, never to refuse the
    writer's operation.)
- **Snapshot / clone = a per-backend capability, crash-consistent by default**: the local backend
  delivers it with `btrfs subvolume snapshot` (whole-tree, atomic, instantaneous), and whether it is
  supported is decided by the backend's capability query (a plain directory / some remote backends may
  not support it, in which case the call is a 400 rather than silent). A snapshot is the backend's
  atomic snapshot of the bytes currently on disk and **does not quiesce writers**, so its consistency
  is equivalent to one crash (the bottom line §4.2 already accepts). For app-consistency, pause the
  holding sandbox first and then snapshot — a **composition** of existing primitives, with the engine
  adding no mechanism; **shared-rw offers no** app-consistency (coordinating N sandboxes is product
  semantics, rejected by §1.3). Why this is asymmetric with the Overlay's Promote is derived in
  [`deep-dives/volume.md`](./deep-dives/volume.md) D6.
  - **A live mounted volume can be snapshotted directly, with no degradation and no disturbance to the
    guest** (measured: under continuous writes the guest's write rate is unaffected with zero write
    errors, every capture point lands inside the host-observed live window, and an append-only log is a
    complete prefix in every capture; occasionally a file just emptied by `O_TRUNC` and not yet
    rewritten is captured, which is exactly what crash-consistency should give). **No `fsync`/freeze
    prelude is needed** — sync makes no measurable difference, and freeze cannot be composed with
    snapshot at all (D6.2).
  - **A snapshot does not break per-volume cap semantics: the writer pays, in both directions**
    (measured). Overwriting the source charges only the source's account while the snapshot's row stays
    put at its metadata cost; overwriting the snapshot charges only the snapshot's account and leaves
    the source alone. Three consequences: **a snapshot volume nobody writes does not grow into its cap
    by itself**, the two volumes' caps are isolated from each other (the snapshot is still writable
    while the source is pinned at its cap), and after the source is deleted the snapshot's accounting
    does not retroactively inherit the released extents.
  - **A snapshot / clone produces an ordinary, writable Volume** with no read-only marker: writability
    is a per-mount adjudication (the same cardinality argument as `access`), and immutability is
    expressed with a `shared-ro` mount row. The two are therefore **the same kind of object on the same
    backend path**, differing only in semantic annotation (the derivation and falsification are in
    [`deep-dives/volume.md`](./deep-dives/volume.md) D6.1a).
  - **The new volume's cap must be written explicitly by the engine within the same backend operation,
    with `size` required and not inherited**: measured, what the substrate gives a snapshot is **a
    brand-new uncapped** quota row, with nothing to inherit; and failing to write it is **silent and
    undetectable after the fact** (the "uncapped" that `Usage()` reads back is byte-identical to being
    deliberately uncapped). So `size` is likewise required at both these endpoints when disk is metered
    (§3.4 / §6.2, the same rule), and missing it is a 400.
    - The cap is necessarily a **second** call after the snapshot ioctl, so "the volume exists, the cap
      is not set" is a real intermediate state: it belongs to the in-flight set, and **a sweep / adopt
      must treat "the volume record exists while the quota row is uncapped" as a repair state rather
      than a legal one** (D7.3).
- **A volume's capacity cap is editable while unmounted, and equally editable while mounted** (the
  write point being §6.1's `PATCH /v1/volumes/{id}`). This is **exactly the opposite** of sandbox
  resource ceilings being editable only in `paused`, and the reason is mechanical: a sandbox's cap
  materializes at placement time, whereas **a volume has no placement of its own** — "rematerialize at
  the next placement" means "never" for an unmounted volume, so a volume's cap lands directly on its
  storage unit, there is nothing to defer, and therefore no "pause first" 409. Three incidental
  disciplines:
  - **Changing the cap is not governed by the write regime**: it moves an allocation upper bound rather
    than bytes, so there is no tearing. (Unlike **changing `access`** — that one is forbidden under a
    live mount, see the exclusive lease below.)
  - **Raising the cap must re-run the admission gate** (§3.4: a volume cap lands on the disk axis, and
    writing to it changes that Σ on the spot — the gate evaluates on the **disk / memory** axes only,
    and CPU enters no node inequality), and may 507; lowering need not.
  - **But lowering below current usage must be refused (400, the criterion being a strict inequality)**:
    measured, this wedges the unit **irreversibly** — afterwards even `mkdir` and "truncating an
    existing file to 0" return `ENOSPC`, and the quota ioctl itself returns `ENOSPC` too, so the cap can
    neither be removed nor raised and the only way out is deleting the guest's bytes. The engine cannot
    do that on the caller's behalf, so it does not accept such a request (details in D7.3).
- **The mount relation is a Binding (§3.4)**: "which are mounted, where, and under what policy" is
  recorded in the control-plane mount table, and a resume **remounts** them per the current mount table
  rather than restoring from frozen bytes — a Volume is always that one real thing, never a forked
  copy.
- **Replacing the whole binding is one atomic commit, promised by the store contract**
  (`ReplaceMounts`): §6.2 says a sandbox holds one complete binding **at every instant** and that
  `PATCH` only replaces one whole binding with another, so this must not be implemented as two
  independent commits of "delete then insert". **The delete must come before the insert** (otherwise a
  resubmitted identical binding collides with the very rows it is replacing: the lease predicate and
  `UNIQUE(sandbox,path)`), so the solution is not to reorder but to **bring both steps into the same
  commit**. Three consequences, each a property two independent commits cannot hold: ① on failure the
  sandbox stays on its **original binding**, and there is no empty binding of "the new one was refused
  and the old one is gone too"; ② the exclusive lease is **not briefly released** during the
  replacement, so a concurrent create cannot steal a volume this PATCH intends to keep; ③ the write
  regime's predicate (`ListMountsByVolume`) cannot read the intermediate state of "zero mount rows",
  so a concurrent direct write is not wrongly allowed.
  - **This is not opening a transaction on the seam** but a second application of §4.4-C's discipline:
    **the interface method itself promises atomicity and each backend delivers it** (SQLite through a
    local transaction, Memory doing a wholesale replacement under its existing mutex), with one
    algorithm that does not branch on backend capability. Two contract tests: a replacement that loses
    the lease keeps the **old** binding readable; a concurrent `ListMountsByVolume` **never** observes
    zero rows.
- **The exclusive lease = the existence of that Exclusive mount row itself**: the lease is not a
  separate object but a derived state of §4.3's mount row (a first-class persistent object with a
  derived refcount), so there is no `Lease` type and no belonging field. **The invariant stated
  precisely**: a volume has **at most one `exclusive` mount row, and that row does not coexist with any
  other mount row on that volume**.
  - **Delivered in one store statement, with the decision and the insert inseparable**:
    `INSERT … SELECT … WHERE NOT EXISTS`, and `RowsAffected()==0` ⟹ a lease conflict → 409. **A
    mediator precondition check plus an insert will not do** — that is a genuine TOCTOU serialized by
    no lock across sandboxes (every create mints a new SandboxID, so two concurrent creates hold two
    disjoint transition locks), the same reasoning as why the admission gate needs a mutex of its own.
    This is not a new mechanism: the same authoritative store serializes the **decide-and-act** pair,
    isomorphic to §4.3's reference-reclamation. The statement, the measured reproduction, and the lock
    order are in [`deep-dives/volume.md`](./deep-dives/volume.md) D3.
  - **The `UNIQUE(volume) WHERE access='exclusive'` partial index + `CHECK (access IN (...))` are
    schema-level assertions rather than the invariant's deliverer**: the partial index only forbids "two
    exclusives" and **does not forbid an exclusive coexisting with a shared** (covering only half); and
    its predicate is a **literal string comparison**, which is why the `CHECK` is required — a
    misspelled case would make the partial index simply not apply, a class of silent failure.
    **The predicate must be a row-immutable attribute** (`access` here) and **must not be built on a
    mutable lifecycle field** (the reasoning and an external cautionary example are in D3).
- **Two uniqueness assertions on mount topology, judged 400 rather than 409**: `UNIQUE(sandbox, path)`
  and `UNIQUE(sandbox, volume)`.
  - **Two volumes mounted at the same dest within one sandbox must be forbidden**: a mount materializes
    as an OCI bind, and two binds on the same dest mean the second **shadows** the first — so one volume
    is invisible inside the guest while still counting toward §3.4 and still holding its lease. "An
    exclusive lease on a volume the guest cannot see" is exactly the class of silent failure the schema
    assertion exists to catch.
  - **Mounting the same volume twice within one sandbox is forbidden too**: the exclusive lease's
    predicate (D3) already refuses a second row on the same volume whenever either row is exclusive, so
    not forbidding it would make the rule fork by access mode; forbidding it uniformly keeps the rule
    single. It has no known consumer, and forbidding is a validation — relaxing it later breaks no
    caller, whereas the reverse is not true. (§5.C's "mountable at different paths in several
    sandboxes" above speaks of several **sandboxes** and is unrelated.)
  - **Judged 400**: a sandbox holds one complete binding at every instant, and `PATCH` merely replaces
    the whole thing (§6.2), so there is no operation of "add one mount to an existing set". Every
    duplicate is therefore **a single commit's binding contradicting itself internally** — decidable
    without reading another row, and no retry or state change can make it legal, which is exactly §6.2's
    400 criterion. Nesting (see the mount topology rules) is the same class and also a 400. **§6.2's
    four 409 outcome categories do not grow** (a category ≠ a sentinel count: ② has two FK paths, the
    volume side and the sandbox side, with two sentinels, and still counts as one category, see §6.2).
  - **Paths must be normalized before insertion (absolute, cleaned, not `/`), or the UNIQUE silently
    fails to apply**: `/data`, `/data/`, and `/data/../data` are one mount point as three strings — the
    same trap as why `CHECK (access IN (...))` must exist beside the partial index (a literal string
    comparison inside a uniqueness assertion failing **silently**). **And `UNIQUE(sandbox, path)` cannot
    catch nesting** (`/data` and `/data/sub` are different strings), so nesting rests on a prefix scan
    in validation and cannot be made a UNIQUE. Both are kept: validation hits first, and the assertion
    is the net for when validation is bypassed or written wrongly.
  - **`access` has no write point of its own, and that is deliberate**: changing access is possible only
    through a whole-binding replacement (`ReplaceMounts`), so it necessarily re-runs the lease
    predicate. **Do not open a per-mount write point for it** — flipping exclusive to shared under a
    live mount would let an additional writer into a volume whose original holder never learns it lost
    exclusivity, which is exactly the path that bypasses the assertions above; if one is ever opened, it
    must carry a live-mount check with it.
  - **Keeping the lease across a crash is the correct behavior** — a crash is dead-recoverable (§4.3)
    and the sandbox will resume onto the same volume, so releasing immediately would make the resume
    collide (§4.2). A lease is **released only when the mount row is torn down** (a destroy, the
    reaper's evict of a paused sandbox past its TTL included), and **the reaper's TTL IS the backstop
    for "nobody released it"**, with no heartbeat introduced; heartbeat-based reclamation is a
    multi-node provider's obligation and does not enter the core (the falsification is in
    [`deep-dives/volume.md`](./deep-dives/volume.md) D4).
  - **There is no preemption verb and no force-detach, and that is settled rather than deferred**:
    every release path already terminates without one. `DELETE /v1/sandboxes/{id}` tears the mount
    rows down and is legal from `running` **and** `paused`; a **detach through
    `PATCH /v1/sandboxes/{id}` is legal on a `running` holder and releases the lease immediately**
    (§6.1's `lease_released: true` — the guest's mount topology still changes only at the next
    resume, which is precisely why those two effect times are reported separately); and the reaper's
    `retention` evict closes the case where nobody acts, on a clock a `PATCH` can shorten after the
    fact. **A *stuck* holder needs no verb either**: §4.3 crash-pauses it without operator action and
    `retention` then runs from that demoting CAS, which §4.3 states plainly is not exempt from
    reclamation. So the residual is not "the volume cannot be reclaimed" but **"it cannot be
    reclaimed without addressing the holder"** — and that is the property the lock order rests on,
    since every release point goes through the holder's own record under the holder's own transition
    lock.
    - **A volume-dimension verb that looks up its holder is therefore prohibited, not merely
      unbuilt.** It would acquire in the `vol → sbx` direction while every existing verb acquires
      `sbx → mu` / `sbx → admission mutex`, closing a cycle (derived from the lock order, no
      reproduction in the repo — [`deep-dives/volume.md`](./deep-dives/volume.md) D3). Were one ever
      built it must resolve the holder from the store, **release first, and re-acquire in the global
      order** — which forces it to re-validate and retry, so it could **not** carry the
      decide-and-act pair in one store statement the way `ReplaceMounts` already does. The lock order
      does not merely make preemption awkward; it makes a worse-shaped mechanism the price of a
      capability the engine already has.
- **Mount-topology changes happen at the resume boundary and are not hot mounts**: runsc takes mounts
  from the OCI spec **at startup** and **does not support** hot-adding or hot-removing while running
  (there is only an EROFS debug bypass, which is not used). So "mount a new Volume into a running
  sandbox" = edit the mount-table Binding → a cold resume carrying the new table, reusing the same path
  as §4.2's FS-first pause/resume.
- **Mount topology rules**: the host directory for a mount point belongs to the Overlay artifact (the
  sandbox rootfs); **Volumes do not nest inside each other** (one Volume is not mounted into another
  Volume's subtree), since otherwise the ownership of the mount-point placeholder in a Promote capture
  would be ambiguous.
- **The capacity quota is a per-volume property, not per-sandbox (the disk capacity axis, §3.4)**: a
  Volume's capacity ceiling is recorded on its own object record and materialized by the §5.C backend on
  its backend; it **cannot be attributed to some sandbox** — a shared-rw Volume is written by several
  sandboxes and its occupancy cannot be split onto any single one. This and §5.B's Overlay upper cap
  (per-sandbox) are two independent capacity caps, enforced separately but **sharing the same underlying
  substrate primitive** (a btrfs subvolume + an squota limit) — that primitive is reused across
  §5.B/§5.C, see [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6 and
  [`deep-dives/volume.md`](./deep-dives/volume.md) D7.
  - **Behavior when it fills is the same as §5.B's per-sandbox cap** (the guest sees an ordinary
    `ENOSPC`, the sandbox lives on as usual, and the control plane does not get involved, §3.4),
    **and it is insensitive to the number of writers**: when several sandboxes write a full volume
    concurrently, each writer sees an ordinary `ENOSPC`, every sandbox survives, and the cap remains
    exact — but **how the bytes are divided among the writers is arbitrary**, who writes more is
    nondeterministic, and one writer can keep succeeding while its counterpart is already at `ENOSPC`.
    So neither direction is promised.
  - **Three user-visible semantics (a caller will hit these; they are not internal accounting
    details)**:
    1. **The guest cannot discover its own volume cap: `statfs` reports the carrying filesystem, not the
       cap** (measured: a volume with a 32 MiB cap reports 476 GiB available). So any space precheck on
       the guest side (an installer, a package manager, a `df`-based guard) will report hundreds of GiB
       available moments before `write()` fails, and decide wrongly. **The engine does not promise that
       guest-side statfs reflects the cap**; a backend able to project it honestly can do better, and
       that is that backend's obligation rather than a core promise (the cause and external corroboration
       are in [`deep-dives/volume.md`](./deep-dives/volume.md) D7.4).
    2. **Under squota the cap is a "lifetime write budget", not a "ceiling on live volume".** It **does
       not refund overwritten** extents (rewriting the same 32 MiB file in place twice charges 64 MiB),
       but it **does refund deleted** ones (settled immediately after an unlink, refunding exactly the
       file's size). So a long-lived volume repeatedly rewritten in place hits its cap far below its
       "current size", and **the way to settle it is deletion, not overwriting**.
    3. **`ENOSPC` is not a sticky wall.** Measured repeatedly: the **next** small write after hitting
       the cap sometimes still succeeds, and only then does it fail stably (the mechanism is consistent
       with residual headroom + over-reservation released by the failing write, though only the behavior
       was measured, not the cause). Under a shared cap, a counterpart's activity also moves this line.
       So "stop as soon as you get ENOSPC" and "after getting ENOSPC one more small write may still
       land" are two different contracts, and what the engine promises is the latter.
- **Safety requirements for reclamation and sweeps (the local arm, specific to squota)**: residue comes
  in three classes corresponding to **three mutually independent entry points**, merely called one after
  another at the same startup position and in the same reaper tick — orphan bytes
  (`SweepOrphanVolumes`) · D7.3's repair state (`RepairVolumeCaps`) · squota's leaked quota rows
  (`ReclaimQuotaRows`); **do not read them as three phases of one sweep**, since their predicates,
  failure dispositions, and reachable paths all differ. All three are **silent when they hit the wrong
  thing**, so the safety requirements are part of the contract.
  **The division of responsibility has two layers**: **the decision and the scheduling belong to the
  mediator** — only it holds the live set, and pushing the decision to the backend (having it take the
  live set and delete the difference) was measured to **delete the bytes of committed volumes**; the
  derivation and both sets of measured numbers are in [`deep-dives/volume.md`](./deep-dives/volume.md)
  D2. **Accounting for and enumerating residue belong to the backend** — only it knows whether it has
  quota rows at all: `Backend.ListUnitIDs` enumerates the storage units it holds and
  `Backend.DeletedRowIDs` reports the row ids it recorded at deletion time — and a backend with no quota
  rows at all returns empty, making this path cost it nothing (§1.3: callers do not grow capability
  branches).
  So each safety requirement below constrains **the mediator-side decision**, resting on accounting
  facts the backend supplies:
  - **A leaked quota row is a correctness problem, not untidiness**: a leaked row does not reserve its
    own storage-unit id, so one node restart can hand that id **together with its cap** to a brand-new
    volume, and the engine cannot detect the mismatch internally. So the sweep must genuinely reclaim
    these rows, and **must run once at startup before any storage unit is created** — otherwise a
    restart can hand out the inherited cap before the first sweep tick. The leak predicate, the polling
    predicate, and the reclamation procedure are in [`deep-dives/volume.md`](./deep-dives/volume.md) D7.
  - **Enumeration must probe directory by directory and must not use filesystem-level subvolume
    enumeration**: the latter's scope is **the whole filesystem / the containing storage unit** rather
    than the given directory (measured: running it in the volumes directory and in the sibling overlay
    directory returns byte-identical results, including storage units belonging to other software on the
    node). Probing one by one is safe with no precondition, and it is the shape of the engine's existing
    `internal/btrfs`.
  - **There must be a live-mount check independent of FK**: skip any unit whose id appears in **any**
    mount row (live or not); **on an enumeration or read error, skip — and never treat an empty result
    as "no owner"** (measured: under concurrent churn a batch read of quota rows fails wholesale and
    returns zero rows). Quota rows are **located by the id recorded at deletion time and never by
    scanning rows**.
  - **A mistaken sweep has no detection path whatsoever, which is why every requirement above is a hard
    one.** It is a pure data-plane event — the sandbox lives on as usual, **the guest's writes still
    return success**, and the control plane sees nothing (the measured distribution of five approaches
    and their consequences are in [`deep-dives/volume.md`](./deep-dives/volume.md) D7.2). One
    **acceptance-testing discipline** follows: "is the sandbox alive" is always true here, so as an
    assertion it can never fail and has no guarding value; what must be asserted are the properties that
    **would pass silently**.
  - **`Backend.Materialize` must be create-if-absent**: were it open-only, a wrongly swept sandbox could
    **never resume again**, whereas create-if-absent gives a clean restart (all data lost but
    recoverable). This is also where it carries "a volume's bytes exist on demand".
- **Not included**: the sandbox's own rootfs / Overlay artifact (→ B).

### 5.D Network (the reachability declaration and its enforcement)

- **Target**: make each sandbox's network reachable set **declarable and enforced**. This subsystem is
  the home of that declaration and generates the **abstract rule set** that delivers it (rendering and
  installation belong to §5.A, below).

> This section's design derivation, its measurements item by item, and the falsified options are in
> [`deep-dives/network.md`](./deep-dives/network.md) D5, and **are not duplicated here**.

- **This subsystem does exactly two things: hold the declaration, generate the rules.** It **moves no
  bytes, terminates no connection, and contributes no process.** The criterion is a direct application
  of §1.1's first principle — what the engine should do is the thing "only it can physically do" (§2.4:
  the enforcement mechanism must be atomic with the placement, and the predicate must bind a fact the
  guest cannot change) — whereas "terminate a connection and decide where it goes by application-layer
  information" **does not need** the side that creates the namespace, so it is not inside the engine.
  - **Generating the rules is a pure function**: `(declaration, L0 floor) → an abstract rule set` —
    **placement-free and root-free**, so **every difference between declaration values can be pinned in
    unit tests at this layer** without waiting for the substrate. That abstract rule set is exactly what
    `SteeringSpec` carries in §5.A's contract. **There is only one input, the declaration**: the L0 floor
    is a static fact held by the `ruleset` leaf and is not passed as a parameter, so no caller can swap in
    a different floor. **The output is an ordered list, each rule carrying its hook ownership** —
    cross-hook ordering is itself a security property (`via[]`'s narrow allowance must be in the **same
    chain** as the floor's drop, D5 ⑩), so it cannot be split by hook up front. Grouping by hook happens
    only downstream (in the renderer and `RuleSet.ByHook`), while what the unit tests pin is precisely
    "what came out on each hook": one declaration produces rules on several hooks (next bullet), and
    flattening both sides into one list without ownership would let exactly the hook differences slip out
    of the part being pinned.
  - **The declaration's allowances are emitted on both `input` and `forward`, and the `input` side must
    carry a sibling drop of its own.** The reason is this section's own semantics ③: a caller must be able
    to declare their resolver, and the only stably usable host-local resolver address is **this sandbox's
    own `/30` gateway**, which lands on `input` — an allowance emitted only on `forward` **cannot reach
    it** and that promise would have no delivery path; while without that sibling drop on the `input`
    side, a single `allow 10.0.0.0/8` would let this sandbox reach a sibling's **gateway**. **This does
    not conflict with "the floor needs no daddr list on the `input` side"**: on that side the floor is a
    pure ct-state rule narrowed by `iifname` (`iifname` being the entire reason it is a per-sandbox rule;
    it is already exhaustive over any address of the host itself and never enumerates), and the sibling
    drop is **another** rule whose predicate is the sibling CIDR — two rules, not a daddr list hung onto
    the floor (the derivation and measurement are in
    [`deep-dives/network.md`](./deep-dives/network.md) D4.2).
  - **Rendering and installation belong to §5.A**: rendering the nft text, and binding `iifname` / the
    guest IP / the publish host endpoint, all happen inside the provider at placement time — those three
    are products of the placement and host primitives only it holds, so putting them into this
    subsystem's signature would both fail to run (no placement exists yet) and lift host primitives onto
    the cross-provider contract surface (§5.A's ❌ list). The rule set is a placement-local resource,
    installed by the provider **between** building the netns and bringing the isolation unit up (§2.4's
    atomicity obligation). The ordering is not a matter of style — see §5.A's `SteeringSpec`.

- **The declaration's shape: four fields, all in a vocabulary of IP / port / protocol only** (the wire
  schema is in §6.1):

  ```text
  egress.default    : deny | allow          # allow = the public internet (minus the L0 floor and the
                                            #   deny-safe private tier = RFC1918, §2.4)
  egress.allow[]    : {cidr, port, proto}   # IP literals only, no domain names
  egress.via[]      : {match[], endpoint}   # match[] is likewise {cidr, port, proto}; matches are sent
                                            #   into endpoint
  ingress.publish[] : {guest_port, proto}   # → a host endpoint
  ```

  - **`{cidr, port, proto}` is one tuple, not three optional items.** `proto` is always explicit because
    it is a **format requirement** rather than an optional dimension: in the kernel mechanism that carries
    it, port matching itself depends on protocol matching (reasoning in
    [`deep-dives/network.md`](./deep-dives/network.md) D3.1). So `{cidr, port}` is not well-formed.
    - **`cidr` and `port` each have one wildcard value, both expressed by "omission"; `proto` has
      none.** Omitting `cidr` = **any destination** (that is, `via[].match[]`'s transparent-intercept
      shape, D3.3), and omitting `port` = **any port**. In the rules the two are the same mechanism: the
      renderer emits that clause **if and only if** the field is specified, so a wildcard has **one fewer
      clause** than a specified value. Both directions carry weight: adding a clause for a wildcard tuple
      would **refuse traffic the declaration did authorize**, while missing a clause for a specified tuple
      would **authorize every other port on that destination** (the counterexample for each is in
      network.md D5 ⑩). **`proto` has no wildcard not as a trade-off but as a mechanism**: the dependency
      above means a tuple lacking it cannot be rendered at all.
    - **A wildcard must be "writable", not merely a side effect of a zero value.** The criterion is the
      "`egress.default` must be written" rule below (an empty value cannot tell who chose it), applied
      here to wildcards. So
      **a wildcard is spelled "omission" on both surfaces** — when the read surface renders a wildcard it
      **does not emit** that field (rather than emitting `"port": 0`), so the read surface's output is
      still a legal write body (§6.1's read-modify-write). **On the write surface both `"port": 0` and
      omission are legal and persist as the same value**: the two spellings **merge at decode time** and
      do not land in the record separately — §6.2's eighth class (v4-mapped) refuses a second spelling
      that **lives on into the record and then compares unequal on every read-modify-write**, and there is
      no such thing here.
    - **`ingress.publish[]`'s `guest_port` is the opposite case, where zero is illegal.** It is **the
      target of a DNAT** rather than a match item, and zero has no rewritable destination; whereas a
      tuple's `port` is a **match item** where omission means match all. Their shapes are opposite, so
      `guest_port: 0` falls into §6.2's **well-formedness** class.
  - **There is no "posture enumeration".** A posture is not an engine concept but a **combination of
    these fields' values**: `default:deny` + no `allow[]` + no `via[]` is "no egress"; `default:deny` +
    some `via[]` is "only through some rendezvous point". One enumeration fewer is one coupling point
    fewer between the engine and product semantics (§1.1).
  - **`egress.default` must be written and defaulting is illegal** (the same shape as `idle_timeout` /
    `retention`): if "unspecified" were interpreted as some default value, an empty value in the record
    could not tell "the caller chose it" from "this record was created before this field existed".
  - **But "omitting the whole `network`" and "giving `network` while omitting `egress.default`" are two
    different things, and the former is legal.** `network?` may be omitted at create (§6.1), and omission
    means **`{default: deny, allow: [], via: [], publish: []}`**, which the engine **persists explicitly
    at the write point** — so the record always holds a chosen value and the "an empty value cannot be
    told apart" problem the previous bullet guards against **cannot be constructed**. This is not an
    exception to that bullet: that one governs **emptiness in the record**, whereas what lands in the
    record here is the definite value `deny`. **`deny` rather than `allow`**: it is the fail-closed side,
    and it happens to be the minimal declaration meaning "no network" (§5.D has no posture enumeration,
    and a posture just is a combination of values). **Why not simply make `network` required**: every
    placement generates a rule set from the record, so this field has no "unbound" state to speak of; and
    requiring it would make every caller who does not care about networking **spell out** that minimal
    declaration verbatim in exchange for nothing mechanical — the binding's completeness is guaranteed by
    normalization at persist time and need not be guaranteed by redundancy on the wire.
  - **What sits behind `via[].endpoint` is something the engine neither knows nor asks.** It is an
    address; the engine installs a rule sending matching traffic there, and that is all. **So that
    address is neither an engine constant nor an engine configuration item** — it is a value in the
    declaration, given by the caller.
  - **Domain names are not accepted, only IP literals.** The reason is **cost rather than
    infeasibility** — that road requires a DNS proxy growing inside the engine plus a product list that
    churns, breaking §1.1 (the full reasoning, and the corroboration that "someone has made it work", are
    in network.md D2). For a narrowing like "only this one domain", use `via[]` to hand the traffic to a
    process outside the engine to judge by application-layer information.

- **`publish` is the explicit ingress exception, and the only item that can be hot-added.** It exposes a
  port inside the sandbox as a host `ip:port`, and being a **purely host-side rule**, purely additive, and
  not touching the isolation unit, it can be hot-added on a running sandbox; whereas **changes to the
  three egress fields go through the resume boundary** (as with §5.C's mount-table edits), which is a
  design trade-off rather than a technical limitation.
  - **One publish is a rule set, not one rule**: a DNAT on `prerouting`, an SNAT rewriting the return
    source address to **this sandbox's own `/30` gateway**, **another** DNAT on the `output` hook, and an
    accompanying accept on **the chain the hook requires** (the post-translation destination is the guest
    IP and must be routed out, so it lands on `forward`, not `input`). **Missing any of the four is not
    "slightly less functionality" but a broken publish**: without the SNAT it works only on sandboxes that
    happen to have a default route, and without the `output`-side DNAT the host itself cannot dial it (and
    vice versa, which is why "it dials from the host" is not evidence that publish works). The
    measurements item by item are in [`deep-dives/network.md`](./deep-dives/network.md) D5 ⑬⑭⑮, and **the
    numbers are not duplicated here**. The obligation is here: **the generator produces the four as one
    unit and the provider installs them atomically** — there is no observable state in between of "only
    the DNAT went on". **The rule set being wider does not change the three properties above**: it is
    still purely host-side, purely additive, and does not touch the isolation unit — and those three
    properties are exactly what endorses hot-adding.
  - **What the engine delivers is that `host ip:port`, and whatever sits in front of it is outside the
    engine**: TLS termination, a public hostname, certificates, any form of caller validation. **So the
    declaration has no "authentication" field** — validating an inbound caller necessarily means
    terminating and understanding an application-layer protocol, which is exactly what "the engine
    contributes no process" above excludes. (**Narrowing by source CIDR is an exception, because it is
    only a rule predicate and needs no process**; it is not among today's four fields, and adding it later
    is purely additive.)
  - **It is therefore a precise exception to the uniqueness of the Binding write point (§3.4: `PATCH` is
    the only write point besides create, and fields consumed by the provider are editable only in
    `paused`)**: `POST …/publish` writes the **single** field `ingress.publish[]` and may be written on a
    `running` sandbox; the three egress fields are not in this category. The reason is the previous bullet
    — a purely host-side addition that does not touch the isolation unit, so there is no "the record says
    one thing while the live isolation unit does another".
  - **The exception is "may be written on `running`", not "only on `running`"**: `paused` is equally
    legal, and the two states differ only in the effective point. §3.4's state gate is **a tightening on
    `running`** (not accepting an edit that takes effect only at some future resume), while the `paused`
    side is open for every Binding field; a publish endpoint that refused `paused` would become **the only
    field harder to write in `paused` than in `running`**, inverting §3.4's structure in exchange for
    nothing mechanical. **"The record describes an ingress port that does not exist" on a `paused` sandbox
    is not a new false invariant** — the placement is released with the pause, so a `paused` sandbox's
    **entire** declaration (the three egress fields included) has no rule installed at all; the record
    describing **more than what exists** is the definition of `paused`, and it is the reverse that §2.4
    exists to stop. **The cost is more concrete**: refusing `paused` would leave only two roads for adding
    a port to a paused sandbox — "resume first" (a cold start plus an admission gate, possibly a 507) or a
    whole `PATCH`, and the latter is precisely the write point named dangerous below, so the tightening
    would instead drive callers onto the less safe road.
  - **A hot-added item still lands in the declaration record, because it must survive resume**: publish is
    not a one-off runtime bypass, and the next placement still regenerates the rule set from the current
    record.
  - **Atomic ordering: persist first, then install the rules; if installation fails, delete that row back
    and return an error.** The invariant to hold is **"there exists no ingress port the record does not
    describe"**, which is one-directional, so of the two half-finished states only one is acceptable:
    **"the record has it, the rules are not installed" = a functional gap, fail-closed and automatically
    filled at the next placement**; **"the rules are installed, the record does not have it" = an open port
    the engine itself does not know about**, living until the next placement. So the ordering is not a
    matter of style.
    **That sandbox's transition lock is held throughout** (§4.4-B), so it does not interleave with a
    concurrent resume/pause; **and the engine's own write must be a read-append-commit inside the lock**,
    or two concurrent publishes would lose one — which would be a bug in the engine, not something the
    caller-side sharp edge below can cover.
  - **The substrate's atomicity covers only the "install" side; the delete-back side has no equivalent
    protection.** Installation is atomic (measured: if one rule in a batch is illegal the whole batch rolls
    back), so "only part of the rule set went on" is not a state to be handled. But the failure path is
    **if installation fails, delete that row back**, and that delete-back runs on the **teardown** side: it
    must likewise emit the four **as one batch**, and if that delete-back itself fails, the residue is a
    **half-torn-down rule set** — whose failure mode is the "connects, then reads time out" kind, that is,
    the hardest to diagnose (D5 ⑰). **The disposition is to acknowledge it and narrow the promise, not to
    add a mechanism**: it lands back in the very "the rules are installed, the record does not have it"
    state above, but in **its lightest form** — the residue can only be a **subset** of what the engine
    just installed for a `{guest_port, proto}` **the caller explicitly requested**, and since rule sets are
    entirely additive, a subset's reachability is **strictly narrower** than the full set's: it can open
    nothing beyond what was requested, and it does not outlive the next placement (a whole rebuild clears
    it). **The remaining real cost is only in diagnosability**: the engine must log this failed delete-back
    together with the `{guest_port, proto}` involved, or an operator faces a host endpoint that "connects
    but delivers no data" with no corresponding item anywhere in the control-plane records.
  - **Publishing the same `{guest_port, proto}` twice is an idempotent no-op** (return the existing item,
    do not append a second one). **Deduplication must happen at the declaration layer, because the
    substrate will not discover it for the engine**: measured, nft accepts two byte-identical DNAT rules
    without error and the second never matches. And with two identical items, deleting one through `PATCH`
    still leaves one — that is, "the publish was withdrawn and the port is still open", a silent failure on
    a security-relevant field.
  - **This write does not touch `UpdatedAt`**: `retention` counts from that column (§3.4's discipline ②),
    or else adding a port to a `paused` sandbox would incidentally extend its eviction deadline. **This
    obligation exists only because `paused` is allowed.**
  - **From which follows a hard requirement on a whole-`network` `PATCH`: it must be read-modify-write** —
    `PATCH` replaces the whole thing, so a binding assembled from a stale read-back **silently drops** items
    added by `publish` in the meantime. This is **a real danger on a security-relevant field** (losing a
    publish is a detectable functional gap, whereas the reverse — writing back a publish that had been
    deleted — reopens an ingress port), not a stylistic reminder; the wire-side rules are carried by §6.1 /
    §6.2.
  - **Removal has no dedicated endpoint today**: withdrawing a publish goes through a whole-binding
    `PATCH`, so like the three egress fields it **takes effect at the next resume**. **This is not an
    oversight in symmetry but the fact that "withdraw" and "add" are not mechanically dual**: adding is
    purely additive and takes effect on all flows at once; whereas **withdrawal cannot be done cleanly on a
    `running` sandbox** — measured, after deleting those two DNAT rules new connections are refused **while
    already-established flows keep passing data** (conntrack holds that NAT binding and a rule deletion is
    not evaluated against it; item by item in network.md D5 ⑰). So an un-publish on a `running` sandbox
    could promise only "no new connections are accepted" and not "this port is closed", and the difference
    between the two is exactly what a caller wants when withdrawing an ingress port. A whole `PATCH` going
    through `next-resume` **does not** have this ambiguity (the placement is rebuilt entirely and old flows
    disappear with it). Implementing a dedicated endpoint would first require answering "should withdrawal
    semantics also break in-flight connections", which is an independent semantic decision plus a
    measurement, so it is not for today.
  - **The caller-side read-modify-write carries one ordering requirement: that GET must come after the
    pause.** The two write points **overlap** on `paused`, so this lost update **needs no state transition
    to interleave** — doing the GET before the pause brings the entire pause window into the danger zone.
    The safe sequence is `pause → GET → PATCH → resume`. **The contract accepts last-writer-wins here and
    says so, introducing no optimistic-concurrency stamp**: a record-level version number is exactly the
    counter §3.3 rejected (and `generation` is barred outright by the §1.1 vocabulary boundary).
    This class of conflict has **a purely additive cure** — give `ingress.publish[]` a delete write point,
    after which `PATCH` need not carry that field at all and the conflict **class disappears** rather than
    being detected; that endpoint does not exist today (previous bullet), so today this is a documented
    sharp edge.

- **Four user-visible semantics the contract must state plainly** (a caller will hit these; they are not
  internal details):
  1. **A service bound to `127.0.0.1` cannot be published to.** A sandbox has its own netns and its own
     loopback, the host-side forward lands in the guest netstack, and a process listening only on loopback
     receives no delivery. This is standard TCP/IP semantics rather than a substrate quirk, but nearly every
     dev server binds loopback by default, so it will **look like publish is broken**. The contract: require
     `0.0.0.0`, and when bound to loopback **the connection is refused and fails fast** (rather than hanging
     until timeout). **The contract stops there and does not promise "the error is attributable"**: measured,
     this case and "there is no listener in the guest at all" are **word-for-word identical** on the caller's
     side (both are an RST from the gVisor netstack, that is, `connection refused`, both sub-millisecond), so
     an external caller **cannot** tell the two apart (network.md D5 ⑯). Attribution needs the engine's
     side — it knows which rules it installed for this port and can read their counters, while the
     party holding only an RST cannot. **That party is the external TCP client, not the API caller**:
     §6.1's `GET …/network` now reports those counters per declaration entry, so an API caller *can*
     attribute, and what stays unattributable is a refusal seen from outside the engine.
  2. **Unallowed UDP is refused (not dropped), so QUIC does not work under `default:deny` rather than
     degrading.** The refusal shares its origin with TCP's: refusals facing the guest are always `reject`,
     and on UDP only `icmpx port-unreachable` is honored by gVisor (`admin-prohibited` is ignored and it
     still hangs for seconds), so the renderer takes the former — which makes the fallback happen **fast**
     instead of waiting for a timeout. `via[]`'s matching does not hold for UDP (mechanically the original
     destination cannot be recovered, see network.md D3.2). Most clients fall back to TCP, **but that is not
     guaranteed.** This failure direction is safer than "allow whatever did not match", at the cost of
     presenting as "some sites inexplicably do not work". **This exclusion applies only in the egress /
     `via[]` direction** — inbound `publish` accepts `proto: udp` as usual, since DNAT to a fixed guest port
     needs no recovery of the original destination and that mechanical objection cannot reach it (the wire
     and status codes are in §6.1 / §6.2).
  3. **For DNS to work under `default:deny` the caller must declare it themselves — the engine deliberately
     does not do it for them.** DNS is almost entirely UDP, so the previous bullet's cost lands on it first:
     when only TCP/53 is allowed, resolvers **do not fall back to TCP**, and the measured symptom is a 10–20 s
     hang followed by a **name-resolution failure** (rather than a policy refusal — an instance of D4.3's
     diagnostic mismatch; the numbers are in network.md D5 ⑦). So the usage is to write the resolver's address
     into `egress.allow[]` together with `proto: udp`; **the only stably usable host-local resolver address is
     this sandbox's own `/30` gateway**, whose delivery path is that declaration allowance on the `input` side
     (the one in the rule-generating obligations above, reasoning in network.md D4.2).
     **Why not do it for them**: doing so would require the engine to know the concept "resolver" and to hold
     a deployment-specific address, breaking §1.1 — the same criterion as "domain names do not enter the
     engine" (network.md D2). **Which resolver addresses are actually declarable is a property of the
     deployment environment**: the loopback-bound kind is unreachable by construction for a gVisor guest, and
     the kind falling inside the floor's daddr items is stopped by the floor (measured item by item in
     network.md D5 ⑦).
  4. **`default:allow` + publish together form a bidirectional path from outside → the sandbox → the public
     internet** (that is, an open proxy). Each feature alone is compliant, and the composition's semantics
     must be stated; **whether to forbid it is product policy and is not adjudicated by the engine.**

- **The declaration is a Binding (§3.4)**, recorded on the sandbox's own control-plane record, with the rule
  set regenerated from the **current record** at every placement and rendered and installed by §5.A. So it is
  stable across resume and rebuilt after a crash by §4.4-A's reconciliation — the rules, like the netns, are
  crash-surviving host state.
- **"No authorization subject is needed here"**: the declaration is keyed by the **opaque sandbox ID**, so
  this subsystem **reserves nothing for multi-tenancy** (§1.3's cost criterion: that would be a reservation).
  Multi-tenancy remains a non-goal, and its path in is in §1.4.
- **Not included**: the host↔guest control channel (→ E); any application-layer behavior and any credential
  concept (§1.4 permanently excludes it).
  **Do not read this section as "the engine contains a stripped-down gateway"** — connection termination,
  protocol recognition, and content rewriting do not exist on the engine's side, and neither does a seam that
  would carry them.

### 5.E Transport (the host↔sandbox channel)

- **Target**: let the host reliably run processes and read/write files inside a sandbox, with several
  concurrent connections. **The engine exposes only two general, stateless capability services (process /
  fs), plus a `Health` probe for its own use** (§4.3's L2 + the readiness gate + the activity signal, not an
  outward capability surface); the two services being self-sufficient = an E2B-style sandbox, with agent
  session semantics outside the engine (last bullet).
- **The host-side client is cut into those three pieces rather than one large interface**: the `Health`
  probe / process / fs are each their own interface, and the concrete client satisfies all three. The
  criterion is that **each site depends only on the piece it genuinely uses**: the §4 mediator uses only
  `Health` throughout (the readiness gate + §4.3's classifier), and the data plane is what consumes
  process/fs. The criterion's cost is measurable: with one large interface, every L1 fake would have to
  implement a dozen methods it never calls, so adding one fs RPC would disturb test files that have nothing
  to do with fs.
  (`Deps.Dial` still returns the merged, complete client — because the mediator must hand it to the data
  plane, which keeps **endpoint resolution inside §4**, lets callers address only by SandboxID, and prevents
  a placement from leaking out (§6.2). This one bit of width buys that invariant, and is worth it.)
- **The in-guest daemon (`sandboxd`, a core component)**: a connection's endpoint inside the guest is a
  **payload-agnostic daemon** exposing only two **general** capabilities and **knowing nothing about agents /
  turns / sessions**:
  1. **process** — start a process / reconnect / input / signal / resize / list; PTY is the special case
     carrying a pty field (a shell being `exec /bin/bash` with a PTY), and processes are addressed by pid
     (guest-native).
  2. **fs** — read and write sandbox files: `read` (optionally with an offset) / `write` / `stat` / `list` /
     `mkdir` / `remove` / `move`, plus a **streaming bypass for large files** (host-side `OpenRead` /
     `WriteStream`, sharing the guest's same listening port with the RPCs, with the threshold set by the
     caller, D4.4); **no watch** (a consumer polls for increments itself with `read`+`stat`).
  The exact RPC list has the proto as its authoritative contract (§6.1).
  This daemon is the concrete carrier that makes "the CLI/TUI/WebUI are all peer clients of one API" (§6)
  true. It **must stay general**: it embeds no agent/turn/session semantics, since those are an adapter's
  business outside the engine (last bullet).
- **The carrier: TCP inside the netns, not vsock**: `sandboxd` listens on a TCP port inside the guest netns,
  connections are terminated by the gVisor userspace netstack (the sentry), and the host connects directly
  through that netns's veth IP — unary over HTTP, streaming over h2c (server-stream/client-stream + immediate
  flush), order-preserving, sub-millisecond, with no deadlock on large payloads. A cold start onto a new
  netns IP (the old one void) reconnects with zero changes, **directly delivering §3.3's "the carrier does
  not encode the placement"**. That veth IP is allocated by the placement (§5.A) and is not a runsc product;
  how the host obtains the IP belongs to §5.A and does not enter this subsystem. Details in
  [`deep-dives/transport.md`](./deep-dives/transport.md) D4.
  **The endpoint's two halves belong to two subsystems: the IP to §5.A (which allocates it), the port to
  §5.E (`sandboxd` listens on it)**. For §5.A to assemble `IP:port` it must know the port, but **that must
  not become a §5.A → §5.E dependency** (§4: subsystems do not call each other) — the port is a shared
  constant and therefore lives in a **dependency-free leaf**, with both ends depending on that leaf and not
  on each other. The port can be constant across sandboxes because each sandbox has its own netns, and
  disambiguation comes from the veth IP rather than the port.
- **Stateless**: what this constrains is the **control plane's durable SoT** — the engine keeps no durable
  record for a connection, and "what processes are running on this sandbox right now" = a live
  `process.List` rather than a control-plane registry. **It does not mean "dialing uses no process-level
  resources"**: the host-side client is constructed per endpoint and can be discarded freely (it has nothing
  to close), but the **HTTP/2 connection pool beneath it is shared process-wide** — keyed by `IP:port`, so a
  resume onto a new IP naturally gets new connections and connections to retired endpoints are reclaimed by
  idle timeout. **Sharing one pool is a correctness requirement rather than an efficiency optimization** —
  the leak reasoning and measurement for building a pool per dial are in
  [`deep-dives/transport.md`](./deep-dives/transport.md) D4.1.
  Restartability is not the engine's burden: a raw pty byte stream is explicitly lossy (a shell has no
  durable SoT of its own), and any "lose nothing" guarantee is for a layer above to handle. The transport's
  in-process, transient, non-authoritative output ring buffer (for `Connect` replay + non-blocking discard
  for slow subscribers) is not within this constraint, and its mechanism is in
  [`deep-dives/transport.md`](./deep-dives/transport.md) D4.3.
  A transient signal of the same class: the **sandbox-global "last I/O" timestamp** the `Health` probe
  carries back (§4.3's connection-activity signal for the idle health state), which is in-process, dies with
  the transport, does not survive resume, and does not enter the control plane — it measures "how long since
  I/O, right now", not a connection registry. Why it is measured on the guest side is in
  [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D1.
- **A session is a purely additive thing outside the engine**: agent session semantics (turns / history /
  resumption) are **not implemented inside the engine** but by an adapter outside it, layered on
  `process`/`fs`: the turn loop is recognized on the process stream, history is persisted driver-natively
  (inside the Overlay artifact, readable through `fs`), and resumption calls the agent's native `--resume`.
  The engine adds not one new concept — **the openness is in the adapter layer, and the engine stays small
  and stable** (§1.3 / §1.5#4; the ACP shape mapping and the seam to hold are in
  [`deep-dives/transport.md`](./deep-dives/transport.md) D1.2 / D3).
- **The engine supports exactly one class of "running things": processes brought up through exec** (a
  monitor / a build / an `apt`|`pip` install / a dev server / an artifact server), and **system daemon
  services (the `service`/`systemctl` world) are not a target**. This moves the whole class of needs that
  "require a complete init mechanism" out of scope (detail in
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md)).
- **Not included**: agent / session / turn semantics (→ an adapter outside the engine), and the network
  declaration and its enforcement (→ D).

#### 5.E.1 The guest startup topology: `sandboxd`'s two process roles

`sandboxd` is the core in-guest component, carrying **two orthogonal responsibilities** split across
**one binary, two processes** — not merged (merging would let a crash in the most complex part, the
transport, crash the entire guest) and not a second binary (holding §1.1's single-binary deployment).
runsc uses `sandboxd` as the container entrypoint, starting it in **init mode** as PID 1, which then
**re-execs itself into transport mode** through `/proc/self/exe` as a child:

```
PID 1 = sandboxd (init mode: reaper + signal forwarding + subreaper backstop + supervise/restart)
  └── sandboxd (transport mode: this section's process/fs endpoint, knowing nothing about
      |         agents/turns/sessions)
        ├── /bin/bash                  (process + pty)
        ├── an agent process           (process)
        └── apt / http.server / build  (children brought up by exec)
```

- **Init mode (PID 1)**: minimal init semantics written ourselves (reaping orphans/zombies,
  container-level signal fan-out, supervising and restarting transport mode in place), with tini's
  source as the correctness checklist and **no tini binary bundled**. It acts only as the **backstop**
  reaper: transport mode declares itself a subreaper through `PR_SET_CHILD_SUBREAPER`, so orphans from
  the process tree it started reparent to it and are reaped by it, and PID 1 reaps only genuinely
  global orphans.
  - **"Declaring" and "reaping" are one action and cannot be implemented apart**: declaring subreaper
    without running `Wait4(-1)` is **worse** than not declaring at all — without the declaration
    orphans reparent to PID 1 and are drained by it, whereas with the declaration and no waiter every
    orphan piles up under the transport as a **permanent zombie**. And wildcard reaping and per-child
    waiting **cannot coexist** (the measured detail and the shape of the handoff table are in
    [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.3).
- **Transport mode (the child)**: it is this section's process/fs endpoint, **staying purely general**
  and knowing nothing about agents/turns/sessions. **Directed signals** (to one process / a process
  group) belong to it (it holds the pid/pgid handles); **container-level signal fan-out** belongs to
  PID 1.
- **The cost: delivering exit codes has two boundaries.** ① Lossy across a transport restart: the exit
  code is guaranteed **only within one transport lifetime**, and across a restart it degrades to
  "`process.List` still sees it, the exit code may be lost"; ② **within** a lifetime, a process's pid
  becomes unaddressable the moment it exits — the `end` frame goes only to subscribers online at that
  instant, after which `Connect` on that pid is not-found, and **no exit status is kept for someone to
  collect later** (the pid is reclaimed by the kernel, and keeping an exited record would start
  describing a different process once the pid is reused). Both share their origin with the raw pty
  byte stream's lossiness: the engine is stateless, a "lose nothing" guarantee is for a layer above,
  and no internal state is added for it. The mechanism and the seam are in
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.5 (C2 / C3).
- **Guest-side fault injection does not enter the production binary.** Producing a "the transport
  cannot come up" crash loop for tests rests on a sentinel file inside the guest; it is **compiled only
  under the `testguest` build tag**, and that code simply does not exist in a production `sandboxd`.
  The reason is §1.1's guest being root-capable and able to write its own FS: a fault path compiled
  into production would let a sandbox stop its own transport by touching a file. Host-side fault hooks
  (the mediator's cold-start injection) are not subject to this — the engine itself controls when those
  fire.
- **A resident service (if the template ships one) is a peer of transport-mode `sandboxd`** managed by
  PID 1, rather than being stuffed into `sandboxd` (§3.2.1: the engine treats every process alike).
- **The resume cold-start timing** (the exact order of init→re-exec→listen→reconnect, and at which step
  the probe judges healthy) is in
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5.

> **Further detail is in [`deep-dives/transport.md`](./deep-dives/transport.md)**: the Connect-RPC
> choice, the process/fs wire protocol, where a session is delimited and by which seam, and the
> outer-edge projection (control-plane REST + data-plane Connect, D5); **the full reasoning for the
> guest startup topology is in
> [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4**.
