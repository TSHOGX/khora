# Transport deep dive: the two-service foundation (process / fs) + session semantics as an addition

> **Position: the transport-layer internals reference.** The engine exposes just the two
> stateless services process / fs, with session semantics outside the engine); this document develops
> what lies behind that boundary — **the choice of framework, the ACP session mapping, and the seams to
> hold** — and gives the **wire protocol (D4)** plus the outer-edge projection (D5, whose boundaries and
> residuals are in D5.4).
>
> This page is self-contained. Product-layer session protocols and external daemon implementations
> are outside the engine; the current wire contract is defined by `proto/` and generated clients.

---

## D0. The conclusion in one sentence

The transport uses **Connect-RPC** (HTTP-native, streaming). **The engine exposes only two general,
stateless capability services (process / fs)**, which form a self-sufficient foundation — equivalent to
an E2B-style sandbox. There is also a **`Health` probe for the engine's own use** (D4.5), which is not
part of the outward capability surface and so is not in the table (the same wording as the transport subsystem):

| Service | Nature | Inside the engine? | Who uses it |
| --- | --- | --- | --- |
| **`process`** | A stateless mechanism primitive: start a process + an optional PTY + bidirectional streams + signals | ✅ inside | Anyone who wants to run something in a sandbox |
| **`fs`** | A stateless mechanism primitive: read(offset)/write/list/stat | ✅ inside | Anyone who wants to read or write sandbox files |

**`session` (agent session semantics) is not implemented inside the engine** but is **a layer of
semantic addition stacked on `process`**, living outside the engine (an adapter / an upper-layer
component). Its shape aligns with ACP's `session/*`, but its implementation, state, and audit trail are
**entirely outside the engine**; the engine core knows nothing about "agent / turn / session".

