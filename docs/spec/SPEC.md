# Sandbox Engine — SPEC

> The **architecture spec** (a spec, not a roadmap) of a standalone, self-hostable
> agent sandbox engine that is freely configurable inside the guest. It describes
> **what the engine should be, where its boundaries lie, and what it contracts to
> do**; delivery scheduling (roadmap) is out of this spec's scope.

## Document map (§ numbers are globally unique and run continuously across files)

This spec is split by *rate of change × reader*. `§` numbers are globally unique and
continuous across files, which makes them a stable addressing anchor — a reference
writes just `§3.3` and need not care which file it lands in.

| File | Covers | Role |
| --- | --- | --- |
| **SPEC.md** (this file) | §1 positioning and boundaries · §2 security model | The charter: most stable, rarely changed |
| [`model.md`](./model.md) | §3 sandbox model · §4 lifecycle | The core model, evolves at medium speed |
| [`subsystems.md`](./subsystems.md) | §5 subsystems (A–E target definitions) | Subsystem boundaries |
| [`api-contract.md`](./api-contract.md) | §6 API and CLI contract | Changes most often |
| [`open-decisions.md`](./open-decisions.md) | Register of unsettled items | Choices not yet decided / assumptions still to verify |
| [`deep-dives/`](./deep-dives/) | Single-topic deep dives hung under a § (proposals) | Written back into the owning § once the conclusion is stable |

For the discipline, see [`CLAUDE.md`](../../CLAUDE.md): settled goes into this spec,
unsettled stays in `open-decisions.md`, and the two never overlap.

---

## 1. Positioning and boundaries

### 1.1 Positioning

A **persistent, mutable, root-capable agent sandbox engine**. The isolation substrate
is a pluggable runtime provider (§5.A); today the only implementation is gVisor
(runsc). Once a sandbox is launched from a Template it is an isolated environment: at
runtime it freely installs dependencies with `apt`/`pip` and modifies its filesystem,
and those changes **persist**. It can be paused at any time (releasing RAM/CPU,
occupying only disk) and resumed. Several connections (a shell, various agents) can be
attached at once, sharing one FS and one network.

The core orientation is **lightweight, no-hypervisor**: no dependency on KVM and none
on nested virtualization — the sentry is just an ordinary unprivileged process on the
host, so it **runs in the places where hardware virtualization is unavailable**
(commodity cloud VMs, inside a container, a CI runner). The target deployment shape is
**one orchestrator binary + one config + a local store**, with no heavyweight external
dependencies.

> **One qualifier: that sentence describes the engine's OWN deployment shape, not "nothing
> a sandbox can do requires another process".** A network declaration that narrows egress
> down to some rendezvous point (§5.D `egress.via[]`) requires a process **outside the
> engine** listening behind that endpoint. The engine does not start it, does not
> health-check it, and does not know what it is — it only installs a rule pointing at some
> address (§5.D). So under a declaration with no `egress.via[]` the sentence holds
> literally; under one with `egress.via[]` **the deployment topology gains a process while
> the engine's dependency list stays the same**: an unreachable endpoint has its
> consequence on that one connection (§2.4 fail-closed), not on the engine's startup path
> and not on the create path.

> **gVisor terminology** (reused throughout): **sentry** = gVisor's userspace kernel,
> which intercepts and virtualizes the guest's syscalls; **systrap** = the sentry's
> default interception mode, catching syscalls with a seccomp trap (no KVM needed);
> **gofer** = the host-side process in gVisor that proxies the guest's file access.
>
> **This system's terminology**: **engine** = the sandbox abstraction this spec
> describes; **orchestrator** = the single binary hosting the engine; **control plane** =
> the part of the engine that holds the authoritative records; **store** = the durable
> backend for control-plane records (Bindings, each object's lifecycle-state, and the
> reference rows a derived refcount needs), abstracted behind an interface and
> replaceable. There are three kinds of **pluggable-implementation seam**, materializing
> respectively "what runs the isolated process" (**runtime provider**, §5.A), "which
> layers compose a sandbox's rootfs and where it lands" (**rootfs provider**, §5.B), and
> "which backend an independent Volume lands on" (**volume backend**, §5.C). **driver** =
> the CLI binary of one specific agent (claude-code / opencode / pi …); **Overlay
> artifact** = this sandbox's writable delta layer relative to its Template (§3.1).
>
> **Canonical short names of the five subsystems** (§5 — code package names, logs, and
> review all use these, never a section number as a stand-in): **Runtime** (§5.A, the
> execution substrate) · **Rootfs** (§5.B, rootfs/Overlay) · **Volume** (§5.C) ·
> **Network** (§5.D) · **Transport** (§5.E). Four uses of `fs`/`FS`/`Rootfs` are
> deliberately distinguished — do not merge them: **Rootfs** = subsystem 5.B; **`fs`** =
> one wire service of §5.E Transport (`read/write/list/stat/mkdir/remove/move`); **rootfs
> provider** / **volume backend** = two differently shaped, separately registered storage
> materialization seams, and not the same thing (why they are separate, and which
> substrate primitives they share, is in the §5 preamble). Likewise **Runtime**
> (subsystem 5.A) vs **Sandbox** (§3.2, the top-level persistent unit) mean "what runs
> it" vs "which sandbox".

