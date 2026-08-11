# Sandbox Engine — API and CLI contract (§6)

> Belongs to [SPEC.md](./SPEC.md); § numbers run continuously across files, and the
> navigation is the "Document map" at the top of SPEC.md.

---

## 6. API and CLI contract

The backend exposes a **sandbox-centric** API only, **entirely ID-addressed**; product
concepts (session / mode …) do not enter the backend. **The CLI and the frontend are peer
clients of the same API** — the CLI is a first-class citizen, not a back door.

### 6.1 Resources and endpoints (control-plane REST + data-plane Connect-RPC; the split is in [`deep-dives/transport.md`](./deep-dives/transport.md) D5)

> **The outer-edge protocol split**: sandbox/template/volume/network **lifecycle = REST +
> JSON** (the control-plane block, the authoritative endpoints, with OpenAPI describing them
> after the fact); the **process / fs data plane = Connect-RPC** (`engine.transport.v1`,
> whose authoritative contract is `transport.proto`, with clients generating stubs via
> `buf`. That package holds **three** services: the outward-facing `ProcessService` /
> `FsService`, plus `HealthService` for the engine's own use — the last is not an outward
> capability surface, see §5.E). Clients always go through `orchestrator` in the middle and
> never connect directly to `sandboxd` (§4 mediator / D5.1). The REST lines in the
> data-plane block are **shape illustrations only**, not authoritative endpoints (D5.3).
>
> **This API does no authentication/authorization itself**: there is no authorization
> subject inside the engine (§1.4 non-goals), so §6 assumes the caller is already trusted.
> Who may call is decided **outside the engine** (the deployment binds it to localhost / a
> private network, or puts a gateway in front), which is a join-table addition rather than
> an engine primitive.

**Control plane (REST + JSON, the authoritative endpoints)** — sandbox / template / volume
/ network lifecycle, plus event-stream replay. **All of it is low-frequency
request-response, with no long-lived connections and no server push**: every one has
one-round-trip CRUD semantics, which is why even the event stream is a **cursor-paginated
GET** (`/v1/events`, below) rather than SSE/websocket — the engine holds no per-subscriber
state at all (§1.3), and that is exactly why it can stay in the control plane. **"No
streaming" means there is no push surface, not that there is no event surface.**