**The core discipline (the one to hold to the death)**: a session must be able to stack on `process` as
a **pure addition** — adding it **changes nothing in the engine core** (aligned with the extension rule's "cost
criterion"). This document holds three seams for that purpose (D3), and the engine reserves no slot for
a session.

---

## D1. Why the engine does only these two, with session drawn outside

The design pressure comes from three directions: E2B (how mechanisms converge), ACP (how agent sessions
are standardized), and the asymmetry between rendering and interaction. Where the three meet:
**process/fs are all of the engine, and a session is an addition outside it.**

### D1.1 `process` + `fs`: "doing work in a sandbox" is only ever two things, and they are self-sufficient

Everything doable in a sandbox either touches the filesystem or runs a process. Make the process
primitive complete (an optional PTY + bidirectional streams + signals) and **a shell, command
execution, a background service, and a jupyter kernel are all special cases of "start a process"** —
with no need for a separate interface per shape.

So both capabilities are defined as **Connect-RPC services**:

- **`process`**: a PTY is not a separate service but a special case of `Start` carrying a PTY field —
  resize = updating the size, keyboard input = input's pty branch. **Processes are addressed by pid**
  (guest-native; why tags do not enter the model is in D4.6). The authoritative RPC list is in D4.3.
- **`fs`**: all unary + an HTTP streaming bypass for large files, with **no `watch`** (consumers poll
  for increments themselves using `Stat`+`Read(offset)`). The authoritative RPC list and semantic
  choices are in D4.4.

These two services are **stateless**: the engine maintains no durable SoT for them. "What processes are
running on this sandbox right now" = a live `process.List` to sandboxd, with no control-plane record:
zero process registry.

**The key point: these two alone are enough to be a complete product.** process + fs = a persistent,
root-capable, multi-connection, file-readable-and-writable, arbitrary-process-running isolated sandbox
— that is, the entire foundation of the engine scope, deliverable independently **without
depending on a session existing**.

### D1.2 `session`: the shape is taken from ACP, but it is an addition outside the engine rather than the engine's job

With only process/fs, an agent is "start a process and read its stream". That is entirely sufficient for
a shell; for a coding agent three things are missing — **turn semantics, session history and
resumption, and structured events**. ACP (Zed's Agent Client Protocol, "the LSP of coding agents")
standardizes those three. But **the question that matters is: who should provide them?**

The answer is **outside the engine**. Three reasons, each building on the last:

1. **All three can be assembled on top of process/fs and need no engine primitive.** The turn loop = an
   adapter recognizing prompt/output/end on the process stream; session history = the agent writing to
   its own FS anyway (`~/.claude`'s JSONL, landing in the Overlay artifact and readable through `fs`);
   resumption = the adapter calling the agent's native `--resume`. **The engine adds not one new
   concept.**
2. **They are product semantics, and stuffing them into the engine contaminates the foundation
   (the extension rule).** "turn", "session", and "which agent" are all product-layer concepts; making the
   engine know them leaks product concepts into a sandbox engine — vetoed by the first principle.
3. **ACP itself treats an agent as a peer outside the engine.** In ACP an agent is a child process the
   client starts, conversing through `session/*` — the agent and "the environment running it" are two
   separate layers. This project maps that layer as: **the engine provides the environment (process/fs),
   and a session adapter outside translates a concrete agent into an ACP-shaped session protocol.**

So `session` = **ACP's `session/*` method family** (`new` / `prompt` / the `update` stream / `cancel` /
`load` / `resume` / `stopReason`), **implemented by an adapter outside the engine**. The `control`
payload belongs to that layer: it is **not a payload type of the engine but a protocol of the session
layer outside it**, aligned with ACP, and the engine does not hold it.

The three points by which ACP validates this line (turn state belongs to the agent process / session
history is produced on the agent side and stored by a driver-native mechanism / resumption across a
restart is an **explicit capability bit**, `session/load` vs `session/resume`, rather than something the
engine guarantees with a lease or a grace period) **hold naturally** in this project — there is no turn
table in the engine to arbitrate and no session content to hold. The session layer's state (the
sessionId↔driver mapping, its resumption capability, its audit event stream) is all managed by that
layer outside the engine.

### D1.3 On the engine side there is only process / fs: no lane, no payload registry, no turn arbitration

After two stateless services (D1.1) plus session moved outward (D1.2), the engine side needs no unified
transport primitive, no payload type system, and no turn arbitration. Locking down each one's legitimate
substitute, so an unnecessary abstraction is not carried back in:

- **No unified transport primitive (a "lane") is needed**: the five things a
  "handles-everything" transport abstraction would carry — multiplexing, per-connection ordering,
  restartability, control-plane registration, and quiescing at freeze time — are absorbed one by one by
  **Connect-RPC / HTTP-2's native multiple streams + each stream's inherent ordering + `Connect`
  reattachment + process/fs being stateless**; only "quiescing at freeze time" is a genuine need, and
  the pause/resume lifecycle's pause quiescent point provides it separately. Nothing is left for a "unified lane" to do.
- **No payload type system is needed**: jupyter = start a kernel process and read the stream, file = the
  fs primitives, cli = start a process — all assemblable on process/fs; `control` (an agent session) is
  drawn outside the engine (D1.2). So the engine side has only the two general mechanisms process / fs,
  and **adding any new interaction shape = stacking a layer outside the engine, with not one line
  changed inside it**.
- **No turn arbitration**: turns / sessions live wholly outside the engine, and the engine's control
  plane **does not store** turns, leases, ownership, cursors, or history. The engine's only
  reliability-related promise is kept minimal: a process's bidirectional stream is a best-effort live
  stream, and in-flight data may be lost on an interruption — a raw pty byte stream tolerates dropped
  characters (a shell has no durable SoT), and audit idempotency for an agent session is guaranteed by
  the session layer outside the engine.

---

## D2. Rendering vs interaction: an asymmetry, which is why fs has no watch

"Watching what a group of agents is doing" (rendering) and "driving one agent" (interaction) are **two
asymmetric things** that should not be forced into one primitive:

| | **Read / render** | **Write / interact** |
| --- | --- | --- |
| Semantics | Spectating, replaying, looking at state | Driving a running process |
| Shape | fan-out, many-to-one, pull | point-to-point, duplex |
| The engine's role | The general `fs` (read/stat, content opaque) | `process` (duplex + signals) |
| Is a connection needed | **Not necessarily** (no stream need be opened at all — just poll fs) | **Yes — that stream IS the process interaction** |

- **The rendering face**: the engine guarantees only "the bytes can be read + the size can be read"
  (`fs.Read(offset)` + `fs.Stat`). Whether to push and how often to poll is the consumer's own policy (a
  component outside the engine); polling `stat` is reliably available in a run-anywhere setting. The
  reason **no watch is provided** is in D4.4.
