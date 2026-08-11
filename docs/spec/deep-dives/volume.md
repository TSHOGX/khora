# Volume: the derivation and falsification of the backend seam / writes / snapshots / leases

> **Position: the Volume deep dive for [`§5.C`](../subsystems.md) (spec; navigation in
> [`SPEC.md`](../SPEC.md)) together with [`§3.4`](../model.md), [`§4.2`](../model.md) and
> [`§4.3`](../model.md).** §5.C carries Volume's model and its settled contracts; this document keeps
> the three things a deep dive should: **why each contract has the shape it has**, **which alternatives
> were ruled out (the falsifications)**, and **what remains outside the contract boundary** (registered
> in [`open-decisions.md`](../open-decisions.md)).
>
> **A whole family of mechanisms this document repeatedly rules out** (WAL→object-store materialization,
> content-addressed layers, cross-node heartbeat reclamation, head CAS, application-layer encryption)
> are all products of **"POSIX over a remote, non-CoW, untrusted object store"** — solving problems a
> single-node engine **simply does not have**. The criterion: anything that is a **remote backend's
> implementation obligation** is sealed inside "that backend" and does not enter the core; the core
> knows only "a Volume is materialized through the backend seam into one host mount point".
>
> **Two substrate facts (measured)**: ① runsc with `--overlay2=none --file-access=shared` proxies an
> **additional rw host directory bind mount** into the guest bidirectionally through the gofer (the
> foundation for the local backend); ② a **host-side FUSE mount point** can likewise be proxied into the
> guest through the gofer (the foundation for a remote backend). The seam shapes below rest on those two
> facts.

---

## D0. The conclusion: one backend seam, two materialization arms

A Volume has only a few more properties than §5.B's Overlay, and each corresponds to one contract (whose
text is in §5.C) and one falsification. **The shape that governs the whole document**: a Volume is
materialized through a pluggable **backend seam**, and the seam yields **exactly one thing — a host mount
point**; runsc writes it into the OCI mount spec and the gofer proxies it into the guest, entirely
isomorphic to §5.B's merged rootfs. The backend has two arms:

| Arm | Materialization | Who delivers POSIX | Measured |
| --- | --- | --- | --- |
| **local (the default)** | A directory on the host (a btrfs subvolume) | The host kernel (the gofer proxying an ordinary directory) | Verified |
| **remote** (S3 / NFS / …) | A host-side **FUSE** mount point, maintained by the backend and translated into operations against the backend | The backend itself, inside FUSE | Partly verified (the read/enumerate path is measured; the write path and a real backend are not, see D8) |

The two arms have **zero difference on the guest side** (both are one gofer-proxied OCI mount); the
difference is only whether an ordinary directory or a FUSE mount point sits behind the mount point.

**The unmeasured half of the remote arm is not an unverified premise of the seam's shape, so it does not
enter open-decisions.** The seam's shape depends only on **penetrability** — that the gofer descends into
a host-side FUSE mount point and POSIX semantics pass through — and the read/enumerate measurements are
enough to establish that; the write path's behaviour is **that backend's own acceptance item** (D8) rather
than a premise of the core model. Volume's new properties relative to Overlay, with their contracts and
falsifications:

| New property | Contract (detail in §5.C) | Alternatives ruled out |
| --- | --- | --- |
| **Several backends** (local / a remote object store / a network FS) | The backend seam yields a host mount point (D1/D2) | FUSE-for-everything (D1) · moving a userspace segment engine into the core (D6) |
| **Several write paths** (mounts + the direct API) | A single write regime (D5) | A reverse proxy for direct writes (D5) |
| **Concurrently mountable by several sandboxes** | Cross-sandbox rw coherence is undefined (D4) | — (it is inherently so) |
| **Independent snapshots / clones** | crash-consistent by default, a per-backend capability; producing **an ordinary writable volume + a `parent` lineage**, with the cap necessarily declared explicitly (D6/D6.1a/D7.3) | A two-phase checkpoint (D6) · a userspace segment engine (D6) · a read-only snapshot type (D6.1a) · inheriting the source's cap (D6.1a) |
| **Exclusivity** | The lease = §4.3's mount row (D3/D4) | Dual reclamation by heartbeat (D4) |
| **A capacity cap** | A btrfs subvolume + squota, a primitive shared across §5.B/§5.C (D7) | Project quota (D7; the substrate does not support it) |

---

## D1. Materialization: one unified "host mount point" abstraction, local = a directory / remote = host-FUSE

A single-node local backend is just a directory on the host, and its path into the guest is **entirely
isomorphic** to §5.B's merged rootfs — written into runsc's OCI mount spec and proxied by the gofer under
`--overlay2=none --file-access=shared`. exclusive / shared-ro / shared-rw are all merely "the same host
directory entering one to N guests", with **no special mechanism**. The measured coverage (both halves: a
single mount and **concurrent** multiple mounts):

- A single mount: rw in both directions, directory enumeration, and nested subdirectories are all correct.
- **Two sandboxes rw-mounting the same source concurrently both succeed** — no EBUSY, no "source in use",
  neither gofer complains, and the second mount is **genuinely writable** rather than silently downgraded.
  **This is the most critical half of the foundation beneath the seam**: if a second gofer could not
  attach, SharedReadOnly's read fan-out and SharedReadWrite would be unimplementable and the AccessMode
  enumeration would collapse to Exclusive alone.
- The ro arm can coexist with two live rw arms, `statfs` inside the guest reports read-only, and writes
  fail with `EROFS` (a clean POSIX refusal rather than EIO or a fault), and it **does not silently
  succeed** (the host never sees that write).
  - **But this holds only for "a writable unit + a ro mount"; the reverse is not a runtime error but a
    startup failure.** Pair a **read-only** btrfs snapshot with a rw mount spec and `runsc create` fails
    outright at the gofer mount stage (`creating gofer mounts: … EROFS`); the container is never created
    and the guest never runs. Classified, that is a **control-plane failure** rather than a data-plane
    `EROFS` — better than silently succeeding, but it is **a different** failure mode: a cold start turns a
    configuration error that "should have been refused with a 4xx at admission" into an opaque mount
    failure. So ro's directionality must be written into §5.C and cannot be covered in both directions by
    the single sentence "ro is enforced at runtime".
  - It also rules out a possible implementation-path dependency: a read-only snapshot can be thawed with
    `btrfs property set … ro false`, after which client writes succeed and **the cap survives the ro→rw
    transition** (independent of, and pointing the same way as, D7.3's Q3). So `Clone` **can** be
    implemented as snapshot-then-unfreeze — the substrate does not force us to produce a non-read-only
    snapshot from the start, which is why snapshot and clone can share one backend path (D2).
- **The mounts are independent of each other**: after killing and force-deleting one of the sandboxes, the
  other can still read **and write**, and the ro arm can still read — tearing down one sandbox does not
  disturb the other gofer's view of the same source, so destroy is not a cross-sandbox side effect.
- **Cross-sandbox coherence was observed but does not enter the contract**: every cross-read succeeded and
  the ro guest saw a later rw write within a half-second poll, and it is mechanically explicable (in this
  mode each gofer proxies the same host dentries and there is no page cache on the sentry side), so it
  **very likely** continues to behave this way. But that is a fact about one runsc build rather than a
  promise (D4); pinning today's gofer cache behaviour into a test would manufacture exactly one **false
  invariant**, and the honest position is undefined.

### D1.1 FUSE's correct place: host-side, never in the guest; a remote backend's materialization, not a core mechanism

FUSE does not enter the core and is not on the single-node local path; it is the **materialization means
of a remote backend**. A remote backend (an object store / a network FS) inherently needs a layer that
"translates the remote into a mountable POSIX FS", and that layer is FUSE — but it comes with one
immovable positional constraint:

> **FUSE is mounted host-side (inside the backend process) and proxied into the guest by the gofer; never
> run FUSE inside the guest.** gVisor's support for in-guest FUSE is limited/evolving, whereas the path
> "a host FUSE mount point → gofer proxying" is measured: the gofer descends into the FUSE mount point and
> proxies FUSE-backed files and nested directories into the guest as ordinary POSIX. (Mature FUSE-portal
> implementations likewise mount on the **host** side and speak the protocol to `/dev/fuse` directly,
> rather than inside the sandbox.)

So FUSE is not a question of "should it enter the engine" but of "**which backend implementation is it
sealed inside**": it is a remote backend's internal materialization means, identical above the seam to the
local backend's ordinary directory (§3.1.1's "ID addressing applies internally too": the core knows only
"a Volume is materialized by a backend into one host mount point" and not whether a directory or a FUSE
mount point is behind it).

### D1.2 Falsification: why not "FUSE-for-everything" (local going through FUSE too)

A tempting unification: route **every** Volume (local included) through the same host FUSE frontend plus a
pluggable `Backend{Read/Write/List/Stat…}`, whose benefit is that caching / encryption / quotas / auditing
could all be done uniformly in the FUSE layer. **Not adopted**, on the cost criterion:

- **The local path's POSIX is free**: the gofer proxies an ordinary host directory, and rename / directories
  / permissions / locks / mmap are all delivered by the **host kernel**. Going through FUSE would mean
  **re-delivering POSIX semantics ourselves in userspace** — a complete FUSE session interface is **dozens
  of methods** (Lookup/Rename/Flock/CopyFileRange/xattr/…), and an object-store backend can only return
  `EOPNOTSUPP` for Rename/locks/Fallocate/xattr: that is the engineering volume and the semantic loss of
  "implementing POSIX yourself".