```text
# Sandbox lifecycle
POST   /v1/sandboxes                 {template_id, network?, volumes?, idle_timeout?, retention?,
                                      cpu_millis?, memory_byte?, disk_byte?}        → {sandbox_id}
#   `network?` = the §5.D four-field declaration; the schema is in the Network block below
#   (**if given, it MUST contain `egress.default`**).
#   **Omitting the whole thing is legal, normalized to `{deny, [], [], []}` and persisted
#   explicitly** (§5.D): so "network omitted" and "network given but egress.default missing
#   → 400" are two different things, and only the latter is class 1 of §6.2's list.
#   The three resource-ceiling axes are in §3.4 (Binding); the per-axis conditional-required
#   rule is in §6.2. Insufficient admission → 507.
#   Each `volumes` item = {volume_id, path, access}, with `access` ∈ {exclusive, shared-ro,
#   shared-rw} — it is a property of the **mount row** (§5.C), so it is given here rather
#   than on the volume record. Colliding with an exclusive lease → 409 (§6.2's third of four
#   409s).
GET    /v1/sandboxes/{id}                                     → {sandbox_id, state, death_cause?}
#   Read-only lifecycle state + cause of death (§4.3). `death_cause` ∈ {`gone`,
#   `resource-exceeded`, `unknown`}, and **the key is absent when no death was observed**
#   (not an empty string, not null): `CauseNone` is "there is no cause of death" rather than
#   a kind of cause, and serializing it as `""` would place a falsy-but-present field on a
#   health response, tempting a caller to use it as a liveness criterion.
#   **Why it is on the read surface**: §4.3 assigns the recovery decision to the caller
#   (dead-recoverable → resume; resource-terminated → do not resume), so the caller must be
#   able to read the fact it decides on directly, rather than sending a resume that is
#   certain to be refused and inferring backwards from a 409 error string.
#   The value is §4.3's **abstract enumeration**, never an errno / cgroup path — a remote
#   provider cannot produce host primitives (§5.A).
PATCH  /v1/sandboxes/{id}            {cpu_millis?, memory_byte?, disk_byte?, idle_timeout?, retention?,
                                      volumes?, network?}
                                     → {sandbox_id, state, applies_at, lease_released?}
#   Edits the Binding; the effective point and which states allow the edit are in §3.4.
#   **The wire semantics differ from create's**: here "not given" and "given as 0" must be
#   distinguishable (a cap's 0 = no ceiling, `retention: 0` = never evict), so request fields
#   are **nullable** — not given = leave that field alone. TTLs take a duration string
#   ("48h"). `applies_at` ∈ {"now" (only TTLs changed), "next-resume" (any resource ceiling
#   **or the mount binding or the network declaration** changed)}.
#   **`volumes` is the mount table's complete binding** (each item {volume_id, path,
#   access}); §6.2 already designates PATCH as the rebinding point, so it is listed here
#   rather than in a separate attach/detach endpoint. **Its effective point splits in two,
#   and one `applies_at` value cannot answer for both**: a mount-topology change materializes
#   only at **next-resume** (runsc takes mounts at startup, §5.C does not hot-mount), whereas
#   **detach releases the exclusive lease immediately** (the lease IS that mount row, and
#   deleting the row releases it). So a detach responds with `applies_at: "next-resume"`
#   **and `lease_released: true`** — the two things being out of step is a design fact, not a
#   detail that can be folded into one value. Attaching to an already-occupied volume → 409
#   (§5.C's atomic insert returning `RowsAffected()==0`).
#   Changing a cap on a `running` sandbox → 409 "pause first"; raising a ceiling re-runs the
#   admission gate (may 507 / 400). No §4.1 event is emitted (this is not a state
#   transition).
#   **`disk_byte` going `0 → N` is refused (400)**: an uncapped sandbox's upper has no
#   writable quota row, so this write would declare a cap that no placement can ever deliver,
#   and accepting it would turn the sandbox into a brick (the full reasoning, and why the
#   refusal is at the write point, are in §3.4).
#   `N → 0` (removing the cap) and `N → M` (changing it) are both legal, but **`N → M`
#   lowered below current usage → 400** (a strict inequality, the same criterion as
#   `PATCH /v1/volumes/{id}`): the substrate would accept such a cap and the unit would then
#   be irreversibly wedged, and the sandbox side is worse — the cap is replayed on every
#   placement, so that sandbox can never resume again. `cap == usage` means **full** and is
#   refused too. **Cap-raising is checked as well**: once a quota row has been cleared
#   externally, usage can exceed the cap in the record, so even a raise "up to M" can land
#   below usage. If usage cannot be read → refuse (fail closed). **The same criterion is a
#   409 rather than a 400 on the placement side**: resume / adopt carry no cap, so a 400
#   would send the caller to change something they did not submit this time — the same split
#   as the `via[]` pair of "400 at the write point / 409 at placement" (§3.4 / §5.B).
#   **`network` is likewise whole-for-whole** (a §5.D declaration is a Binding), effective at
#   `next-resume`, so the same rule as resource ceilings applies: editing it on a `running`
#   sandbox → 409 "pause first". **publish does not go through here** — it has its own
#   hot-add endpoint (see the Network block).
#   **So editing `network` must be read-modify-write: GET `…/network` first, edit the copy
#   you read back, then write the whole thing.** Two write points write the same declaration
#   while PATCH replaces it wholesale: writing back a declaration read too long ago
#   **silently drops** any publish hot-added in the meantime. The engine does no field-level
#   merge and offers no optimistic-concurrency stamp to detect such a conflict; the last
#   PATCH is the result — a lost update on a security-relevant field, which is why that GET
#   on the caller's side is a contract requirement rather than advice. **There is no
#   un-publish endpoint today**: withdrawing a publish is a whole `PATCH` (omitting that
#   item), effective at `next-resume` per the Binding rule.
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
#   A resource-terminated sandbox refuses resume → 409 (§4.3; PATCH the cap up first)
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                        (list; each item has the same shape as the
                                             single read, `death_cause?` included)
#   list carrying the cause of death too **saves an N+1 round**: §6.2 is entirely
#   ID-addressed, so without it a caller seeing several `paused` sandboxes would have to GET
#   each one to learn which are resumable — a fact the list already holds.
GET       /v1/sandboxes/{id}/files?path=                        (large-file download, streaming)
PUT|POST  /v1/sandboxes/{id}/files?path=                       → {path, bytes_written}(upload)
#   `POST` and `PUT` are synonymous here (an upload is not a dispute about idempotent-replace
#   semantics, and accepting both verbs saves the caller one adaptation).
#   This is §5.E fs's large-file bypass (D4.4), the one data-plane operation that goes over
#   REST; small payloads still go through `FsService.Read/Write`. See below.

# Event-stream replay (§4.1 transition events / §4.4-C outbox); **pull only, no push surface**
GET    /v1/events?after=<seq>&limit=<n>     → {events:[{seq,id,sandbox_id,kind,state,occurred_at}],
                                               earliest, latest}
#   `seq` is the **cursor**: pass the last `seq` you received back verbatim as the next
#   `after` to continue where you left off. `after` omitted/0 = from the start of the
#   retention window. **The ordering key is `seq`, not `occurred_at`** (the latter is not a
#   total order; reasoning in §4.4-C).
#   `limit` has a **server-side hard ceiling** (default 100, ceiling 1000), and exceeding it
#   **truncates to the ceiling** rather than being refused. A hard ceiling rather than only a
#   default, because the parameter is caller-controlled and giving only a default is giving
#   nothing. And **no trimming today ⟹ the window = all of history**, so the rows `after=0`
#   must page through only ever grow — not trimming **aggravates** the unbounded-backlog risk
#   rather than relieving it, which is the real reason the hard ceiling is mandatory. (How
#   large the window should be is still open: see §4.4-C /
#   [`open-decisions.md`](./open-decisions.md) §2 there, "§4.4-C the size of the replayable
#   event-stream window". Once trimming is added, the caller contract does not change — see
#   the 410 note below.)
#   `{earliest, latest}` are the retention window's boundaries (0/0 when the outbox is empty).
#   **410 is reserved semantics, unreachable today and not implemented**: no trimming ⟹
#   `earliest` never advances, so the state "`after` is earlier than the window" cannot be
#   constructed. It is written into the contract so that **when trimming is genuinely added
#   the caller contract needs no change** — at that point `after < earliest` is a 410 and the
#   caller must restart from `earliest`. **No code is written for an unreachable state**
#   (§1.3).
#   Replay is **by the retention window, not by delivery state**: whether the engine's own
#   sink has seen some row is not a fact an external subscriber should depend on.
#   **Why it is in the control plane**: a cursor-paginated GET is just CRUD, and an
#   engine-level event stream is neither per-sandbox nor per-pid, so the data plane has
#   nowhere to address it in the first place (§6.2). **Why it is pull-only**, and why push is
#   a zero-cost adapter: §4.4-C.

# Template (an immutable artifact: an opaque ID is its identity, and existence is its state,
# §3.1 / §4.3)
GET    /v1/templates
POST   /v1/templates                 {from_sandbox_id}  (Promote, §3.1)         → {template_id}
#   The source must be `running` or `paused` (otherwise 409). A `running` source is quiesced
#   to `paused` and then frozen (emitting an ordinary `paused` event as usual); a `paused`
#   source is frozen in place. It passes the §3.4 admission gate (an insufficient cost
#   forecast → 507), and the gate is **before** quiescing, so a refused Promote is not an
#   observable transition.
DELETE /v1/templates/{id}            (referenced by a sandbox / a child layer → 409 template
                                      in use; reference-reclamation atomicity in §4.3)

# Volume (decoupled from any sandbox's lifecycle)
POST   /v1/volumes                   {size?}                                  → {volume_id, size}
#   **No `access_mode`**: that is a property of the mount row, not of the volume (one volume
#   can be shared-rw for A and shared-ro for C at the same time, §5.C), so it appears only in
#   the `volumes` items of create/PATCH sandbox. `size` is **required** when the disk axis is
#   metered (§3.4 / §6.2 per-axis conditional-required), otherwise 400.
#   **No `backend` request field**: the backend is a single implementation injected at
#   deployment time (the same shape as store / runtime provider — §1.3 registration rather
#   than selection), so it appears only in the **response**, never as a request key.
GET    /v1/volumes                                                                 (list)
GET    /v1/volumes/{id}              → {volume_id, size, backend?, used_byte?}     (inspect)
#   `size` is **the cap in the record** (the sole authority: reading back from the backend
#   cannot distinguish "never capped" from "deliberately uncapped", volume.md D7.3);
#   `used_byte` is the used bytes as the backend reports them, and **the key is absent when it
#   cannot be read** (not reported as 0 — a 0 would be read as "empty", while the caller wants
#   to use it to pick a new cap strictly greater than it). list does not carry `used_byte` (one
#   backend round trip per volume, multiplied by the number of volumes on a list).
PATCH  /v1/volumes/{id}              {size}  (**required**; it is the only editable field)
                                                                              → {volume_id, size}
#   The only write point for a volume's cap besides create, and `size` is **required**.
#   **Editable both when unmounted and while mounted**, so there is no "pause first" 409 here
#   — the opposite of sandbox resource ceilings, which are editable only in `paused`; the
#   mechanical reason is in §5.C. Raising the cap re-runs the admission gate (may 507);
#   **lowering it below current usage → 400** (a strict inequality, §5.C / volume.md D7.3).
#   Editing the cap is not governed by the write regime, but editing `access` has its own
#   rules (§5.C). No event is emitted.
DELETE /v1/volumes/{id}
#   Two kinds of 409 (the volume side of §6.2's ①②): a live mount row exists → 409
#   (referenced); **a live snapshot/clone (a child volume) exists → 409** (lineage RESTRICT,
#   the same shape as a Template referenced by a child layer). These are different object
#   relations, so the sentinel and the message must be distinguishable.
POST   /v1/volumes/{id}/snapshot     {size?}                                  → {volume_id, size}
POST   /v1/volumes/{id}/clone        {size?}                                  → {volume_id, size}
#   **Both take `size`, required when disk is metered (missing → 400), and neither inherits
#   the source's cap** — the substrate has nothing to inherit, and inheriting at the policy
#   layer would silently double the disk claim the node has already declared (§5.C /
#   volume.md D6.1a).
#   What they produce is an **ordinary writable volume** (with no read-only marker; for
#   immutability use a `shared-ro` mount row), whose record carries a `parent` pointing at the
#   source. The new volume's cap passes the admission gate as well (may 507). Backend does not
#   support it → 400, and the capability must be asked **before** an ID is allocated or a byte
#   is moved.
#   **A snapshot is crash-consistent, and no response field says so.** It is the backend's
#   atomic capture of the bytes on disk and **does not quiesce writers** (§5.C), so the result
#   is what a power cut would leave: measured, every capture is a complete prefix, and a file
#   just emptied by `O_TRUNC` and not yet rewritten can be captured **empty** — which is what
#   crash-consistency should give, not corruption. **So do not read a snapshot as a restorable
#   backup of a database inside the volume**: it is restorable exactly insofar as that
#   application recovers from a crash. For app-consistency the caller **composes** — pause the
#   holding sandbox, then snapshot (an exclusive volume only; `shared-rw` offers none, §5.C).
#   **Why no field annotates it**, so this is not re-proposed: it would be **constant** across
#   every snapshot the engine takes (there is one recipe — `Snapshot`/`Clone` never quiesce), and
#   in the one case that matters it would be **wrong** — the engine cannot know a caller paused
#   first, since the pause is a separate request and no linkage is recorded, so the field would
#   read "crash-consistent" on precisely the snapshots a careful caller took most care over.
#   A field the engine must sometimes lie in is worse than prose. It also has no home in §6.1's
#   omission rule, which governs **facts that cannot be read** — consistency is not unreadable,
#   it is not a fact about this volume at all. (The industry norm is the same: the block-layer
#   precedent puts "crash-consistent" in the *operation's* description with no consistency member
#   on the snapshot resource, and where a cloud does accept a consistency **request** it is
#   marked input-only and nothing is recorded about whether it worked.)
POST|PUT /v1/volumes/{id}/import     ?path=  (direct write, the body IS the byte stream;
                                              allowed only when there is no live-rw mount)
                                             → {volume_id, path, bytes_written}
GET    /v1/volumes/{id}/export       ?path=  (direct read)
#   **No sandbox needed** — this is where "a volume is decoupled from any sandbox's lifecycle"
#   is delivered: **seeding from nothing** (no sandbox exists yet) and **reading a volume after
#   the last sandbox is gone** both go through here. `path` is relative to the volume root, and
#   escaping it is unreachable (the seam's `OpenRoot` is root-confined, §5.C / D5.1).
#   A live-rw mount holds the volume → **409** (the write regime; a `paused` sandbox does
#   **not** count as a live writer, so it is allowed then — the predicate and the critical
#   section around the in-flight marker are in §5.C). Bytes stream; nothing is buffered whole.

# Network (the declaration is bound at create; changing it goes through the resume boundary;
# publish is the explicit ingress exception and can be hot-added)
GET    /v1/sandboxes/{id}/network    → {state, egress:{default, allow[], via[]}, ingress:{publish[]}}
#   Reads that declaration back (§5.D's four fields). **All three arrays are always present,
#   written as `[]` when empty — never omitted, never null**: they are determined values in the
#   record, whereas the omission rule for `death_cause` / `used_byte` governs **facts that
#   cannot be read**, and a declaration has no unreadable case.
#   Each `ingress.publish[]` item = {guest_port, proto, host_endpoint?}. **The `host_endpoint`
#   key is absent when the engine holds no live placement** (not an empty string, not null): an
#   endpoint is a product of the placement (§3.3), and a `paused` sandbox simply has no endpoint
#   to report. The reason is word for word the `death_cause` one — a falsy-but-present field
#   would tempt a caller to use it as the criterion for "is this publish in effect"; **and that
#   criterion is itself misplaced**: the rule set is installed atomically and whole with the
#   placement (§2.4), there is no per-publish installation state, so per-item presence would
#   just restate the same bit N times. An endpoint changes with the placement and must not be
#   cached as a stable identifier (§3.3).
#   **`state` is a member of this response**, taking the same value from the same store read as
#   `GET /v1/sandboxes/{id}`, sparing the caller a round of "having read it back, I must fetch
#   the state to interpret it" (the same N+1-saving criterion as list carrying `death_cause`):
#   **`running` ⟺ what is installed in the kernel IS the declaration in this response; not
#   `running` ⟹ nothing is installed.**
#   That biconditional is a mechanical consequence of §5.D + §3.4, not a new promise by this
#   endpoint: the declaration is regenerated from the current record on every placement, and
#   neither write point can make a `running` sandbox's record diverge from its live rule set
#   (editing the three egress fields on a `running` sandbox is a 409; publish makes "persist"
#   and "install the rules" one write that cannot half-complete). **So no durable difference
#   exists between "declared" and "in effect"**, and the only bit the read surface has to expose
#   is "is there a placement". **Hence no `in_effect` / `pending` / `applies_at` is added**: ①
#   `in_effect` is a second spelling of `state` and **overreaches** — the engine can honestly
#   report "I hold a placement", not "these rules really are in the kernel right now" (a
#   `running` record whose isolation unit has died but has not yet been demoted by §4.3 is the
#   counterexample); ② `pending` would require a second declaration in the record, while §6.2
#   states plainly that a binding has "no pending/unresolved values", and the `paused` one is
#   not pending — it IS the declaration; ③ `applies_at` is the vocabulary of a **write**
#   response, the read surface has no such edit, and forcing one in would only make it a
#   function of `state` while giving the same key different meanings at two endpoints.
#   **This response's body IS a legal `PATCH` request body** — the read-only members the read
#   surface emits (`state`, and `host_endpoint` in each `publish[]` item) are **accepted and
#   ignored** on the write surface. The reason is not convenience: the PATCH note above makes
#   read-modify-write a **contract requirement** (a lost update on a security-relevant field),
#   and all three write points reject unknown keys, so without this a caller writing the GET
#   response back verbatim would get a 400 for **echoing back keys the engine itself just
#   emitted** — putting all the friction on that load-bearing path and pushing callers toward
#   the road §5.D names as dangerous.
#   The rule is "**every read-only member the read surface emits**" rather than "`state`
#   alone": unknown-key rejection **applies recursively**, so missing the `host_endpoint`
#   nested inside `publish[]` would get a `running` sandbox-with-publish response body rejected
#   one level down.
#   **And that 400 must not be kept as an ordering guardrail either**: it appears only on the
#   ordering §5.D names as dangerous (in the safe order `pause → GET → PATCH → resume` the GET
#   comes after the pause, so with no placement no endpoint is emitted), which would amount to
#   reporting a concurrency-ordering requirement through a JSON schema error; that requirement
#   is carried in §5.D's prose.
#   **This is not a general exception to "reject unknown keys"**: only read-only members the
#   read surface produces itself are on the list, and a misspelled `netwrok` (or a misspelled
#   `host_endpiont` inside a publish item) is still a 400. If the read surface gains a
#   read-only member later, the write surface gains a correspondingly ignored one.
#   Non-GET → **405** (§6.2's general rule, not falling through to 404).
POST   /v1/sandboxes/{id}/publish    {guest_port, proto}
#                        → {guest_port, proto, host_endpoint?, applies_at}
#   **Hot-addable on a running sandbox** (the opposite of the three egress fields, which go
#   through the resume boundary; why those three properties endorse hot-add is in §5.D).
#   **Both `running` and `paused` are legal and both 200**, differing only in the effective
#   point: `running` installs the rule on the spot (`applies_at: "now"`), while `paused` only
#   persists it and the rule appears at the next placement (`applies_at: "next-resume"`, with
#   **no `host_endpoint` key**).
#   **The response follows `GET …/network`'s omission rule** — a write surface and a read
#   surface diverging on the same field is a contract defect. Why `paused` is legal, and why
#   "refusing `paused` would instead drive callers onto the less safe whole `PATCH`", are in
#   §5.D. `creating` / `destroyed` → 409 (the verb's source-state rule); an unknown ID → 404.
#   **Publishing the same `{guest_port, proto}` twice is idempotent**: return the existing item
#   and do not append a second one (why deduplication can only happen at the declaration layer,
#   and the consequences of missing it, are in §5.D). **No §4.1 event is emitted** (not a state
#   transition), and **`UpdatedAt` is not touched** (`retention` counts from that column).
#   `proto` accepts tcp and **udp** — the mechanical reason `via[]` excludes UDP does not hold
#   in the ingress direction (§6.2).
#   **It persists**: what is hot-added is a rule, what is written down is a declaration, so this
#   survives resume (a declaration is a Binding, and every placement reinstalls the rule set
#   from the current record, §5.D). It is therefore the **second write point** on
#   `ingress.publish[]` and interacts with PATCH's whole-for-whole replacement — the
#   read-modify-write requirement and "there is no un-publish endpoint" are in the PATCH note
#   above.
#   **There is no authentication key, and that is not an oversight** — what the response
#   delivers is a `host ip:port`, and whatever sits in front of it is outside the engine
#   (reasoning in §5.D).
#   **The guest side is required to bind `0.0.0.0`**: a sandbox has its own netns and its own
#   loopback, so a service bound to `127.0.0.1` cannot be published to — the contract is that
#   **the connection is refused and fails fast** (rather than hanging until timeout), and
#   **attributability is not promised** (why the cases are indistinguishable is in §5.D's first
#   of four user-visible semantics).
```

