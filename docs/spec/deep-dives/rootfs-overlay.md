# The rootfs / Overlay carrier: how the persistence layer actually lands

> **Position: the persistence-layer deep dive for [`§5.B`](../subsystems.md) (spec; navigation in
> [`SPEC.md`](../SPEC.md)) together with [`§3.1.1`](../model.md) and [`§4.2`](../model.md).** The carrier
> was settled by measurement on the real substrate, with the conclusions in spec §2.5 / §5.B; this
> document keeps what lies behind them: **the falsification process, the measured evidence, the
> alternatives ruled out, the seams to hold, and the agenda still to be refined** (D5).
>
> **Basis of verification:** measured end to end on real runsc + kernel overlayfs (D0's three properties,
> and D1's three assumptions checked one by one).

---

## D0. The conclusion in one sentence

**The persistence layer does not use runsc's self-managed overlay** (the whole `--overlay2` family
`memory`/`self`/`dir=` is non-persistent across a container's lifetime — measured, `dir=` `unlink`s the
filestore at startup, so a clean stop means the data evaporates, D1). **The carrier is instead a host
kernel overlayfs the engine manages itself outside runsc** (the materialization is in D3) — at which
point the upper is a genuinely ordinary directory tree on the host: tar-able, diffable, remountable on a
cold start, and byte-identical, delivering all of §5.B's additional capabilities. **The persistent unit =
the resident upper directory** (a content-addressed diff layer is an evolution item, whose settlement and
cost are in D3).

This conclusion **does not move the model layer**: §3.1.1's "an Overlay is an ID-addressed frozen layer
decoupled from host paths" and §4.2's "pause captures nothing, resume cold-starts by remounting" hold as
written. runsc's overlay flags do not provide an upper that persists across a container's lifetime (§2.5's
fact check), so that upper must be managed by the engine outside runsc.

