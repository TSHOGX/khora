# Reaper deep dive: how health is judged, how the reaper acts on it, and how periodic reconciliation coexists with the transition lock

> **Position: the reaper and health internals reference.** It covers the health and
> reclamation model, periodic reconciliation and the activity signal's carrier.
> The architecture reference 4.3 fixes the shape "two probe layers + connection activity → a health state → dispositions
> that all differ", and §4.4-A fixes "reverse reconciliation = one local scan diffed against one local
> table + dual reclaim/takeover"; this document is the **mechanism layer** for both — six mechanisms,
> each with one correct home (the six rows below): **where the activity signal comes from (D1/D2) · how
> the health classifier coexists with the transition lock, including the classify→act activity race
> (D3) · the two differences between periodic and startup reconciliation (D4) · transport-down's
> convergence boundary (D4b) · how the cause of death is carried out with the classification result
> (D4c).**
>
> **Numbering convention:** this document's own sections use a `D` prefix; `§` always means a global
> spec section, and the two are never mixed.

---

## D0. The conclusion in one sentence

The reaper is **not an independent collector** but **a consumer of the health model** (§4.3): it first
decides each sandbox's current health state, then calls the verbs the §4 mediator already has (the
reaper variants of `pause`/`destroy`) according to that state. The only new machinery it introduces is
a **periodic background loop** — the engine's first actor that advances the state machine autonomously
rather than on an external API call. Six mechanisms, each with one correct home:

| Mechanism | Conclusion | Lands in |
| --- | --- | --- |
| The connection-activity signal | **A guest-side, sandbox-global "last I/O" timestamp**, with the idle duration carried back by the `Health` probe; when L2 is unreachable it cannot be read and must never be misjudged as idle | D2 / §5.E |
| The classifier vs concurrent verbs | The classifier is read-only and changes no state, but runs inside the **same non-blocking try-lock** as the disposition; if `TryLock` fails, skip (something is being built or transitioned — do not judge) | D3 / §4.4-B |
| The classify→act activity race | The lock guards engine-side transitions, not the guest's I/O: the sandbox may become active between the two probes, and activity is not lifecycle-state so a CAS cannot compare against it; hence `auto-pause` **re-reads activity once more inside the lock** and freezes only if it is still idle | D3 / §4.4-B |
| transport-down convergence | A transient waits for the restart, but **continuously exceeding the restart budget → escalate to dead-recoverable and take a resume**, guarding against being stuck in a crash loop | D4b / §4.3 |
| How the cause of death travels from classification to disposition | The classifier is read-only and **discards** the placement, so the cause of death must be **carried out together with the classification result** rather than letting the disposition ask again | D4c / §4.3 |
| Periodic reconciliation | **The same reverse reconciliation** as startup's, plus exactly two things: skip locked sandboxes, protect live placements in the live-map | D4 / §4.4-A |

---

## D1. Why the activity signal must be measured on the guest side and not the host side

§4.3's criterion for the `idle` health state is "**all** connections are idle — no process activity, no
pty I/O". The natural temptation is to measure it on the host side: the orchestrator already proxies the
entire data plane (`processProxy`/`fsProxy`, transport D5), so it could stamp a timestamp as bytes flow
past, with **zero guest changes and no process semantics touching the transport layer**.

**That is wrong and would misjudge a working agent as idle and freeze it.** The root cause is §3.2's
detached process model: `sandboxd`'s `proc.run` **continuously** pumps process output into a per-process
ring buffer **whether or not a client is attached** — which is the very precondition for "a process
outlives its Start stream and can be reattached later with `Connect`". So an agent can **run for
minutes producing prolific output while zero bytes flow past the orchestrator** (nobody is attached). A
host-side timestamp would read that as idle and `auto-pause` a sandbox that is working. This is a
correctness bug at the level of idle's definition, not something tuning can rescue. Whereas the same
activity is precisely visible on the guest side — every output frame passes `proc.broadcast()` and every
input passes `writeInput()` — where a timestamp is very cheap on paths that **already exist**. So the
signal is measured on the guest side.

**Why I/O and not CPU.** The architecture reference 4.3 words its criterion as I/O activity (`process.List has process
activity / the pty has I/O`), not "there is CPU occupancy". A purely CPU-bound process with zero I/O
(very rare in agent workloads) does not count as activity — that is a choice of §4.3's idle definition
itself; should it ever prove necessary, upgrading to "CPU+I/O sampling" is an additive change in §5.E
(healthServer reading one more `/proc/*/stat` jiffies value) and changes no other conclusion here.