- **Uniform caching / encryption / quotas / auditing are distributed concerns**: encryption is necessary
  only when data leaves the trust boundary (into S3 / a shared cache), and uniform caching compensates for
  "an object store has no POSIX cache". On a single trusted node, quotas have btrfs squota (D7) and auditing
  has §4.1's event stream, and neither needs to be crowded into a FUSE layer.
- **The conclusion**: a complete FUSE `Session` interface is **reserved for "synthesizing" backends alone**
  (those that synthesize a non-FS backend into an FS); a local backend (a real host FS) simply does not need
  it. So the engine's seam can be narrow (D2) — the unification happens at the seam (both yield a host mount
  point) rather than by "everything goes through FUSE".

---

## D2. The backend seam's shape: a narrow interface yielding a host mount point

Volume is its own subsystem with its own seam `volume.Backend`, and it is **a differently shaped,
separately registered interface from §5.B's `rootfs.Provider`**: §5.B materializes **layered composition**
(resolving the multi-lowerdir lower stack along the Template lineage + a writable upper/work, composed into
a merged view by kernel overlayfs, with Promote freezing supported); §5.C materializes **one self-sufficient
independent storage unit** (bound directly, with no composition, no lower stack, and no lineage). The two
share only the underlying substrate primitives (a btrfs subvolume + quotas + reflink/snapshot, see D7) and
do not share one materialization interface.

The seam follows the convention already established by §5.A's `runtime.Provider` and §5.B's
`rootfs.Provider`: **IDs cross the seam, host paths stay inside it; policy + handles, with no host
primitive leaking**.

> **One shape rule running through this section: "allocate", "set the cap", and "place" are three actions
> and cannot be compressed into one call.** Merging them leaves creation, capacity, and the admission term
> all with nowhere to go — `DestroyVolume` finds no counterpart for creation, "leave the quota alone" needs
> a sentinel value, and nobody reports an uncapped volume's bytes. This rule was **found with a second
> backend**: a test-only plain-directory arm runs the same shared contract cases as the btrfs arm (both
> arms pass), and a merged shape rings at each of those three points. So the seam always **keeps a
> test-only second arm** — not as filler, but as this seam's only means of falsification.

The core actions:

- `Materialize(ctx, volumeID) → Materialized` — prepare a mountable host mount point (local: ensure the
  storage unit exists; remote: bring up a FUSE mount point). It returns an **opaque handle** carrying only
  two things: a backend-private host path (`SourcePath`, consumed only by `local_runsc`, exactly like
  `Merged.MergedPath`), and `Volume` — the latter is not decoration, since otherwise `Release` could not
  resolve which volume this materialization belongs to.
  - **The handle carries no backend-private handle field**, which is the same judgment as `Capabilities`
    reserving no `DirectIO` bit below (§1.3 forbids reservations): no backend needs it today, and the local
    arm materializes into a path and holds nothing else. **The condition for adding it back is precise**:
    once the first remote/synthesizing backend exists and needs to start/stop a per-materialization process,
    then an unexported field + `SetHandle`/`Handle` is added additively per §1.3 — `runtime.Placement.handle`
    (which has a live consumer) is the precedent to copy **at that point**, not a precedent already followed
    here. **And it must then land on the handle rather than in a side table**: an in-process side table keyed
    by `volumeID` is precisely the "in-process handle registry" D3.1 rejects.
  - **It does not take `accessMode`**: that changes nothing the backend does (the host directory is identical
    under all three modes) and its cardinality is wrong (`AccessMode` is per mount row while `Materialize` is
    per volume), so it rides `MountSource.ReadOnly` — the normative statement of both belongs to §5.C.
  - **The contract is create-if-absent** rather than open-only (the reason is in §5.C: runsc fails hard when
    the source is missing, and open-only would make a wrongly swept sandbox unable to resume forever).
    **Concurrency must be written into the contract** — the entire shared fan-out design depends on "the same
    volume being materialized several times concurrently" holding (verified).
- `Release(m)` — release this materialization (local: a no-op; remote: umount the FUSE). **The name is not
  `Unmount`**: that word names a mechanism the default arm **does not have**, and that is exactly how a seam
  grows into the shape of one implementation. Four contract points: this materialization's source is no longer
  valid, **the bytes persist**, it is idempotent, and it tolerates the target having already disappeared — so
  the local arm's no-op is a correct implementation of an explicit promise rather than a bad smell (as with
  `runtime.Stop`); the bad smell would be "a no-op or a detach", one name with two undefined contracts.
- `AdoptMaterialized(volumeID) → Materialized` — re-recognize a still-mounted mount point after a crash
  (§4.4-A, the twin of `AdoptMerged`).
- `CreateVolume(ctx, volumeID, capBytes)` / `SetCapacity(ctx, volumeID, capBytes)` — **capacity belongs to
  creation plus a method of its own**, not attached to the placement entry point. `rootfs.Provider` needs a
  `CapUnchanged` sentinel because the cap's intent rides `OpenOverlay` (a call already doing another job);
  here **the call itself is the intent**, so no such sentinel is needed. And this is the only correct
  placement: **a volume's cap must be editable even when it is mounted nowhere**, which the placement entry
  point cannot express — and `DestroyVolume` had no counterpart for creation anyway.
- `DestroyVolume(volumeID)` — reclaim that Volume's backend bytes (terminal). **Idempotent** (§4.4-A reaches
  it by several paths).
- `UncappedBytes(ctx) → int64` — the bytes this backend holds that are covered by no declared cap (the §5.C
  half of §3.4's second term, in the same shape as §5.B's). **This is a real gap rather than decoration**:
  without it, every uncapped volume's bytes are invisible to the admission gate.
- `Usage(ctx, volumeID) → (used, limit int64)` — one volume's backend-reported usage. **It is forced by the
  volume cap's write point rather than chosen**: §5.C requires refusing "a lowering below current usage" with
  a **strict inequality** as the criterion, so the engine must be able to read current usage; `inspect`
  reporting usage goes through it too.
  - **`limit` may be used only for diagnostics and never for validation, and the authority is always the
    volume record** — the reason (the `none ↔ 0` mapping makes "the cap was never set" and "deliberately
    uncapped" read back byte-identically, so after-the-fact reconciliation is unimplementable) is in D7.3.
  - **`used` must be the backend's *current* statement, and must never *under*-report.** The two halves of
    this call therefore pull in opposite directions, and both are load-bearing: `limit` is diagnostic-only,
    while `used` is the number a write point **decides on**. Because that decision is a strict inequality, a
    stale or optimistic `used` does not degrade it gracefully — it makes the engine **accept exactly the cap
    the check exists to refuse**. A backend that cannot answer currently must **return an error**, which the
    write point already treats as fatal (refuse rather than cap blind), rather than an old number. Stated
    because it is invisible on the local arm — a quota row read is always current — and becomes the live
    hazard on a remote arm whose usage may be eventually consistent.
- `Snapshot(ctx, srcID, newID, capBytes)` / `Clone(...)` — they belong to this seam, but **the new volume's ID
  is allocated by the mediator** (the same rule as Promote). The decisive reason: an ID the backend minted
  itself **cannot be registered into the in-flight set before the bytes exist**, so a crash midway would leave
  bytes under a name no sweep could classify.
  - **Both take `capBytes` rather than "inheriting the source's cap", because there is nothing to inherit**,
    so in the contract **setting the cap is an unskippable second call** — the non-inheritance, the necessity
    of a second call, and the load-bearing window that follows are all in D7.3.
  - **Both can be the same backend code path** (measured: `property set ro false` succeeds on this substrate,
    a `-r` snapshot can have its cap set while still read-only, and the cap survives the ro→rw transition
    intact). The difference is only §5.C's semantic annotation, not the mechanism.
- `OpenRoot(ctx, volumeID) → Root` — the direct content accessor, **usable while unmounted** (§5.C's promise,
  shaped in D5.1). `Root` is a **root-confined** file access handle (`Stat` / `List` / `ReadAt` / `WriteAt` /
  `Mkdir` / `Remove` / `Rename` / `Close`) whose paths are **relative to that volume's root**, with escaping
  unreachable. The local arm = `openat2(RESOLVE_IN_ROOT)`.
  - **It is stricter than `Materialize`, not looser**: the handle has **no `SourcePath`** and yields no host
    path at all, making it the one materialization-class method on the seam that needs no exemption.
  - **It is not a replacement for `Materialize`**: that one must hand runsc a bindable host path, while this
    one only reads and writes bytes inside the engine process. Their consumers differ (`local_runsc` vs the
    orchestrator's import/export endpoints), so they are two methods.
- `Name()` — the backend's identity (for registration/diagnostics).
- `ListUnitIDs(ctx) → []string` — **enumerate only, do not decide**. The backend reports every storage-unit id
  under its own storage root; "which are orphans" is decided by the mediator, which then calls `DestroyVolume`
  one by one. **This split was forced by measurement rather than chosen for style** — **"push the decision to
  the backend too" is rejected** (`SweepOrphanVolumes(live, building)`, with the mediator computing two sets,
  feeding them in, and the backend deleting the difference): it **deletes the bytes of committed volumes**.
  The mediator reads `live` and `building` in two separate reads, and a volume whose **entire creation** falls
  between those two reads is in neither set, so the backend deletes a fully committed volume. **Measured: under
  458 concurrent sweeps, 3 of 6 snapshots were deleted; with enumeration and decision separated, 913 sweeps /
  6 snapshots, zero wrongful reclamation.**
  - **It closes the window by ordering rather than by locking**: every candidate's bytes precede all of the
    checks; and a candidate no longer in the in-flight set **necessarily has its row already inserted** (the
    marker is removed only after the insert), so a subsequent per-candidate store read is certain to see it.
    No new lock, and no store call inside a lock.
  - **The Overlay side deliberately keeps the merged shape**: there the row precedes the unit insert, so all
    that is exposed is snapshot skew, and its comment records that residual as acceptable; **a volume is
    bytes-first, so what is exposed is the entire creation** — the same shape at a different order of risk.
    Cross-references are left on both sides.
  - **A failure midway continues rather than aborting** (a best-effort reclamation path: one stuck unit should
    not leave all the others behind).
  - **Why by unit rather than by reference**: a layer is a read-only base **shared** by lineage and refcount
    (the criterion being "is any row still referencing it"), whereas a Volume and an Overlay unit are both
    **self-sufficient storage units** (the criterion being "does this unit itself still have a live owner").
  - **The in-flight set is more necessary here than on the Overlay side**: there it only guards against a
    **stale keep-set** (the row is inserted first), whereas **a volume's creation is bytes-first** (the ID must
    name that unit first), so a healthy in-flight creation presents as "bytes present, no row" **even with a
    fresh keep-set**; and `Snapshot`/`Clone` are two more producers in the same window.