- **The interaction face**: only when something must **write to a live process** (send keys to a shell,
  post a message to an agent) and get a real-time bidirectional stream back is `process`'s bidirectional
  stream needed. Purely viewing history or state opens no stream at all.

One corollary: **the engine knows nothing about content.** An agent's own session log (JSONL) naturally
lands in the Overlay artifact, so "persistence / `--resume` / frontend reads (polling read+stat)" all
reuse the same file, and the engine **need not build** a "session log" mechanism or change
notifications. Reconstructing a sidechain tree, drawing a swarm topology — all outside the engine, in
the product layer.

> **A PTY stays minimal and gets no scrollback file.** A PTY is an ephemeral byte stream and its output
> travels on the process stream; giving it a manufactured on-disk SoT for the sake of symmetry with
> "render by reading a file" adds complexity to the simplest thing there is. The two paths stay minimal
> separately rather than being levelled: an agent that writes logs has its own on-disk session to render
> (the SoT is its own file), while a bare pty shell has only a live stream and no on-disk SoT. That
> faithfully reflects a semantic difference between two things, and the engine invents for neither a
> semantics it does not have.

---

## D3. The seams to hold, so that a session stays a pure addition

A session lives outside the engine, and adding it **does not come back and change the engine**. That
requires the engine to hold three seams:

- **`process`'s bidirectional stream is general enough**: it can carry arbitrary bytes/frames, letting a
  session adapter run ACP's JSON-RPC (prompt/update and the rest) over it without the engine needing to
  understand the stream's content.
- **`process` supports `Connect` reattachment**: a session's "resumption" semantics rest on **the
  agent's native `--resume` + reading history through `fs`** — reattaching to a stream that is still
  running, or replaying from history on disk. **They do not rest on a process-state checkpoint**: the
  whole memory-state-freezing family (a full VM image and a single-process checkpoint alike) is
  permanently excluded by the engine boundary for portability (memory state pins CPU features, the runtime portability constraint). This road does
  not require the engine to understand a session.
- **`fs` can read the bytes and size of the agent's own log from outside the sandbox** (read+offset /
  stat): letting the session layer / a frontend render history, with the engine neither providing nor
  needing the concept of a "session log".

As long as those three seams exist, a session can land as a pure addition outside the engine. **This
document does not design a session's internals, only ensures the foundation does not stand in its way.**

> **Three boundaries where ACP is borrowed but must be adapted** (left for whoever actually builds the
> session layer; recorded here only): ① ACP's fs is the agent calling **back** into the client
> (direction agent→client) while this project's fs is **host→sandbox**, so a session adapter must adapt
> the direction; ② ACP has **no audit**, while this project wants durable events (hung beside the
> session layer, not in the engine); ③ ACP recovers by **replay**, and so does this project — a resume
> is a cold start (the pause/resume lifecycle stores no memory), so recovery goes through `--resume` + reading history through
> `fs`, and there is no process-state recovery road.

---

## D4. The wire protocol

### D4.1 The carrier: TCP inside the netns (see the transport subsystem)

The carrier is settled (the transport subsystem): `sandboxd` listens on a TCP port inside the guest netns, connections
are terminated by the sentry's userspace netstack, and the host connects directly through that netns's
veth IP — unary over HTTP, bidirectional streams over h2c streaming (server-stream/client-stream) with
immediate flush, no vsock; the framework is Connect-RPC over h2c (D4.2) rather than a bare WebSocket
duplex.

The transport depends only on what the placement produced — **who builds the netns, who allocates the
veth IP, and how the sentry adopts the netns all belong to the runtime subsystem** and are not restated at this layer. Only
one constraint lands on the wire protocol: a reattachment takes the **current** netns veth IP (which
changes on a cold start) and does not cache a fixed address (the placement model).

**One process-wide shared connection pool is a correctness requirement (the transport subsystem).** `ReadIdleTimeout`
presumes it too: a connection that served exactly one unary call and was then abandoned does not live long
enough for a PING to come due; the one to a retired endpoint ages out by the pool's idle timeout.

> **The argument from the other side: if the pool were not shared, but built fresh per dial.** The client
> is by contract discarded, so nobody would ever close that pool, and every connection ever established
> — along with its read and write goroutines — would stay resident until the process exits. And dialing
> happens both on every data-plane RPC and on the reaper's probe of every running sandbox each round, so
> the growth is **independent of load** and tracks only the sandbox count and the tick period (measured:
> 50 calls to the same endpoint = 50 connections and about 150 resident goroutines), ending in fd
> exhaustion.