---

## D2. The signal's shape: one sandbox-global timestamp, carried back by the `Health` probe

- **Granularity = sandbox-global, not per-pid.** The reaper's dispositions are **per sandbox**
  (auto-pausing the whole sandbox) rather than per connection; so all that is needed is "the moment of
  this sandbox's most recent I/O of any kind", the max over all processes + the pty. One process-shared
  atomic unix-millisecond timestamp, written once by `broadcast()` and once by `writeInput()`.
  Lock-free, O(1), and unbiased by the number of processes.
- **It rides `Health` rather than inventing another protocol.** The probe itself (one thing serving
  three purposes: the L2 probe / the resume readiness gate / the activity probe) is shaped in transport
  D4.5 and is not duplicated here. Only the activity purpose's shape is pinned here: `CheckResponse`
  carries `idle_millis` (= `now - lastIO`), **obtained in the same RPC as L2's reachable-or-not** —
  splitting it into two RPCs would be a pointless round trip. The host-side `Client.Health`'s domain
  type carries the idle duration back accordingly; cold start's ⑦ gate and the takeover L2 probe look
  only at err==nil and may ignore it.
- **It is still stateless (§5.E).** This timestamp and the output ring buffer are the same class of
  transient signal, and §5.E lists all five properties already; not restated.
- **"I/O" means every operation the engine mediates for the guest, not only process frames.** The
  criterion is "this sandbox is doing something", and the engine mediates two data planes, not one:
  `ProcessService` frames *and* `FsService` reads/writes/uploads. Stamping only the first makes a
  sandbox that is being actively written to look idle for as long as nobody types — a real
  misclassification, and its remedy (auto-pause) discards the very work in flight. So **`fs` writes
  stamp the same clock**, on the same argument that put the stamp in `broadcast()`: it is the path all
  such I/O already flows through, so no caller has to remember. **Reads stamp, `Stat`/`List` do
  not**, and that line is where the criterion actually gets decided: a read moves file contents, which
  is a guest being *used*, whereas stat-and-list are the calls an external watcher makes **in a loop**
  — stamping those would let an observer hold a sandbox active forever. Same reason the `Health` probe
  and the reaper's own reconciliation do not stamp: **an observation must never be indistinguishable
  from the thing observed** (D3's rule here, ③'s in §4.4-C), and a probe that stamped would make every
  sandbox permanently active, silently deleting the whole idle disposition rather than failing
  visibly. **Stamped on arrival, not on success** — the request arriving is the activity, exactly as
  `writeInput` stamps before its blocking write; stamping only successes would make a run of `EACCES`
  look like an idle sandbox.
- **An external caller may stamp it explicitly, and that is not the rejected keepalive.** §6.1's
  `POST /v1/sandboxes/{id}/activity` records activity **now**, with no duration attached. The
  distinction is not a technicality — every rejected shape (pause-inhibit, keepalive, lease) carries
  **a protected period**, and a period is what forces the engine to hold per-caller state, arbitrate
  overlapping holds, and expire them; those obligations, not the intent, are what put those shapes
  outside §1.3. A bare stamp has none: it writes one timestamp the engine already owns, in the
  vocabulary `idle_timeout` already defines, and is indistinguishable afterwards from a keystroke.
  **What it does not do is make the signal host-side.** It goes over the transport and stamps the
  guest's clock, so D1 holds unchanged and so does this section's ordering rule: it is unavailable
  exactly when a `Health` probe would be, which is the property that keeps "activity could not be
  probed" from ever becoming "idle". A host-side stamp would break precisely that, by producing an
  activity reading for a sandbox nobody can reach.
  **Why an endpoint at all, given a caller could write to stdin.** Because that is the only other
  externally repeatable, side-effect-free stamp point (the two stdin-close paths are one-shot and
  irreversible; the output path is not the caller's to trigger, and the case that needs this — a
  sandbox waiting on something slow — is by definition producing no output). Reaching it means
  injecting bytes into the pipe the guest reads its own protocol on, so the reaper's idle criterion
  becomes a constraint on **every guest protocol** ("must tolerate arbitrary ignorable input"). That
  is a coupling this document's D1 already refuses in its other direction, and one endpoint removes it.