> **`network?`'s schema is exactly §5.D's four fields, all in a vocabulary of IP / port /
> protocol only** — `egress.default` (`deny|allow`, **required**) · `egress.allow[]` ·
> `egress.via[]` · `ingress.publish[]`. The tuple in all three places is `{cidr, port, proto}`,
> with `proto` **always required** and `cidr` accepting **IP literals only**. **Both `cidr` and
> `port` may be omitted, and omission means wildcard** (any destination / any port, §5.D),
> while **the read surface renders a wildcard as "omitted" rather than `"port": 0`** — that is
> what "the read surface's output must be a legal write body" (the PATCH note above) requires
> concretely on this field. The reasons behind the required fields and the narrowing are in §5.D
> (the mechanical side in network.md D2 / D3.1), and the **status codes** for violating them
> belong to §6.2's list. **There is no "posture" enumeration**: a posture is a combination of
> these four fields' values (§5.D).
> The declaration is a Binding (§3.4), so like the resource ceilings it is **editable only in
> `paused`**, through `PATCH /v1/sandboxes/{id}`, with `applies_at: "next-resume"`; **publish is
> the sole exception** and has its own hot-add endpoint (above).

> **Why the files bypass uses REST rather than RPC: its entire point is streaming.** A large
> file should not become one protobuf message, nor be resident whole at either end — the RPC
> path was measured to occupy about 37× more guest memory (transport D4.4), and a sandbox
> exceeding its RAM cap is terminated (§3.4), so this is a security rather than an efficiency
> trade-off. **The threshold is the caller's to set.** §6.2's constraints are unchanged: the
> sandbox is still addressed by an opaque ID, and the guest endpoint (the placement) never
> appears on the wire.