**The channel's trust model: why the transport carries no in-channel auth (an explicit argument, not an
omission).** `sandboxd` listens **in the clear** inside the guest netns and the host connects directly
through the veth IP, with no token or mTLS on the channel itself. That is the **conclusion** of an
attack-surface analysis rather than a missing item. Taking each party who can reach that port in turn:

- **The guest itself**: it can reach it, but **gains no new capability**. Inside the guest it is already
  root (the guest identity boundary), and everything the channel permits (starting processes, reading and writing **this
  sandbox's** FS, sending signals) it can already do as root; there is also **nothing on the channel to
  steal** — `sandboxd` does only process/fs, and the concept of a credential does not exist in the engine
  at all (the engine boundary excludes it permanently), so nothing of the sort can flow over the channel. So "the guest
  can reach the control channel" is not a privilege escalation.
- **A sibling sandbox**: **it cannot reach it, by design** — cross-sandbox network isolation blocks it
  (the sandbox security model / network.md D5 ①: a sibling IP is in the L0 floor; the point-to-point veth is a layer of depth
  rather than the materializer).
- **From outside**: unreachable unless **explicitly published** (the network subsystem). **publish itself carries no
  authentication field** — the engine delivers a `host ip:port` and any caller validation happens outside
  the engine (the network subsystem) — so it provides no additional protection here.

**By contrast**: a guest daemon hanging off a **publicly reachable URL** must carry an access token; this
project's `sandboxd` is only inside the netns with the host dialing over the veth, an attack surface an
order of magnitude smaller, so **in-channel auth is redundant under the current carrier**. But that
**depends on the carrier's shape**: should some provider (the runtime subsystem) ever expose the transport to an untrusted
network, in-channel auth turns from redundant into necessary — added by that provider at that point, as
part of a runtime provider's network obligations, without changing this wire protocol.

### D4.2 The framework and the division of streaming

- **The RPC framework = Connect-RPC over h2c.** One IDL speaks Connect / gRPC / gRPC-web at once, unary
  and streaming share an origin, and it can run over plain HTTP (which fits D4.1's carrier).
- **The streaming division = an asymmetric three-way split, with no full duplex.**

  | Direction | Shape | RPC |
  | --- | --- | --- |
  | **Output** (stdout/stderr/pty-out + process events) | **server-stream** | `Start` / `Connect` |
  | **Input** (stdin/pty-in, order-preserving) | **client-stream** | `StreamInput` |
  | **Control** (signals/resize/listing/one-shot writes) | **unary** | `SendInput` / `SendSignal` / `Update` / `List` / `CloseStdin` |

  Several independent implementations converge on this split — **output is fan-out to many subscribers
  (natural for a server-stream), input is an order-preserving single writer (natural for a
  client-stream), and control is a point operation (natural for unary)**. Forcing them into one duplex
  stream would knead three different backpressure / ordering / fan-out semantics into one strand. The seam
  D3 must hold (a stream general enough to carry ACP's JSON-RPC) is satisfied by the `Start`/`StreamInput`
  pair's bidirectional capability, with no duplex required.
- **Long streams disable idle read/write timeouts and are kept alive by `keepalive`.** An output
  server-stream is long-lived (an idle terminal can hang for hours), and the server **must not** impose a
  default idle read/write deadline on it — that would silently kill an idle shell (a known, reproducible
  pit). The `keepalive` in the event `oneof` (D4.3) exists for this: the server periodically sends a
  `keepalive` frame so intermediaries and frontends know the connection is alive and do not reclaim it for
  going quiet. This is an implementation-level constraint, recorded here rather than entering the core
  protocol.

### D4.3 The process service surface

The RPC list (addressed by pid; why tags do not enter the model is in D4.6):

| RPC | Shape | Semantics |
| --- | --- | --- |
| `Start{cmd,args,env,cwd,pty?}` | server-stream | Start a process, streaming back `oneof{start,data,end,keepalive}` events |
| `Connect{pid}` | server-stream | Reattach to an existing process and pick up its output stream (the transport model's multi-connection behavior) |
| `StreamInput{pid, stream<bytes>}` | client-stream | Order-preserving writes to stdin / pty-in |
| `SendInput{pid, oneof{stdin,pty}}` | unary | A one-shot write (when no stream need be held) |
| `SendSignal{pid, signal}` | unary | See below |
| `Update{pid, pty.size}` | unary | PTY resize (not a separate service — a field of process) |
| `List{}` | unary | Live enumeration (**no registry**, the transport subsystem) |
| `CloseStdin{pid}` | unary | Close stdin (equivalent to sending Ctrl-D under a pty) |

The event `oneof{start(pid), data(stdout|stderr|pty), end(exit_code), keepalive}`. **A PTY is not its own
service**: `Start` carrying `pty.size` is the pty special case, resize is `Update`, and input travels on
input's pty branch.

- **Multi-connection fan-out: output is broadcast, input is not arbitrated.** the transport model's "several connections
  attached to one sandbox concurrently" applied to a single process means "several `Connect`s on the same
  pid". **Output**: every attacher gets its own copy of the output stream (one source fanning out to N
  subscribers, and one unsubscribing does not affect the rest). **Input**: the engine does **not**
  arbitrate or lock among several writers — several connections writing one pty at once = several people
  `screen`-ing the same terminal, and interleaved bytes are Unix's own semantics (the transport model's "several people
  ssh'd into one machine"); whether to coordinate is the **product layer's** business, and the engine
  forwards faithfully. This holds "the engine maintains no state for connections" (transport statelessness).

- **Fan-out's two correctness invariants: the source never blocks on a subscriber + replay is bounded.**
  "One unsubscribing does not affect the rest" speaks only of disconnection; fan-out has two more rules
  that must be pinned explicitly, or one stuck frontend could freeze a real process inside the guest:
  1. **A slow subscriber is dropped without blocking, and the source's `write` never blocks.** Every live
     subscriber has an independent **bounded** send window; on overflow **that subscriber** is dealt with
     without blocking, and **backpressure is never propagated back to the source** (the PTY/process). This
     turns D1.3's "a bare pty is lossy" from a vague statement into an acceptance-testable rule: what is
     lossy is **that slow subscriber's copy**, not the source.
     - **The overflow disposition = disconnect that subscriber.** The reason: once the window overflows,
       that copy's byte stream already has a hole and cannot be seamlessly continued; rather than leaving a
       **mutilated** live stream for an upper layer to mistake for a complete one, disconnect and let the
       client `Connect` again — a reattachment first replays the bounded ring buffer's recent tail
       (invariant 2), which is cleaner than a stream with a hole. The disconnection is **that subscriber's**
       affair, and the source and other subscribers are unaffected. This adds no "guaranteed delivery"
       semantics to the source (D1.3's lossy boundary is unchanged).
     - **"You were disconnected" is observable to that subscriber**, and must be: otherwise "the process
       ended normally" and "your copy was dropped" both present as the stream closing, while the correct
       reactions to the two are opposite (accept the exit code / reattach and read again). So an overflow
       close reports `resource_exhausted`, and the host-side client projects it into a termination event for
       the consumer. **But it is not a health signal**: the health and recovery model judges health only from the L2 `Health` probe
       and the guest-side activity clock, never from "some subscriber cannot keep up" — visible to the
       caller, invisible to the reaper.
  2. **`Connect` replays the bounded ring buffer first, then joins the live stream.** Each process
     maintains a **per-process, in-guest, byte-bounded** output ring buffer (oldest bytes are dropped
     silently); `Connect{pid}` **replays the buffer's recent bytes to the new subscriber first and then
     cuts over to the live stream**. So a reattachment / a page refresh / attaching from several places
     **immediately shows recent state rather than a blank screen** — which is exactly what a REPL or an
     interactive terminal requires.
  - (the transport subsystem has already placed this ring buffer outside the "stateless" constraint — it is an in-process,
    transient, non-authoritative acceleration structure, and losing it costs only the replay of recent
    activity.)
  - **The boundary: the engine guarantees a best-effort byte tail, not screen reconstruction.** A raw byte
    replay may land in the middle of an ANSI escape sequence; **full screen-state reconstruction
    (tmux/screen-style reflow) is the job of a client/adapter outside the engine**, which does not
    understand the stream's content (the D3 seam) and does not invent for a terminal a semantics it does
    not have (echoing D2's "a PTY gets no scrollback file").
- **A signal goes to the process group by default and can address a single process.** `SendSignal` sends by
  default to **the process group of the target process** (first `kill(-pid)`, falling back to `kill(pid)`;
  the asymmetry in guest-runtime D4.5 C1 is the precondition for that working), because `apt`/build/
  dev-server all fork subtrees and killing only the parent leaves a pile of orphans. This is where the
  the pause/resume lifecycle's "send a directed signal to a process inside the guest" lands (note that the pause quiescent point
  itself does not take this road: it is a host-side drain barrier and sends the workload no cooperative
  signal, guest-runtime D4.4).
  **So `pid` must be > 0, otherwise `invalid_argument`**: in POSIX `pid <= 0` is the fan-out form and
  belongs to PID 1's side — that division (directed signals to the transport / container-level fan-out to
  PID 1), the validation point, the measured consequences, and "a sandbox-level grace period is rejected"
  are all in guest-runtime.md D4.3 (the guest transport) and not duplicated in this table.

### D4.4 The fs service surface

The RPC list: all unary + an HTTP streaming bypass for large files, with no watch.

`Read{path,offset,len}` / `Write` / `List{path,depth}` / `Stat` / `MakeDir` / `Remove` / `Move` are all
unary; `Stat` returns `EntryInfo{name,path,type,size,mode,mtime,symlink_target?}`.

**The semantics take the most forgiving option** (as mature implementations do): `MakeDir` creates parent
directories (`mkdir -p`) and reports `already_exists` when it exists (rather than silently succeeding);
`Remove` is **recursive and idempotent** (deleting something absent counts as success; the sole exception
is the guest root `/`, judged `invalid_argument` — one mistakenly passed empty path should not equal wiping
the entire sandbox) and offers no `recursive` switch — for a trusted caller the strict option is just a
trap plus one more error branch every caller must handle; `Move` creates the destination's parent and
**overwrites** the destination (refusing would break the whole "replace this file" class of usage); `Stat`
is **lstat** (a symlink is reported as itself, with `symlink_target` absolutized against the directory the
link itself is in — a relative path is meaningless to a caller outside the guest, and there is no
`Readlink` RPC to repair it afterwards); and `List`'s `depth` must be ≥ 1, with **0 being
`invalid_argument` rather than "unlimited"**, or one mistakenly passed zero would walk the entire
filesystem.

`EntryInfo` carries no owner/group (the engine does not model guest users, the guest identity boundary/the transport identity rules), and `EntryType`'s
zero value is `UNSPECIFIED` — a client meeting a type it does not recognize should skip rather than guess,
which is what makes "add a type later" an addition.

**`Stat` and `List` share one `EntryInfo` construction point**, which is not a style question: one
reference implementation wrote the two paths separately, and the result was that `List` did not fill in the
symlink target while `Stat` did — the same entry looking different depending on how you asked.

**Large files take an HTTP streaming bypass** (`/files?path=`, `GET` to download / `PUT`|`POST` to upload,
sharing the guest's single listening port with the three Connect handlers) rather than crowding the RPCs.
**The threshold is the caller's to set and the engine does not guess**: a small payload saves a round trip
over RPC, while a large one must stream. What the bypass buys is **not a size ceiling** (an RPC can accept
a very large message too) but that **neither end holds the whole thing** — measured on the same 64 MiB
payload, the guest-side `sandboxd`'s VmRSS: +9 MB via the bypass, +338 MB via RPC (about 37×, with the
ratio growing with file size). This is **a matter of security rather than efficiency**: a sandbox has a RAM
cap and is terminated when it exceeds it (the resource-capacity model), so pushing a large file over RPC means using the sandbox
being written to to break its own cap. An upload writes a temporary file in the same directory on the guest
side and then `rename`s, so an interrupted transfer leaves no half file (and for the same reason this path
has none of `Write`'s truncate window).

- **No `watch` is provided**: neither a server-stream push nor a polling watcher — **neither**. **There is
  only one load-bearing reason**: watch would require the engine to hold subscription/merge/reconnect
  state, breaking transport statelessness. (There is a second, **unmeasured** concern — the reliability of
  inotify on a gVisor overlay upperdir; it has no measured basis, so it is a side note and carries no
  weight.) Consumers poll for increments with `Stat`+`Read(offset)` (D2's rendering-vs-interaction
  asymmetry).
- **After a resume there is no offset-continuation contract for reads**: `fs.Read` reads the Overlay
  artifact's **current** bytes, and after a resume onto a new container the engine does **not** guarantee
  offset continuity across the resume (it is stateless and keeps no read cursor). A consumer crossing a
  resume must either "read the whole thing again from the start" or hold its own cursor and decide the
  increment from changes in `Stat.size`. Same origin as "input is not arbitrated": the engine does not keep
  state for connections.

### D4.5 liveness / readiness: one `Health` probe

`sandboxd` in transport mode exposes one unary `HealthService.Check{}`→`{ok, idle_millis}` (in the proto it
is **a third service, peer to** `ProcessService`/`FsService`, rather than a method hung on either). It
serves **one purpose three ways** and invents no second protocol: the transport probe (the health and recovery model's transport
layer of the two probe layers), the resume readiness gate (guest-runtime D5.2 point ①: the first successful
`Health` flips it to `running`, without trusting a bare TCP accept), and the **connection-activity probe**
(the health and recovery model's idle health state: `idle_millis` = `now - the sandbox-global last-I/O timestamp`, stamped by the
transport in `broadcast`/`writeInput`). **A dedicated `Health` is used rather than falling back on
`process.List`** — `Health` has constant cost and lists no processes, so neither the probe nor the activity
reading is biased by the number of processes, and idle plus L2's reachable-or-not come back in one RPC
rather than two round trips. Why the signal is measured on the guest side (rather than at a host-side
data-plane proxy) is in [`reaper-health.md`](./reaper-health.md) D1.

### D4.6 Three deliberate deviations

- **No tag addressing**: E2B's `ProcessSelector{pid|tag}` has tags, and we have **only pid** (guest-native).
  A tag is a namespace above the engine and is product-layer data that does not enter the model (the architecture boundary;
  the transport identity rules: a name is data, not an engine primitive).
- **No session/control payload**: `control` (an agent session) is a protocol of the session layer outside
  the engine, not a payload of the engine (the full argument is in D1.2).
- **No payload type system**: jupyter/file/cli are all assembled on process/fs, and the engine side has no
  payload registry (D1.3).

---

## D5. The outer-edge projection: control-plane REST + data-plane Connect (client↔orchestrator)

D4 fixes the **inner-edge** wire (host↔`sandboxd`); this section fixes the **outer-edge** wire (a client
[frontend/CLI/SDK]↔`orchestrator`), and the two edges divide protocols differently.

### D5.1 One unbypassable premise: a client never connects to `sandboxd` directly

E2B's SDK **connects directly** to envd (through a routing proxy, addressed by an `E2b-Sandbox-Id/Port`
header). This project **cannot copy that**, for reasons the carrier and the discipline jointly determine:

- **The carrier (D4.1)**: `sandboxd` listens in the clear only inside the guest netns, and only the host
  can reach that veth IP; a client is outside the engine and physically cannot.
- **the lifecycle mediator's mediator discipline**: every sandbox verb goes through `orchestrator` in the middle (which resolves
  the current endpoint by `sandbox_id`, and a placement never leaks outward, the API error contract). A client connecting to
  `sandboxd` directly would leak the placement outside the engine, breaking the placement model.

So the outer-edge data plane **must terminate at `orchestrator`, which re-proxies to the inner-edge
`sandboxd`**. This project cannot have E2B's "direct data-plane connection" benefit (the carrier decides
that), **but it can have the typed-contract + multi-language codegen benefit** — which is the entire
motivation for D5.2.

### D5.2 The division: control plane REST/OpenAPI, data plane Connect-RPC

| Face | Protocol | Endpoints (the API contract) | Why |
| --- | --- | --- | --- |
| **Control plane** (sandbox/template/volume/network lifecycle) | **REST + JSON** (OpenAPI describing it after the fact) | `POST /v1/sandboxes`, `pause`/`resume`, `DELETE`, `GET`, volume/template/publish … | Low-frequency CRUD, friendly to hand-invocation / curl / documentation; no strongly-typed streams |
| **Data plane** (process / fs) | **Connect-RPC over h2c**, proto contract first | `khora.transport.v1.ProcessService` / `FsService` (D4.3/D4.4) | Has server-streams + strongly-typed frames; the proto is **the single contract source**, and the frontend (TS) plus any future multi-language SDK all generate client stubs from it with `buf` |

**The key gain: the inner edge's proto contract projects straight onto the outer edge.** `orchestrator`
**reuses the same** `transport.proto` outward (rather than erecting a separate outer-edge schema), so the
outer-edge data plane is just **Connect re-proxying + resolving the endpoint by `sandbox_id`** — no
translation layer, and one wire representation across both edges. **The "re-proxy" is at the RPC layer, not
the HTTP layer**: the orchestrator mounts the same set of Connect handlers itself and re-issues the call
through the resolved `transport.Client`; it is **not** an HTTP reverse proxy forwarding bytes. That
distinction is load-bearing: errors must be **recast** at this layer rather than forwarded verbatim, or an
inner-edge dial error would carry the netns IP outward (the API error contract / D5.1: a placement never leaks outward). This
delivers the API contract's "the CLI and the frontend are peer clients of one API": both ends share the same
proto-generated client.

### D5.3 Why the outer-edge data plane uses Connect rather than a REST facade

The other candidate for the outer-edge data plane is a **hand-written REST facade + NDJSON chunked
streaming** (the orchestrator translating the inner-edge Connect stream into NDJSON frame by frame). It is
rejected, for three reasons:

- **It invents a third stream representation out of nothing**: an inner-edge Connect server-stream, an
  outer-edge NDJSON, and (possibly another one the frontend later wants) — three coexisting and held
  together by hand-written translation in the orchestrator, which is exactly the kind of addition the extension rule's
  "cost criterion" guards against; projecting Connect directly leaves one.
- **It loses types at the boundary that needs a contract most**: the data plane's process/file frames
  (`oneof{start,data,end}`, `DataEvent.stream`) are where strong typing pays off most, and NDJSON flattens
  them into schemaless JSON that a frontend/SDK must parse by hand. Projecting Connect directly lets the
  frontend generate with `buf` and align at compile time.
- **It does not buy simplicity**: a REST facade's hand-written handlers (the `?path=` query param, an NDJSON
  encoder, a flush loop) are no less code than Connect re-proxying, and add a mapping layer to maintain.