- **`idle_millis` is meaningful only when L2 is reachable — an unreachable L2 must never be read as
  idle.** The activity timestamp rides the `Health` response, so `idle_millis` is obtainable only when
  the probe **returns successfully**; when L2 is unreachable (transport-down / mid-cold-start / dead)
  there is no trustworthy activity reading at all.
  The classifier therefore keeps a strict order: **only the branch where L2 is reachable (which implies
  L1 is alive, §4.3) reads `idle_millis` to separate healthy from idle** — the `idle` health state's
  criterion already includes "L2 reachable" (§4.3's table). So "activity could not be probed" always
  belongs to transport-down / dead-recoverable (separated by L1), and **never becomes a misjudgment as
  idle leading to an auto-pause** just because a timestamp could not be read.

---

## D3. The classifier and concurrency: a read-only probe running inside the disposition's own lock, with locked sandboxes skipped

**The health classifier is itself read-only and changes no state** (it acquires no lock), but the sweep
runs "classify → act" to completion **inside the same non-blocking try-lock**: having taken that
sandbox's transition lock it rereads the record, classifies, and acts according to the health state, all
in one critical section. What it does to a sandbox is: **L2 first** (`Health`, the same RPC carrying the
idle value back) — L2 reachable ⟹ L1 alive (§4.3), so it separates healthy / idle directly by
`idle_millis` and **skips the L1 probe entirely**; **only when L2 is unreachable does it add an L1
probe** (`Adopt`), used to separate transport-down (L1 alive) from dead-recoverable (L1 dead). The
disposition still goes through §4.4-B's CAS, except that it **need not take the lock again** — it is
already inside the hold the classification used, so **there is no window between decision and
disposition** and what the disposition sees is the in-lock view consistent with the criteria.

Two rules follow:

- **`TryLock`-skip locked sandboxes.** An in-flight resume **holds the transition lock throughout**
  (§4.4-B): the guest is not yet listening, L2 is necessarily unreachable, and a classifier running
  anyway would misjudge it as `dead-recoverable`, spend a resume attempt certain to fail, and raise a
  false alarm (the architecture reference 4.3 names this pit explicitly). So the sweep `TryLock`s each sandbox first: failing
  to get it = some verb is advancing it (being built / being transitioned) = **skip this round**,
  judging and acting on nothing. This is the architecture reference's single-flight guard, landed as "a non-blocking
  try-lock + skip", and it **adds no §4.1 state** (still four).
- **The lock is load-bearing for correctness, but what it guards is engine-side transitions, not the
  guest's I/O.** Two races: one is closed completely by the lock, the other is only narrowed:
  1. **A concurrent verb changed lifecycle-state** (a user `pause`/`resume`/`destroy` cutting in): this
     one **has no window** — they want the same lock, and a round that cannot get the lock is skipped
     outright; having taken the lock the sweep rereads the record first (letting go if the source state
     is no longer `running`), and the disposition then goes through the **CAS precondition check**
     (requiring source state `running`). The lock + the CAS catch it directly.
  2. **The sandbox has just become active** (the user sent input / a process resumed I/O): **this one
     the lock cannot close and a CAS cannot catch** — activity is a fact inside the guest rather than
     lifecycle-state, and holding an engine-side lock does not stop the guest from working. And between
     the classifying probe and the pre-disposition probe lies **one RPC round trip**, in which the
     sandbox can become active. So the architecture reference requires `auto-pause` to **re-read the activity signal once
     more inside the lock** (freezing only if it is still idle), as stated in §4.3; the window starts
     from "one round trip", and this section only adds what remains after that.
  - The residual sub-microsecond window (the user sending input exactly after the in-lock re-read and
    before runsc freezes) is harmless and ineliminable: it is indistinguishable from "the user sent
    input at the instant a pause was starting", is handled normally by §4.2's pause quiescent-point
    semantics, and does not count as a wrongful pause. §4.4-B's "the lock is load-bearing for
    correctness, single-flight only for efficiency" becomes precise here: the **state** inside the lock
    is authoritative, and only **activity** is a reading that can be momentarily "seen wrong", which is
    why the disposition judges it once more before committing.

**The cost of running classification inside the lock, and what it is not.** The intuitive objection is
"classifying under the lock makes the sweep contend with user verbs for a long time and turns the reaper
into a source of latency". That does not hold, because **the sweep never blocks**: it tries the lock and
skips the round on failure (previous rule). So the contention runs the other way — it is not the sweep
stuck behind a user verb, but **at most one user verb on that sandbox waiting one extra probe round
trip** (milliseconds normally, bounded by the probe timeout). Mistaking "how long a full scan of every
sandbox takes" for "how long the lock is held on one sandbox" is where that objection miscounts: the
hold is per sandbox and independent of the total number of sandboxes. What it buys is **no window
between decision and disposition** — the disposition need not add a compensating reread for "the state
may have changed", and the lifecycle-state race is closed rather than merely caught.

---

## D4. Periodic reconciliation = the same logic as startup reconciliation, plus exactly two things

Periodic reconciliation is still "one local scan diffed against one local table", sharing the body of
`Reconcile` with startup reconciliation. (The normative statement of the two differences is in §4.4-A.)
This section lands the four things §4.4-A asks of this document: trigger timing, coordination with the
transition lock, host resource enumeration, and batching and failure strategy.

**Periodic reconciliation vs the reaper loop: two consumers, one health criterion, and they may share a
tick.** The reaper acting on `idle`/`paused`-TTL is "the state is right but should advance"; periodic
reconciliation acts on "the state does not match reality" (the store says `running` while the unit is
dead → `crash-paused`; an orphan placement → reclaim). So in the implementation **two consumers run in
order within one periodic tick while only one of them classifies**: reconciliation goes first, reading
the live-map and acting on the placement side (orphan reclamation / takeover) without sending probes;
the sweep goes second and does that **one and only** classification, whose result feeds both the TTL
dispositions and crash-pause. Reconciliation runs first so that a `running` record whose unit is dead is
demoted before the sweep reaches the same conclusion — the two are idempotent, and the ordering only
saves one redundant judgment, so **probing does not happen twice**.

**The failure strategy follows §4.4-A: skip-and-log, and be conservative about an uncertain surviving
`running`** (protect its placement from reclamation — failing to tear down is reversible, tearing down
wrongly is not); reconciliation is idempotent and a skipped item comes round next time. **The batching
strategy**: judge sandbox by sandbox serially, scanning all of them in one round (single node, a bounded
number of sandboxes, which suffices); should the scale grow later, batching + rate limiting is additive
tuning and does not change this shape. **Host resource enumeration** covers runsc containers + netns +
overlay mounts + §5.D's nft rule sets (`ReclaimOrphans` enumerating the last by table-name prefix).
**The deletion order for the rule-set item is a hard constraint**: the keep set must be computed
**before** any deletion, or a reclamation ordered before `Adopt` would delete a live sandbox's table in
passing — and that is word for word a "delete first, then install", which would break live flows
(network.md D5 ㉒).

---

## D4b. transport-down's restart budget: the convergence boundary between a transient and a death

§4.3's table defines transport-down as a **transient** (L1 alive, L2 unreachable = the transport child
crashed and PID 1 is restarting it in place, §5.E.1), whose disposition is **wait for the restart**
rather than take a resume. But "wait" cannot be unbounded: a transport child in a **crash loop** (it
panics on binding the port, re-execs, and crashes again) never flips back to L2-reachable, and with
unbounded waiting that sandbox neither returns to healthy nor is ever auto-paused / crash-paused,
becoming a **wedged black hole**. So transport-down **must have a convergence boundary** — which is
exactly the mechanism §4.3's "escalate only after exceeding the restart budget" clause wants landed.