**Data plane (Connect-RPC; the authoritative wire is the proto, not the REST lines below)** —
process / fs have server-streaming plus strongly-typed frames, and **the authoritative contract
is `engine.transport.v1`'s `ProcessService` / `FsService`** (D4.3/D4.4). Clients generate stubs
from `transport.proto` via `buf` and are reverse-proxied through `orchestrator` to `sandboxd`
(D5.1). **Sandbox addressing travels in an out-of-band request header, `Engine-Sandbox-Id`**:
the proto itself addresses only by guest-native pid (D4.6 vocabulary boundary), and the reverse
proxy strips that header and resolves the current endpoint inside the engine (a placement never
leaks outward, §6.2 / D5.4). The REST lines below are **shape illustrations only** (showing which
operations exist and how pid/path addressing works) and **are not implementable authoritative
endpoints** — for the real wire, read the proto:

```text
# Process (connections) — start / reconnect / input / signal / list; PTY is the special case
# carrying a pty field, addressed by pid
#   Authoritative: ProcessService.{Start,Connect,StreamInput,SendInput,SendSignal,Update,List,CloseStdin}
(illustrative)  processes                     {cmd, args?, pty?, cwd?, env?}   → {pid, stream}
(illustrative)  processes/{pid}/connect                     → {stream} (reconnect: replay the
                                                             bounded ring buffer first, then
                                                             attach live, D4.3)
(illustrative)  processes/{pid}/signal        {signal}
(illustrative)  processes                                   (live list, no registry)

# FS (reading and writing sandbox files) — read may carry an offset; no watch. Semantics and
# effective points are in D4.4
#   Authoritative: FsService.{Read,Write,Stat,List,MakeDir,Remove,Move}
(illustrative)  fs?path=&offset=&len=                                          (read / stat)
(illustrative)  fs?path=                                                       (write)
(illustrative)  fs/list                       {path, depth}
# Agent session semantics do not enter the engine API — they are an addition made by an adapter
# outside the engine on top of process/fs (§5.E / D1.2)
```