**Why the control plane does not move to Connect as well**: the control plane is CRUD, with no streams and
no strongly-typed-frame dividend, so REST/OpenAPI's hand-invocation and documentation value is higher, and
forcing proto onto it is negative. The dividing line falls on "are there streams / strongly-typed frames" —
the data plane has them, the control plane does not.

### D5.4 Boundaries and residuals

- **Authentication aligns with D4.1's layered conclusion**: whether the outer edge (client↔orchestrator)
  adds auth is decided by `orchestrator`'s deployment shape (it may be exposed publicly), and that is
  **orthogonal to the inner edge's conclusion that `sandboxd` needs no in-channel auth** — do not misapply
  the inner edge's "in-channel auth is redundant" to the outer edge. Outer-edge auth is an obligation of the
  orchestrator's access layer and is not part of this wire protocol.
- **A session still does not enter the outer-edge API**: D1.2's line does not move because of the
  projection — the outer edge projects only process/fs, and a session adapter outside the engine brings its
  own protocol face (ACP-shaped).
- **Multi-language frontend stubs are a pure addition**: the outer edge reuses the same `transport.proto`,
  and the frontend (TS) plus future SDKs generate clients from it with `buf`; adding a language's client =
  adding one `buf` output target, without changing this projection's shape (the API contract's current face is
  in the [API contract](../api/contract.md), which grows by adding endpoints rather than changing this
  projection).
- **`sandbox_id` travels in an out-of-band request header and does not enter the wire.** D4.6 keeps
  `sandbox_id` out of the proto (processes are addressed only by guest-native pid) and D5.2 forces the outer
  edge to reuse **the same** `transport.proto` — together those **force** an out-of-band carrier: an
  outer-edge Connect request carries the id in the request header `Engine-Sandbox-Id`, and the re-proxy
  strips it, resolves the current endpoint through the mediator, and passes the call through to the inner
  edge (a placement never leaks outward, the API error contract / D5.1). The header name is an implementation convention and
  does not change the wire's shape.
- **Implementation-level values**: the ring buffer's upper bound, a slow subscriber's send window, and the
  `keepalive` period are **numeric knobs** adjustable with experience rather than design questions (the
  shapes are fixed by D4.3/D5). So the values live only in the code (the constant block in
  `cmd/sandboxd/process.go`) and are neither registered as GitHub Issues nor duplicated here — a knob with
  two homes ends up with two values. **The overflow disposition (disconnect) is not in this category** —
  that is a design choice already settled in D4.3 invariant 1, not a knob.