**The three measured properties**: CoW is correct (modifying a lower lands only in the upper, with lower
bytes uncontaminated); after a clean stop the upper is an ordinary host directory tree and tars out as a
complete diff layer; and the in-container sha256 is byte-identical to a host-side direct read after the
stop. Remounting the same upper on a cold start, and rebuilding into a brand-new upper by unpacking the
tarred diff, both yield byte-identical content (the latter incidentally proving a diff layer can be
extracted / moved / rebuilt, that is, feasibility evidence for D3's evolution item).

---

## D1. Why not runsc's self-managed overlay (the falsification)

A seemingly viable path was to bet directly on runsc `--overlay2`'s `self` / `dir=/path`, assuming it
lands the writable layer as an ordinary directory tree on the host and that a `dir=` upper can be
remounted on a cold start byte-identically. **None of those assumptions survived measurement**, item by
item:

| Assumption | Measured result |
| --- | --- |
| The upper is an ordinary host directory tree | It is a single `runsc-filestore-*` file, **`unlink`ed the instant it starts** |
| After a clean stop it can be remounted on a cold start | The data evaporates when the process exits, and a cold start on the same upper is empty |
| Byte-identical | The file is simply gone, so there is nothing to compare |

**The mechanism**: gVisor's `--overlay2` is not kernel overlayfs but the sentry's own overlay format. The
semantics of `dir=` are "the filestore **lands on the disk this directory is on**" (using disk rather than
RAM as the overlay's backing store, to control size), **not** "persist the upper to the host as an
ordinary directory tree". The flag documentation reads "filestore will be created", it is `unlink`ed as
soon as it is opened, and it survives only on the sentry's open FD — reclaimed when the last FD closes
with the process.

This also rules out the apparent fallback of a "sentry-owned overlay" — that **is** the `--overlay2`
family, the same mechanism rather than another road. So the paradigm had to change (D0: the engine manages
overlayfs itself outside runsc) rather than shifting gear within runsc's self-managed overlay.

---

## D2. The carrier's shape and its evolution target: an outer overlayfs + a content-addressed diff

The outer overlayfs carrier (①②, materialized in D3) naturally accommodates one mature evolution path
(③), and that is what determines §3.1.1's abstract shape. All three steps' persistence **never touches
runsc's overlay**:

**① runsc's overlay is switched off** (`--overlay2=none --file-access=shared`): the rootfs runsc receives
is **the merged directory of a host kernel overlayfs the engine has already mounted outside**, proxied
into the guest by the gofer (`file-access=shared`).

**② The upper is an ordinary directory tree of a kernel overlayfs**: the merged mount's `upperdir=` is a
real path on the host, walkable directly with `filepath.Walk` — a different world from that gVisor
filestore `unlink`ed at startup (D1).

**③ This carrier has room for a content-addressed diff evolution path** (the persistent unit itself is
**the resident upper**, already settled, see D3; ③ does not say "the persistent unit is really something
else" but "under the same abstraction, this path **can also** grow"):
- Extract the upper into an OCI diff tar + a `{MediaType, Digest, Size}` descriptor and push it to an
  object store.
- On resume/claim, apply the diff back onto a **new container's** live rootfs bind mount.
- Take an **incremental** diff against some baseline layer — naturally layered.

**The key insight**: the persistent unit **need not** be "an upper directory lying on disk the whole
time" — today it is, but the abstraction does not depend on that.
**This is precisely §3.1.1's bet** — defining an Overlay as "an ID-addressed frozen layer decoupled from
host paths" rather than "an upper permanently on a local disk", so that changing the persistent unit does
not move the model. This capability set maps onto the model item by item (the evolution item's settlement
and cost are in D3):

| Diff-layer capability | How it is given | Status against the shipped carrier |
| --- | --- | --- |
| pause / resume | The latest diff = the latest checkpoint, applied back into a new container on resume | **Already delivered** without it — resume remounts the same upper, and pause captures nothing (§4.2) |
| Promote → a layered Template | Take an increment against a baseline and stack layer on layer | **Already delivered** — multi-lowerdir layering ships (§5.B Promote / layered Templates) |
| fork / clone | Claim several sandboxes from the same diff/snapshot | **Already delivered** — create-from-Template, a Promote layer and a fork are all **one kind of reference** (§4.3 / §3.1 fork) |
| Portability across hosts | A diff is an OCI blob in an object store, not pinned to a local path | **Already true, and measured** — after a clean stop the upper tars out as a complete diff layer, and a fresh upper rebuilt from that tar is byte-identical (D0) |
| snapshot / restore | Named diff records that can be listed/restored/deleted | The one genuinely absent capability (no corresponding §) |

> **So the evolution item is dedup and integrity, not capability.** Four of the five rows above are
> delivered by the shipped carrier through other means, which is why this item reads larger than it is.
> What a content-addressed layer would actually buy is that identical bytes stop being stored once per
> layer — Promote copies byte-for-byte today, so N layers sharing a base hold N copies — plus a digest
> to verify a layer against. Both are real wants; neither is a capability the model currently lacks.

---

## D3. The materialization

**The engine assembles lower (the Template) / upper / work with the host kernel's overlayfs outside runsc
and hands the merged result to runsc with `--overlay2=none --file-access=shared`.** This holds three
established properties: §3.1's three states (a read-only shared Template + an Overlay increment), §5.B's
incidental capabilities of the underlying storage (CoW landing on reflink), and §3.1.1's
carrier-independence. The overlayfs mount/umount lifecycle belongs to the Rootfs subsystem (§5.B).

**The persistent unit = the resident upper directory.** The upper stays in place on the backend, pause
captures nothing, and resume remounts the same upper for a cold start — closest to §4.2's semantics, and
already sufficient to deliver pause/resume plus Promote's at-rest read. **A content-addressed diff layer
(extracted as an OCI tar + digest) is an evolution item**, and what it buys is **dedup plus integrity** —
not layered Templates, fork or cross-host portability, all three of which the shipped carrier already
delivers (the table in D2). Its cost is tar extraction + baseline increments + an object-store interface.
Both share one Overlay abstraction, layers above notice nothing, and D4's seam guarantees it.

**The alternative ruled out — a bind mount as the writable layer** (giving the container a host bind
directory as its writable rootfs, likewise with `overlay2=none`): the simplest option, but **it loses CoW
layering** — a Template is no longer a read-only shared base and every sandbox must copy a whole rootfs;
Promote/fork would have to be built separately on an external CoW FS (reflink/btrfs/zfs). It does not fit
§3.1's three-state model; and if the underlying FS supports reflink to restore layering, that slides back
into the overlayfs incidental capabilities above, so overlayfs may as well be used directly. Hence not
adopted.

---

## D4. Ownership and the seams to hold

**Ownership (§4's "verbs belong to §4, mechanisms to §5" + subsystems do not call each other)**: the
overlayfs mount/umount is a mechanism of the **§5.B Rootfs subsystem**, **called** by the §4 lifecycle at
create/resume — it is neither §5.A Runtime assembling the overlay nor Rootfs poking the runtime provider.
runsc receives only an **already-merged rootfs path** as its `(rootfs source)`, which lands exactly in
§5.A's provider contract input `(rootfs source, …) → (placement handle, …)` — **the provider does not care
how the rootfs was assembled**, and assembly is the Rootfs subsystem's business.

**The seams to hold (a resident upper must not wall off either the diff-layer evolution or a future
provider):**

1. **The Overlay abstraction exposes upward only "an ID + the Template it references + readable at
   rest", and never "it is a host upper path"** — so that evolving from a resident upper to a
   content-addressed diff changes **not one line** of §4.2's resume or §3.1's Promote/fork. Host paths
   must be locked inside the Rootfs subsystem (§3.1.1's "ID addressing applies internally too").
2. **Materialization goes through the rootfs provider seam** (§5.B): the sole implementation is the
   single-node "local kernel overlayfs + local directories"; a diff layer (the evolution item, D3) and
   multi-node (a PV / object storage) are **registering another rootfs provider or changing the
   persistent unit**, with the model unchanged — the abstraction must have room for them.
3. **Do not introduce containerd** (which conflicts with §1.1's "a single binary, no heavyweight external
   dependencies"): reusing containerd's overlayfs snapshotter only pays off when already running on
   containerd. We **implement thinly inside the engine** the layer of "assemble overlayfs + (at the
   evolution stage) extract a diff", without pulling in the whole containerd stack. That thin
   implementation is itself seam 2's single-node provider.

---

## D5. Mechanical constraints this carrier brings with it (plus one evolution item)

> On top of the carrier and the persistent unit (a resident upper), this section records the constraints
> the carrier brings with it. **All but the last (the content-addressed diff) are settled** — what was
> rejected is recorded as rejected rather than left as a to-do.

- **Cleaning up / reclaiming the work dir and the upper**: overlayfs needs a workdir (constrained to the
  same overlayfs-capable FS as the upper). **A resume remounts the same upper, but the workdir must be a
  newly created empty directory** — the previous work cannot be reused (the kernel refuses it or it
  breaks). It is mounted in two halves: **cleanup at destroy** hangs on §4.3's reaper object-reclamation
  invariant (the upper being a resident persistent unit); **orphan reclamation after a crash** — an
  overlay mount is a placement-local resource, and a half-built mount left by the orchestrator crashing
  mid-transition is a **special case** of §4.4-A's crash variant (startup reconciliation) rather than
  something resting on refcount GC. The overlay mount is created and unmounted by the engine outside
  runsc, with the unmount timing aligned to §4.2's pause.
- **Mounting is a privileged operation**: overlayfs mount/umount requires root (CAP_SYS_ADMIN), and files
  copied up into the upper are therefore **root-owned** — so deleting the upper at destroy needs privilege
  too. This is one more concrete reason for "host paths + the mount lifecycle are locked inside the §5.B
  subsystem and not exposed to callers" (§3.1.1): a privileged mount is a subsystem mechanism, not the
  mediator's job.
- **The capacity quota (the disk capacity axis, §3.4)**: how the per-sandbox upper+work cap is
  materialized is in D6.
- **The pause flush consistency point**: **the substrate half is settled and depended upon** — after a
  clean stop (kill+delete), the at-rest consistency of the sentry's/gofer's writes flushed into the upper
  is verified (D0/D3), and what Promote reads is exactly that at-rest upper after a preemptive stop
  (guest-runtime D4.4: depending on no process cooperation). **An application-layer cooperative safe point
  is not an open item but a rejected one** (guest-runtime D4.4: the engine does not know about agents, so
  it can neither require a workload to stop at a safe point nor verify that it stopped at the right one;
  the semantics of a timeout / hard kill match those of a crash, and a crash is the bottom line §4.2
  already accepts), so **there is no still-open consistency-point question on this carrier**.
- **Excluded paths / portal paths**: extracting a Promote/snapshot must filter out runtime paths the
  engine injected itself (such as the working directory of the in-guest daemon).
  - **sandboxd's two portal files (the transport pidfile / the crash-loop sentinel) are inert entries on
    that list**: `/tmp` is mounted as **tmpfs** by the OCI spec, so they **never enter the upper at all** —
    excluding them targets a state unreachable in materialization (which is why their guard test asserts a
    situation the substrate cannot produce, reading like coverage while being empty). Keeping them is
    harmless (one list entry is cheaper than "a Template every sandbox is born unable to serve"), but
    **they are not the reason this mechanism exists**.
  - **What the engine injects that genuinely lands in the upper is the large-file bypass's upload
    temporary file** (excluded by base-name prefix, because the exact path is unknowable in advance — it
    lands in a directory the caller chose). That is the live consumer of the exclusion mechanism.
  - **An empty placeholder needs no exclusion entry.** Measured: when the dest exists in neither lower nor
    upper, an **ordinary empty directory, mode 0755 root:root, with zero overlayfs xattrs** already appears
    in the upper after `runsc create` (before any guest code) — not a whiteout, not opaque, with no
    origin/redirect, and not inheriting the source directory's permissions. So **the placement materialized
    it, not the payload**. The result §5.B already states ("a mount point in the frozen result is just an
    empty placeholder directory") holds by default. The reverse hazard is clean too: the guest's writes
    under the bind land **entirely** in the Volume's host directory and the upper gets nothing (with a
    positive control: another control file in the same launch did enter the upper), that is, **Volume
    writes correctly land outside the sandbox's disk cap and outside a Promote's capture**.
  - **A sharp asymmetry: a missing dest dirties the upper, while a Template that provides the dest keeps
    it pristine.** A dest already present in lower **does not trigger copy-up** (measured: the upper is
    byte-for-byte empty afterwards). **A design by-product**: preinstalling common mount points in the
    built-in Template keeps the common paths clean, degrading the exclusion entries to a correctness
    backstop rather than a routine mechanism. It cannot be relied on for a caller's own choice of dest.
  - **What the exclusion entries genuinely have to do is prune pre-existing bytes shadowed by a mount
    point.** Measured: seeding `dest/stale` into the upper first and then shadowing it with a bind, the
    guest **cannot see** those bytes at all (seeing only the Volume's entries) while they remain on disk
    and in the upper — so they would be copied whole into the frozen layer, **and counted into that
    sandbox's capacity cap and cost forecast at the same time**. The existing mechanism can already prune
    correctly (an exclusion matching a directory skips the whole subtree), so **what is missing is not the
    matching logic**.
  - **Hence the set of mount points must be a per-Promote input and must not be a static package-level
    list**: a Volume dest is a per-sandbox mount row, which a static list has no way to know. Passing it as
    a parameter is also the architecturally cleaner road — the §4 mediator holds the mount table already
    and calls Promote already, so **§5.B never has to ask §5.C** (producing no cross-subsystem call). **The
    cost forecast uses the same set**, or it would keep counting shadowed bytes that will not be copied
    (a safe direction, but knowably wrong). Paths follow the existing convention: **relative to the upper,
    with the leading slash removed**.
  - **A nested dest needs an explicit decision, and it is settled**: the **whole chain** of a nested dest
    enters the upper (when all three levels are absent, all three are created), so excluding only the leaf
    would leave ghost intermediate directories in the layer, while excluding the top component is wrong in
    the general case (the guest may legitimately have written something else under that directory).
    **Settled as: prune the leaf subtree and accept an empty intermediate chain** — the same shape as the
    empty placeholder §5.B already sanctions, only one level deeper, and it **never loses real guest
    data**.
- **The content-addressed diff layer**: the **only genuinely still-open** item on this carrier. Its
  settlement and cost are in D3, and the capability mapping is in D2 — which also records that four of
  the five capabilities usually attributed to it already ship, so what is open is **dedup and
  integrity**.

### D5.1 The properties this seam must keep for a future arm to stay additive

A content-addressed arm registers behind `rootfs.Provider` **unchanged** — walking the interface member
by member found one strain point and it is not this document's (`Merged.MergedPath` is a host path, which
is the same value `runtime.RootfsSource` carries, and open-decisions' §5.A provider row already owns the
remote-handle question). So **no surface is added here**: §1.3 forbids reserving it, and the repo already
litigated this on the sibling seam — `volume.Backend` has a `Capabilities()` because a **second
implementation shipped**, and refused a `DirectIO` bit because none had; §5.B has one implementation and
so has no such debt.

What is worth writing down instead is the list of properties a future arm must inherit. Each is measured
or structural, and each has a defect behind it that a fresh implementation would otherwise repeat:

1. **Reflink *outward* leaks one quota row per Promote, and the leak outlives the layer.** The predicate
   is *extent sharing*, not "a snapshot was taken": reflink **inward** (copy-up's shape) does not leak,
   outward does. Because a leaked row reserves no subvolume id, a remount can hand that id **and its
   stale cap** to a brand-new unit (volume.md D7.1's measured `ENOSPC` at 3.3 MB on a unit nobody
   capped). Any dedup arm creates outward sharing **by construction**, so the sweep-side reclamation
   path must exist before it does.
2. **The best-effort quota-row cleanup at subvolume deletion is safe today only because the engine never
   snapshots an upper.** That precondition is stated as the reason; a diff layer invalidates it, so both
   it and its failure policy must be revisited in the same change.
3. **The quota mode is not negotiable, and a network *filesystem* can never host the upper.** squota
   because this substrate physically cannot change modes, and squota charges an extent to **whoever
   first allocated it**, the charge surviving that allocator's deletion — so a shared content store
   cannot be charged "fairly", only once, to its first writer. Separately, the kernel refuses a network
   filesystem as `upperdir` on two independent grounds (missing `d_type`/xattr support, and durability).
   The viable remote shape is therefore a network-attached **block** device carrying a local filesystem.
4. **`UncappedBytes`'s "shared bytes count at most once" does not survive cross-layer sharing as
   written.** The walk dedups by dev+ino **within one call**, and it is called once per layer directory —
   sound today only because nothing creates cross-layer sharing. A dedup arm creates it, at which point
   shared bytes are counted once per referencing layer and §3.4's admission gate over-refuses. The set
   must be hoisted across the whole enumeration.
5. **The workdir must be *emptied*, never recreated** (D6.6 ③): `mkdir` needs a metadata extent, and a
   unit at its cap has none — measured, recreating made a sandbox that hit ordinary `ENOSPC`
   **permanently unresumable**, breaking §4.2's reversible-pause promise.
6. **Exclusions are a per-Promote input, and the freeze and the cost forecast must share one set** (D5
   above), or the forecast counts bytes that will never be copied. A content-addressed arm must apply
   exclusions **before hashing**, or it addresses content it must not ship.
7. **The cost forecast must not under-report** — over-reporting refuses a Promote that would have fit,
   under-reporting spreads `ENOSPC` onto unrelated sandboxes (§2.3). An arm answering "≈0, those bytes
   are already in the store" is contractually anticipated, provided it is true.
8. **Bytes-first ordering and the in-flight keep-sets stay** (§4.3 existence-is-state). Note the recorded
   asymmetry: §5.C had to split into "backend enumerates, mediator decides" after this shape destroyed
   committed data, while §5.B keeps the simpler shape deliberately because its row precedes its bytes. A
   **shared** store changes that calculus — a shared blob has no single owning row — so it likely needs
   the §5.C split.
9. **An addressable layer format must not use `index=` or `metacopy=`.** With `index=on` the kernel
   stamps the lower root's file handle into `trusted.overlay.origin` and verifies it on every later
   mount, so a layer copied to another tree or machine fails `ESTALE` — precisely the portability such an
   arm wants, broken by the feature. `metacopy` is worse in kind: **no mount-time verification at all**,
   so a wrong lower set mounts clean and misbehaves later.
10. **Lineage depth is bounded well below the kernel's limit, and the substrate fails opaquely at it —
    so the engine refuses first.** The lower stack is passed as one colon-separated `lowerdir=` string,
    and the classic mount path caps its options at a single page and **truncates past it rather than
    refusing** — so with this repo's Template ID width the ceiling is on the order of **~77 layers**
    (82 for `/var/lib/warren` paths; the exact number is a function of path length), not the kernel's
    `OVL_MAX_STACK` of 500. Escaping the limit itself means the per-layer `lowerdir+=` configuration API
    (trading one aggregate limit for a per-path one) or passing descriptors; neither is used.
    **Two checks, because one cannot cover the other.** `Provider.MaxLowerDepth` reports the depth the
    provider can guarantee, and the mediator refuses past it at the gate — for a **Promote before the
    quiesce**, since the layer bytes are frozen and the row inserted before anything mounts the result,
    so an unrefused Promote reports success and leaves a Template that can never start a sandbox. That
    number bounds a promoted ID exactly and a **builtin's not at all** (its ID is a deployment-chosen
    directory name), so `Mount` additionally checks the assembled options string and returns an error
    naming depth and limit. Lineage resolution carries a cycle check as well; **the earlier statement
    that it carried no depth bound described the code before that gate existed.**

### D5.2 Multi-node addressing: settled — the writable upper does not travel

Registered previously as an open item ("multi-node addressing across nodes via the rootfs provider"), and
it resolves rather than staying open, because the premise was wrong: **no mature system moves a live
writable layer between nodes.** The evidence is consistent across four of them — a pod is bound to its
node and never rescheduled elsewhere with its writable layer; the kernel refuses a network filesystem as
`upperdir` (③ above) and fails closed on a copied `index=` layer (⑨); one container manager attempted live
migration and retreated to stop-move-start; and a microVM treats resuming one snapshot twice as unsafe.
What crosses a node boundary is always a **quiesced artifact** — an image layer, or a frozen delta plus a
deletion list — or nothing at all, with the writable disk declared the caller's problem.

So the shape is: **the upper is promoted into an immutable layer, and only that layer crosses nodes.** One
correction to the obvious reading — the artifact **need not be content-addressed** to travel, because
Promote already produces it and D0 already measured that it tars out and rebuilds byte-identically. Dedup
(D2/D3) is therefore an **independent** question from portability, not its precondition. The only residual
that touches the engine is the **handle shape** a remote provider would receive, which
open-decisions' §5.A provider row owns.

---

## D6. The capacity quota: how the per-sandbox upper cap is materialized

> **Position: the deep dive on [`§3.4`](../model.md)'s disk capacity axis + [`§5.B`](../subsystems.md)'s
> capacity-quota responsibility.** The carrier (an outer kernel overlayfs, D0) makes the upper an ordinary
> host directory tree, which is exactly what makes the lightest quota mechanism available.

### D6.1 The problem: kernel overlayfs has no built-in size ceiling

The outer kernel overlayfs (D0) solves only "persistent + portable + diffable" and **carries no capacity
ceiling**: the upper is an ordinary directory tree, and how much can be written into it depends on the
free space of the FS it sits on. Without a quota, a single sandbox's `dd if=/dev/zero of=big` can consume
the whole disk the upper is on, making the upper writes of **every sandbox on that disk** fail — breaking
§2.3's "the amplification happens only inside this sandbox's data plane". The capacity ceiling must
therefore be applied explicitly by the rootfs provider (§5.B), orthogonally to D0's carrier: the carrier
gives "an ordinary directory tree", the quota gives "how big that tree may get".

### D6.2 The materialization mechanism (single-node): a btrfs subvolume + squota is the first choice

| Mechanism | Isolation strength | Cost | Fits |
| --- | --- | --- | --- |
| **A btrfs subvolume + an squota limit** (a hard quota limit on the subvolume holding upper+work) | Medium, a hard `ENOSPC` at the cap | Lightest: quotas are already enabled fs-wide on this substrate, two `btrfs` subcommands, no extra mount; a subvolume also delivers cheap atomic snapshots | **The single-node first choice** |
| **A loop device** (one fixed-length image file per sandbox, with an FS inside it mounted, upper/work landing there) | Strong, physically fixed-length isolation | A per-sandbox loop dev + mount lifecycle to manage, heavier on resources | When strong physical isolation is needed |
| **A separate FS / thin LV** (one thin volume or separate partition per sandbox) | Strongest | Heaviest, depending on LVM/volume management | Special high-isolation scenarios |

The three are **different implementation branches of the same storage substrate primitive**, and the
abstraction upward does not change by one word: layers above see only "this Overlay has a capacity cap (a
Binding)", never whether the cap is squota or a loop image.

> **The first choice = a btrfs subvolume + squota (measured, full stack).** The substrate (ADR-0002) is
> **btrfs**, which has **no project quota**, so project-quota mechanisms (xfs/ext4) do not apply on this
> disk. Measured through the whole chain
> `subvolume → overlayfs upper/work → gofer → sentry → guest`: filling it triggers a **hard `ENOSPC`
> precise to the cap** (errno 28 reported on `write()`, with `fsync`/`close` returning 0, landing right
> against the cap — measured once at ~0.05%, **but that number may be cited only as evidence of magnitude
> and never as a reproducible precision figure**: no script prints it, and the only test asserting ENOSPC
> precision, `spike-volume-cap-snapshot.sh`, uses a tolerance far wider than it, ±2 MiB on an 8 MiB cap),
> and **the container lives on as usual** (it can be exec'd again). No bypass of the accounting was found
> through sparse files, `fallocate`, hardlinks, or renaming out of lower. Choosing it also brings a
> structural gain — fixing the storage unit as a subvolume simultaneously delivers cheap atomic snapshots
> for Volumes (§5.C) (volume.md D6/D7).

**Why squota rather than full qgroups.** Three reasons: ① squota is designed for exactly "one subvolume per
workload" (low overhead, no rescan storms); ② **the semantics are what actually match the thing being
governed** — the bytes a sandbox **newly writes**. Under full qgroups, copy-up's reflink pushes
`referenced` past the cap (measured: 98 MiB against a 64 MiB cap), falling back only afterwards by
refusing new writes, so **the cost is paid and a hard ceiling still not obtained**; and charging a sandbox
for bytes it reflinked from a Template (content it did not write) is questionable in itself; ③ the
development/test substrate (`sbx`) **cannot change modes** (`BTRFS_IOC_QUOTA_CTL` returns EPERM even as
root with all of `CAP_SYS_ADMIN`), so choosing full qgroups would mean the spec and the substrate
diverging long-term (the drift CLAUDE.md warns about).

> **A cost that must be handled along with it**: under squota a sandbox cannot burst its own cap, but
> **Template / layer bytes still occupy real disk and are covered by no cap**. **This is the reason the
> node accounting inequality's second term exists** — summing caps alone would miss those bytes, and the
> inequality itself is in §3.4.
> **Note that the second term's criterion is "covered by no cap", not "is it a layer"**: a capped Overlay
> upper has already been counted by the first term and is not counted twice; only uncapped bytes (layers,
> and an uncapped upper where one is permitted) enter the second term.

**A copy-up hitting the cap does not damage the merged view (measured).** When a real copy is forced
across filesystems, `open(O_WRONLY)` cleanly returns errno 28, the file is **the correct size in merged
and byte-identical to the lower original**, and there is no half-finished residue in upper/work —
overlayfs copies into a temporary file in the workdir and links it into the upper only on success, rolling
back completely on failure. **But copy-up on the same fs is a reflink**, accepted without being subject to
the cap, so the cap is not a hard ceiling for copy-up (see above: under squota a reflink is permanently
free, which is one of the reasons for choosing it).

**`btrfs-progs` is an explicit dependency** (installed on `sbx`, measured v6.17.1). Another pit that will
be hit at implementation time: **the sentry fabricates `st_blocks`** — the guest reports `536870912` for a
512 MiB sparse file where the host's true value is `0`. **Capacity accounting trusts only the quota and
never a guest-side number** (§5.B).

### D6.3 A key constraint: work must share the quota with upper (a subvolume satisfies this naturally)

overlayfs's copy-up (copying a lower file whole into the writable layer when it is modified) **lands in the
workdir first and is renamed into the upper afterwards**. Quota-ing only the upper and missing work would
make writes during copy-up a path that **bypasses the cap**. D5's measured constraint that "work and upper
must be on the same overlayfs-capable FS" tightens here into: **work and upper must be accounted into the
same quota unit / the same loop image / the same thin volume** — capacity accounting treats the two as one.
**A btrfs subvolume satisfies this naturally**: upper and work live in one subvolume → one quota covers
both, with no bypass path (measured: `work`/`upper` are invisible in the merged view, and all copy-up
traffic is accounted).

### D6.4 Ownership: one storage substrate primitive shared across §5.B/§5.C, producing no cross-subsystem call

The capacity cap's materialization mechanism (create-subvolume + set-quota-limit + delete-subvolume) is
**the same one** for **§5.B's Overlay upper+work subtree** and **§5.C's per-volume unit** — it is a
**shared storage substrate primitive** reused across two subsystems.

> **What is shared is code, not a seam.** It lands as a **leaf package** (`internal/btrfs`) wrapping only
> subvolume create/delete/snapshot + quota limit and **knowing nothing about Overlay / Volume /
> SandboxID**; §5.B and §5.C each import it as they each import `os`. **No cross-cutting `QuotaProvider`
> interface is built** — a remote rootfs provider would deliver a capacity cap by completely different
> means (an object-storage quota / a quota API), and a cross-cutting interface would only weld btrfs
> assumptions into every implementation. Neither calls **the other**; they merely share one substrate tool,
> so "subsystems do not call each other" is not violated. **The Overlay upper is the first consumer**
> (§5.B), and Volume reuses the same package.

The applying action happens entirely on the storage materialization side and calls no other subsystem; the
cap's **value** (a Binding) is handed down by the §4 mediator during create/resume orchestration together
with the rootfs source / the mount table — which is the mediator's job anyway and does not violate
"subsystems do not call each other" (top of §4). Under a multi-node shape, a PV-backed unit's capacity cap
= the PV's size request, still that backend's business, with the abstraction unchanged (§3.1.1).

### D6.5 Boundaries: there are two more layers of behavior beyond the cap

D6 solves only **how the per-sandbox cap is materialized** — downgrading "fill the whole machine" to "fill
your own share". Two other layers:

- **The standard behavior when it fills**: the guest sees an ordinary `ENOSPC` and **the sandbox lives on
  as usual** (§3.4: exceeding capacity is an event inside the data plane and the control plane does not get
  involved). **Only the hard threshold refuses writes** — no capacity-pressure event is emitted and no
  auto-pause is triggered. **The soft threshold is settled: emit no event and add no event kind** (§4.4-C)
  — pressure is a sample of a continuous quantity while §4.1's event stream is a stream of discrete state
  transitions; to monitor it, attach outside the engine as a gauge/sampling (§1.3 add a subscriber).
- **No node-level oversubscription**: the inequality is in §3.4, and why the second term must exist is in
  D6.2. Only **who computes it when** is fixed here: the §4 mediator runs it before create / resume
  materializes the upper; if it does not fit, create fails / resume stays `paused` (§4.4-A's atomic
  rollback to the source state).
  **Bounded oversubscription + disk-pressure monitoring are not in this shape** (a later increment).

  The criterion for the two terms being **mutually exclusive** ("take the strongest available statement
  about each object"; a capped Overlay upper counts 0 in the measured term) belongs to §3.4; this document
  only adds the mechanical grounds on which it holds: under squota a copy-up's reflink is not charged to
  the sandbox unit and that extent has been counted on the layer side — **the property that forces the
  second term to exist (D6.2) is exactly what also makes the two terms naturally non-overlapping**. The
  reasoning that every sandbox must have a cap once the axis is metered (or the inequality stops describing
  that disk) is likewise in §3.4.

  **The residual (stated honestly)**: the second term is measured, so the inequality holds **at the instant
  of admission** and can be broken afterwards. Layer bytes grow only at Promote / seed / sweep — all
  control-plane events, and **therefore gateable** — while §4.3's object-reclamation sweep makes them
  shrink. Any control-plane action that increases uncapped bytes passes the same inequality. **Promote is
  the only one of these that has a verb today**, and seed / sweep follow suit when their verbs appear; its
  three criteria:

  - **The gate comes before the quiesce**: `Promote` first only opens the Overlay to get a handle (without
    touching the cap) and asks the provider for a cost forecast, refusing if it does not fit (mapped to a
    507 by §6). At that moment **the source sandbox is still in its entry state** and has not been frozen,
    so a refused Promote is not an observable transition (the same discipline as §4.4-A's atomic rollback).
  - **This term is a forecast, not a measurement**: the cost is given by
    `rootfs.Provider.PromoteCostBytes`, whose contract is an **upper bound** — it must answer before the
    action, whereas the second term is measured everywhere else. The conservative direction is deliberate:
    overestimating refuses a Promote that could have fitted, while underestimating spreads ENOSPC onto
    unrelated sandboxes (§2.3). A reflink / snapshot implementation may honestly answer about 0.
  - **An in-flight reservation is not a third term**: bytes become visible to `UncappedBytes` only as the
    copy progresses, and the copy is too long to hold a node-level lock throughout, so the mediator records
    a reservation before releasing the lock, serving as Promote's "commit" (equivalent to the row insert at
    create time) and released once it lands. It is **the same term** as the second, merely making the
    in-flight portion visible to a concurrent gate; during the copy's landing it briefly **double-counts**,
    in the same conservative direction.

### D6.6 The limit side of the cap: lowering below usage wedges the unit, and the sandbox side is worse than the volume side

D6.1–D6.5 are about **how a cap is materialized** and **how a node accounts**. The write criterion on the
limit side is the other half: **a cap not above the unit's current usage is accepted by the substrate, after
which the unit is irreversibly wedged**. §5.C settled this for volumes long ago (volume.md D7.3), and this
section records only **the results of re-measuring on §5.B's unit shape** and **three corollaries that hold
only on the sandbox side** — the unit shape differs (one subvolume holds both `upper` and `work`, with
`merged`, a live overlayfs mount point, inside it), so the volume-side conclusions cannot be cited directly.

**The parts identical in shape to D7.3** (whose reasoning is not duplicated): an empty unit's quota row
already accounts 16384 B; lowering below usage is accepted by the substrate with **zero validation**;
afterwards creating a file / `mkdir` / **truncating to 0** all return `ENOSPC`, and **the quota ioctl itself
returns `ENOSPC`** (so the cap can be neither raised nor removed), with only unlink still working; and once
deletions bring usage back below the cap the quota ioctl revives.

**Three that are unique to the sandbox side:**

1. **A wedged unit cannot even get through a placement, so the sandbox becomes a brick.** `applyCap`
   replays the cap from the record at **every** placement, and replaying the same cap on an
   already-wedged unit likewise returns `ENOSPC`. A volume has no placement of its own, so a wedged volume
   is merely "cannot be written to"; on the sandbox side it **can never resume again** — **the same
   consequence** as the already-refused `0 → N` write (§3.4: one 200 PATCH turns the sandbox into a brick).
2. **`limit == usage` is a wedged state too.** Set exactly equal to `referenced`: it is accepted and stored
   the first time, but **replaying it returns `ENOSPC` and raising it returns `ENOSPC`**. So the criterion
   must be a **strict** inequality, word for word the same as the volume side's.
3. **`Mount`'s own "new workdir" step is blocked by the wedge too**, so even skipping the quota write would
   not get a resume through. This one has a separate consequence of its own, below.

**From which two write-point shapes follow:**

- **§6.1's `PATCH` refuses (400, `lifecycle.ErrDiskCapBelowUsage`)**, with the criterion
  `cap > current usage`. It requires §5.B to add a **sandbox-level usage query**
  (`Provider.Usage(ctx, overlayID) (used, limit, err)`), because `UncappedBytes` **counts capped units as 0
  by contract** (§3.4's mutual exclusion) and that is precisely the unit a cap change targets. **Taking an
  ID rather than an `*Overlay` handle**: a handle can be obtained only through `OpenOverlay`, and that is a
  call that **writes the quota**, so running a placement-shaped call to read one number would turn a
  "query" into a side effect.
- **The materialization side uses the same criterion (`rootfs.ErrCapBelowUsage`)**, because §3.4.5 requires
  the write point's criteria to be walked again at the next placement / a crash reconciliation's reinstall /
  a repair rescan, and this criterion reads **exactly the thing that is not durable** (the unit's current
  usage): one external change clearing the quota row ⇒ the guest grows past the cap in the record ⇒ the
  next placement would write the cap below usage.
  - **The §5.C answer cannot be copied here.** `RepairVolumeCaps` **skips** such a volume, leaving it
    temporarily uncapped, with safety caught by §3.4's second term; a placement has no such option — §5.B's
    rule is "if the cap cannot be delivered, refuse this placement, and never silently degrade to running
    uncapped". So this side **refuses** rather than skipping, and refusing is the reversible direction (the
    sandbox stays `paused`, with exits being a PATCH that raises/removes the cap, or rebuilding after a
    Promote), whereas applying is the irreversible one.
  - **The status code splits two ways**: create **carries** `disk_byte` ⇒ the mediator wraps it as
    `ErrDiskCapBelowUsage` (400); resume / adopt **do not carry it** ⇒ it stays 409 (the remedy changes
    state elsewhere). The same split as §5.D's second-hop uid pair, "400 at the write point / 409 at
    placement".
- **The criterion does not wrongly hit a sandbox that "filled its own cap"**: squota stops the guest while
  usage is **strictly below** the limit (measured: a 16 MiB cap filled to 16728064 B, leaving 49152 B), so
  such a unit both replays its cap and raises it successfully. This is a reverse guard with an acceptance
  test of its own.

**③'s separate consequence: `Mount` must "empty work's contents" rather than "delete and recreate work".**
Switched to `RemoveAll(work)` + `MkdirAll(work)`, a sandbox that filled its own cap (§3.4's **normal**
outcome) would **never resume again**: `mkdir` wants a metadata extent while a full unit has only tens of
KiB left (measured ENOSPC); and deleting that **already-empty** directory frees **not one byte, so the
sequence spends headroom it does not have to arrive at the state it is already in**. That violates §4.2's
"pause is a **reversible** gate" — §3.4.4 paid a whole budget for that promise.
**Emptying the contents** keeps the original inode, the kernel then creates `work/work` itself, and mounting
was measured to succeed on that same 49 KiB of headroom. What overlayfs requires is that the workdir **be
empty** (D5: reuse is refused or corrupts), not that it be a new inode.