### 6.2 Design constraints

- **ID addressing**: outwardly only the opaque IDs of sandbox / template / volume and process
  pids are recognized; host paths and placements are never exposed. The same requirement is
  carried through to the Overlay artifact inside the engine (§3.1.1).
- **Bindings are injected at create, and rebinding goes through the same injection point**:
  network and Volume mounts are bound once at create (there is no late binding); but the
  **flow shape** of **injecting through the control-plane channel** is not short-circuited, so
  wiring up a warm pool (late binding) later just reuses the same injection point.
  **Rebinding** goes through `PATCH /v1/sandboxes/{id}`, and a sandbox **holds one complete
  binding at every instant** — `PATCH` only replaces one complete binding with another
  (fields not given are unchanged, and there are **no "pending/unresolved" values**), with
  materialization still happening at placement time through the same control-plane injection
  point. This is **rebinding**, not **late binding** in §1.4's warm-pool sense (late binding =
  the sandbox exists first while its binding is undetermined), so it does not relax this
  constraint. **Guest identity is not among these**: guest root is the default, the engine does
  not model guest users, and neither create nor `Start` has a run-as parameter (§2.2 / §3.2.1).
- **Resource-ceiling fields are conditionally required per axis**: when an axis is metered by
  the node budget, create must give a value on that axis (`0 = no ceiling` is legal only on an
  unmetered axis); missing it is a refusal (400), which is a different thing from "the node is
  full" (507). This is **one of two** field semantics in §6.1 that depend on deployment
  configuration (the other is the network declaration's **deployment-natured 400** below:
  `via[]` meeting a deployment that gave no second-hop uid), so both are written out explicitly
  here rather than left to the implementation.