- `ReclaimQuotaRows(ctx, ids) → (n int, err)` — reclaim the quota rows **an earlier deletion leaked** (the
  backend half of D7.1's path). **It takes ids and does not scan rows itself**: the failure mode of scanning is
  an uncapped live volume plus zero signal (D7.2). A row not yet reclaimable is **left for the next round**
  rather than being an error — a `<squota space holder>` may legitimately refuse destruction for the entire
  lifetime of a live clone.
- `DeletedRowIDs(ctx) → []uint64` — the input the previous method consumes: the quota-row ids this backend
  recorded **at the moment of deletion** and has not yet reclaimed. **That it lands on the backend rather than
  the control plane is a shape argument rather than a convenience**: a leaked row is **that substrate's residue,
  not engine state** — a backend with no quota rows returns empty and the whole path costs it nothing, so the
  caller needs no capability branch (§1.3).
- `Capabilities() → {Snapshot, Clone, Quota}` — **a per-backend capability query** (D6): local (btrfs) reports
  snapshot/clone/quota supported; a plain directory or some remote backends may not support them, and a call is
  then a 400 rather than silent. **The members must be `bool`**: a `map[string]string` feature table would carry
  mechanism names across the seam **as values**, where no structural guard scanning identifiers can see them.
  **Only capabilities with a consumer are listed** (§1.3 forbids reservations): `Quota` has one (§3.4's "if the
  cap cannot be delivered, refuse the placement"), whereas a bit like `DirectIO` does not — it would stay on the
  contract surface forever in the name of "we might need it later".
  **The scope is per backend**, not per volume.

**The structural guard**: `seam_test.go`'s twin keeps §5.A's host-primitive list verbatim and adds `reflink` /
`fuse` / `loopdev` / `mountopt`; the sole exemption is `SourcePath` on the handle (following `MergedPath`'s
precedent, with the reason written into the exemption table alongside it).

The mechanism for mounting into the guest: in `coldStart` the mediator assembles each `Materialized` handle into
`LaunchSpec.Mounts []MountSource{SourcePath, Dest, ReadOnly}` (a contract member peer to `RootfsSource`), and
`local_runsc` appends each `MountSource` in `buildOCISpec` as one `ociMount{Type:"bind", Source, Destination,
Options}` — exactly the shape that was measured. A Volume's **identity** above the seam is always an ID; the host
directory is resolved inside the backend and surfaces only as an opaque `SourcePath` (§6.2: outwardly only Volume
IDs are recognized and no host path is exposed).

---

## D3. The mount table = a first-class store object, reusing the existing object model (not rebuilt)

The mount relation is a Binding (§3.4): "which are mounted, where, and under what policy" is recorded in the
control-plane mount table, and resume remounts from the table. Every row of the mount table is a **first-class
persistent object** (§4.3's mount row), and it is simply **`sandboxes.template`'s reference doubled**:

- **The structure = bidirectional FK RESTRICT**: one mount row `Mount{Sandbox, Volume, Path, Access}`, with the
  `sandbox` and `volume` columns each `REFERENCES … ON DELETE RESTRICT`. That alone gives both invariants "a
  volume with a live mount row cannot be deleted" and "a sandbox with a live mount row cannot be deleted" —
  SQLite through FKs, Memory through a Go scan, one set of assertions (§4.3's seam).
  - **Be clear about which half is "free"** (measured): the **detection predicate** is reusable as is — the
    existing FK-violation check is table-agnostic and column-agnostic string matching, true for **both
    directions** of every new FK path (a missing-parent insert / a RESTRICT delete, six directions across three
    FKs) and false for a uniqueness violation (`isFKViolation`, one predicate shared by eight call sites).
    **But the translation point cannot be inherited — it is per call site**: without that FK branch in
    `DeleteSandbox`, a RESTRICT surfaces as a bare driver error in the API's `default` branch and becomes a
    **500 instead of a 409**; and the existing `ErrInUse` message reads literally "template in use", so reusing
    it yields a 409 naming the wrong object. So the shape is **one sentinel per outcome**, landing as a separate
    `ErrSandboxHasMounts` (each backend has its own branch, and the contract case pins the reason — "an
    untranslated driver error becomes a 500" — into its failure message); while **which sentinel maps to which
    outcome category, and the counting rule that what is counted is the kinds of semantics a caller must tell
    apart rather than the number of error values, belong to §6.2** — this document claims only "do not reuse
    one". The Memory backend does not even get the predicate free: it has no FK engine, and the reference scans
    on the sandbox side and the volume side are hand-written — "free" covers only the SQLite half, and that is
    its boundary.
  - **The deletion order is necessary rather than a matter of style** (measured): RESTRICT takes effect
    **immediately**, and wrapping the two deletes in one transaction **does not buy** order-independence, so the
    mount row must be deleted before the sandbox. **`defer_foreign_keys` is a trap and was rejected on
    measurement**: it does make the order irrelevant, but the violation surfaces only at `Commit()`, and when
    Go's `Tx.Commit()` returns an error SQLite **has not rolled back** — on the pinned single connection the
    transaction stays open, and subsequent queries through the pool **run inside that transaction**, seeing
    uncommitted state no other handle can. That is a store-level correctness risk rather than a style question.
  - **FK enforcement is itself a load-bearing premise, and losing it is silent** (measured: with the
    `foreign_keys` pragma off, deleting a referenced row simply succeeds with zero errors). So it deserves a
    contract test guarding it rather than resting on the presence of a DSN string.
- **Existence is the state, refcount is derived, and there is no outbox event**: a mount row has no state column
  and no count column; "is this volume exclusively held" is derived from a mount row's existence plus its access
  (no materialized count). A mount row is not a §4.1 lifecycle transition, so creating/deleting one **emits no
  event** (as with Template CRUD, unlike a sandbox transition).
- **bytes-first, inside the transition lock**: mount rows are written inside the **per-sandbox transition lock**
  along the create/resume cold-start chain, and a failure is rolled back by `unwind`.
  **`unwind` must delete the mount rows explicitly — that is a design requirement rather than an implementation
  detail**: mount rows are written **before** the cold start, so every unwind branch after it would hit RESTRICT
  when deleting the sandbox record; and an unwind branch that discards that error leaves **an undeletable zombie
  sandbox record**.

**The hook points in the mediator** (verbs belong to §4, mechanisms to the §5 backend; subsystems do not call
each other):

| Hook | Action |
| --- | --- |
| `Create` | Write the mount rows after the record insert and before `coldStart` (included in `unwind`'s rollback); `CreateSpec` gains a volume-binding input |
| `coldStart` (shared by create+resume) | Before `Launch`: `Backend.Materialize` each volume → group the handles into `LaunchSpec.Mounts`; `liveState` gains a `mounts` field for pause/destroy to release |
| `resumeLocked` | **Read** the mount table back from the store (mount rows are durable across pause) and re-`Materialize` — the same reasoning as `resolveLower` re-resolving lineage each time: the volume is the same real thing, taking a new host materialization |
| `pauseLocked` | After the CAS: `Backend.Release` each volume (**the mount rows are kept** and the lease is not released), symmetric to the Overlay's "zero-capture" |
| `destroyLocked` | Unmount live volumes → **delete the mount rows first, then `DeleteSandbox`** (otherwise the `mounts.sandbox` FK blocks the sandbox delete); this is the lease release point |
| `RebindVolumes` (the `volumes` key of `PATCH /v1/sandboxes/{id}`) | Replacing the whole mount binding: one `store.ReplaceMounts` deleting the old rows and inserting the new ones **in the same commit**, with the new rows going through **the same lease predicate**; on refusal the old binding is left exactly as it was (the atomicity is a store contract stated in §5.C) |
| Startup/periodic reconciliation | **Three independent entry points**, called one after another at the same startup position and in the same reaper tick: `SweepOrphanVolumes` (`Backend.ListUnitIDs` enumerates → the mediator decides → `DestroyVolume` one by one; it does **not** mirror `SweepOrphanOverlays`'s merged shape, for the reason and measurement in D2) · `RepairVolumeCaps` (D7.3's repair state) · `ReclaimQuotaRows` (D7.1's leaked rows) |

**The only new invariant: the exclusive lease** (stated normatively in §5.C: at most one `exclusive` row, and it
does not coexist with any other mount row on that volume). This is the only new store assertion Volume
introduces. **Two statements that read naturally and are both false on measurement**, each worth a falsification:

1. **It is not "at most one rw mount row per volume"** — that would directly contradict SharedReadWrite's fan-out
   (§5.C): shared-rw is rw too.
2. **It is not "a control-plane precondition check plus an insert, serialized by the single-writer store"** — a
   single-writer store serializes **each individual call**, not **a decide-then-act pair spanning two calls**; and
   two concurrent `Create`s each mint a new SandboxID and therefore hold two **disjoint** transition locks, the
   only lock held at that moment being their own sandbox's. §3.4's admission gate is another instance of the same
   shape: the gate's invariant is node-global while the transition lock is per sandbox, so concurrent creates do
   not serialize against each other at all and the gate needs a separate mutex of its own.
   Measured: 8 racers with no scheduling hooks broke the lease 2 times in 4000 iterations, and with an explicit
   scheduling point it reproduces reliably.

**How it is delivered: let the store do it atomically, with no application-layer lock.** One statement expresses
the complete cross-row predicate (including all four combinations the partial index is blind to):

```sql
INSERT INTO mounts(sandbox, volume, path, access)
SELECT ?1, ?2, ?3, ?4
WHERE NOT EXISTS (
  SELECT 1 FROM mounts m
  WHERE m.volume = ?2
    AND (?4 = 'exclusive' OR m.access = 'exclusive')
)
```

`RowsAffected()==0` ⟹ a lease conflict (→ 409). Measured with 24 racers and no application lock at all: rows=1 /
exclusive=1 / zero driver errors; multi-volume all-or-nothing uses one transaction (a single connection makes it
naturally serial) and measured winners=1/16, partial=0. **It is recommended over adding a new per-volume lock
class** because it introduces no new mechanism: it is exactly what `store.Store` already requires every backend to
deliver, and it is **isomorphic** to §4.3's reference-reclamation (the same authoritative store serializing a
decide-and-act pair). On the Memory side it is one method under its existing mutex. The partial index and
`CHECK (access IN (...))` fall back to being schema-level assertions (each covers only half, see §5.C).

> **There are only two racing writers, and both are writes of mount rows**: the INSERT on `Create`'s cold-start
> chain, and `RebindVolumes`'s delete+insert through `store.ReplaceMounts` (in the same commit, through the same
> lease predicate, so it is not a second arbitration but a second call site of the same predicate). That narrows
> the fix considerably: resume only **reads** the table, destroy only deletes its own sandbox's rows with FK
> RESTRICT covering the order, and pause writes no rows.
>
> **"The predicate must be a row-immutable attribute"** (§5.C's prohibition) has a concrete external cautionary
> example: one implementation built its live-owner partial unique index on a soft-delete timestamp
> (`UNIQUE(...) WHERE cleanup_requested_at IS NULL`), and the path that writes it exists while **no path anywhere
> in the codebase clears it back to NULL** — so one mistaken marking both permanently condemns that volume and
> frees the uniqueness slot for a second holder to insert into. Our predicate is `access` (row-immutable), which
> is exactly why that failure mode is avoided; **it is a choice rather than a coincidence**, so it is written
> down, or the next person could easily change it to some `detached_at`.
>
> **The lock order (a total order)**: `transitionLock(sandbox) → the mediator's `mu` (home to both the live-map
> and the direct-write in-flight marker)`, and `transitionLock(sandbox) → the admission mutex`. **There is no
> per-volume lock class, deliberately**: volume-dimension mutual exclusion is carried by the store statement above
> (the whole point of D3), and the write regime's two halves share the live-map's `mu`, so Volume **adds no lock
> layer**. `mu` is strictly a leaf (taken and released, with no store call, no backend call, and no waiting on
> another lock in between); **the admission mutex is not** — by contract it spans {sum → materialize → commit},
> and while held it waits on the store's single-writer lock. The two still compose, because **no store or backend
> path takes either lock in the reverse direction** and they do not nest within each other.
> **This is also an independent argument for keeping preemption outside the door**: a volume-dimension verb that
> looks up its holder (force-detach / lease preemption, which §5.C prohibits rather than defers) would need a
> vol→sbx-direction lock, closing a cycle with `transitionLock → …`. **This one is derived from the lock order and
> has no reproduction vehicle in the repo** (unlike D3's first two, whose numbers are in
> `store.Store.CreateMounts`'s comments). Should it ever be implemented, it must resolve the holder from the
> store, **release first, and re-acquire in the global order**.

### D3.1 Falsification: a store-level exclusivity token vs an in-process open-handle refcount, not to be conflated

A tempting alternative keeps **two layers** of refcount: ① the DB mount row (who holds it exclusively, across
processes) and ② an in-process count (how many mounts on this node share **the same open backend session**). Layer
② is meaningful only when "one FUSE session serves N sandboxes" — it counts references to an **open synthesized-FS
handle**. This engine's local backend is "each gofer proxying a host directory of its own", with **no shared open
handle**, so layer ② degenerates away and only layer ① (the mount row) remains. Layer ② is therefore **a
remote/synthesizing backend's internal implementation detail** and does not enter the core mount-table model —
recorded here so that a distributed two-layer refcount is not mistaken for a core requirement.

---

## D4. Access modes and the exclusive lease

The definitions and semantics of the three access modes, and "the exclusive lease = the existence of that
Exclusive mount row itself" (introducing no `Lease` type and no belonging field; including "keeping the lease
across a crash is the correct behaviour" and the reaper TTL backstop) are stated normatively in §5.C. Only two
points are left here: the naming takes the **observable properties** of sharing and writability (§1.1's vocabulary
boundary bars entity names in the belonging sense), and SharedReadWrite's "cross-sandbox rw coherence is
undefined" shares **one origin** with D1.2's refusal of FUSE-unified caching — sharing mutable state across a
boundary is a hard problem, and the response is **not to promise** rather than to build a fragile mechanism that
pretends to. The rest of this section gives the alternative it ruled out.

**Falsification: heartbeat expiry + Pod destruction as dual reclamation.** It is a product of **being distributed
with no single authoritative store, where a node can vanish into thin air** (nobody knows the node died, so it can
only be inferred from a heartbeat timeout): registration-plus-heartbeat and "find live mounts by heartbeat
timeout" is its standard shape.
This engine is single-node with one orchestrator, and the store is the authority (§1.1): there is no "node that
vanishes", and a crash is a local fact the L1 probe can decide immediately. So **keep the reaper TTL backstop and
drop the periodic heartbeat loop** (there is no other node to hand off to). Heartbeat-based reclamation is a
future multi-node provider's obligation (node death being caught by its scheduling layer) and does not enter the
core.

**The one apparent residual closes without a mechanism**: a long-paused or stuck sandbox holding an exclusive
volume that someone else wants now. Every release path already terminates — destroy, a **`running`-legal detach
that releases the lease immediately**, and `retention`-evict for a holder §4.3 has already crash-paused — so what
is left is not "unreclaimable" but **"not reclaimable from the volume dimension"**, which is the same fact the
lock order needs (D3). §5.C states both the paths and the prohibition on a volume-dimension verb; **this is not
policy awaiting a product layer.** The external precedent points the same way and sharpens why: force-detach
exists at the block layer and carries an explicit corruption warning (AWS EBS: a last-resort option, with file
system check and repair pushed onto the caller), while the safe path in a distributed system requires a **human
attestation that the holder is dead** — Kubernetes' `out-of-service` taint, whose documented precondition is that
the node is already shut down, because force-detach otherwise breaks the CSI ordering requirement. That
attestation is the thing a force-detach API cannot supply from inside, and it is exactly the problem this engine
does not have: single-node with an authoritative store makes "is the holder dead" a local fact the L1 probe
decides immediately, so crash-pause → `retention` → evict covers it with no attestation and no new verb.
Sandbox platforms converge on the same answer from the other end — the common shape is that no such operation
exists, and destroying the holder *is* the mechanism.

---

## D5. The write regime: one invariant, and no reverse proxy

The hazard is real: a volume may be written rw through the gofer by a running sandbox and written to the same
backend from the host side by the direct API at the same time → tearing, and the gofer's/sentry's page cache is
not coherent with a direct write. §5.C blocks it with one **invariant** — the normative statement of that
invariant (a live-rw mount held ⟹ direct mutations are refused outright; the gate is on a **live** writer and
`paused` does not count as one) belongs to §5.C, and this document gives only its boundary and falsification.

**The `ro` direction is not part of that invariant and is no longer a boundary item**: a reader is not a writer, so
nothing tears. The reader's exposure (the complete previous inode for a holder that already has it open, a momentary
`ENOENT` on a fresh lookup, or a prefix — **never** a torn file, since an import unlinks rather than overwriting in
place) and its unpromised freshness are stated normatively in §5.C. **That non-promise is D4's, not a new one**: it
shares **one origin** with D1.2's refusal of FUSE-unified caching, and D1 already refuses to pin today's gofer cache
behavior as an invariant — which is the same reason no version stamp is introduced here.

**Direct operations while unmounted** (create/delete/list/inspect/import/export): the local backend operates
directly on the host directory/subvolume, which is simple in a single-node setting and needs no "lazy attach". The
whole "open a temporary backend session to serve direct access when nothing is mounted, and reclaim it when idle"
pattern is **needed only by a synthesizing backend** (which needs an open FS session to serve from) and is
unnecessary for a local directory — sealed inside the remote backend.

### D5.1 The direct accessor's shape: `openat2(RESOLVE_IN_ROOT)`, not through the guest and not using chroot

Direct import/export is a **narrow accessor** landing on `volume.Backend` (`OpenRoot`, D2) rather than going
through §5.E's `fs`. Two arguments:

1. **Going through §5.E is structurally impossible** (checked against the code): `fs` exists only inside the guest
   and is reachable only through a **live placement**, whereas an unmounted volume has no guest; and routing
   volume content through §5.E would create a `volume → transport` subsystem edge, failing `internal/arch`'s empty
   allowlist on the spot.
2. **A narrow accessor's security boundary needs exactly one syscall.** `openat2(dirfd, path, RESOLVE_IN_ROOT)`
   gives path semantics **identical item by item** to chroot's, with no privilege, no mount namespace, and no
   thread pinning. (The implementation also passes `RESOLVE_NO_MAGICLINKS` — which independently blocks escapes
   through `/proc/*/fd` magic links and **does not change** the four path semantics below, so it is hardening
   rather than a second boundary.) Four measured properties (on this machine's 6.x kernel):
   `..` is **clamped** at the root (not an error — it resolves to the root itself) · an absolute path is
   **reinterpreted against that root** · a symlink pointing outside the root (tried both relative and absolute)
   resolves back inside it · a symlink loop reports `ELOOP`. The write side is verified too: creating and writing
   through a path shaped like `../../../d/f` lands the bytes inside the volume, and a write through a symlink
   pointing at a host file **does not** touch the host file.
   - **The magnitude**: `openat2(RESOLVE_IN_ROOT)` is **0.52 µs/op**; what one reference implementation pays for
     the same guarantee — "a fresh mount namespace + a bind + `pivot_root` per request" — is **178 µs/op**
     (measured on the same machine, and the latter also requires root). **342×**, and it saves the whole
     namespace lifecycle plus `runtime.LockOSThread`'s thread-ownership protocol.
   - So "direct access" is not a capability needing a new data plane but **one dirfd plus a family of `*at`
     calls**. It need not even yield a `SourcePath`: the handle exposes no host path at all, is stricter than the
     existing `Materialized`, and passes `seam_test.go`'s structural guard automatically.
   - **The remote arm accommodates it**: a FUSE mount point is a directory and `openat2` behaves identically on
     it; a true object-store backend implements the same set of path and byte semantics itself (the interface is
     paths and bytes, not host primitives).

The accessor **is the only trigger of the write-regime invariant and its in-flight marker**, so all three land
together — implementing the latter two alone would be a contract member with no consumer, which §1.3 forbids.

**The write regime's gate must not degenerate into "refuse if there is any row in the mount table".** That would
be more conservative and would not tear, but it would destroy a promise §5.C states explicitly — a `paused`
sandbox does not count as a live writer, direct writes are safe then, and the new content is naturally seen after
a resume. And the live-map **genuinely is** a faithful gofer predicate: checked against the code, a placement is
`put` into the live-map **at the moment it physically exists** (before the health gate, one step before
`coldStart`'s ⑦ in `lifecycle.go`), precisely so that a concurrent reconcile does not take it for an orphan. So
reading the live-map is right, `sandboxes.state` is wrong (§5.C already records both directions of its
unfaithfulness), and "read the mount table only" is a **third** kind of wrong — it trades a settled capability for
an implementation's convenience.

**Falsification: reverse-proxying direct writes into the mounting sandbox** (while a volume is mounted, reverse-
proxying a direct write into the holding sandbox, to allow "writing from outside while mounted"). **Not adopted**:
① this is **product convenience** rather than an engine correctness need — the engine only needs "do not tear",
and §5.C's **refusal** suffices at a cost far below building a reverse-proxy data plane; ② a reverse proxy would
require the engine to understand "who is mounting it, and how to reach through their gofer to write their FS",
pulling **cross-object write orchestration** into the engine (§1.3's litmus: not register-impl / add-join-table /
add-subscriber but changing existing structure → which should prompt "should this be in the engine at all").
The answer: whoever wants that convenience stacks an adapter outside the engine (read the mount table → write into
the holding sandbox through §5.E's `fs`), and the engine does not. A product platform counting that convenience
among its own responsibilities is reasonable; the engine draws it outside the boundary, and that is **a difference
in positioning rather than in correctness**.

---

## D6. Snapshots / clones: btrfs-native CoW, a per-backend capability

### D6.1 Single-node uses btrfs and does not build a userspace segment engine

"POSIX over an object store" takes a whole machine: a WAL (a node-local change log) → periodically materialized
into immutable manifests + content-addressed segments → a head pointer CAS, where a snapshot = referencing the same
batch of immutable segments and a fork = rewriting segment references for CoW.
**That entire machine is a remote backend's internal obligation and does not enter the core**: the only reason it
exists is "an object store is not POSIX, has no durability-on-write, no reflink, and is untrusted". On single-node
btrfs, **the filesystem itself already gives CoW** — `cp --reflink=always` has been verified to share extents (the
same physical offset with the `shared` flag, and an explicit reflink still works even under a `nodatacow` mount) —
and `btrfs subvolume snapshot` is moreover **whole-tree, atomic, and instantaneous** (measured: a new snapshot has
only 16KiB of exclusive metadata and copies zero data).

So: **adopt the semantic model** (a snapshot = an immutable point in time, a fork = a CoW subvolume, both cheap at
the metadata level) and the **capability matrix** (snapshot/fork are a per-backend capability), and **discard the
segment/manifest implementation** — letting btrfs be the "materializer". Modelling a Volume as a **btrfs
subvolume** makes snapshot/clone = `btrfs subvolume snapshot`, landing on **the same object** as D7's capacity cap
(the quota being applied to the subvolume): one storage unit gets both cheap snapshots and a hard capacity cap,
which is the strong argument for "a volume = a subvolume". **But do not read that as "one step"**: that ioctl moves
**bytes** and does not move the **cap** (D7.3), so the merger is two operations rather than one.

### D6.1a A snapshot's / clone's object shape: the same kind of object, with lineage, and the cap necessarily declared

Three sub-decisions which together define "what a snapshot is":

- **A snapshot volume is an ordinary, writable Volume with no read-only marker.** The reason is word-for-word
  isomorphic to §5.C's rejection of a "per-volume `access` field": **the cardinality is wrong** (writability is a
  per-mount adjudication, and one volume can be shared-rw for A and shared-ro for C at once). The harder reason is
  the property §5.C has already measured and written down — "a snapshot does not break per-volume cap semantics:
  **the writer pays**, in both directions", where overwriting the snapshot charges only the snapshot's account and
  leaves the source alone. That measurement **is meaningful only when a snapshot is writable**; defining a snapshot
  as read-only would throw away a verified property and additionally require a cross-object validation on the mount
  row's `access` (which fails §1.3's litmus: neither registering an implementation nor adding a join table, but
  adding a field plus a cross-object check). For immutability, express it with a `shared-ro` mount row, or leave it
  to product-layer policy.
  - It also **contracts the mechanism**: were a snapshot an independent read-only type, there would be two seam
    methods, two API shapes, and two teardown rules for something btrfs implements with **one** identical ioctl.
    One shape lets `Snapshot` and `Clone` share a path (D2).
- **The volume record carries `parent`, `REFERENCES volumes(id) ON DELETE RESTRICT`**, exactly like
  `templates.parent` (§4.3's lineage). This is a **correctness argument rather than tidiness**: D7.1 measured that
  "delete the sharers first, then the source, and not one row leaks", and the FK is what makes that order the
  **only reachable** one — deleting a source with a live snapshot becomes a 409, and the quota-row leak **does not
  arise at all** (prevention, free, introducing no new mechanism). By this engine's own criteria it passes
  cleanly: it is §1.3's "add a join table" (self-referential here), it reuses the existing FK-violation predicate
  (measured table- and column-agnostic in D3), and it has no state column and no materialized count ("is this
  volume a source" is derived from whether any row points at it).
  - **An external cautionary example pins this down** (source checked): one implementation chose **a pointer with
    no constraint** (`ADD COLUMN ... TEXT` plus an index, no `REFERENCES`), and consequently had to hand-write
    children checks in three separate places — and **all three are reclamation guards, with not one a deletion
    guard**. The consequence is that the control-plane row disappears first while the bytes must remain, and since
    every reclamation criterion is keyed on the source volume's own record, once the source row is deleted **no
    later pass can ever find those bytes again** (its "skip reclamation if there are children" function is called
    exactly once on the synchronous deletion path, with no periodic sweep revisiting it). Our
    `SweepOrphanVolumes` could cover such a situation, but **it should not have to**: RESTRICT keeps it from
    arising.
  - **It needs a sentinel of its own**: a volume deletion blocked by a live snapshot and one blocked by a mount row
    are **two different object relations**, and the existing `ErrInUse` message reads "template in use". This is the
    same trap D3 already identified, recurring on a fourth FK path.
- **`size` is required when disk is metered and is not inherited**, so both `POST /v1/volumes/{id}/snapshot` and
  `.../clone` grow a `{size}` request body. **The substrate side has no choice left** (D7.3: there is nothing to
  inherit); and on the policy side, inheritance looks free but is not — a cap is **a declared admission claim**
  rather than a property of the bytes, and §3.4 stipulates "count by the declared cap" precisely because a
  snapshot's actual bytes are about 16 KiB of metadata. Inheritance would silently double the node's declared disk
  claim at every snapshot, something the caller never asked for; and §5.C already states that an squota cap is a
  "**lifetime write budget**", so a unit that has written nothing yet inheriting a number is meaningless. Requiring
  it also keeps the rule count unchanged: §6.2 already has "conditionally required per axis", and this is the same
  rule appearing at a second and third endpoint. When the source is uncapped and the axis is unmetered, `size`
  remains omissible, covered naturally by the existing rule.

### D6.2 A snapshot is crash-consistent by default, asymmetric with the Overlay

The full statement that a Volume snapshot is **crash-consistent** by default and asymmetric with the Overlay is in
§5.C (including the composition path to app-consistency: for an exclusive volume the caller pauses first and then
snapshots, and shared-rw offers none). Only two things this document should keep are added here: the **root** of the
asymmetry is that the Overlay has a **single writer** and can be paused for an at-rest read (§5.B), while a Volume
breaks that premise (shared-rw has several writers, and one does not want to stop while mounted); and **why there is
neither an `fsync` nor a freeze before a snapshot** (a tempting "to be safe" prelude, whose sync half is useless and
whose freeze half is a net negative):

- **sync makes no measurable difference**: a snapshot after a sync and a bare snapshot fall in the same consistency
  class (every capture is a complete prefix), and a bare snapshot **does not stall the guest** anyway (zero write
  errors, unchanged write rate).
- **`fsfreeze` and a snapshot simply cannot be composed.** Creating a snapshot is itself a **write** to the very
  filesystem just quiesced, so the snapshot blocks **uninterruptibly** for the entire frozen window and lands only
  after the thaw — placing it **after** the very quiesce it meant to exploit, capturing post-thaw state, for **zero
  consistency gain** over a bare snapshot; the only actual effect is stalling the guest, plus freezing the whole
  node's root filesystem (on a single node, volumes and engine state are often on the same btrfs).
- **A harness constraint that comes with it**: do not wrap a snapshot inside a frozen window with `timeout` — a
  blocking snapshot interrupted by a signal **corrupts the freeze count**, after which the thaw returns `EINVAL` as
  if already thawed while the snapshot lands anyway (reproduced 4/4). Stepping on it makes a measuring tool's
  manufactured failure ("the snapshot failed + the thaw failed") read as a substrate failure.

**Why this stays prose and never becomes a response field** (the falsification, since the alternative is the
obvious thing to propose): the annotation would be **constant** — the engine has exactly one recipe, so every
snapshot on every capable backend is crash-consistent — and in the one case where it would carry information it
would be **wrong**. A caller who pauses the holder and *then* snapshots has an app-consistent snapshot, and the
engine cannot know that: the two are separate requests and no linkage is recorded, so a field would report
"crash-consistent" on exactly the snapshots taken most carefully. Making it right would mean inferring
app-consistency from request adjacency, which is product semantics §1.3 rejects. The wording therefore lands at
§6.1's snapshot/clone endpoints, where the neighbouring semantic notes already live.

### D6.3 Falsification: a two-phase checkpoint

Taking a consistent snapshot of a mounted volume with a two-phase "block new operations + drain in-flight
transactions + force a flush" — **not adopted**, since it fails at both ends: ① it is redundant for an exclusive
volume (a two-phase protocol is in essence §4.2's pause quiescent point, already covered by "pause the holding
sandbox then snapshot", and building a second one would make §5.C depend backwards on §4.2, violating "subsystems
do not call each other"); ② it is insufficient for shared-rw (N independent sandboxes' gofers have no common
coordinator, and forcing one into existence would be stuffing swarm orchestration into the engine, which §1.3
rejects).

---

## D7. The capacity quota: a btrfs subvolume + squota, a primitive shared across §5.B/§5.C

The mechanism (a btrfs subvolume + an squota limit), the choice of mode (squota rather than full qgroups, and that
this substrate cannot change modes), the full-stack measurements, and the ownership (the leaf package
`internal/btrfs`, where **what is shared is code rather than a seam**) **are all settled in rootfs-overlay D6 and
are not duplicated here** — §5.C's per-volume cap and §5.B's per-sandbox upper cap are two applications of the same
materialization problem (§3.4's capacity axis), and Volume **reuses the same package without rebuilding it or
changing its shape**.

This section keeps only the three things Volume **itself introduces** (absent on the Overlay side, because the
engine never snapshots an upper): **quota rows leak and that is a correctness bug** (D7.1) · **the blast radius of
one wrongful sweep** — which explains why every §5.C sweep requirement is a hard one (D7.2) · **the limit side:
there is no inheritance, a missed cap is undetectable, and a lowering is irreversible**, which fixes two contract
shapes (D7.3).

Two direct consequences are recorded here first:

- **The reflinked bytes of a volume produced by snapshot/clone are not charged to the new volume under squota** —
  so the property that snapshots are cheap holds in quota semantics too; but "how much real disk a volume occupies"
  must be seen from node-level accounting rather than from a single volume's quota (the same problem as Template
  layer bytes in rootfs-overlay D6.2).
- **This engine is not the sole owner of that disk's quota namespace** (measured: there are 11 external qgroup rows
  on that fs, and OrbStack uses them too; nested subvolumes were measured to coexist without trouble) — which is
  the premise for §5.C's "quota rows are located by the id recorded at deletion time and **never by scanning
  rows**".
- **The cap's mechanism belongs to the backend, but the capability bit is not negotiable — and there is no third,
  "advisory" state.** `Capabilities().Quota` asserts exactly one observable thing: **a write past the cap fails**.
  A remote arm can deliver that either through a backend quota API or through accounting inside its **own** FUSE
  frontend — which is sound because that frontend is the sole write path for *both* ways bytes enter a volume (the
  guest's writes arrive through the gofer onto that mount point, and the engine's own direct access is `openat2` on
  that same mount point, D5.1) — and either is `Quota: true`. A backend that merely **records** the number without
  ever refusing a write is `Quota: false`, which is already completely modelled (a typed refusal of any non-zero
  cap, no side effect). Inventing a middle capability would legalize "a cap that does not stop a write", which is
  precisely what §3.4 refuses to let any §2.3 assertion believe in.
  - **One consequence follows from two existing gates composing, and is worth stating outright because a backend
    author would otherwise meet it as a 400 on every create: on a node that meters disk, a `Quota: false` backend
    can create no volumes at all.** A non-zero `size` is refused for want of the capability, and a zero `size` is
    refused because a metered axis requires a cap (§3.4). Such a backend is deployable **only with the disk axis
    unmetered** — which is the honest reading of the object-store reality, where a per-prefix byte ceiling does not
    exist and external CSI drivers carry the size field as decoration while the only limits with teeth are local
    cache bounds. A network-FS arm is the opposite case and maps onto `Quota: true`, its enforcement being the
    exporting filesystem's own quota.

### D7.1 The new problem Volume brings: squota's quota rows leak, and it is a correctness bug

§5.B's Overlay upper **does not leak today**, because the engine never snapshots it; §5.C wants to snapshot
volumes, so this is a new problem Volume introduces. **The predicate for a leak is not "it was snapshotted"** but:
**a row leaks ⟺ that unit (i) owns extents and (ii) is deleted while live sharers still reference those extents**.
Two corollaries from the measured matrix:

- **Whether it has a cap is irrelevant to leaking** (an uncapped source leaks too); **a source that was snapshotted
  but is empty does not leak** (it owns no extents); and an **intermediate** node in a chain that wrote data and is
  deleted first also leaks (it is not the chain's root).
- **Delete the sharers first, then the source, and not one row leaks.** So **the teardown order is a real lever**
  rather than merely a mitigation — it turns reclamation from "cleanup" into "prevention", for free.

**Why it is a correctness bug rather than untidiness**: a leaked row has no performance consequence in itself
(enumeration is still milliseconds at 60 rows, with no row-count ceiling), but **the next-free subvolume id is
recomputed from the root tree at mount time and the quota tree has no say — a leaked row does not reserve its own
id**. So one remount (a node restart) can hand some leaked row's id, **together with its cap**, to a brand-new
storage unit: measured, a new unit that never had any limit set hit `ENOSPC` at 3.3 MB (another shape: a 20 MiB
write died at 11.8 MiB). The direction can also be the opposite (a leaked row carrying a larger cap ⇒ silent
over-provisioning), and **the engine cannot detect it internally** — the limit it reads back looks exactly like
this unit's own. Reachability depends on the shape and is not universal, but since nothing reserves that id, **it
cannot be argued away**.

> **`parent` + FK RESTRICT (D6.1a) demotes this path from "routine" to "backstop".** The necessary condition for a
> leak is "deleting the source while live sharers remain", and RESTRICT makes that deletion **unreachable through
> the API** (→ 409). The only remaining reachable paths are two, neither passing through user intent: a **crash**
> stuck between deleting the sharers and deleting the source, and the ordering when a sweep itself reclaims by
> unit. So the procedure below must still exist and must still be correct, but what it guards is the residue rather
> than the main line — which is also why its failure disposition is "skip and leave it for the next round" rather
> than "block".

**The reclamation procedure** (the path §5.C requires, `ReclaimQuotaRows` on the mediator side). It and
`SweepOrphanVolumes` (reclaiming orphan bytes) and `RepairVolumeCaps` (repairing D7.3's state) are **three mutually
independent entry points**, merely called one after another at the same startup position and in the same reaper
tick — **do not read them as three phases of one sweep**, since the three residues' predicates, failure
dispositions, and reachable paths all differ. Each round:

1. **Delete the sharers (snapshots/clones) first, then the source** — per the matrix above, that order leaks not
   one row.
2. **Once per round** run a `filesystem sync` and then a `subvolume sync` (not once per unit). The order is a
   practical finding: `subvolume sync` used alone **sleeps until the next periodic commit**, so its duration floats
   with the commit period and load and **is not a fixed property** (measured repeatedly at anywhere from seconds to
   a dozen-plus: 7–16 s, 19.1 s); doing a `filesystem sync` first forces that commit, and the two together return
   on the order of **tens of milliseconds** (measured 14–29 ms, 53 ms). **What is load-bearing is that magnitude
   difference (about three orders), not any particular interval**: the effect is the same, but "wait for a commit"
   becomes "bounded".
3. For each id deleted this round, read its quota row and **check the exit code first**: a failed read → **unknown**,
   and no conclusion may be drawn from it; no row → done; **`<stale>` → destroy** (the polling predicate is exactly
   "the label is precisely `<stale>`"); `<under deletion>` → settle again and retry; `<squota space holder>` (live
   sharers exist) → **leave it** for a later round.
4. **A retry bound of 3 rounds within a round**, past which the row is left with a warning (carrying the id and the
   label) and retried next round. **Never spin on EBUSY** — a `<squota space holder>` may legitimately be
   undestroyable for **the entire lifetime** of a live clone, so that bound is not an error path.

> **The transition is condition-triggered rather than time-triggered**: the moment the sharers die and the deletion
> settles, the **first** attempt succeeds; so the predicate must be polled rather than slept on and prayed for. A
> live unit's label is its path and every reclaimable state is marked `<...>`, so that predicate **cannot possibly
> misfire on a running unit** (separately confirmed: destroying a live unit's row returns EBUSY, and its cap
> enforcement and usage readability are unaffected).
>
> **A read trap that will bite**: a batch read of quota rows under concurrent churn **fails wholesale and returns
> not one row** (it resolves a path for every row, and one of them vanished underfoot; the single-path form does it
> too, measured at about 1/150 — and that is precisely the condition while a sweep is running).
> **A reader treating empty output as "the row is gone" silently skips a destroy, which is a permanent leak.**
>
> **The frequency rises with churn concurrency, so 1/150 is a lower bound rather than a typical value**: under
> three-way concurrent churn, measured 157/2142 ≈ **7.3%**. The error also **names somebody else's row**
> (`cannot find the qgroup 0/21680`), so the id in the error string cannot be used to judge which unit had the
> problem.
>
> **All of the frequencies above are PER-ATTEMPT and now describe only the first attempt: the single-path read
> is retried, and the retry changed the picture rather than tidying it.** The failure is transient, so under the
> same three-way churn three attempts turned **8 first-attempt failures into 0 final failures (300 reads)**. The
> error contract is unchanged — an error still means "unreadable, draw no conclusion" — it is simply much rarer,
> which matters directly to the callers that must fail closed on an unreadable row (the §6.1 cap write points and
> §5.B's materialization guard, the latter on the create/resume hot path). **Do not cite 7.3% as the rate a
> caller sees**; cite it as what one attempt sees under deliberate churn, and note that the **batch** read's
> wholesale failure is not retried away by that change.
> **This is also why the aggregate question is now narrower than "is a failed read rare or common"** (see
> [`../open-decisions.md`](../open-decisions.md), "whether §3.4's second term needs a per-unit unknown channel
> when a quota read fails"): the direction is settled by the do-not-synthesize rule, and what the residual
> frequency decides is only whether the aggregate needs a per-unit unknown channel.
>
> **A future trap of the same family (worth a comment on the §5.B side)**: the real trigger is **extent sharing**,
> not "a snapshot was taken". Measured, a reflink **inward** (copy-up's shape) does not leak while a reflink
> **outward** does. So an obvious future optimization — having Promote use `cp --reflink` instead of a byte-by-byte
> copy — would **leak one row per Promote**, and that leak would outlive the layer itself.

### D7.2 The blast radius of one wrongful sweep: why every §5.C sweep requirement is a hard one

Measured by taking a volume out from under a live guest that was **continuously reading and writing**, five
different ways: **all five are merely data-plane events** — the sandbox never died once (the container stayed
running, a newly started exec returned rc=0, and the runtime reported no error), so **the control plane sees
nothing**, structurally the same as the already-measured ENOSPC case. But the corollary is **worse** than the
hypothetical bad case of "the guest sees I/O errors":

| Method | What the guest sees | Data outcome |
| --- | --- | --- |
| Delete the storage unit | Already-resolved paths **keep working forever**; only a new lookup reports `ENOENT` (not EIO, not ESTALE) | Writes go into an unlinked inode and are **silently lost** |
| Delete the unit's contents | **Zero errors**; the guest immediately rebuilds its own files | **Silently lost, with no signal whatsoever** |
| unlink an open file | **Zero errors**; reads and writes through the old fd continue | The gofer delivered POSIX; fsync does not resurrect the host path |
| Clear the quota row's limit | **The cap is silently removed**: it first hits ENOSPC at the cap, then writes resume and reach 2.5× the cap | §3.4's accounting is broken, **with no signal at all** |
| `mv` the unit away | **Zero errors**; it never sees the decoy left at the old path | The gofer **follows the inode**, and subsequent writes land in the moved-away directory |

**Three of the five arms produce not one errno**, so "the guest will report an error" is not a detection path. Three
hard facts come with it:

- **`limit none` is the only one of these that breaks an engine promise beyond data integrity**, and it **has no
  EBUSY self-protection** (destroy does, on a live unit: it returns EBUSY). That is exactly why §5.C requires
  "quota rows are located by the id recorded at deletion time and never by scanning rows" — the failure mode of
  scanning is an uncapped live volume plus zero signal.
- **Wrongly sweeping a live unit does not even reclaim the space it wanted**: the deletion is only **queued** (the
  gofer holds fds, so btrfs cannot reclaim the extents) while **the quota row disappears immediately** — that is,
  the accounting goes first and the bytes remain, so "the sweep reclaimed N bytes" is unobservable at sweep time.
- **Two processes in the same sandbox will give different answers about whether the volume still works**: a new
  process has no cached dentry and gets `ENOENT` where an old process can still write.
- **Recovery is "recoverable but all data lost"**, and that conclusion **depends on `Materialize` being
  create-if-absent**: when the source does not exist runsc fails hard rather than silently creating a directory,
  and recreating the unit empty gives a clean restart.

### D7.3 The limit side of the cap: no inheritance, a missed cap is undetectable, and a lowering is irreversible

Everything above in D7 concerns the **usage** side (which bytes are charged to whom). **The limit side is the other
half, and its conclusions fix two contract shapes** (D2's `Snapshot`/`Clone` signatures and §5.C's volume-cap write
point). Three measurements:

- **There is no inheritance whatsoever.** A snapshot gets a **brand-new quota row with `max_referenced = none`**, in
  all four shapes (a rw snapshot / a `-r` read-only snapshot / a snapshot of an uncapped source / snapshotting a
  snapshot after giving it a cap). And that is **verified by writing** rather than read off: pouring 96 MiB (3× the
  source's cap) into an uncapped snapshot all landed, while the negative control with an explicitly set cap failed
  precisely at the cap in the same shape — so `none` really is no cap rather than a misread. Isolation in both
  directions is confirmed too: while pouring 96 MiB into the snapshot the source's `referenced` did not move at all,
  and with the source pinned at its cap, writes to the snapshot still succeeded.
  - **`btrfs subvolume snapshot -i <qgroupid>` looks like the answer and is not.** Under squota it is accepted, but
    what it sets is **membership** rather than a cap: the new unit's own row is still `none`, while a limit on the
    level-1 parent **is** enforced. That is, it is **a ceiling shared by all members**, cannot express "this volume
    gets N", and one member exhausting it makes its **sibling volumes** hit `ENOSPC` too. It would look usable in a
    single-volume test and blow up the neighbours in production — hence explicitly excluded.
- **A missed cap is silent and undetectable after the fact.** An engine that "snapshotted, wrote the record with
  `size=N`, and forgot to set the cap": the writer can pour in far more than N bytes, with zero errors, no lifecycle
  event, and the control plane none the wiser. Worse, the `limit=0` that `Usage()` reads back is **byte-identical**
  to a **deliberately uncapped** volume (`internal/btrfs` maps `none` ↔ `0` in both directions), so no
  reconciliation, sweep, or inspect can discover the discrepancy afterwards. **That forces the invariant to write
  time**: setting the cap must be part of the same backend operation that creates the bytes.
  - It is D7.1's **mirror image, and strictly worse**: D7.1 needs a remount to land on a particular id to trigger
    (shape-dependent), whereas a missed cap **triggers at every snapshot, with no precondition**.
  - It produces a **load-bearing crash window**: the cap is necessarily a second call after the snapshot ioctl, so
    "the snapshot exists, the cap is not yet set" is a real intermediate state. The in-flight set (`building`) must
    cover it, and `RepairVolumeCaps` (and the adopt path) must treat "the volume record exists, the quota row is
    `none`" as a **repair state** rather than a legal one — otherwise what a crash leaves behind is a registered,
    live, uncapped volume. It is **its own entry point** rather than a branch inside `SweepOrphanVolumes` (D7.1's
    three entry points).
- **Lowering the cap below current usage wedges the unit, irreversibly.** The substrate accepts the lowering with
  **zero validation**; the existing bytes are unharmed (they read back byte-identical); but afterwards `mkdir` /
  creating a file / **truncating an existing file to 0** all return `ENOSPC` (only unlink still works), and **the
  quota ioctl itself returns ENOSPC** — so the cap can neither be removed nor raised, **not even raised above
  current usage**. The only way out is deleting bytes until usage falls back below the cap (condition-triggered
  rather than time-triggered: verified by lowering usage step by step, and it keeps refusing as long as it is at
  the cap). The boundary case: `limit == usage` is accepted and stored, but the very next write returns `ENOSPC` —
  so it means **full**, not "just enough".
  - **So a volume cap's write point must refuse such a lowering (400), with the strict inequality
    `size > current usage` as the criterion.** The reason is not that the substrate forbids it (it accepts it
    happily) but that **accepting it manufactures a state the control plane cannot get out of**: repair would have
    to go through deleting the guest's bytes, and nothing in §6 permits the engine to do that on a caller's behalf.
    §3.4's "a lowering need not re-run the admission gate" therefore carries a caveat.

### D7.4 Why guest-side statfs does not reflect the cap

The user-visible semantics (including the measured figures and the obligation on "a backend able to project it
honestly") are in §5.C; this section keeps only the root cause and the external corroboration.

**The root cause**: btrfs squota has **no statfs projection**, so what the guest sees is a **faithful pass-through**
of the host's whole-disk numbers — not a sentry fabrication (`f_type` is the gofer's rather than btrfs's, i.e. that
number never passed through the cap at all). This is the other direction of the same rule as §5.B's "guest-side
numbers may not be used for capacity accounting", but it is **an observable surprise for the guest**. **An
independent external implementation stepped on the same thing** (its mounted volume reporting about 4.6 EB
available, for a different cause — apparently a network-FS proxy — with a symptom of the same class), so this is not
an accident of this substrate.

So this section earns a statement stronger than "we do not promise statfs reflects the cap": **on squota it
definitely does not** — that is not an unmade promise but a known falsehood.

---

## D8. Open items outside the contract boundary (registered in [`open-decisions.md`](../open-decisions.md), not settled here)

The following are boundary items whose design is settled but whose **concrete value / landing shape is not**
(product-layer policy, or to be fixed when §6 lands):

- **The landing shape of a remote backend** (D1/D2/D6): when and in what shape an S3 / NFS backend lands; its
  internals (a host-FUSE frontend, a complete FUSE session's POSIX interface, and — if it fronts an object store —
  the whole WAL/manifest/segment/content-addressing/encryption/GC machine) are **all sealed inside that backend**
  and do not enter the core; this document only ensures the seam's shape has room for it. **D2's handle shape was
  reasoned from the one verified fact that a host FUSE mount point can be proxied through the gofer, and the FUSE
  arm has only its read/enumerate penetration measured (D0), with the write path and a real remote backend
  unmeasured.**
  - One lesson that holds for **any** future remote arm (from a distributed reference implementation's failure —
    the mechanism need not be copied, since we are single-node with the store as the authority and no split-brain):
    **keep the bytes unreachable until the ownership claim is committed, and never let two epochs share one key.**
    A CAS placed after the write cannot stop two writers computing byte-identical keys; and "a checksum stored in
    the payload" does not save it, because the same writer will make both sides self-consistent with each other.
  - **What "the seam has room for it" does and does not claim.** It is a claim about the seam's **shape**, member by
    member — every `Backend` member's contract is satisfiable by a FUSE-fronted remote arm as written, with no
    signature change — and **not** a claim about its **cost**. The distinction matters because the only falsifier
    this seam has is a second **local** arm (the test-only plain-directory backend, D2): it proves the
    three-actions shape rule and the capability-negative path, and it can prove **nothing** about latency, partial
    failure, handle-holding materialization or refcounted release. So nothing in the repository can catch a caller
    that assumes a backend call is cheap — and **two such callers exist today**. They are the remote arm's real
    entry cost, and they are §4 concurrency work rather than §5.C shape work:
    - **The node-global admission mutex is held across `CreateVolume`, `SetCapacity`, `Snapshot`/`Clone` and
      `UncappedBytes`**, justified in the mediator by "the middle step is bounded metadata work" — true of a btrfs
      ioctl (a snapshot is ~16 KiB of metadata, D6.1), false of a network round trip, and on a non-CoW object store
      a snapshot is a **full object copy**. The fix is already precedented and needs no new invention: it is
      Promote's reservation shape (§3.4's in-flight term), adopted for exactly this reason — holding a node-global
      lock across a byte copy would serialize every create on the node behind one operation.
    - **The boot sweeps and every reaper-tick pass call the backend on an unbounded context**, so a wedged remote
      store makes the orchestrator hang at boot **ahead of serving** rather than failing; and `RepairVolumeCaps`
      calls `Usage` once per volume per tick, against a reaper interval sized for a local full scan. Both are
      additive fixes (a deadline, a bound); naming them here is what keeps them from being discovered **by** a
      remote arm instead.

(The seam ownership of direct import/export is **not in this table**: it is settled, see D5.1.)

---

## D9. Three divergences from the reference implementations

1. **Addressed by ID, not by name.** One reference implementation's mount entries are **addressed by volume name**,
   and its SDK sends only the name even while holding the complete volume object (ID included). That is directly at
   odds with §6.2's "outwardly everything is addressed by Volume ID", and we have an independent reason (§3.1.1's
   internal ID addressing is the same rule extended inward). **Keep ID addressing.**
2. **Three things we have that both reference implementations lack**: read-only mounts / exclusivity or leases in
   any form / a per-volume capacity cap. The capacity cap especially — neither implementation has any byte-ceiling
   field, and one's capacity is even **virtual** (`statfs` hardcoded to 4 TiB, with its documentation stating plainly
   that this is "not a storage allocation, not a hard quota, and not the node filesystem's capacity"), with quotas
   only at team level and **measured after the fact**; whereas we measured btrfs squota delivering a **hard `ENOSPC`
   precise to the cap** (precision ~0.05%, rootfs-overlay D6.2). So D8's items are **under no external pressure to
   be cut** — what is load-bearing is the qualitative difference between "a hard quota" and "team-level
   after-the-fact measurement", not the precision's particular value.
   (That precision figure is **evidence of magnitude only and not a reproducible indicator**; its force is
   described at that point in D6.2.)
   - **Snapshot/clone is not on this gap list**: one implementation has real implementations of both (a snapshot is
     a separate table, a clone is the same type plus a source pointer), and those two implementation details became
     D6.1a's two strongest arguments in reverse (whether a snapshot should be an independent type, and whether
     `parent` should carry an FK) — see that section.
   - One **shape worth recording** (should delegated direct access ever be wanted): issue each volume an
     independently scoped token as the only credential its content data plane accepts, with the list interface
     **deliberately not returning it** — which cleanly separates "may enumerate volumes" from "may read and write
     this volume's bytes". That implementation lacks rotation/expiry (the token is static for the volume's whole
     lifetime and cannot be revoked without destroying the volume), and **that missing half is not worth copying
     along with it**.
3. **The third points the other way: on the direct content access *capability* both agree with us, and on the
   mechanism they do not.** (Sources checked.)
   Both provide host-side reading and writing of volume content **requiring no sandbox at all**: one is a separate
   service of 9 RPCs on the orchestrator (create/delete + two families for dir/file/path, with files streaming
   bidirectionally in 1 MiB chunks, and requests carrying only `{volume_id, volume_type, team_id}` plus a path, with
   no sandbox in the signature at all); the other makes the same capability an HTTP file API and states in its code
   that it **must** work when no sandbox is live — its volume record carries a pair of default POSIX uid/gid fields
   **whose reason for existing is precisely that "there is no live sandbox to supply an identity"**. This convergence
   is no coincidence: for an object whose lifecycle is independent of a sandbox, if content can only enter and leave
   through a sandbox then its independence is only at the record level (D5.1).
   - **But we do not copy the mechanism.** One of them pays for path confinement with **a fresh mount namespace + a
     bind + `pivot_root` per request** (which also requires root and `LockOSThread`'s thread-ownership protocol),
     measured on the same machine at **342×** the cost of `openat2(RESOLVE_IN_ROOT)` (both figures in D5.1), and
     privileged. Their shape is not "security has to cost this much" but code that predates (or overlooked)
     `RESOLVE_IN_ROOT`. **Copy the capability, not the mechanism.**
   - **We explicitly do not copy their other half either: neither puts any coordination with mounts on this path.**
     One's host-side writes and the guest's live NFS mount independently open the same tree with no lock, no lease,
     and no version stamp (both call sites checked); the other's deletion is "delete the row, return 204, and let a
     detached goroutine delete the bytes, with a comment saying 'clean it up later if it fails'" — and **that
     'later' cleanup program does not exist anywhere in the repository** (searched by category: cron, periodic
     tasks, sweepers). At the same place we put **the write-regime invariant + an in-flight marker** (D5) and **two
     sweep faces** (D7). This is the same capability at two completely different degrees of completion, **and the
     difference is exactly the part the engine should hold**.
