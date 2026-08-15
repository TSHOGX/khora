# The guest runtime boundary: what runs, who is PID 1, and how resume cold-starts

> **Position: the guest-runtime internals reference.** It develops the reasoning behind
> the guest startup topology, pause quiescence and probe layers described by the concepts pages.
> states only as conclusions: ① **scope** — the engine supports only "processes exec'd on demand" and
> does not target system daemon services (D1, D3); ② **the guest startup topology + the pause quiescent
> point + the resume cold-start timing** (D4, D5). The normative conclusions are in the guest transport / the pause/resume lifecycle /
> the health and recovery model.
>
> The `D` headings are local navigation labels, not external identifiers. Keep durable constraints
> in code comments and current concepts/API pages rather than relying on heading numbers.

---

## D0. The conclusion in one sentence

The engine runs exactly one class of thing — **processes exec'd on demand through `process`** — and does
not do system daemon services (D1, D3), so the guest needs only a **lightweight reaper**. This is
implemented as "one binary, two modes": `sandboxd` runs in **init mode** as PID 1 (a minimal init
written ourselves, with tini's source as the correctness checklist) and re-execs a **transport mode**
child as the process/fs endpoint (D4). This split-process topology buys crash isolation, plus the **two
probe layers** that separate transport-down from substrate death (D5).

---

## D1. Scope: the engine runs exactly one class of "thing"

There is one class of "running things" the engine must support: **processes brought up on demand through
`sandboxd`'s `process` capability** (a monitor / a build / an `apt|npm|pip` install / a dev server / an
artifact HTTP server). It does **not** target "system daemon services started at boot" (the
`service`/`systemctl` world). The criterion is clean: **does it need init to "start it at boot and keep
it resident"** — if yes → out of scope; if no, and it can be exec'd → the two existing primitives
`process` + `publish` already cover it.

The criterion lands on two classes of thing:

| | System daemon services (not done) | Processes exec'd on demand (the whole model) |
| --- | --- | --- |
| Typical | `service postgresql start` / `systemctl` | a monitor, `apt install`, a dev server, an artifact server |
| How it starts | init brings it up at boot and it stays resident as infrastructure | brought up through `sandboxd`'s `process` capability |

---

## D3. Both typical needs fall on the "exec on demand" side

### D3.1 `apt install`: a one-shot command that does not touch init