**The criterion = a bounded restart budget; the escalation direction = dead-recoverable.** Once the
duration for which a sandbox has been **continuously** classified transport-down (the criterion takes
duration rather than a count of PID 1 restarts — PID 1 need not report a count to the control plane)
exceeds the **restart budget**, the reaper **escalates** it from transport-down to a
`dead-recoverable` disposition: take a resume (§4.2), cold-starting the whole chain on a new IP. The
reason: an in-place transport restart is the fast path that "costs far less than a resume" (§4.3), but
**when the fast path does not converge, the only remaining means of recovering that sandbox is a
whole-chain cold start** — escalating to a resume steps back from "in-process recoverable" to
"substrate-level recovery", the same disposition as dead-recoverable, inventing no new state and no new
verb.

**This does not fight single-flight or the transition lock.** The escalated resume still goes through
§4.4-B's hold-throughout plus §4.3's single-flight guard (classification suspended while the lock is
held), exactly the same path as a resume triggered by dead-recoverable. The reaper here only **changes
which health state is acted on** (transport-down→dead-recoverable), and the disposition itself is not
special.

**The restart budget's value is pure tuning** (like idle_timeout / retention): a conservative default
(giving PID 1 a generous window of restarts, so one slow restart is not escalated into a cold start),
annotated at the configuration point, adjustable, and not affecting this convergence boundary's shape.