**Engine vocabulary boundary.** The first principle (see the engine's root CLAUDE.md)
requires that product semantics not enter the engine; stopping them at naming time is
cheaper than removing them at implementation time. The table below constrains
**engine-internal entity naming** (types / packages / fields / API endpoints). It does
not constrain ordinary prose that *describes something outside the engine* — when the
engine states clearly what it does **not** do, it naturally has to name these things.

| Barred as an engine-internal name | Why / what to use instead | Allowed in ordinary prose? |
| --- | --- | --- |
| `generation` | The engine introduces no standalone generation counter; the four things such a counter wants to solve each belong somewhere else: **where it is** → Placement (§3.3) · **staleness** → placement/IP as the epoch (§4.3) · **concurrency arbitration** → per-sandbox transition lock + CAS (§4.4-B) · **failure recovery** → atomic rollback to the source state + startup reconciliation reclaiming orphan placements (§4.4-A, which is also why there is no `failed` state) | No (barred outright, to stop the counter seeping back in from elsewhere) |
| `principal` / `tenant` | The engine has no authorization subject and no tenancy inside it (§1.4 non-goals); multi-tenancy is an addition made by a join table outside the engine | Only in prose asserting "these live outside the engine" (e.g. §1.4 "multi-tenancy", §3.2.1 "which agent belongs to which tenant"); **never as an engine-internal name** |
| `owner` (belonging / authorization sense) | A sandbox has no owner column; reclamation looks only at refcount/TTL (§4.3 / §1.4) | Only in the **responsibility** sense (e.g. "the owner of the idle decision is the control plane") — that is "who is responsible for it", not "who possesses it" |
| `session` / `turn` | Agent session semantics are an addition made by an adapter outside the engine (§5.E); the engine has only process/fs | Yes, and it necessarily appears in prose asserting "these live outside the engine" |
| `agent` | The engine neither knows about nor addresses agents (§3.2.1); it has only the sandbox/process/fs atoms | Yes (when describing what is outside the engine, the product layer included) |
| `credential` / `secret` / `token` / `api_key` | The credential family is permanently excluded (§1.4); the engine has no such concept — all it sees is a TCP flow to some endpoint (§5.D). Today this is **vacuously true** (there is no credential material in the engine at all). **This row is held by the table, not by a structural guard**: `token` has plenty of legitimate uses in this repo (the opaque ID `tmpl_<token>`, lexical tokens), so a mechanical token list would produce false positives at scale — unlike the situation at the two `seam_test.go` files in `runtime`/`volume`, where host-primitive spellings are finite and have no legitimate use | Only in prose asserting "these live outside the engine" (e.g. that §1.4 non-goal, §5.D); **never as an engine-internal name** |

> **Premise: deployed on a Linux host able to run gVisor** (able to install seccomp
> filters, handle signals, on a new enough kernel). That is systrap's inherent
> threshold; any platform that can run gVisor satisfies it, and the engine treats it
> as given.

> **Two bottom lines: no-hypervisor and portability, independent of each other.** Any
> decision that **trades portability for performance is rejected, or hidden behind a
> provider as pluggable** (§5.A) — platform is one such case: `local_runsc` pins it to
> `--platform=systrap` and does not inherit the runsc default (reasoning in §5.A), while
> another provider may choose otherwise. No-hypervisor governs "no KVM / no nested
> virtualization"; **not storing memory (§4.2) is a consequence of portability**, not of
> no-hypervisor — gVisor under systrap could checkpoint, and we decline to, because
> memory state pins CPU features (§2.5), which would make the persistent state
> unmovable across host/CPU (§1.5#3). Hence resume is a seconds-scale cold start.

**"Where, and with what, to run an isolated process context" is exactly what the
runtime provider abstracts away.** The point of this interface is to **keep the
abstraction boundary clean**, not to support other backends: the only implementation is
the single-node host runsc process, and the "scheduled remote isolation unit" (a K8s
Pod / GKE Sandbox) serves purely as a paper reference for calibrating the interface
boundary, never entering the delivery path (§5.A).

### 1.2 In scope

- A persistent, mutable, root-capable single sandbox; dependencies installed freely at runtime, changes landing on disk and staying there.
- Sandbox lifecycle: create / pause / resume / destroy + automatic idle pause + expiry reclamation (§4).
- Custom Templates: platform-provided plus user-frozen-from-a-sandbox (Promote — the engine reads at-rest bytes after quiescing internally, §3.1).
- Volumes: independent, ID-addressed storage decoupled from any sandbox, which can be created / operated / shared on its own.
- Declarable networking: every sandbox may declare its own network reachable set at launch, and the engine enforces it (§5.D / pillars ③④).
- Multi-connection transport: one sandbox can have several connections attached concurrently (shell / agent / tools), sharing one FS and one network.
- One clean, sandbox-centric API / CLI contract (§6).

### 1.3 Extension principle: a duality that guides how we design today

This is a **general methodology** (it applies to any system); §1.4 then treats each
non-goal as one application of it. The bottom line is **one-way dependency**: the core
never depends on an extension, an extension depends on the core one way only, and there
is no cycle. On top of that sits a **duality**:

- **The constraining face**: extensions are additive only and do not alter existing structure.
- **The enabling face**: today we must leave seams (joints where things can be added)
  and invariants (cross-cutting protections) **at the right joints**; otherwise an
  addition has nowhere to land, the extender is forced to modify, and the constraining
  face goes bankrupt.

**Two kinds of design action:**

1. **Leave seams** — the system grows along three axes, each with one additive-only archetype:

   | Axis | Seam archetype | "An addition" = |
   | --- | --- | --- |
   | **Behavior** (how it is done) | open registry (an interface + registrable implementations) | register an implementation |
   | **Data** (what is stored) | external join table (ID-addressed) | add a join table |
   | **Consumption** (what is emitted outward) | event stream (broadcast + subscribe) | add a subscriber |

   All three are seams **inside the engine**. There is also an **orthogonal axis outside
   the engine**: some extensions do not enter the engine at all, instead **layering an
   adapter** on top of the primitives it exposes, or adding policy at the placement
   layer. An addition outside the engine is inherently zero-intrusion — it **cannot
   reach** the engine's internals, and needs the engine to leave no seam at all. So the
   criteria below constrain only extensions **inside the engine**.

2. **Hold invariants** — an invariant does not "let you add something"; it "guarantees
   some core property stays uncontaminated", which is what lets a seam actually
   accommodate a future implementation. (Which invariants this system must hold, and
   where each lands, is in §3.3 / §4.3 / §6.2.)

**Cost criterion (the easiest thing to get wrong):**

> A seam / invariant is good design you owed the **current implementation** anyway,
> which incidentally becomes the landing point for a future addition; a **reservation**
> is useless structure added only for an **imagined future**, worth nothing today.
> **Build the former, refuse the latter.**

The subtle timing: **build the seam / invariant now** (a present necessity), **build the
join table later** (guaranteed zero-intrusion by the invariant). That is: leave no
concrete "slots" (columns / hooks), but do have the right seam shape.

**Litmus test (run it before every table you create and every interface you define):**

> Any future need that wants **to enter the engine** should reduce to one of **register
> an implementation / add a join table / add a subscriber**. If none of the three fits it
> and existing structure must change, ask first: **does it belong in the engine at
> all?** — anything that can be an adapter outside the engine should stay outside. If it
> genuinely belongs inside the engine and yet none of the three fits, **only then** does
> it mean a class of seam is missing or an invariant has not been held — a design defect
> to repair now.

### 1.4 Non-goals: what we do not do, and which seam / invariant keeps it addable

Explicitly outside the boundary. First, two categories: **permanently excluded**
(vetoed by one of the bottom lines, never to be added) and **not done today, addable
with zero intrusion later** (deferred items).

- **Permanently excluded — freezing/restoring memory state (the whole family)**:
  pause/resume takes no memory snapshot. This is not a deferral; it is vetoed by the
  portability constraint (memory state pins CPU features, §2.5). **The scope is the whole
  family, not just "a memory image of the entire sandbox"**: single-process checkpoint is
  under the same constraint (it pins the same set of CPU features) and so is permanently
  excluded too — the difference is image size, not portability cost. What we provide is
  **landing on disk + logical continuation across a cold start** (§4.2); session
  continuation is done by an adapter outside the engine on top of process/fs (`--resume`
  plus reading the history off disk) — that is **not this non-goal's "future path in"**,
  it is the alternative itself.

- **Permanently excluded — credentials (the whole family)**: the engine **does not store,
  does not inject, and does not know** any provider / model credential; there is **no
  credential type / field / package / endpoint inside the engine**, and `credential` /
  `secret` / `token` / `api_key` are not engine-internal names. This too is not a
  deferral; it is vetoed by two independent reasons:
  - **The engine cannot know the correct scope, and pinning any candidate is deciding on
    the product layer's behalf.** A table of "which credential this sandbox uses" keyed by
    sandbox ID asserts that sandbox↔credential is 1:1 — some product may choose that, but
    the engine has no basis to assert it; keyed by the real scope, it would have to be
    named `tenant` / `principal` — barred outright by the §1.1 vocabulary boundary. Both
    roads are **closed to the engine**, and that is exactly the point: this is not a
    trade-off, this table does not hold inside the engine.
  - **Anything the engine hands the guest is readable by guest root.** Every byte the
    engine holds and hands into the sandbox is readable by uid 0 inside the guest (§2.2),
    and whether the carrier is a Template layer, a store column, or a process env changes
    nothing about it (§5.B / §5.E) — so the engine cannot, by construction, be the secrecy
    boundary for **any** credential material.
  **This one leaves no path in, because none is needed**: the credential lives in a
  process outside the engine, sitting behind the endpoint §5.D `egress.via[]` points at.
  The engine-side addition is zero — all it must provide is "narrow egress down to that
  endpoint" (the §5.D declaration) and "a sandbox's identity is unforgeable" (pillar ④),
  both of which it owed anyway. **So the engine also has no need for the "real credential
  vs fake credential" dimension** — it does not know whether some string is a key, and
  therefore need not know whether it is a real one (the operational consequence of this is
  in [`deep-dives/network.md`](./deep-dives/network.md), end of D1).

Below are the **deferred items**: each states what is not done today, and which §1.3 seam
/ invariant gives it a zero-intrusion path in later — that is the precise meaning of "not
walled off": it points at a seam that already exists, or an invariant held today.

| Non-goal (not done today) | Future path in | The seam / invariant that guarantees it |
| --- | --- | --- |
| **Warm pool**: no fully-ready sandboxes kept around for ms-perceived startup | Reuse the flow shape of "network / Volume bindings injected through the control plane at create time", switched to late binding (§6.2) | The injection flow shape (§6.2) |
| **Authorization subject / multi-tenancy**: no owner / principal concept | Add a `sandbox↔principal↔capability` join table | Data join table + the ID-addressing invariant |
| ↳ which also defers **microarchitectural side channels** (Spectre-class, across tenants) | When multi-tenancy arrives, add a co-residency policy at the placement layer (no shared core / no shared scheduling) | Placement policy outside the engine (activated together with multi-tenancy; §2's five pillars unchanged) |
| **Multi-node / multi-orchestrator HA / cross-host migration**: focused on single node, single orchestrator | Register a provider facing a "scheduled remote isolation unit" (its scheduling layer carries the cross-node concern) | Behavior registry (runtime provider, §5.A) + identity that does not encode placement |
| **Cost ceiling (max-lifetime)**: reclaim by idle-TTL + refcount only, with no hard ceiling unrelated to activity | An upper-layer quota adds a table / a subscriber consuming lifecycle events | Data join table / Consumption (§4.1) |

> What they share: these "paths in" **have not a single line of code written today**;
> they only ensure the corresponding seam shape exists and the invariant is held
> (§1.3 cost criterion).

### 1.5 Differentiating position (a different axis from E2B)

E2B is a **managed cloud** selling a fast proprietary microVM running on its own fabric
plus KVM; we are an **embeddable, no-hypervisor, self-hostable, state-transparent**
engine. Five footholds:

1. **No-hypervisor** — runs where E2B physically cannot (gVisor systrap, an unprivileged process, no KVM).
2. **Self-hosted / data sovereignty** — one binary + a local store; code and data never leave your own infrastructure.
3. **Transparent, portable state** — the persistent state is a plain-file Overlay
   artifact (§5.B), movable and auditable across host / CPU / runsc version; not a RAM
   image that pins CPU features.
4. **An open interaction model** — the engine has only two general primitives
   (process/fs), and agent / session semantics are an addition made by an adapter outside
   the engine (§5.E), so the openness lives in the adapter layer rather than being locked
   into the engine.
5. **Agent-runtime scope** — concurrent multi-connection is inside the engine (§5.E),
   while swarm topology and credentials are outside it (§3.2.1 / §1.4), and the engine
   provides the ground for the latter: a **reachable set that can be enforced narrow** +
   an **unforgeable sandbox identity** (§5.D / pillar ④). E2B hands that whole layer to
   the user; what we hand over are the two invariants that let that layer be correct.

A detailed competitive comparison is out of scope here; this section pins only **our own
boundary**.

---

## 2. Security model

The isolation boundary is carried by the **host-side pillars**, orthogonal to the
freedom inside the guest. Everything that is relaxed happens inside the guest, and
**host isolation is not modified at all**.

### 2.1 The five pillars, cut into two groups by "who guarantees it"

Each pillar is written as **an abstract contract + today's single-node
materialization**, and is annotated with its **enforcement locus** — which directly
delineates "what changing the provider would touch". Two groups: **substrate-borne**
(guaranteed by the isolation substrate itself, and touched by a provider change) and
**control-plane-borne** (the authority sits in the engine's control plane, orthogonal to
the provider).

> A few words used below are only detailed later: **placement** = the physical resources
> rebound on every resume (netns/IP / container / cgroup / node, §3.3); **Binding** = the
> class of state recorded in the control plane and rebuilt from the record on resume
> (§3.4); **runtime provider** = the pluggable implementation of "what runs the isolated
> process" (§5.A).

| # | Pillar | Abstract contract | Single-node materialization | Enforcement locus |
| --- | --- | --- | --- | --- |
| ① | **Syscall interception** | Syscalls from any uid inside the guest (0 included) are intercepted and virtualized by a userspace kernel, never passing straight through to the host kernel | gVisor sentry (systrap) | **substrate** |
| ② | **The substrate's host process is unprivileged + seccomp'd** | The host process running the isolation substrate is itself unprivileged and narrowed by seccomp; what the host sees is always a confined process | The sentry runs unprivileged + seccomp'd | **substrate** |
| ③ | **Network reachability is enforced narrow** | A sandbox's network reachable set is exactly **the set its declaration allows** (which may be an enumerated set of endpoints, or "the public internet minus the L0 floor and the deny-safe private tier"), and all other egress is refused; ingress is closed by default and opened by an explicit publish | An nft rule set on the host-side veth, installed atomically with the placement (§5.D) | control plane (authority over the declaration) · the enforcement mechanism belongs to placement |
| ④ | **A sandbox's network identity is unforgeable** | The identity a sandbox presents on the network cannot be altered by any uid inside the guest (0 included) | A separate netns per sandbox + a point-to-point veth; rule predicates bind the **ingress interface** (`iifname`, by name rather than ifindex — D5 ⑫) rather than the source address (§5.D) | **substrate** (the netns boundary) + control plane (choice of predicate) |
| ⑤ | **A controlled mount surface** | What gets mounted into the guest is adjudicated by the control plane; the guest cannot widen its own mount surface | The mount table is a Binding, and resume remounts from the table (§5.C) | control plane (materialization belongs to placement) |

**This table is the entire source of the "provider security obligations"** (narrowed into
one acceptance-testable obligation in §2.4): changing the provider can touch only ①②④
and the **materialization layer** of ③⑤, and **never the declaration/policy layer of
③⑤**.

> **Why ④ is a pillar rather than an implementation detail of ③.** Not because it is
> independent of ③ — quite the opposite: **④ is upstream of ③**. ③'s contract takes "the
> reachable set of *this sandbox*" as its subject, and that "exactly" quantifier is
> meaningful only when identity is unforgeable; a forgeable predicate lets A claim B's
> rules, so A's reachable set is no longer the set A declared — **which breaks ③ at the
> same time**. Only the reverse direction is independent (rules written in the wrong order
> can break ③ alone, with ④ intact). It earns its own row on two other counts: **it has a
> consumer ③ never mentions** (everything outside the engine that judges by sandbox
> identity rests on it — see the next note), and its **enforcement locus differs** (the
> substrate's netns boundary + the control plane's choice of predicate, whereas ③'s
> authority is entirely in the control plane). This has been measured: guest root can `ip
> addr add` an address to itself, so **a predicate matching on source address is bypassed
> and one matching on ingress interface is not** (reasoning and measurement in
> [`deep-dives/network.md`](./deep-dives/network.md) D5 ③). The choice of predicate is
> therefore a security assertion, not a matter of style.
>
> **④ is also the only place the engine is load-bearing on the "credential boundary".**
> A credential itself is **not an engine concept** (§1.4 non-goals / §5.D): the engine
> does not hold it, inject it, or know about it. But anything outside the engine judging
> "which credential this sandbox may use" has its correctness resting on "a sibling
> sandbox cannot impersonate this sandbox" — which is this pillar. Hold this one, and
> judgments about which credential belongs where have ground to stand on; fail to hold it,
> and the outer layer's authorization is wrong no matter how it is written.

### 2.2 Guest identity is not a security invariant

**Root inside the guest is not the host isolation boundary.** uid 0 in gVisor is root
inside the guest, virtualized by the sentry (pillar ①), and the sentry itself is
unprivileged on the host (pillar ②). So whether the guest is root, and what capabilities
it holds, is **only hygiene inside the guest, not a host isolation pillar** — not one of
the five pillars takes "the guest is non-root" as a premise. That is exactly what "install
dependencies freely" needs (`apt` wants root).

Hence: **guest root is the default**, with no non-root default identity introduced and no
user-switching mechanism preinstalled. Since it is worth nothing on the isolation axis,
the root default is the most direct expression of §1.1's "root-capable agent sandbox"
(§5.A; [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1). **The engine
does not model guest users**: "run as some user" is native to the guest (`su`/`sudo`, for
the product/template layer to handle), not an engine primitive (§3.2.1); the engine has
neither a create-time "default identity" field nor a per-`Start` run-as parameter. The
cost of allowing root lands on the **data plane inside the guest** (§2.3), an accepted
product trade-off. If a consumer ever genuinely needs engine-level run-as, it can be
added back additively per §1.3 — nothing is reserved for it.

### 2.3 The data-plane trade-off: exactly which surfaces are reachable and which are not

Relaxing guest identity (root + a writable Overlay artifact) widens **one dimension and
one only: this sandbox's data plane**. Pinning the boundary exhaustively:

**Reachable (the accepted blast radius, all inside this sandbox's data plane):**

- **The entire content of this sandbox's Overlay artifact** — `/usr`, `$HOME`, any package
  installed at runtime (§3.1.1). **Including filling its own disk capacity cap**: a guest
  `dd` hitting the per-sandbox capacity cap (§3.4 capacity axis) only makes **its own**
  `write()` return `ENOSPC`, which is the expected in-data-plane outcome and does not spill
  outward.
- The **Volumes** mounted in per the mount policy, each honoring exclusive / shared-ro /
  shared-rw semantics (§5.C); what root can reach is "those already mounted for this
  sandbox", not the full set of Volumes.
- The **egress endpoints the declaration allows** plus the **ingress ports** exposed by
  publish (§5.D) — that is, the side the network declaration permitted in the first place.
  **The width of this side is determined by the declaration's value, not by a fixed
  constant**: under `egress.default: allow` it is the whole public internet (minus the L0
  floor and the deny-safe private tier), and in that case "bytes do not flow out" is not in
  the engine's set of promises; under `default: deny` plus a single `egress.via[]` it is
  approximately one address. Both are correct engine behavior; the difference is what the
  caller declared (§5.D).

**Not reachable (host-side / cross-sandbox invariants, unmoved by relaxing guest identity):**

- The host FS, host processes, and **other sandboxes' Overlays and Volumes** — isolated
  outside the guest by pillar ①'s syscall interception.
- **Other sandboxes' network endpoints** — a sandbox's reachable set is exactly {the
  endpoints the declaration explicitly allows}, **sibling sandboxes are unreachable**, and
  this does **not** relax with the declaration's value: the sibling address space is in the
  L0 floor and a declaration cannot lift it (§2.4). **This one is not held by pillar ①**:
  ① intercepts syscalls, not network packets (the guest uses the network legitimately), so
  cross-sandbox network isolation is an **independent control-plane invariant**,
  materialized by §5.D's rule set, standing alongside "cross-sandbox storage isolation
  rests on pillar ①" (see network.md D5). **A point-to-point `/30` veth only removes L2
  adjacency, and the host still routes between the two ends**, so the materializer is that
  nft floor and the veth is one layer of depth.
- **This sandbox's own network identity** (pillar ④) — guest root can add addresses to its
  NIC and change its routing table, but cannot change the fact of "which veth belongs to
  which sandbox", so the rules the engine writes by ingress interface are unbypassable by
  it. **This one does not include credentials**: a credential is not an engine concept
  (§1.4), and the engine neither holds nor promises it (why this identity is the only place
  the engine is load-bearing on the credential boundary is in §2.1 ④'s second note).
- **The control-plane records themselves** (the authority of the Bindings, §3.4) — the
  values of the mount surface, the network declaration, and the resource ceilings are
  adjudicated by the control plane, and root inside the guest cannot change them (pillar ⑤).
- The **host kernel attack surface** beyond the sentry — confined inside the userspace
  kernel by pillar ①.
- **The physical capacity of the host disk** — a guest write consumes only the per-sandbox
  capacity cap of its own Overlay upper (§3.4 capacity axis) and cannot consume the whole
  disk, so it can neither fill the machine nor break other sandboxes' writes. **But the
  capacity cap governs only the bytes the guest writes itself, not those the kernel writes
  on its behalf**: which is why "deterministic, whole-group termination" is a component of
  this invariant rather than a tuning knob — the path by which **it bypasses the capacity
  cap** is in §2.4.

**The resource-exhaustion dimension is likewise not held by syscall interception (pillar
①)** — ① virtualizes syscalls, not resource usage: CPU/RAM/IO rate rests on cgroup (§5.A)
and disk capacity on FS quotas (§5.B/§5.C). Without a capacity cap, `dd` is a perfectly
legal `write()` that fills the shared disk and breaks this invariant.

### 2.4 The provider's security obligations (abstract, bound to no specific provider)

§2.1's two-group split, narrowed into one acceptance-testable obligation, for evaluating
any future provider:

> **A qualified runtime provider must maintain the substrate-borne ①② (a userspace kernel
> intercepting syscalls + the substrate unprivileged on the host); for ③ it is responsible
> for one thing only — narrowing the isolation unit's reachable set exhaustively down to
> "the set the declaration allows" (which may be an enumerated set of endpoints, or "the
> public internet minus the L0 floor and the deny-safe private tier"), refusing all other
> egress by default (an escape is a failure, not a degradation); for ④ it must bind the
> rules' predicate to a **fact the guest cannot change** (not a source address the guest can
> write); and for ⑤ it only enforces the declaration the control plane hands down, holding
> no authority over the declaration itself.**
>
> **One incidental obligation that is easily read past: the enforcement mechanism must be
> atomic with the placement.** The rule set must be fully installed in the interval where
> "the isolation unit's network already exists but the guest is not yet running" — any
> window of "network up, rules not installed" is a fail-open window, and **only the side
> that creates the placement has that hook point**. Like the netns, the rule set is
> crash-surviving host state, so startup reconciliation must rebuild/tear it down along
> with everything else; it is not something that can be patched in afterwards — the
> mechanism belongs to §5.A. **And "rebuild" means re-evaluating the write point's
> admission criteria, not merely re-rendering the content** (§3.4.5's general rule: the
> declaration is durable, the criteria's inputs are not, so a reinstall is a path that can
> bypass that one refusal at the write point, and every assertion about the first hop is
> completely insensitive to it). This settles the question of "who enforces" **physically**
> — it is not that the engine wants to manage the network, but that it is the only party
> that **can** enforce at the correct moment (§5.D).

**Materializing the RAM ceiling carries two acceptance-testable obligations** (§3.4: RAM
is a security limit, and the consequence of exceeding it is sandbox termination):

- **Termination must be deterministic and whole-group**, not the substrate picking a
  victim. A victim-picking shape is both nondeterministic (the same configuration gives
  different results across runs) and able to land in a "no killable victim → retry forever"
  state, and the retry path **bypasses the capacity cap** and can fill the host disk
  (§2.3). `local_runsc` delivers this with `memory.oom.group=1`; a remote provider delivers
  it through its scheduling layer's equivalent whole-group termination semantics.
- **The termination cause must be decidable**, and across a seam it is an **abstract
  enumeration** rather than an errno / cgroup file — otherwise the control plane cannot
  separate it from a crash and will go resume a sandbox that is certain to be terminated
  again (§4.3 places `resource-terminated` alongside `dead-recoverable`). **A provider must
  hand the cause of death out before tearing down its own placement**, and if it cannot
  decide, it reports `Unknown` and **does not guess** (why the signal is volatile, why it
  must be persisted on the spot, and how the unknown case is handled, are all in §4.3).

**The three invariants of egress enforcement (the load-bearing wall is the IP layer, not
the domain name).** The engine's enforcement evaluates in a vocabulary of **IP / port /
protocol** only — it does not resolve domain names, does not look at TLS, and knows no
application-layer protocol (why domain names do not enter the engine, and the "solvable but
at an explicit cost" road, are in [`deep-dives/network.md`](./deep-dives/network.md) D2):

- **The L0 floor is always deny and a declaration cannot lift it** (enforced in code) =
  link-local (IPv6 included, which naturally covers every cloud metadata address) /
  loopback / **any address of the host itself** (covered statically by a single pure
  ct-state rule on the `input` side narrowed by `iifname`, **never enumerated** — this
  formulation is both **strictly wider** than "only cover the control-plane endpoint" and
  cheaper: one static rule that is never computed, so the engine need not know its own bind
  address, and the whole class of "the host address changed, must we re-render?" questions
  does not exist; measured in network.md D5 ⑧) / **the sibling sandbox address space**.
  Above it sit two tiers determined by the declaration: **private internal ranges are
  deny-safe by default** (liftable by an explicit declaration) · **the public internet
  follows `egress.default`'s value**. **The members of this tier are exactly the three
  RFC1918 ranges** (`10/8` · `172.16/12` · `192.168/16`), with the v6 side derived
  mechanically (`fc00::/7`, plus the NAT64 image **of each member** — derived per RFC 6052
  §2.2 as **the members' images, never the `64:ff9b::/96` prefix itself**, since the latter
  maps the whole v4 space including the public internet, and dropping the entire range would
  refuse the public egress that `allow` promises; measured in network.md D5 ㉓). **This tier
  is taken rather than the wider IANA GR=False snapshot**: it covers the only exposure
  surface actually measured (a sandbox reaching the host LAN when all three egress
  mechanisms are in place, D5 ⑤), whereas the wider snapshot would have to carry five
  GR=True reverse allowances and would also run into "interval sets refuse nested members /
  `auto-merge` silently coalesces subnets" (D5 ⑱) — RFC1918 has no GR=True member, so both
  of those costs disappear together. **Widening it later is purely additive** (adding a line
  to a static list, D7 litmus), so no mechanism is reserved for it.
  **This tier and the floor are two rules in sequence and do not share one set**: the
  sibling `/16` falls inside `10/8`, and sharing would either fail to install or silently
  coalesce the floor away (D5 ⑱). **The ordering**: the floor (always deny, unliftable) sits
  **above the declaration's allowances**, while this tier sits **below** them (that a
  declaration can lift it is the entire meaning of deny-safe). **It only actually emits a
  rule under `default: allow`**: under `deny`, the bottom catch-all refusal already covers
  every address in this tier, the reachable set is verbatim identical, and installing one
  more rule that **never matches** would manufacture a crowd of legitimate zeros on the one
  health signal we have, "the counter is 0" (the same discipline as D4.1). The floor admits
  only things that "break some engine invariant on arrival, and that the engine can name
  without relying on deployment configuration" — which is why the engine knows no cloud
  vendor (membership criteria in network.md D1 ③ / D5 ④).
- **Both hops are pinned, and the second hop must be enforced by the kernel rather than
  resting on the other party's self-discipline.** The first hop is the guest's egress; the
  second is **the egress of that engine-external process sitting behind `egress.via[]`** —
  it originates the connection on the guest's behalf, so it must be held behind the same
  host rule set, **rather than being handed the floor list to obey**: handing it down would
  make it a trusted party, which is exactly the confused deputy this invariant exists to
  refuse (derivation in network.md D4.2; the sandbox address space is a fixed CIDR, so this
  is a **static** rule, evidence in network.md D5 ④). One contract follows: **if
  `via[].endpoint` is not host-local, refuse outright** — the kernel's reach does not extend
  to it, and accepting it would stand the floor on that process's self-discipline. The uid of
  that local process is an input **the deployment hands the engine**, not something the
  declaration carries. **And it must not be the engine's own uid**: the engine's transport
  endpoint is a guest veth address dialed from the host side, and that falls inside the
  floor's sibling item, so such a value would make the engine hit the second-hop floor on
  every single dial to a guest — taking out readiness / liveness probing / RPC for all
  sandboxes at once, and presenting as an **intermittent fault** because of `skuid` missing
  matches. So it is refused at startup (before the table is installed).

  **The second hop is on a third hook, not on either of the first hop's two.** The second hop
  is originated by a **host-local process**, so it traverses neither the guest's ingress
  interface **nor `forward`** — measured, it counts 0 in any shape on `forward` and is
  visible only on `output`, where the only usable narrowing predicate is **`skuid`**
  (network.md D5 ㉔ — hence the deployment input needs only a uid; gid has no corresponding
  clause on that hook). ⟹ **Holding the second hop down is an independent base chain**, not a
  by-product of the first hop's rules; reading it as "the floor covers it incidentally"
  yields a rule set in which the second hop is never evaluated at all, and **the symptom is
  that the floor's counter does not move**. That hook therefore carries a narrowing
  obligation **of the same origin as the per-sandbox chain but with a different predicate**:
  a drop on `output` not narrowed by uid is a **host-level egress kill switch** (same
  measurement), and `iifname` cannot serve as that narrowing there.

  **The boundary between the floor and `via[]` (which looks self-contradictory until written
  out)**: `via[].endpoint` must be host-local ⟹ the steered traffic lands exactly in the
  floor's "any address of the host itself" item. The way the hole is opened is **ordering,
  not narrowing the floor**: `via[]`'s narrow allowance sits **above the floor's drop, in the
  same chain**, and must simultaneously pin the **pre-NAT tuple ∧ the post-NAT port ∧
  `iifname`** (each of the three has a live counterexample when omitted, network.md D5 ⑩).
  This is the floor's **one sanctioned exception**, not a weakening of it — the guest never
  reaches that in-floor address; what reaches it is a host-local process.
- **Fail-closed, scoped per-connection**: a destination the declaration does not allow, and
  the one connection affected when `via[].endpoint` is unreachable, are both **refused**
  rather than **falling through to the next rule and being let out**. An escape or a
  degradation is a failure; the effect is confined to the connection that failed and does not
  implicate the rest of the allowed egress.
  > **The scope of the "clean connection failure" half-promise is the refusals with a guest
  > waiting on them, not every refusal.** Egress the declaration does not allow (that is,
  > the fail-closed rule itself above) uses `reject`: gVisor honors ICMP errors, and the same
  > refusal written as `drop` versus as `reject` differs by orders of magnitude in the guest's
  > experience. `drop` is used at the three refusals that **owe the guest no error**: **the
  > second hop's own egress** (originated by a host-local process) · **the deny-safe private
  > tier** · **ingress default-deny** (facing an external caller the engine promised nothing
  > to, where an ICMP error would confirm outward that this sandbox exists). Security-wise the
  > two are identical: neither lets anything through. The criteria and the measured magnitudes
  > are in [`deep-dives/network.md`](./deep-dives/network.md) D4.3 / D5 ⑨.

This obligation list **does not depend on any specific provider shape** — whether a
"host-local process" or "an isolation unit scheduled onto a remote node", it holds word for
word; the latter merely moves ①②④'s materialization onto its own isolation mechanism and
swaps ③'s rule set for an equivalent such as CNI / NetworkPolicy.

> **One spec-level clarification: ③'s obligation does not set up a separate regime for
> IPv6.** A pure ct-state `inet` rule **with no family qualifier** covers IPv6 along with
> everything else, and stops even a global prefix the guest made up itself (measured in
> [`deep-dives/network.md`](./deep-dives/network.md) D5 ⑪), so `fe80::/10` in the floor list
> is **a member at the declaration layer**, not an independent v6 mechanism.

### 2.5 Substrate fact check

These are the runsc facts the single-node `local_runsc` materialization depends on, and they
directly affect the judgments in the sections above. The key supporting fact for the
persistence model (not storing memory ⟹ persistent state = plain file layers) and its
downstream extra capabilities are developed in §5.B; only the runsc-side facts are listed
here.

| Fact | Consequence for the engine |
| --- | --- |
| systrap (the default) intercepts syscalls with `SECCOMP_RET_TRAP` and **needs neither KVM nor nested virtualization** | Establishes the technical premise of no-hypervisor (§1.1) |
| Memory checkpoint/restore **requires matching CPU features across hosts** | We do not store memory (§4.2), so **this constraint disappears outright** |
| iptables/nftables inside the guest is **only partially supported**; and guest packets are injected at L2, so they **do not traverse the sandbox netns's L3 hooks** (measured) | Establishes that enforcement can only hang on the **host-side veth** (or a `netdev` hook) (§5.D): do not push it down into a firewall inside the guest, **and do not put it inside the sandbox netns** either — the latter counts 0 (network.md D5 ②) |
| Mounts are taken from the OCI spec **at startup** and cannot be changed live | A mount-topology change is a resume-boundary operation (§5.C) |
| The whole runsc `--overlay2` family (`memory`/`self`/`dir=`) **is non-persistent across the container's lifetime** — `dir=` unlinks the filestore at startup, so a clean stop means the data evaporates | The persistence layer **does not use runsc's self-managed overlay**; instead the engine assembles it outside runsc with the host kernel's overlayfs, and runsc gets `--overlay2=none --file-access=shared` (§5.B) |

> The carrier's full materialization, the CoW/consistency evidence, and the alternative
> paths belong to §5.B (developed in
> [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)); this section only
> anchors the runsc facts it depends on.