Installing a toolchain (Python / npm / Node / Rust …) and deploying a resident DB are two different
things. `apt`/`npm`/`pip`/cargo are all **one-shot commands** — exec, run, exit — rather than
`service xxx start` daemons, and they **do not touch init at all**. Entering the sandbox gives root by
default (the architecture reference: the engine defaults the guest to root and **does not model guest users**, with "run
as some user" being the guest's native `su`/`sudo`), so the privileges for installing dependencies are
satisfied naturally; and root inside the guest is intercepted in userspace by the sentry and cannot
escape to the host, so "let installs that need root run in there without worry" is exactly the gVisor
posture. The only prerequisite is networking: `apt`/`npm` reaching out depends on the egress allowances
in the network subsystem declaration (`egress.default: allow`, or one `allow[]` covering the upstream).

> What makes postgres awkward is not gVisor's syscall compatibility (postgres-on-gVisor does run) but
> "the container has no init system" plus the usual slim-image afflictions around locale, switching
> users, and so on. Not targeting a resident DB service means none of that string of pits is walked.

### D3.2 Serving HTML / artifacts: no new primitive needed

An artifact server is two steps, both on existing capabilities:

```
process.Start("python -m http.server 8000")     ← the transport subsystem's process capability
POST /v1/sandboxes/{id}/publish {guest_port:8000, proto:"tcp"}   ← the network subsystem's inbound publish (the
                                                                   engine delivers only a host ip:port,
                                                                   and the declaration has no
                                                                   authentication field)
```

listen/accept plus reading a file and sending a response are all within the gVisor netstack's support
(throughput is below host networking, which is fine for serving artifacts). The engine does not know
"this is an artifact server"; it sees only "a process + a published port". "Being an artifact" is
product-layer semantics and does not enter the engine (the architecture reference).

One detail: an artifact server **may be long-lived** (staying up while things are being viewed), which
does not conflict with "on demand" — "on demand" refers to **who starts it and how** (explicitly and
observably, through `process`), not to a short lifetime. A publish's lifecycle is independent of any
connection anyway.

---

## D4. The guest startup topology: who is PID 1

Two orthogonal questions: **Q1** who implements init semantics (write our own vs bundle tini), and
**Q2** whether init should share a process with `sandboxd`.

### D4.1 Q1: write the init semantics ourselves, using tini's source as the correctness checklist

**Write our own and bundle no tini binary; use tini's source as an executable reference and copy
its semantics item by item.** tini is the authoritative answer to "a minimal init has a pile of pits",
and the checklist is:

- the `waitpid(-1, …, WNOHANG)` reaping loop + handling `ECHILD`;
- signal forwarding (to the single child by default, to the whole process group with `-g`);
- **PID 1 has no default signal dispositions** — the kernel installs no default handlers for PID 1, so
  signals not handled explicitly (other than `SIGKILL`/`SIGSTOP`) are silently discarded and each must
  be installed explicitly;
- propagating exit code `128+signo`, `SA_RESTART` semantics, and subreaper interactions.

Writing our own rather than bundling: for the architecture reference's deployment shape of "one binary + a config + a
local store", **not stuffing a tini binary into every base rootfs** is a concrete gain — one fewer
moving part to version, track CVEs for, and bake into templates. The cost is that PID 1 correctness has
ugly failure modes (zombies piling up, signals lost, shutdown hanging), but it is a **bounded** problem
(~300 lines) with tini's checklist as a backstop.

### D4.2 Q2: init and `sandboxd` in separate processes, via "one binary, two modes"

init and `sandboxd` are **not merged into one process**, for two engineering reasons:

1. **Crash blast radius / restartability (the hardest reason).** `sandboxd` is the most complex,
   fastest-growing, most bug-prone part of the whole system (the wire protocol, stream multiplexing,
   pty, fs ops). Were it PID 1, one crash would **take the entire guest with it** — the user's running
   `apt` / dev server / build all gone. A separate ~300-line init that almost never changes can
   **restart `sandboxd` in place** without crashing the sandbox, downgrading a `sandboxd` crash to
   **in-process recoverable** (aligned with the architecture reference's "a crash = an involuntary pause") rather than
   promoting it to a substrate death. **That restart is crash recovery only and is not a path for
   changing versions**: `/proc/self/exe` is a handle on the inode PID 1 already opened rather than a
   path re-resolved at exec time, so the supervision loop brings up **the same bytes** every time —
   changing versions has exactly one road, "a new Template + a new placement".
2. **PID 1's signal semantics are hostile to a complex server** (D4.1). The Go runtime (multithreaded,
   with its own signal handling) layered on PID 1's special rules requires great care; letting a tiny
   init be PID 1 with `sandboxd` as an ordinary child sidesteps the problem entirely.

The merge motive "the reaper must know about the processes `sandboxd` started, so they have to be
together" is dissolved by a kernel feature: **`prctl(PR_SET_CHILD_SUBREAPER)`** lets `sandboxd` declare
itself a subreaper, after which orphans in the process tree it spawned reparent to `sandboxd` rather
than to PID 1 — so it can reap the descendants it cares about (having the context for them) without
being PID 1. PID 1 then acts purely as **a backstop**, reaping genuinely global orphans (a very small
set under an "on-demand processes only" model).

**The implementation shape: one binary, two modes** (init mode as PID 1 re-execing a transport-mode
child through `/proc/self/exe`), with the full topology diagram in the guest transport topology.

### D4.3 What transport mode must implement, how signal targets divide, and two shapes to refuse

Reaping semantics can be copied from nowhere — they must rest on D4.1's own implementation plus tini's
checklist. But the other three mechanisms transport mode must implement are mature process-management
patterns, shaped as follows:

- **Per-process supervision**: one monitor goroutine per process, broadcasting an `end` frame on exit
  (with exit code `128+signo` on a signal). Should finer process states ever need exposing to callers
  (running/stopped/killed/crashed), that is an additive addition on top of this monitor and does not
  change this topology.
  - **But its status must not come from `cmd.Wait()` — that cannot coexist with subreaper duty, and only
    one of the two can win.** Transport mode declares `PR_SET_CHILD_SUBREAPER` per the guest transport **and must
    therefore run `Wait4(-1)` itself**, and `Wait4(-1)` **cannot exclude a particular pid**: the
    wildcard waiter and a per-child `cmd.Wait()` compete for the same status, and whoever receives it
    first takes it. **Measured: when the wildcard waiter wins, `cmd.Wait()` returns
    `waitid: no child processes` 10 times out of 10** — so every `end` frame's exit code would be
    **fabricated**, while delivering exit codes is a load-bearing contract the guest transport states explicitly. So
    the shape is: **a single wildcard reaper + a handoff table** (pid → waiter), from which a monitor
    collects its own status, sharing **one mechanism** with PID 1's reaper (`reapTable`, used by both
    modes, the guest transport; it exists precisely so that "a wildcard waiter must not steal a registered child's
    status"). ("Declaring" and "installing the wildcard drain" cannot be implemented apart, the guest transport.)
  - **One non-obvious obligation comes with it**: skipping `cmd.Wait()` also skips its closing of the
    parent ends of `StdoutPipe`/`StderrPipe`, so a long-lived daemon leaks 2 fds per process — the
    output pump must close them itself at EOF. (The PTY master is the exception: it is also the input
    end and is closed uniformly by the process-termination path.)
- **Per-process pgid discipline**: non-PTY processes `Setpgid` into their own group while PTY processes
  are left to the pty's session — this PTY-vs-non-PTY asymmetry is not covered by tini, and the full
  reasoning is in D4.5 C1.
- **The drain barrier**: before a pause, a barrier blocks in-flight mutating writes and waits for them
  to drain — the primitive D4.4's host-side drain barrier needs (D5.2's single-flight guard is on the
  host side and does not use it).