---

## D4c. The cause of death must be carried out with the classification result: the signal is volatile and the disposition cannot "ask again"

§4.3 defines `resource-terminated` and `dead-recoverable` as **peer** dispositions (the former does not
resume), and the two can be separated only by the **cause of death**. That gives the classifier a
property none of this document's other mechanisms has: **the cause-of-death signal is volatile** — it
lives in the provider's placement-local resources and disappears the moment any of
`Stop`/`Destroy`/`ReclaimOrphans` tears the placement down, and it does not survive a host restart. So
three constraints hold in a chain:

- **There is exactly one read point, at the instant of the L1 probe.** The classifier decides L1 is dead
  through `Adopt` (D3), and that is the last moment the placement is still intact. So `Adopt`'s "already
  dead" conclusion must **carry the cause of death out along with it** rather than letting the
  disposition verb look it up afterwards — afterwards the placement may already be torn down.
- **Carried out, not re-fetched.** The classifier is **read-only** and **discards** the placement handle
  it rebuilt once it has judged (D3). So the cause of death must be returned to the reaper together with
  the classification result, for the reaper to hand to the disposition verb; a classifier that reads it
  without carrying it out has read it and thrown it away.
- **Persisting it shares one commit with the demoting CAS.** Once the disposition verb has the cause of
  death, writing it must be atomic with `crash-paused`'s CAS (the same shape as §4.4-C) — after the CAS
  a teardown may run at any time, the evidence is unrecoverable, and there is no window for a
  fill-in-later write.

**Two measured facts compress the design space to a single solution:**

- **runsc's on-disk state holds no cause of death, so the world of "ask again afterwards" does not
  exist.** The durable `.state` under `--root=<dir>` does survive a host restart, but holds only
  identity + a snapshot from creation time: across four controlled cases (OOM / a normal exit 0 / an
  external SIGKILL / exit 42) the durable fields are **identical field for field** — no exit code, no
  signal, no OOM flag. More treacherously, the durable `status` for an already-dead container **still
  reads `running`** (written at creation and not updated at death), and the `stopped` that `runsc state`
  reports is computed **at read time** by probing pid liveness. **Do not trust that on-disk field**, and
  after a restart that pid is meaningless anyway.
- **The only precise `ResourceExceeded` signal is the memcg leaf's `oom_group_kill`, and it lives only
  until `runsc delete`.** After a whole-group kill, measured: `oom_group_kill 1` and `memory.peak`
  reaching exactly the cap; still readable while the container is `stopped` but not yet `delete`d.
  **Use `oom_group_kill`, not `oom_kill`**: the latter counts individual tasks within the group and
  measured 9 for the same single whole-group kill, so it cannot answer "was this sandbox killed because
  of its own RAM cap".

So the one read point lands just before `Adopt`'s `!alive` branch returns: `teardownPlacement`'s
`kill`→`delete` pair destroys both the memcg leaf and runsc's own container state under `--root`, and
all three death-discovery paths (startup reconciliation / periodic reconciliation / the reaper's
crash-pause) go through `Adopt`.
The mapping: `oom_group_kill > 0` → `ResourceExceeded`, otherwise `Gone`, and unreadable → `Unknown`.

> **There is a fourth teardown path, and it is not a discovery path: a user `pause` on a sandbox that is
> already dead.** `pause` does not probe, its CAS succeeds because the record still says `running`, and
> the subsequent `Stop` → teardown destroys the memcg along with the cause of death, while the cause
> `pause` passes is `nil` (it is not a death observation and should not fabricate one). So a sandbox that
> **really did hit its cap** may leave behind a record of "never died". This does not break §4.3 —
> `Unknown`'s structural self-correction exists for exactly this (§4.3) — but it is **a registered
> window** rather than an overlooked oversight.

**`Unknown`'s disposition and structural self-correction are in §4.3**; here we only add why its
alternatives do not hold.