- **Status codes**: insufficient admission capacity → **507** (it speaks of "capacity", not of
  the caller-retryable state conflict a 409 means); a `resource-terminated` sandbox refusing
  resume → **409**; a missing cap on a metered axis / an illegal request body (a negative cap,
  an unparseable duration) → **400**; deleting a referenced Template → **409**; a verb with an
  illegal source state (changing a cap on a `running` sandbox, resuming a non-`paused` one) →
  **409**; an unknown sandbox / template ID → **404**.
  **Three further general-rule codes**: a write operation while a pause is draining (an
  fs-mutating RPC / a files upload) → **503** (the §4.2 barrier: refuse rather than queue, since
  queueing would starve the drain; retryable); a files upload exceeding the engine's
  single-request ceiling (8 GiB) → **413**; a wrong method on any collection/object route →
  **405**.
  **Success codes**: create / Promote / snapshot / clone → **201**; DELETE (sandbox / template /
  volume) → **204**; all other reads and verbs → **200**.
  **The Volume's four 409s mean different things and must be distinguishable**: ① deleting a
  volume that has a live mount row (referenced, same as the Template one); ② deleting **an
  object blocked by a live reference row** — which has **two sides**, the volume side (deleting
  a source that has live child volumes, lineage RESTRICT, §5.C snapshots) and the sandbox side
  (deleting a sandbox that has a live mount row, `mounts.sandbox` RESTRICT); ③ an attach
  colliding with an exclusive lease (a retryable state conflict); ④ a direct-access mutation
  while a live-rw mount exists (§5.C write regime).
  **The four count "outcome categories", not sentinels**: ②④ each subdivide internally, because
  **the same category can be triggered from two sides and the two sides must name different
  objects** — ②'s two sides are two different FK paths (the volume side's lineage RESTRICT vs
  the sandbox side's `mounts.sandbox` RESTRICT), and ④'s two sides are the write regime's two
  directions (a direct-access mutation hitting a live rw mount vs a cold start wanting to
  rw-mount a volume that has a direct write in flight), so each has its own sentinel — **six
  sentinels covering four categories**. "The four do not grow" constrains **the kinds of
  semantics a caller must tell apart**, not the number of error values in the implementation;
  a new trigger path that lands in an existing category adds a sentinel without adding a
  category.
  On the store side ①②③ are two different classes of constraint violation (FK-RESTRICT vs a
  uniqueness/predicate mismatch), so the **translation point must give each its own sentinel**
  and must not share one "in use" message — the existing `ErrInUse` message reads "template in
  use", and reusing it would return a 409 that names the wrong object.
  **One more thing does not belong to these four**: a verb with an illegal source state
  (changing a cap on a `running` sandbox, resuming a non-`paused` one, or rebinding from an
  illegal source state) is already a 409 under the general rule above; it is **the verb's
  source-state rule**, not a Volume reference relation, so it does not enter this enumeration.
  **Idempotent deletion must not swallow these 409s**: they are the **entire** outward signal a
  RESTRICT triggers, and were they to fall into the "return success even when absent" branch, a
  deletion that failed its guard would be indistinguishable from one that succeeded (an external
  cautionary example is in [`deep-dives/volume.md`](./deep-dives/volume.md) D9).
  **The network declaration's nine classes of 400 + one deployment 400** (§5.D. **All nine
  declaration 400s** are "decidable without reading another row, and no retry can make it
  legal", the same criterion as the mount binding's; **the first class is evaluated only when
  the `network` object is present — omitting the whole thing is legal and is normalized to
  `{deny, [], [], []}` and persisted**, §5.D):
  `egress.default` missing · a domain name given where a `cidr` belongs · a tuple missing
  `proto` · `via[].match` matching UDP · **`via[].endpoint` not host-local** ·
  **`via[].endpoint` falling inside one of the L0 floor's daddr list items** (that is,
  link-local / loopback / the sibling CIDR) · **an `egress.allow[]` `cidr` falling entirely
  inside a floor item that "no reading can lift"** · **a v4-mapped / v4-compatible v6 literal
  given where a `cidr` belongs** · **well-formedness: the declaration object does not stand up
  on its own** (independent of any value in it) — a `via[]` entry whose `match[]` is **empty**,
  or a `publish[]` entry whose `guest_port` is **0**.
  **One further 400 does not belong to these nine and is deployment-natured rather than a
  property of the declaration text**: a `via[]` declaration meeting **a deployment that gave no
  second-hop uid** (see "One more network 400 does not belong to the list above" below).
  **Why the ninth class (well-formedness) is its own class rather than a few more error values
  under the third**: the criterion is the Volume one above — **a class count constrains "the
  kinds of semantics a caller must tell apart", not the number of error values in the
  implementation**. The third class's remedy is "write `proto` into this tuple" (one field, one
  edit, and the error names that field); these two instead require the caller to **add a member
  to an array** (`match[]` has no entries at all, so there is no tuple for `proto` to be missing
  from) or to **supply a port the request never mentioned**. Reporting them as "a tuple is
  missing `proto`" would send the caller to inspect a field that may be written perfectly
  correctly, or may not exist yet. **And these two merge into one class with each other**: they
  are the same kind of semantics ("the shape does not stand up") with the same kind of remedy
  (supply a structurally required member), and splitting them would be counting error values in
  the implementation. **What is NOT a well-formedness failure is "the tuple did not write
  `port`"** — that is **legal and means any port** (§5.D): omission-as-wildcard is a declared
  semantics rather than a missing member, whereas an empty `match[]` steers no traffic under any
  reading.
  Of the first six, classes five and six clamp the endpoint to "host-local, and not inside one of
  the floor's **daddr list items**" (**"host-local" necessarily falls inside the floor's "any
  address of the host itself" item, and that is not a contradiction**: the hole is opened by
  ordering and the floor is not narrowed — the boundary passage in §2.4). The two are two faces of
  one criterion: the second hop's rules are held by the kernel behind the same host rule set, and
  the kernel can only hold local processes down — accepting a remote endpoint would stand the floor
  on that process's own self-discipline (a confused deputy, exactly what §2.4's second obligation
  refuses); while an endpoint falling inside the floor is a direct consequence of "a declaration
  cannot lift the floor", making every matching flow **permanently fail-closed**. Both are
  **declarations provably unable to ever work**, so they are refused on the spot at the write point
  (reasoning in network.md D4.2 / D4.3). **The sixth class reads only the floor's daddr list items,
  not the "host itself" item**: the latter is **a ct-state rule rather than an address enumeration**,
  and counting it into this decision would let the two classes of 400 together clamp `via[]` down to
  nothing. That tension is resolved by §5.D's rule generator with a **narrow allowance** — sitting
  **above the floor's drop in the same chain**, opening the hole by **ordering** and **not narrowing
  the floor** (the §2.4 boundary passage; the shape and the counterexample for each omission are in
  network.md D5 ⑩) — and is not carried by these two classes of 400.
  **The uid used to hold the second hop down is an input handed to the engine at deployment time,
  not a declaration field** (§5.D).

  **One more network 400 does not belong to the list above: it is a property of the deployment, not
  of the declaration text** — **a declaration carrying `via[]` meeting a deployment that gave no
  second-hop uid is refused (400)**. Every class above is decidable by reading the declaration text
  alone, whereas this one refuses even a declaration that is **legal word for word**.
  **Why it must be refused**: the second hop is held down by an `output` table narrowed by the
  deployment-time uid (§2.4 / network.md D5 ㉔), and with no uid there is no such table and therefore
  **no enforcement of §2.4's second invariant**. Accepting it would persist a declaration whose own
  invariant the engine cannot deliver, leaving an unheld confused deputy on the host — and **every
  assertion about the first hop is completely insensitive to it** (measured: the second hop counts 0
  on `forward`, and the first hop's floor does not evaluate a single one of its packets). The
  criterion has the same shape as the list above — decidable without reading another row, no retry
  can make it legal, and nothing is full so a 507 would be a lie — but **it must be distinguishable
  from those classes**, because **the remedy differs: this one's remedy is the operator's (add the
  second-hop uid to the engine's startup inputs), not the caller's** (editing the declaration cannot
  move it). So it carries its own sentinel.
  It is **not merged into the `via[].endpoint`-not-host-local class**: that one decides on the
  endpoint address, whereas this one is independent of the endpoint's value.

  **The same operational condition has a placement-side twin, and that one is a 409 rather than a
  400**: an **already-persisted** `via[]` declaration meeting a deployment with no second-hop uid on
  **resume or publish** → **409**. The difference is not in the criterion (word for word the same as
  above) but in **what the caller submitted this time**: resume and publish **carry no declaration**,
  so a 400 would send the caller to change something they did not submit. Both recorded remedies
  change state **elsewhere** (restart the engine with the uid, or `PATCH` that `via[]` away), which
  is precisely what 409 — "a state conflict, retryable" — means: the persisted record conflicts with
  the running deployment. **It must not be flattened into a 500**: a 500 says "the engine failed,
  please retry", whereas here nothing is broken and retrying will never help.

  **The seventh class covers `egress.allow[]` only, and recognizes only the floor's loopback and
  link-local items** (`127.0.0.0/8` · `::1/128` · `169.254.0.0/16` · `fe80::/10`). The criterion is
  **whole-range containment** rather than intersection: that the floor sits above the declaration is
  contractual (see the two ordering rules below), so an `allow[]` falling entirely inside these items
  is a **dead rule that is never evaluated** — measured, its own counter stays 0 while the floor's
  counter climbs (network.md D5 ㉑). The cost of accepting it is not "one more harmless rule": it
  **manufactures a second crowd of legitimate zeros on the one health signal the floor has, "the
  counter is 0"**, and every deny acceptance test reads that signal. So it shares its criterion with
  "an uncapped sandbox cannot be given a disk cap" — **a declaration provably undeliverable is
  refused on the spot at the write point**. **`port` / `proto` do not participate in the decision**:
  the floor's daddr items carry no port qualifier, so an entry contained by address is dead on every
  port. **This class deliberately does not cover three things**:
  ① **Entries inside the sibling CIDR** — the generator emits the declaration's allowances on
  **both `input` and `forward`** (network.md D4.2), so an `allow[]` falling inside the sibling CIDR
  is **not a dead rule**: this sandbox's own `/30` gateway is inside that CIDR, and that gateway is
  exactly the host-local resolver address §5.D requires the caller to declare themselves (the only
  stably usable one, D5 ⑦) — that is, **this is the sanctioned declaration path, not an escapee**.
  The half that reaches a sibling's **own** gateway is caught by a sibling drop on the `input` side
  (also in D4.2), which is the responsibility of rule ordering, not of this class of 400. **A
  separate, independent reason**: a 400 mentioning the sibling CIDR would put the sandbox address
  space on the wire, which this section's ID addressing explicitly excludes. ② **Entries naming a
  host-local address** — deciding that would require enumerating host addresses, whereas the floor's
  "any address of the host itself" is **a pure ct-state rule that is never computed** (§2.4 / D5 ⑧);
  enumerating it would invite back the class of problem D8 has settled, would fail "no retry can
  make it legal" (the host gaining an address afterwards flips it), and would also make **legitimate
  private-network supernets undeclarable**. ③ **Partial overlap** — `10.0.0.0/8` necessarily
  intersects the in-floor sandbox `/16`, and the measured ordering resolves that correctly (D5 ㉑),
  so the criterion **must be whole-range containment**.
  **`via[].match[]` is never decided under this class**: measured, steered traffic goes through
  `prerouting` DNAT **before** the filter floor, and once in `input` it is caught by that narrow
  allowance from D5 ⑩ — that is, **a `via[]` to an in-floor destination is live** (D5 ㉑).
  Generalizing this class to `via[].match[]` would kill the "hand an in-floor destination to a
  process outside the engine to judge" usage, which is one of `via[]`'s primary use cases.
  **The two fields being decided asymmetrically here is a mechanical fact, not an oversight.**

  **The eighth class is a necessary precondition for the seventh, not independent fastidiousness.**
  A containment decision that branches by family would judge a v4-mapped literal to be v6, compare
  it only against the v6 members, and **let it through** — while the packet leaves as IPv4 and dies
  on the v4 floor (the measured unwrapping behavior and the step-by-step derivation are in
  network.md D5 ⑲, **not duplicated here**). **The API accepting it while the wire silently refuses
  it** is exactly the class of diagnostic mismatch D4.3 describes. So such literals are refused
  **before any containment decision**, rather than "normalized and then compared": §5.D says a
  declaration accepts IP literals only, `::ffff:192.168.1.1` is **a second spelling** of an address
  that already has one, and accepting it would mean the persisted Binding differs from the caller's
  text, while the two spellings compare unequal on every subsequent read-modify-write.
  **At the same time** the predicate still calls `.Unmap()` internally, so that if someone withdraws
  this refusal later, the containment decision does not silently become family-blind.
  **A publish bound to loopback is not on this list** — that is not an illegal request but a legal
  request under which the guest-side connection is refused (§5.D).
  **`proto: udp` in `ingress.publish[]` is legal and not a 400**: `via[]` excludes UDP because the
  original destination cannot be recovered (network.md D3.2), and an ingress rule that DNATs to a
  fixed guest port needs no such mechanism, so that objection does not apply.
  **When one flow matches both `egress.allow[]` and some `via[].match[]`, `via[]` wins** — the two
  are observably different outcomes (steered vs let straight out), and for a declaration that both
  allows and steers the same flow, the most reasonable reading is "steer it away"; if it really
  should go straight out, exclude it from `via[].match[]` rather than covering it with an added
  `allow[]`. **When several `via[]` entries overlap, first-match-wins in declaration order** and
  later ones are not evaluated. Rule order is a security property rather than a matter of style
  (network.md D1 ④), so these two orderings must be pinned in the contract rather than left to the
  implementation: the same declaration must produce the same reachable set under two
  implementations.
  **Validation happens before the admission gate**: an illegal declaration is refused before a byte
  is moved or a budget is recorded, so 400 and 507 cannot both hold — a request is either 400 for an
  illegal declaration, or the declaration is legal and it is 507 because the node cannot fit it.
  `size` missing on a metered disk axis (in all three of create / snapshot / clone) → **400**; a
  volume cap lowered below current usage → **400**; a mount binding self-contradictory internally (a
  duplicate dest within one sandbox / the same volume twice / volumes nested in each other) →
  **400** (§5.C: decidable without reading another row, and no retry can make it legal); the backend
  not supporting snapshot/clone → **400** (the result of a capability query, not silent, and it must
  be asked before a byte is moved).
  **`GET /v1/events`**: normal (an empty window and an empty page included) → **200**; `after` or
  `limit` not an integer / `after` negative / `limit` ≤ 0 → **400** (`limit` **exceeding** the
  ceiling is not an error — it truncates to the ceiling and is still 200); non-GET → **405**;
  **410 = `after` earlier than the retention window**: **reserved semantics**, unreachable today and
  not implemented (reasoning in §6.1).
- **store / runtime provider go through interfaces**: swapping a backend or adding a provider means
  registering a new implementation, not changing the model above it (§1.3 additive only; a provider
  is always an abstract contract, see §1.1 / §5.A).