**The shape to refuse is the "standalone counter" identity model** (common in distributed
init/supervisors: tagging each run with an `epoch`/`generation` label to recognize a stale instance).
The engine does not introduce it, using the architecture reference/the health and recovery model's placement/IP-as-epoch instead (the architecture reference's
"engine vocabulary boundary" names `generation` as the concept most likely to be brought back). A banned
word list stops the word, not the concept — the drain barrier's **draining logic** is wanted, and any
practice of hanging **a counter label** on it is not.

**Signal targets divide naturally**: directed signals (to one process / a process group, the architecture reference's
quiescent point, the transport subsystem's signal channel) belong to **transport mode** (which holds the pid/pgid handles);
container-level signal fan-out belongs to **PID 1** — its `kill(-1, sig)` broadcasts to **every process
in this pid namespace except itself**, so a long-running sibling the template ships is covered along
with the transport's whole subtree, not just the one child it spawned. The criterion is "specifying no
pid/pgid at all": precisely because it is undirected it lands on PID 1's side rather than the
transport's — the handles are for the directed road.

**That criterion is executable at `SendSignal`'s entry rather than merely a description of the division:
`pid <= 0` is judged `invalid_argument`.** In POSIX, `pid <= 0` **is** the fan-out form (`kill(0)` hits
the whole process group, `kill(-1)` the whole pid namespace), so it is not a "dangerous value" but **a
misdelivery to the wrong subsystem** — that road belongs to PID 1, and the transport side has no entry
for it. The validation lands on the **guest side** (`cmd/sandboxd`), because the guest transport's premise is that the
guest can write its own fs and must prove itself; an identically shaped check on the orchestrator side is
only fail-fast (saving one RPC) and is not authoritative. The type stays `int32`: `uint32` would silently
turn a negative value on the wire into a large positive one, trading an explicit 400 for a much
harder-to-trace behavior.

It **validates only pid > 0** and does not consult the process registry — `Connect`/`SendInput`/`Update`/
`CloseStdin` must consult it because they touch per-process state, whereas sending a signal needs only a
legal pid; requiring a registry hit would break the pairing the API contract states explicitly, "`List` gives you a
pid → `SendSignal` hits it" (`List` scans `/proc` live, and the pids it returns were never in that table
to begin with).

**What the validation stops — three values, measured inside a gVisor pid namespace (`SIGTERM`, sent
directly, bypassing the validation).** `killProcessGroup`'s two-level fallback (first `kill(-pid)`, then
`kill(pid)` on failure) turns hostile on both arms when `pid <= 0`:

| Input | What actually happens | Consequence |
| --- | --- | --- |
| `pid=-1` | `kill(-(-1))` = a **directed** `kill(1)`, hitting PID 1's own TERM handler | The sandbox is torn down (RPCs get `unexpected EOF`, Health never returns) |
| `pid=0` | `kill(-0)` = `kill(0)`, hitting sandboxd's own process group — and the transport's `pgrp` is **1**, the same as PID 1's (it is forked by PID 1 and does not `Setpgid`), so PID 1 is in that group | Same as above, the sandbox is torn down |
| `pid=1` | `kill(-1)`, a namespace broadcast. Linux's `kill(-1)` **excludes PID 1 and the caller itself**, so the sandbox survives but **every workload process is killed along the way** (measured: two unrelated processes `exit 143` simultaneously); with no workload at all in the guest that send returns `ESRCH`, falling to the fallback arm `kill(1)`, which becomes the `pid=-1` row — the sandbox is torn down | Fan-out beyond what was asked; degenerates into tearing the sandbox down in an empty guest |

the API contract assumes the caller is already trusted and the engine does no authorization, so what must be added is
**input validation**, not authorization.

**And the fan-out in the `pid=1` row is deliberately allowed: the validation stops only `pid <= 0`.** The
table above states both the mechanism and the consequence in full (a namespace broadcast kills every
workload process; in an empty guest `ESRCH` falls to the fallback arm and degenerates into tearing the
sandbox down). Two reasons: ① **`pid > 1` would break the `List` → `SendSignal` pairing the API contract states
explicitly** — `List` scans `/proc` live and necessarily returns PID 1, so the API would emit a pid it
refuses to accept, a contract defect harder to explain than the fan-out (an extension of the previous
paragraph's "does not consult the registry" criterion: there what is refused is a registry-hit
requirement, here it is cutting the same pairing off from the value side). ② The basis for allowing it is
that **PID 1 is a guest-native addressable object** — of the three rows above, `pid=-1` / `pid=0` are
fan-out **forms misdelivered to the wrong subsystem** (POSIX semantics, with no entry on the transport
side), whereas `1` is a legal directed pid whose fan-out is a consequence of Linux's `kill(-1)` semantics
and is **the caller's own responsibility**.