**A persisted retry count has no derivable reset point (this is its real cause of death).** Every
**completed** `Resume` is a success (`resumeLocked` flips to `running` only after the ⑦ readiness gate),
and death arrives **afterwards**, asynchronously. So an "attempt count" can only be incremented at
crash-pause, making it in substance a **consecutive-death streak** that must span the successful resume
in the middle — and that success is the one positive signal the engine ever gets, where the code
**deliberately clears the cause of death** so stale state does not accumulate. That leaves three reset
points, none of which holds: reset on success ⇒ the count never exceeds 1 and measures nothing; never
reset ⇒ a sandbox that crashed legitimately five times over half a year is permanently bricked; reset on
a time window ⇒ that is **rate limiting**, which never refuses permanently, so it neither provides the
claimed bound nor contributes convergence. Besides, a per-sandbox monotonic count persisted into the
sandbox record **is the very counter §1.1 bars under another name** (barred precisely to "stop it
seeping back in from elsewhere").

**And: refusing an explicit verb by count is a different thing from refusing it by fact.** §4.3 has
already sanctioned one refusal, and its justification is specific: `resource-terminated` refuses on the
basis of **a persisted fact about this sandbox**, which has causal explanatory power and is remediable
in principle (raise the cap or destroy it). Refusing by count rests on no fact about the sandbox at all;
it says "you have tried enough times" — a judgment about **the caller's persistence**, which is
product-layer semantics, and strictly worse than handing the decision back to the caller (who may have
just fixed the workload, something the engine has no way to know).

---

## D5. Boundaries

- **Object-reclamation refcount/GC sweeps (§4.3's `uniform object model invariant`) are not in this
  document.** That is the reference-reclamation atomicity of Template lineage / Volume mount rows (§4.3
  + §4.4-C) and belongs to the object-reclamation sweep. The reaper's `evict` goes through `Destroy`'s
  **synchronous Overlay deletion** and **does not delete bytes asynchronously by refcount** — the reaper
  here only advances **lifecycle state**; bytes left behind by a failure are caught by the orphan-unit
  sweep in the same tick. The seam between the two (evict marking destroyed being decoupled from
  asynchronous byte deletion) is already fixed by the architecture reference 4.4.
- **The TTL values** (the default `idle_timeout` / `retention`) and **the restart budget's value**
  (D4b) are both numeric knobs rather than open items: per the same discipline as
  [`transport.md`](./transport.md) D5.4, take a conservative default, annotate it at the configuration
  point, and leave it adjustable — landing in code, not registered as an GitHub Issue.
  **`retention`'s default deserves one paragraph, because "conservative" points in two directions
  here and the comparison with `--event-retention` (default 0, on the rule that the engine must not
  invent a number) does not settle it.** The two defaults are not the same kind of number. `retention:
  0` is not "no policy" — it has a *meaning* in this engine, "never evict" (§6.1 PATCH) — so
  defaulting to it is not declining to invent a number, it is choosing the one that never reclaims:
  every paused sandbox's Overlay is retained forever, and the failure mode is §3.4's disk budget
  filling until unrelated creates take a 507. So both settings destroy something by default; the
  question is only whether it is a stale sandbox or an unrelated caller's ability to start one. A TTL
  is also the sole reclamation path here (§4.3's evict is the only route from `paused` to reclaimed),
  where the event window has an operator with a delete statement.
  **So the default stays a finite 24h, and the honest part is that the failure it accepts is
  irreversible**: a deployment that treats pause/resume as durable continuation loses that
  continuation when a sandbox sits paused for a day, and the symptom is silent — the fuel is simply
  gone. Hence the obligation this bullet's own discipline already states, *annotate it at the
  configuration point*, applied at the standard `--event-retention` sets: the flag says what expiry
  destroys, that it cannot be undone, and how to argue the value — from how long a paused sandbox must
  stay resumable, never from disk. A deployment that depends on long continuation sets it explicitly
  (`0` for never, which is the setting that means it), and the engine's job is to make that a decision
  someone makes rather than a default nobody read.
- **Enumerating and rebuilding §5.D's rule sets** is already folded into periodic reconciliation:
  `Adopt` reinstalls the whole set atomically and `ReclaimOrphans` reclaims by table-name prefix, with
  the ordering constraints on both in this document's D4 closing paragraph and network.md D5 ㉒.