**No sandbox-level grace period — rejected.** After receiving TERM, PID 1 broadcasts and then
**immediately calls `os.Exit(0)`**, and PID 1 exiting tears down the pid namespace, so that one TERM
cannot in fact be handled and is no different from a direct KILL. **What is refused is not "grace was not
implemented" but that this shape does not fit:** ① what needs grace is a **workload process** — it is
what has something to flush — and process-directed signals already exist (that very `SendSignal` above),
so a caller who wants something shut down gracefully knows which pid to hit; ② for a sandbox-level TERM
to be "graceful", the engine would have to judge "which processes in the guest are worth waiting for, and
for how long", which is **product-layer policy** that the extension rule's litmus keeps outside the engine (the engine
does not know about agents, the architecture reference). So the lifecycle mediator's verb table stays at pause/destroy, with destroy taking
effect immediately.
External evidence points the same way: e2b's `DELETE /sandboxes/{id}` summary reads literally "Kill
sandbox" with no grace field; sandbox0's sandbox PodSpec does not set
`TerminationGracePeriodSeconds` at all (inheriting kubelet's implicit 30s, a value nobody argued for),
while the grace periods it **did** decide on are all at process/session granularity and none on the host
side.

### D4.4 The pause quiescent-point protocol (the mechanism for the architecture reference)

The shape and its two premises are in the pause/resume lifecycle: the protocol is **only one host-side drain barrier** — stop
accepting new mutating fs writes, wait for in-flight ones to drain, then hand over to the rootfs subsystem's flush
(reusing that primitive from D4.3). This section adds only the three things the pause/resume lifecycle does not have: one
measurement, a list of what does not enter the barrier, and why a cooperative freeze was rejected.

**Measured**: colliding a 64 MiB `fs.Write` with a `Pause` and waiting until the write had genuinely
entered the guest before sending the pause, a truncated file appeared with the barrier off (one round in
ten landed only 36.7 MB), while with it on all ten rounds landed complete.

**The gate blocks "writes" only, never "reads"**: `Read`/`Stat`/`List`, `process.List`, and `Connect`
never enter the barrier — they can truncate nothing, and blocking them during a pause would switch off
the observability surface at exactly the moment an operator most wants a look. `process.Start` /
`SendInput` / `SendSignal` are likewise **not blocked**: what they change is guest state, not something
travelling through "a write the engine issued that carries a truncate window"; and `Start` is a long
stream, so waiting for it to drain would self-deadlock the barrier — the drain would be waiting on a
stream that only ends when the drain does.

**No cooperative freeze inside the guest (`SIGTSTP` + observing `/proc` for `T` + degrading to `SIGSTOP`
past a grace period) — rejected.** Two reasons: ① it requires the workload to **voluntarily** stop at a
safe point on a signal, which the engine can neither require nor verify (it does not know about agents,
the architecture reference); ② two mature implementations both chose an **unmaskable** freeze (one `SIGSTOP`ping the
process group, one using the cgroup freezer) and neither offers an application-layer safe-point protocol
— the evidence points the same way. **The reverse channel (a process proactively reporting "I have
reached a safe point") is likewise rejected**, for reason ①: it would require long-running processes to
change their own code for pause (violating the architecture reference). So an application-layer safe point **does not
enter the engine** and the engine introduces no application-layer handshake: it falls on the reconciliation model's
"cooperation is only an optimization, not a correctness precondition" side, where the semantics of a
timeout / hard kill match those of a crash — and a crash is the bottom line the pause/resume lifecycle already accepts.

**A timeout preempts rather than failing the pause.** the reconciliation model places this layer on the "optimization" side,
so every path must land on a legal `paused`: one stuck RPC must not make a sandbox un-pausable. A timeout
merely records which operations had not drained (**by name** — "3 remaining" is useless to a reader) and
freezes as usual: that landing point is exactly where a crash lands, and a crash is the bottom line the pause/resume lifecycle
already accepts. (One mature implementation **abandons the pause** on timeout; here that trade-off is
wrong, for the reason above.)

**A crash-pause does not drain**: the container is already dead, there is no in-flight work to wait for
and nothing left that could be truncated, and waiting would only delay recording a death already
observed. Only the three voluntary paths (a user `pause`, `auto-pause`, and Promote's quiesce) drain.

A parameter-level residual (pure tuning that does not change this layer's structure): the drain's grace
duration — what it waits for is a single fs RPC, which completes in milliseconds when healthy, so
anything still outstanding is stuck rather than slow.

### D4.5 Three seams tini's checklist cannot cover

tini's semantics are authoritative on the premise that **it has exactly one child and is never itself
restarted**. Whereas transport-mode `sandboxd` has **N children + a PTY + gets restarted in place by
PID 1** (D5.3). Three things tini does not cover and this engine must pin down itself, all on the
"transport mode holds the pid/pgid handles" side:

- **C1 — per-process pgid / session, with PTY asymmetric.** tini's `-g` only forwards signals to its one
  child's process group; what `sandboxd` needs is to `setpgid` **each** `process.Start` into its own
  group, so that `process.signal(pid)` hits that one process tree in a directed way. **But the PTY case
  is the reverse**: a pty has already done `setsid` + set a controlling terminal for job control, and a
  further setpgid would fight the pty's session management. So the contract forks — **non-PTY processes
  get their own pgid, PTY processes are left to the pty's session** — and directed signals take
  different paths for the two. This belongs to transport mode, and PID 1 does not touch it.

- **C2 — Wait / exit-code semantics are lossy across one transport restart.** After the transport crashes
  and PID 1 restarts it in place: the children it had adopted as a subreaper reparent to PID 1 (the real
  init) the instant it dies, the new transport has lost its in-memory pid/pgid handle table, and those
  processes now sit under PID 1 — so the new transport cannot `waitpid` them and their exit statuses are
  collected by PID 1. So **exit codes (`end` events) are guaranteed only within one transport lifetime**,
  degrading across a restart to "`process.List` (scanning `/proc`) still sees it, the exit status may be
  lost". This shares its origin with the raw pty byte stream's lossiness: the engine is stateless,
  restartability is not the engine's burden, and a "lose nothing" guarantee is for a layer above (a
  session adapter) to handle. **Zero new mechanism**, and no PID 1↔transport exit-status reporting seam
  is added.

  **The boundary within one lifetime (a pid becomes unaddressable the moment it exits, the guest transport) has
  exactly one way to be landed differently**: for it to remain addressable after exit, the premise would
  be switching to an opaque ID that is never reused — which would also require an allocator + a
  reclamation policy, and would change the handle's semantics from "this process" to "this restartable
  position" (so an attempt/counter number would be needed too), that is, a restart supervisor. the transport subsystem does
  not do that, so wanting "lose nothing" means catching the `end` frame outside the engine.
  > **The seam left open (to be built only if the session layer proves "lose nothing" is required, so the
  > engine need not be revisited)**: PID 1, when collecting a global orphan's exit status, also records a
  > copy on the side for the new transport to query (one very narrow internal reporting seam). Listing it
  > pins down "adding it moves neither the transport nor the engine's stateless contract", the same
  > discipline as [`transport.md`](./transport.md) D3's "hold the seam".

- **C3 — how reaping authority divides between PID 1 and the transport, including the restart transient.**
  In the normal state the transport reaps its own descendants through `PR_SET_CHILD_SUBREAPER` and PID 1
  only backstops global orphans; in the **transient** (the transport dead, the new one not yet in charge)
  the division inverts — descendants briefly reparent to PID 1 and are reaped by it (which is exactly
  what causes C2). So "the transport always reaps its own descendants" does not hold.

> C1 (the pgid asymmetry) is a mature pattern; C2/C3 arise only under the **"a separate PID 1 + supervised
> restart"** topology (an ordinary HTTP child process that is neither PID 1 nor supervised-restarted never
> meets them), and are this engine's own additional agenda.

---

## D5. The resume cold-start timing

The orchestration on top of the carrier (netns TCP, the transport subsystem), the probe model (the health and recovery model), the transport's
statelessness, and the placement discovering the IP (the runtime subsystem).

> **create and resume are one cold-start chain, differing only in prefix and failure exit.** ④⑤⑥⑦
> (bringing runsc up + the readiness gate) are isomorphic for both; all that differs is: (a) the prefix —
> resume remounts an **existing** Overlay artifact, while create first builds an **empty** Overlay + binds
> the Volume/Network records for the first time; (b) the failure exit — resume has a quiescent state to
> fall back to (→ `paused`, point ④) while create does not, so on failure it must clean up half-built
> resources (the empty Overlay + the placement) and then delete the record / go to `destroyed` (the architecture reference
> invariant A).
> create's readiness window and half-dead disposition reuse ⑦'s Health RPC gate, with a timeout / failed
> bring-up landing on its own failure exit. Below, resume is the main line and create's differences are
> annotated inline.

### D5.1 The canonical cold-start chain (shared by create / resume; verbs belong to the lifecycle mediator, mechanisms to the subsystem layer, and subsystems do not call each other)

`resume` / `create` (lifecycle verbs, the mediator) call each subsystem's mechanism in order, bringing it
up → `running`:

```
⓪ the lifecycle mediator write regime : read the mount table; if this bring-up would rw-mount some volume while a direct
                    mutation is in flight on it → refuse (the volume subsystem; roll back to the source state. The gate
                    is first, so a refusal has built nothing)
① the rootfs subsystem Rootfs     : mount the merged overlay (lower=Template / upper=the Overlay artifact / work)
                    [create: build an empty upper/work first; resume: remount the same Overlay artifact]
② the volume subsystem Volume     : Materialize each entry of the mount-table Binding, with the handles grouped into
                    LaunchSpec.Mounts (runsc takes mounts from the OCI spec at startup, so this must
                    precede ④)
③ the network subsystem Network    : generate the rule set from the declaration Binding (a pure function: the allow set +
                    the L0 floor + publish endpoints → rule text)
      └─ ①②③'s products are gathered by the lifecycle mediator and handed to ④ — subsystems do not call each other
④ the runtime subsystem Runtime    : rebuild the placement (netns/IP/cgroup per the resource-ceiling Binding; install
                    ③'s rule set after building the netns/veth and before bringing up, with the
                    predicate bound to the host-side veth's `iifname` (by name; `iif` is by ifindex and
                    silently fails open after a same-name veth rebuild, the runtime subsystem) — the network safety invariants's atomicity
                    obligation)
                    → bring up runsc with sandboxd (init mode) as the entrypoint
⑤ in-guest start  : PID 1 (init mode) installs the reaper + signals + supervision → spawns
                    transport-mode sandboxd → the child does PR_SET_CHILD_SUBREAPER → binds netns TCP
                    → listening
⑥ the runtime subsystem placement  : fix the transport endpoint as the guest netns IP:port — not a post-boot probe: the
                    launch path uses the engine's own allocation directly, and the Adopt path reads the
                    host-side veth's /30 and adds 1 (the guest-side address has been absorbed by the
                    sentry netstack and cannot be read back, the runtime subsystem)
⑦ the lifecycle mediator probe gate   : send a Health RPC to IP:port, polling until the first success or a timeout
      ├─ first success → state=running, emit the resumed/created event (atomic in one commit with the
      │                  state commit, the architecture reference-C) → the API returns
      └─ timeout / failed bring-up → after reclaiming the half-built placement: resume→stay paused and
                         retryable; create→delete the record / →destroyed
⑧ (outside the    : the client re-resolves the placement and reconnects process/fs to the current IP;
   engine)          a session adapter calls driver --resume (the engine does not participate, the transport subsystem)
```

**Ordering constraints**: ⓪ must precede ① (a refused cold start should not have mounted the overlay
already); ①②③ must be materialized before ④ (mounts are taken from the OCI spec when runsc starts and
cannot be changed live); ⑥ requires the netns to exist (after ④); ⑦ requires the IP to be known (⑥)
**and** the port to be listening (⑤) — **⑦ is the only barrier between "brought up" and "running"**.
**Concurrency constraint**: reaper locking's transition lock is **held throughout** this chain — taken before ①
and released only when ⑦ flips it to `running` (or a timeout unwinds it) — so throughout ①–⑦ a
concurrent `destroy`/`evict` cannot take the lock and will not delete an Overlay artifact being mounted
or cold-started.

### D5.2 Design points

- **Point ① — readiness = the first successful `Health` RPC, reusing the health and recovery model's probe rather than inventing a
  ready protocol** (a bare TCP accept may precede the RPC handler being wired up, causing a false start;
  readiness and health probing are the same thing anyway).
- **Point ② — no health classification while a resume is in flight (the single-flight guard, the health and recovery model).** The
  implementation shape ("a non-blocking try-lock + skip", what it stops, and why it is not load-bearing
  for concurrency correctness) is in [`reaper-health.md`](./reaper-health.md) D3.
- **Point ③ — the resume API returning at `running` IS the signal that the caller "may connect now".**
  The engine pushes no readiness to clients (there is no connection registry and no callback). A client
  holding a stale IP handle that fails to connect **re-resolves the current placement** (a cold start
  voids the old IP) — the only timing requirement is that the client not cache an old IP.
- **Point ④ — a failed cold start rolls back atomically to the source state (⑦'s failure arm), adding no
  failed state and introducing no compensating transaction** (the architecture reference-A): a failure is only "this
  placement was not built", and the persistent state is intact on the backend.
- **Point ⑤ — PID 1's internal readiness is absorbed by the transport port's readiness**: PID 1 can only
  spawn the transport child after installing its handlers (⑤), so ⑦'s gate already gates the one
  externally observable readiness and PID 1's internal ordering need not expose a handshake.

### D5.3 Probe failures split into two classes (a consequence of the supervision topology)

PID 1 **supervises and restarts** the transport child in place, so **a transport crash is not a substrate
death** — the two probes' definitions, the health classification, and the dispositions are all in the
current health implementation
the health and recovery model. This distinction is exactly the additional capability D4.2's split-process topology buys: without a
separate PID 1, "the port is unreachable" would have only one possible explanation.

---

## D6. Boundaries

- **The pause quiescent point**: the structure is settled (a single drain barrier, D4.4); **an
  application-layer cooperative safe point is rejected** rather than pending. The grace duration is a
  numeric knob whose home is the code (`internal/lifecycle`'s `defaultDrainGrace`), and per the same
  discipline as transport D5.4 it is **not registered as an GitHub Issue** — a knob with two homes ends
  up with two values.
- **Whether a template preinstalls a resident process**: not needed; should one appear, it is managed as a
  peer and not stuffed into `sandboxd`. **The content list of the built-in Template** is a
  product/template layer choice and is **settled in the rootfs subsystem** (one bare base, nothing agent-shaped; an agent
  stack layers on it outside the engine by exec-then-Promote, which is D3.1's one-shot exec with no init
  dependency).
