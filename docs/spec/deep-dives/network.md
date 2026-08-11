# Network deep dive: why enforcement can only live inside the engine, and everything outside narrowing lives outside it

> **Position: the network deep dive for [`§5.D`](../subsystems.md) (spec; navigation in
> [`SPEC.md`](../SPEC.md)), and it touches [`§2.1`/`§2.4`](../SPEC.md)'s security pillars,
> [`§5.A`](../subsystems.md)'s provider contract, and [`§4`](../model.md)'s mediator ownership.**
> Spec §5.D fixes the boundary (Network holds the declaration + generates an abstract rule set;
> rendering and installation belong to §5.A; application-layer behavior is not inside the
> engine); this document develops **why the boundary falls exactly there** — the criterion is
> enforceability locus (which side can physically enforce) — and gives the mechanical constraints
> on the declaration's shape, the invariants that must be held, and the measurements that must be
> inherited.
>
> **Numbering convention:** this document's own sections carry a `D` prefix (D1, D4.2…); a bare
> `§` always refers to a spec global section (§5.D etc.); the two are never mixed.

---

## D0. The conclusion in one sentence

A network checkpoint looks like one thing, but is actually **two things whose failure
consequences, vocabulary, and deployment position are all different**: **the process that
decides and moves bytes**, and **the side that creates the namespace and is therefore the only
one that can enforce**. The engine **is only the latter**:

| | The engine (this spec) | Outside the engine |
| --- | --- | --- |
| What it is | The side that creates the netns: holds the declaration + generates the abstract rule set | The process that terminates the connection, decides where to go from application-layer information, and moves bytes |
| Vocabulary | ID, IP, CIDR, port, proto, netns, veth, `iifname`, DNAT | Bytes, SNI, Host, upstream address, opaque strings |
| Failure consequence | **The boundary fails** (unbypassability is bankrupt) | That one connection fails closed |
| Process count | **The engine contributes zero processes** | Decided by that layer itself |

**The dividing criterion is enforceability locus, not "what the engine wants to manage"**: does a
control **only the side that creates the placement have the power to enforce**? Yes → it must be
inside the engine (D1's four items); no → it is not inside the engine. This one criterion also
answers "who owns proxying / credentials / auth / multi-tenancy" — all four are outside the
engine, because none of the four needs the side that creates the namespace to do it.

The engine therefore **does not name the layer outside it**. In the spec it appears only as an
address (`egress.via[].endpoint`) — not evasion, but the direct consequence of "the engine sees
only one TCP flow to some endpoint". (§6's existing "control plane / data plane" is **a different
pair of terms**, denoting the REST vs Connect-RPC outer-edge projection — unrelated to this
document, don't conflate the two.)

---

## D1. Four things only the engine can do (enforceability locus applied item by item)

These four are not "responsibilities the engine chose to take" — they are things an **external
process is physically unable to do**:

1. **Rendering and installing the rule set must be atomic with placement.** The rules must be
   inserted in the window between "the netns/veth is built" and "the isolation unit starts
   running". An external process **has no hook point there** — the earliest it can observe
   anything is after "the sandbox is already running", and any window in between is a fail-open
   window. `Adopt` / `ReclaimOrphans` (§5.A) are the same case: the netns is crash-surviving host
   state, and so is the rule set.
2. **Binding the ingress interface (`iifname`)** (by **name**, not ifindex — `iif`'s fail-open is
   in D5 ⑫). Only the side that creates the netns knows **which veth belongs to which sandbox**.
   This is the materialization of pillar ④, and it cannot be moved outside: an external process
   sees only packets, and the source address in a packet is guest-writable (measured, D5 ③).
3. **The L0 floor, with both hops pinned.** Only the engine knows the sandbox address space's CIDR
   and its own host's address. If the engine merely **published** a list for an external process
   to obey on its own, that process would instantly become a trusted party — a confused deputy
   (D4.2).
4. **Rule ordering.** A publish's reverse flow must not become an egress path; deny must be the
   final catch-all; the allowance for "exempt from being hit again by one's own rules" must sit
   before the steering rules, and the floor's drop must sit above it (D4.2). **Only the party that
   writes the whole rule set can guarantee the ordering**, and the symptom of a wrong ordering is
   "it works" — no error.

> **The precise statement**: the **declaration** comes from outside the engine; the **enforcement
> mechanism** lives inside the engine and cannot be moved out. The engine is not "managing the
> network" (a product-layer sense); it is the only party that **can** enforce — because it is the
> party that creates the namespace.

**The converse needs stating just as clearly: everything outside narrowing lives outside the
engine.** Choosing a backend by SNI, swapping credentials, terminating TLS and re-originating,
validating a token by caller — none of these needs any of the capabilities above, so none of them
is inside the engine, and the engine holds **no seam that could carry them**. This is not "not
implemented yet" — it is the boundary (§1.4 permanently excludes the whole credential family).

**One thing to add so "the engine does not know credentials" is not misread as a gap**: §1.4
already gives the conclusion "the engine's storage semantics contradict keeping a secret", and its
actionable side is — the template holds the address of the endpoint outside the engine plus a
value that satisfies the SDK's credential probe, and neither is a secret to the engine, so there is
nothing wrong with the engine storing it. **A real credential is kept out of the template by
construction, not kept safe by the engine.**

---

## D2. The load-bearing wall is the IP layer; domain names do not enter the engine

The engine's enforcement evaluates only in IP / port / protocol. Control at the domain-name layer
**is not inside the engine**, and the reason is **cost**, not infeasibility — this must be stated
precisely, because "infeasible" would be wrong:

- **Domain churn is solvable, and someone has solved it.** One working approach: a DNS proxy
  intercepts resolution and writes the result into a dynamic allow set, whose entries expire per
  the DNS TTL plus some margin, layered with a liveness refresher for active connections
  (addresses with a live connection get re-stamped periodically). The mechanism works.
- **But it requires growing three things inside the engine**: a DNS proxy, a TTL refresher coupled
  to conntrack, and a product-layer domain list that churns. The third breaks §1.1 outright (the
  engine holding a product-layer list); the first two turn the engine into a DNS client. **What
  breaks is §1.1, not feasibility.**
- One incidental semantic flaw that road carries: liveness refresh lets **an authorization outlive
  the name it was granted under**.
- **The other side of the same wall**: mature portable network-policy layers likewise cannot
  express a hostname — narrowing by domain requires bolting on a platform-specific mechanism. This
  is an independent confirmation that "domain names do not belong to the portable layer".

For narrowing like "only this one domain", use `via[]` to hand the traffic to a process outside the
engine that judges by application-layer information (§5.D).

**The strength of domain-layer control varies with the declaration's value, so the spec states it
by value rather than as a global conclusion.** Under `default:deny` plus a single `egress.via[]`,
the guest's reachable set is approximately one address, and both DoH and a direct IP connection
**are blocked by default-deny**; under `default:allow` both escapes **fully revive** (the guest
hides its query inside TLS, or dials an IP literal directly — the engine sees neither and should
not).

**Tuple vs port-independent**: our allowance is the `{cidr, port, proto}` triple. **The counter-
evidence is worth recording**: the implementation that took the working road above has an allow
set that is **port-independent** (the address is in the set, and any port to it is allowed) —
looser than ours. We take the stricter side, at the cost of a more verbose declaration.

---

## D3. Three mechanical constraints on the declaration's shape (why the four fields are shaped this way)

### D3.1 `proto` is a formatting requirement, not an optional dimension

Every steering rule surveyed **has a protocol match, without exception**. The reason is
mechanical: in iptables, port matching is an option of the `tcp`/`udp` match extension (writing
`--dport` without `-p tcp` gives `unknown option "--dport"` directly, and order matters); in
nftables, `tcp dport` **automatically generates an implicit protocol dependency** (not shown when
listing rules); the options for transparent-proxy-style targets likewise explicitly require the
rule to also specify `-p tcp` or `-p udp`.

So `{cidr, port}` is not a well-formed tuple; `{cidr, port, proto}` is. A side benefit: the
pitfall of "port doesn't stack over 80/443, you need two rules for both" is **structurally absent
here**, because port and protocol are always explicit.

**This locks down only `proto`, not `port` — and the difference between the two comes directly
from this mechanism.** A tuple missing `proto` **cannot be rendered** (the interfaces above don't
even accept `--dport` without it); a tuple missing `port` renders fine, it just **emits one fewer
`dport` clause** — meaning "any port to that destination", the same mechanism's second application
as omitting `cidr` giving "any destination". So omitting `port` is **a legitimate wildcard**
(§5.D), while `proto` has no wildcard to speak of.

### D3.2 UDP is not steered, and that is not an oversight

`via[]`'s matching does not hold for UDP, and the reason is mechanical: **retrieving the "original
destination" has no usable interface on UDP.** This must name the interface to be precise —
**measured** (D5 ⑥): `getsockopt(SOL_IP, SO_ORIGINAL_DST)` fails on a UDP socket unconditionally
(`ENOPROTOOPT`, consistent before and after receiving packets) — that half is "unsupported". But
**another interface exists, and it is worse**: the `IP_RECVORIGDSTADDR` / `IP_ORIGDSTADDR` cmsg
**succeeds under redirection, and what it returns is the post-translation local endpoint** (the
guest sends to `1.2.3.4:443`, the cmsg reports `10.199.7.1:18443`) — **indistinguishable** from "no
NAT happened at all". The implementations surveyed agree: mesh-style implementations ship zero UDP
steering rules (only in an opt-in DNS scenario); the other implementation surveyed simply keeps
UDP out of conntrack.

**Where the security argument actually rests must be stated precisely.** It is not "the interface
doesn't exist, therefore safe" — that cmsg interface above exists and **silently returns the wrong
address**; a UDP interceptor written against it cannot tell "no NAT" apart from "NAT'd but the
destination is already lost". What actually blocks it is **our default-deny: UDP that was not
steered is dropped outright, so it never reaches that interface at all**. The direction is the
opposite of mesh, and safer (in mesh, UDP that was not steered **still reaches its destination**,
fail-open), at the cost that **QUIC fails silently instead of degrading** (clients usually fall
back to TCP, **but this is not guaranteed**; an HTTP/3-only upstream fails outright). This must go
into the outward-facing docs, or it presents as "some sites are mysteriously unreachable".

**The same cost bites harder on DNS, because DNS is almost entirely UDP.** Measured (D5 ⑦): under
`default:deny`, when only TCP/53 is allowed, that allow rule **receives 0 packets** while the
UDP/53 drop counter keeps climbing — the resolver **does not fall back to TCP on its own**, so
ordinary tools (`curl`/`git`) first hang until resolution times out, then report **name resolution
failure**, not a policy refusal (the same root cause as D4.3's diagnostic dislocation). So **to
have usable DNS under `default:deny`, the caller must explicitly write the resolver's address, with
`proto: udp`, into `egress.allow[]`** — this is something the engine **deliberately does not do on
the caller's behalf**: doing it would require the engine to know the concept of "resolver" and hold
a deployment-specific address, breaking §1.1 (the same criterion as D2's refusal of domain names).
It is therefore a **user-visible semantic**, belonging to §5.D's "four user-visible semantics"
group — not a gap.

**One thing that could not be confirmed**: neither of the two mesh projects surveyed gives a
written rationale for excluding non-DNS UDP. The behavior is confirmed from source; "this steering
semantic does not fit UDP" is an inference, not a citation.

### D3.3 The two interception shapes are two values of the same field, but their intent signal is not equivalent

"Explicit endpoint" and "transparent interception" are not two engine modes — they are two values
of `via[].match`: an explicit endpoint is `match:[{cidr:<some address>, port:443, proto:tcp}]`,
transparent interception is `match:[{cidr:any, port:443, proto:tcp}]`. **The engine changes not a
single line** — it needs to know neither name, and needs no new seam.

But they are not equivalent in **intent signal**, and that is what determines the leaning order:

- **A port-only rule does not lose the original destination — measured** (D5 ⑥). The mechanism is
  hook ordering: conntrack registration's priority runs **earlier than** address translation, so
  it establishes the original-direction entry straight from the packet's raw header — at that
  point no translation rule has been evaluated yet. A rule's match clause only decides whether the
  translation fires, **it plays no part in building that entry**; after translation only the
  reverse-direction rewrite is recorded, **the original is never overwritten**. This is exactly the
  mechanism every general-purpose transparent proxy relies on (none of them match on destination
  address).
- **Corollary: transparent interception is strictly superior to an explicit endpoint in intent
  signal.** Under transparent interception, the layer outside the engine gets the **real
  destination**; under an explicit endpoint it gets only an alias the guest **self-reported**
  (SNI/Host). Corroborating evidence: a mature interception proxy's certificate-name priority is
  **not SNI-first** — it connects upstream first, sniffs the real certificate to take the name as
  the baseline, and the client's SNI is only **appended**. That is, the upstream's real identity is
  primary and SNI is secondary. So under transparent interception that layer can **verify** the
  guest's declaration; under an explicit endpoint it only has that declaration's word.

> **Measured data (D5 ⑥).** Under a port-only rule, the original destination recovered across 100
> flows (40 sequential + 60 concurrent) was **entirely correct, zero mismatches**; the variant with
> a destination-address clause produced **byte-for-byte identical** results (a reverse control also
> proved the match clause is actually evaluated: an unmatched destination is not redirected at
> all). **So the leaning order is set by the corollary above, and does not flip.** One co-existing,
> unquantified risk: the kernel's own documentation says that recovering the original destination
> across a redirect on TCP **"is racy"** — this run saw zero mismatches across 100 flows with no
> anomaly observed, but its occurrence rate was not quantified, so it is recorded as "documented,
> not quantified", not written down as a number.

**Both values are kept**, but the leaning order now has grounds: explicit endpoint goes first (the
layer outside the engine does not need to terminate TLS), transparent interception is the stronger
tier.

**The capability boundary of that layer has been measured by others, and is recorded here so
`via[]` is not mistaken for a cure-all** (all of it lands outside the engine and does not affect
it): the transparent-interception shape **does not support UDP**; QUIC/HTTP3 is unavailable in that
shape; a plain TCP pass-through **cannot detect a STARTTLS upgrade** (the closest documented
warning to "in-protocol handshake" cases like database connections); h2c is unsupported; DoT/DoH/DoQ
have not been started. **TLS client certificates / pinning could not be confirmed** — undocumented,
marked unknown rather than "supported".

---

## D4. Invariants that must be held (the normative statement is in §2.3 / §2.4 / §5.D)

**Map onto §2.4's three items first, rather than leaving it to the reader to piece together.**
§2.4 lists **the L0 floor / both hops / fail-closed**: the first item's reasoning is not in this
section, it is spread across **D1 ③** (why only the side that creates the placement can pin it),
**D2** (why the load-bearing wall is the IP layer, and the floor only admits things nameable in IP
vocabulary), and **D5 ④** (why the sibling item is a static CIDR); this section develops the
other two (D4.2 / D4.3), plus one item that is **the precondition shared by all three** (D4.1).

### D4.1 Narrowing must be unbypassable (must be exhaustive)

The obligation is **not** "can reach the endpoints the declaration allows" — it is **default-deny
plus allow only the declared set**. Fail to hold this and the declaration is empty in effect: a
direct connection inside the guest that routes around the meeting point, or a protocol/port the
rules do not cover, bypasses the whole §5.D declaration surface. **This is the precondition each
of the other three rests on, not a fourth item alongside them**: the floor can only be "always
deny" if narrowing is exhaustive (otherwise one uncovered port is a side door around it), and the
same holds for both hops and fail-closed — in a non-exhaustive rule set, each of them guards only
the traffic it happens to cover. It is therefore an acceptance-testable security obligation
(§2.4):

> Narrow the isolation unit's reachable set exhaustively down to the set of endpoints the engine
> declares, and default-deny everything else outbound. **An escape is a failure**, not a
> degradation.

**"It works" is an insufficient acceptance assertion**: a rule set with the wrong ordering behaves
completely normally under ordinary traffic, and differs only once it is bypassed. So acceptance
must assert the property that **would pass through silently** (the same acceptance discipline as
§5.C's mis-sweep item).

### D4.2 Both hops are pinned, and the second hop must be enforced by the kernel

Egress has two hops, and missing either one is a leak:

1. **The guest's own egress**: default-deny plus allow only the declared set. What this blocks is
   the guest **bypassing the meeting point with a direct connection** (a direct dial to a sibling,
   a direct dial to link-local included).
2. **The egress of the engine-external process sitting behind `via[].endpoint`**. It originates
   the connection on the guest's behalf, so it is the second hop.

**The first hop itself splits into two hooks, and the predicate shapes on the two sides are
opposite — this is a measured structural fact (D5 ⑧), not an implementation detail.** "The
guest's egress" lands in **two mutually exclusive paths** in the kernel, and the rule generator
must produce output per hook separately:

| Destination category | Which hook it lands on | The floor predicate on that side |
| --- | --- | --- |
| Any address of the host **itself** (veth IP / eth0 IP / the host's own link-local) | `input` | `iifname` + **pure ct-state, with no daddr list at all** |
| Something the host will **route** onward (sibling / transit link-local / private internal / public internet) | `forward` | ct-state + **an explicit daddr floor list** |

**Settled: the declaration's allowance is also emitted on `input`, and that side carries its own
sibling drop.** The generator emits the declaration's allowance **on both `input` and
`forward`**; the `input` side **must also** carry a sibling drop, or an `allow 10.0.0.0/8` would
let sandbox A reach sibling B's **gateway** (measured: banner read back, while the sibling drop on
the `forward` side **counts 0** for this path — it does not sit on `forward`). **Reason**: §5.D
semantics ③ promises the caller can declare its own resolver, and the only stably-reachable
host-local resolver address is this sandbox's own `/30` gateway (D5 ⑦), which lands on `input`; an
allowance emitted only on `forward` **cannot reach it**, and that promise would have no delivery
path. **This does not conflict with ⑧**: ⑧ says the **floor** needs no daddr list on the `input`
side (a pure ct-state rule already exhausts the host's own addresses); this sibling drop is
**a different rule**, predicated on the sibling CIDR, not hanging a daddr list on the floor. Two
rules, not one.

**This table has a real tension with `via[]`, measured in both directions, and the shape of the
narrow allowance has been settled (D5 ⑩).** `via[].endpoint` is contractually **required to be
host-local** (otherwise the kernel cannot hold down the second hop — one of the entries in §6.2's
400 list), and steered traffic, after `prerouting` translation, **lands on `input`** and is `ct
state new` there — exactly what the `input` row's pure ct-state floor above would drop (measured:
the `input`-side dnat observation counts 8 packets, `forward` counts 0). So **a pure ct-state
floor would kill the entire `via[]` feature**, and the rule generator **must** place one allowance
above the floor drop; and that allowance **must pin three things at once** to avoid both killing
the feature and reopening the floor — **the shape, the counter-examples item by item, and "the
`forward` side needs its own default-deny" are all in D5 ⑩.**

**The `input` row's "covers every host address" has been independently measured on IPv6, and the
conclusion is that it covers it (D5 ⑪).** A pure ct-state `inet` rule **with no family
qualifier** **also matches IPv6** and drops it, and the `established,related` return arm needs
**only one shared rule** to carry v6 return traffic too; an explicit second `meta nfproto ipv6`
arm is **redundant** (measured: the packets it would count are **byte-for-byte identical** to what
the family-blind drop already caught). **The strongest form of the test**: the guest crafts its
own global prefix (`fd00:dead:beef::2`) and dials it — still blocked, counter still climbs — that
is, this rule holds even on an address space the **orchestrator has no way to enumerate**, exactly
the property it needs. So `fe80::/10` in the L0 floor list only needs to remain a `forward`-side
daddr item; the `input` side does not need it.

> **But "the engine's IPv6 exposure today" is a separate matter**: the engine never touches IPv6
> (zero hits on ipv6/inet6 in `local_runsc.go` / `oci.go`), and `disable_ipv6` defaults to 0 ⟹ the
> kernel auto-configures a host-side `fe80::/64` on every sandbox veth, and measured, **with no
> floor installed, the guest can reach not only the host-side veth's link-local address but also
> `eth0`'s global IPv6**. (The negative case was also measured: `eth0`'s **link-local** address is
> **unreachable** from the sandbox — link-local is only reachable on its own link — so the real
> exposure surface is that global address, which happens to be exactly what the `input` side's
> list-free rule covers.) **Once the floor is in place, these two cells are delivered differently,
> and per D5 ⑳'s discipline they must be recorded separately**: the host-side veth's **link-local**
> cell passes by **the floor's counter moving** (`TestIPv6IsCoveredByTheSameFloor`); `eth0`'s
> **global v6** cell is today **unreachable with a zero count** — the engine never configures a v6
> address on the guest side, so that packet **never leaves the sandbox in the first place**, and
> what blocks it is not the floor. **It is therefore not an assertable enforcement**, only a
> consequence of the current configuration: the day the guest side gets a v6 address, the same
> family-unqualified ct-state rule will catch it (D5 ⑪), and until then "count is 0" and "the floor
> was bypassed" are **indistinguishable** on this cell — so acceptance only asserts the structural
> half (no `nfproto ipv6` arm in the rendered output).
>
> **"Just turn off the veth's IPv6" is a viable defense in depth, but only its host-side half holds
> up (measured, D5 ⑪).** One end suffices, **taking effect even when set after the interface is
> already up** (no need for a `default`-scoped sysctl, so the engine need not touch global host
> state), and it survives a link flap; **but guest root can flip its own half back with one
> command** and restore full v6 reachability, while the host-side half is **invisible and
> unwritable** from inside the netns. So it is optional hardening, **never the mechanism** — the
> mechanism is that `inet` floor.

⟹ **The host's own endpoints are never computed**, and with them the whole class of "did the host
address change, must we re-render?" questions disappears (measured, D5 ⑧). **And the uid/gid
predicate discussed below in this section lands on `output`, not `forward`**: the second hop is
originated by a host-local process, so it traverses neither the guest's `iifname` **nor `forward`
at all** (locally-generated packets travel `output`/`postrouting`). Measured, all three shapes
**count uniformly 0** on `forward`, while the same flow is fully visible on `output` (D5 ㉔). ⟹
**The two-hook table above covers only the first hop; the second hop is a third base chain on a
third hook.** Reading it as "the `forward` row incidentally covers the second hop too" yields a
rule set that looks complete while the second hop is never evaluated at all — and the symptom is
**the floor's counter not moving**, exactly the silent pass-through D4.1 named.

**The way to hold the second hop down is to pin it behind the same host rule set, not to hand it a
list to obey on its own.** The difference is load-bearing: handing it down means that process
becomes a trusted party, and stuffing an "upstream" pointing at a sibling or link-local address
into it would then walk straight through the floor's side door (a confused deputy). Because the
sandbox address space is a **fixed CIDR** (D5 ④), this is a **static** rule, not a dynamic
membership set that needs maintaining.

**This identity predicate serves two mutually independent needs, at opposite positions with
opposite actions — writing the order wrong fails silently:**

| Need | Why | Rule action |
| --- | --- | --- |
| **Exempt from being hit again by its own rules** | If that process's egress hits the steering rules again it **forms a loop** | Allow / `RETURN`, placed **before** the steering rules |
| **Still bound by the L0 floor** | Confused deputy (previous paragraph) | Sits **inside** the floor rules, not exempted |

Same predicate, two opposite positions: placing the "exempt from steering" allowance **above** the
floor is equivalent to exempting the floor along with it, and the symptom is only "it works". So
both must be asserted.

**But the two rows' actions are not symmetric, and the measured reason is not at the design level:
`skuid` unreliably matches every packet of a flow.** A direct reverse measurement read "all 5
packets had an owner", while the same flow, across two other runs, read `skuid` matching only 4/5
— that is, occasionally one packet arrives without a socket owner (a FIN sent after the process has
exited is a plausible candidate, not pinned down; D5 ㉔). **The consequence lands only on the
accept/`RETURN` side**: a `skuid`-narrowed **drop** is safe (dropping the SYN kills the whole
flow), but a `skuid`-narrowed **`RETURN`/accept placed above the floor drop** is not — the packets
`skuid` fails to match **pass straight through it and land below**. ⟹ **The first row of the table
above cannot be written as an allow relying on `skuid` alone**: either pin some other invariant of
that flow at the same time (the destination tuple), or accept that "loop exemption" must rest on a
different mechanism. **This is enforced by a render guard**: `ruleset.Validate` refuses a
uid-narrowed accept, permitting only drop (`internal/runtime/nftables_test.go`'s second-hop guard
asserts both directions).

**The predicate is `uid`/`gid`, not mark.** The criterion: uid is **a host-side fact that process
cannot change**, while mark is something we write ourselves — one more step that can be written
wrong. (Corroborating: mature mesh implementations' primary predicate is uniformly owner-uid; some
use no mark at all, and those that do use it only for loop prevention.)

**There is one precondition above that was left unstated — spelling it out here is not a new
mechanism, just writing down something already being relied on.** A `uid`/`gid` predicate only
holds when **the process being evaluated is on this same host, and its uid/gid is known to the
engine**. But the four-field declaration carries only one address, and §5.D insists the engine
neither knows nor asks what sits behind `via[].endpoint` — put the two together, and the argument
above delivers less than it appears to. The fix has two halves:

- **uid is a deployment obligation**: handed to the engine by the deployer, not carried by the
  declaration (the engine still does not name that layer — it only knows "the egress of this uid
  on this machine must be held behind the floor"). **This section has been writing `uid`/`gid`
  throughout, but only `uid` survives at the hook** — D5 ㉔ measured that the only usable
  narrowing predicate on `output` is `skuid`, with no corresponding `skgid` clause, so the gid half
  has no materialization surface in this mechanism (rendering it would only be possible by ANDing
  it onto the floor drop, narrowing the floor further). ⟹ **The deployment input is uid only.**
  **And it must not equal the engine's own uid**: the engine's transport endpoint is a guest veth
  address, dialed from the host side, and that address falls inside the floor's sibling item
  (`10.66.0.0/16`), so that value would make every one of the engine's own SYNs to a guest hit the
  second-hop floor — the `hop2_established` return arm **by design** does not accept it (a SYN is
  `ct state new`). The consequence is that readiness / liveness probing / all RPC break for every
  sandbox at once, presenting as an **intermittent fault** because `skuid` matches only about 4/5
  of packets. ⟹ **Refused at startup, before the table is installed.**
- **A `via[].endpoint` that is not host-local is refused**: it is out of the kernel's reach and
  cannot be held down; accepting it would rest the floor on that process's own self-discipline —
  exactly the confused deputy this invariant exists to refuse. So this class of declaration is
  **refused outright** (400; the error code belongs to [`§6.2`](../api-contract.md)'s list, not
  duplicated here).

### D4.3 Fail-closed (scoped per-connection)

The normative statement is in §2.4 (refuse that one connection rather than falling through to the
next rule and being let out, scoped per-connection). This section adds only three things that
statement does not carry on its own:

**It must be a test, not confidence from reading the code.** The symptom of a wrongly-written
rule set on exactly this point is "it works more" — no error — the same acceptance discipline as
the end of D4.1.

**Diagnostic signal dislocation, which the engine side needs to be able to explain clearly**: when
an endpoint is unavailable, what the guest side sees may be a **resolution failure** rather than a
policy refusal (name resolution itself needs egress). So "endpoint unreachable" must be
attributable, or it presents as a domain-name problem. **The measured numbers are in D5 ⑦** (a hang
of ten-odd to twenty seconds, then `Could not resolve host` — the two figures are not duplicated
here) — **this dislocation is not a theoretical risk**.

**Only `reject` delivers the "clean connection failure" half of the promise; `drop` does not**
(D5 ⑨). The gVisor netstack **honors ICMP errors**, so the same "refusal" written as two different
actions gives the guest an experience that differs by orders of magnitude:

| Rule action | What the guest sees |
| --- | --- |
| `drop` | `i/o timeout` after **20.0 s** |
| `reject with icmp admin-prohibited` | `connect: no route to host` in **0.002 s** |
| `reject with tcp reset` | `connect: connection refused` in **0.001 s** |

So a floor made entirely of `drop` **satisfies the safety half of fail-closed but violates the
guest-experience half it wrote down itself**; both halves are needed, so **refusals facing the
guest use `reject`** (security is unaffected: neither one lets anything through), and `drop` is
reserved for cases that owe the guest no feedback. **A separate rule on UDP**: `reject with icmp
admin-prohibited` is ignored by gVisor on UDP (still hangs 5 s); only `icmp port-unreachable` is
honored (0.001 s `connection refused`).

> **Both arms have now been reproduced on runsc**, with every other condition pinned identically
> (the same target `169.254.169.254:80`, the same running guest, only the action changed): the
> `reject` arm is **0 ms** — asserted as a standing test by
> `TestGuestFacingRefusalsFailFastRatherThanHanging`; the `drop` arm is **≥ 25 s** (the probe's own
> `DialTimeout` expires first, so that is a lower bound rather than the measured hang duration).
> The 20.0 s in the table above comes from a separate measurement using a different dialer timeout,
> and is consistent with this. **Acceptance still asserts by the order-of-magnitude gap
> ("sub-second vs tens of seconds"), never pin a millisecond figure into the assertion** — pinning a
> specific number buys nothing and only makes the test brittle. The `drop` arm is **deliberately
> not kept as a standing test**: every run wastes 25 s hanging, and the only conclusion it supports
> (use `reject`, not `drop`) is already encoded in `ruleset.Verdict`.

**One deployment constraint the engine does not validate**: if the engine-external process needs
to make its own outbound query (which it usually does), and its own egress has already been pinned
behind the floor by D4.2's second hop — then whatever endpoint it queries **must not fall inside
the L0 floor**, or the second hop would lock it out of itself instantly. This is **a constraint the
deployment-layer operator must hold, not something the engine can check**: the engine does not
know what sits behind `via[].endpoint`, still less who it turns around and queries (§5.D). Recorded
here because it is the **only** coupling between the floor and that layer that bites, and its
symptom (that process itself being unreachable) looks nothing like something the floor caused.

**The `reject`/`drop` discipline above has a scope, and `via[]` sits partly outside it.** For a
refusal the engine evaluates itself, the choice above is exhaustive. But under `via[]`, the
guest's local handshake completes against the engine's own DNAT target **before** the
engine-external process has looked at anything — from the guest's side that connection already
succeeded, and any refusal now happens one hop further out, inside a process this document already
established the engine neither runs nor can see into (D1, D4.2). So **the engine's `reject`/`drop`
discipline covers only the refusals it evaluates itself; diagnostic quality for a `via[]`-scoped
refusal is that external layer's responsibility, not an inherited guarantee from this section.**
This is not a new mechanism, only stating a boundary this document's reasoning already implies.

---

## D5. Cross-sandbox network isolation, and the measurements that must be inherited

Cross-sandbox **network** isolation is an **independent control-plane invariant** that does not
ride on pillar ①'s back — why pillar ① cannot hold it, conclusion and reasoning both, is in §2.3
(one sentence: it virtualizes syscalls, it does not intercept network packets). What this section
gives is **the other half** of that conclusion: since it can only be materialized by §5.D's rule
set, then **where the rule set must land, what the predicates must bind to, and which members the
floor must hold** all become questions falsifiable by measurement — the items below are exactly
those questions' answers, measured on `sbx`. They are this invariant's materialization evidence,
not a string of corroborating anecdotes. **Do not rediscover them a second time.**

> **The numbering is one global sequence; the subsections exist only for navigation.** A citation
> always writes a numeral like `D5 ⑦`, never a subsection number — when a fact's subsection
> ownership changes, a citation by numeral does not go stale. **The direction split is
> load-bearing**: an egress hook-ownership conclusion **does not carry over** to ingress (⑮ is
> exactly that counter-example), so the two groups must not be read interchangeably.
>
> **Numerals are assigned in the order each fact was measured; subsections are organized by topic
> — so numerals within a subsection are neither contiguous nor monotonic, and that is deliberate,
> not a missing page.** Doing it the other way (numbering to follow document order) would require
> renumbering every existing item on each addition, and a citation elsewhere by numeral **would
> not error, it would silently point at a different item** — word for word the same reason
> open-decisions' closing note refuses citation by position.

**Reproduction provenance comes first, because "do not rediscover" only holds for the items that
can actually be rerun.** Items marked `not checked in` were measured on a one-shot fixture that
**no longer exists** — their conclusions still hold (each item below carries its own mechanical
explanation), but re-confirming them requires rebuilding the measurement rig from scratch, not
rerunning a script:

| Numerals | One line | Reproduction script |
| --- | --- | --- |
| ⑬⑭⑮⑯ | publish needs an accompanying SNAT · the host's own dial test needs an `output`-side DNAT · post-translation traffic lands on `forward` · binding loopback is indistinguishable from no listener | `test/preflight/spike-publish-ingress.sh` (except ⑬'s DNAT-only control, see that item's ⚠) |
| ㉒ | Of the three things a control-plane restart's rule-set handling might do, only "a whole atomic reinstall" keeps flows alive; flushing the ct table is a live-connection killer | `test/preflight/spike-ct-adopt.sh` |
| ㉔ | The second hop is visible only on `output`; all three shapes count 0 on `forward`; the only narrowing predicate on that hook is `skuid`, and it misses matches | `test/preflight/spike-hop2-hook.sh` |
| ⑲⑳㉓, ㉑'s third group, ⑤'s tier paragraph | v4-mapped need not be a rule member (the danger moves to the validation layer) · `forwarding=0` is not the mechanism that guards the host's own v6 addresses · the NAT64 prefix is family-blind to v4 vocabulary, but dropping the whole range refuses public egress · a partial overlap between the floor and the tier is resolved by ordering · under `default:allow` only the tier catches the host LAN | `test/preflight/spike-denysafe-tiers.sh` (except ㉓'s end-to-end bypass, see that item's ⚠; ⑤'s reachability half is still its own runsc measurement, see that item's ⚠) |
| ⑥⑩⑪⑫⑰⑱, ㉑'s first two groups | Port-only retains the original destination · `via[]`'s narrow allowance's three clauses · one family-unqualified `inet` rule covers IPv6 · `iif`'s fail-open · an interval set refuses nested members · a destination inside the floor dies via `allow[]` / lives via `via[]` | **(one-shot fixture, not checked in)** |
| ①–⑤⑦⑧⑨ | sibling reachable · netns-internal hooks count 0 · `saddr` forgeable while `iif` is not · the sandbox address space is a fixed CIDR · the three egress mechanisms · DNS under `default:deny` · the first hop's hook split · `drop` vs `reject` | **(one-shot fixture, not checked in)** — except ⑤'s tier paragraph, see the row above |

### D5.a Outbound

**① A point-to-point `/30` veth does not isolate a sibling.** **Removing L2 adjacency does not
remove reachability** — the host **routes** between two `/30`s (`ip_forward=1`), and east-west
traffic needs no L2 adjacency at all. Measured: two real runsc sandboxes, each with its own `/30`
pair, no shared bridge (i.e. `local_runsc`'s shape today) — sandbox A **completed a TCP handshake
to sibling B's guest IP and read back its payload**. So folding the sibling address space into
the L0 floor is this invariant's **only materialization, not a backstop**; the point-to-point veth
is **one layer of depth defense** (it closes off "direct connection on the same L2 segment" as one
path), not the materialization.

**② Rules inside the sandbox netns never see guest packets.** The gVisor netstack injects at L2
(`fdbased`/`SOCK_RAW`), so guest packets **never traverse that netns's L3 hooks**: measured, 5
packets count **+0** on the netns's `inet output`/`postrouting`, +5 on `netdev egress`, and are
fully visible on the **host-side** `prerouting`. So enforcement can only hang on the **host-side
veth** or the `netdev`/`egress` hook — **transparent interception placed inside the sandbox netns
does not hold**.

> **This also explains why the most complete open-source implementation surveyed is not portable
> to us.** That implementation hangs its whole chain **inside the sandbox's own netns** (a sidecar
> sharing the netns). That holds for an ordinary container (the process really is in that netns);
> for a gVisor guest it **counts 0**. So "host-side veth" is not a style preference — it is the
> direct consequence of this measurement; that implementation's placement cannot be copied.

**③ The source-address predicate can be forged by the guest; the ingress-interface predicate
cannot.** Guest root can add an address to its own NIC (measured `ip addr add` rc=0), which
**bypasses the `saddr` predicate** (3 packets hit the "bypassed" counter); the `iif` predicate
**blocks it entirely** under the same attack. This is the measured basis for pillar ④.

**④ The sandbox address space is a single fixed CIDR.** Every sandbox's `/30` is carved out of one
fixed `/16` in `local_runsc`, so the L0 floor's sibling item is a **static CIDR**, not a
runtime-maintained membership set — it can therefore live in a dependency-free leaf package,
shared identically by both hops, with no cross-process distribution mechanism needed.

**⑤ None of the "unreachability" with no rules installed is enforcement; the number of escape
steps varies by target.** To the host LAN and to a sibling: one `ip route add default via <host>
dev <veth>` inside the guest suffices (**it must carry `dev`** — without it, rc=0 but it does not
work). True egress needs one more thing, host-side SNAT — unreachability on the bare base is "no
default route" **and** "no SNAT" together, and both are only **defaults**, so **rules are the only
enforcement surface**: this is the concrete source of D4.1's "narrowing must be unbypassable" on
the egress side. **Egress needs exactly three host-side mechanisms, not one more** (measured, a
real runsc guest completed TLS and `git ls-remote`): `ip_forward=1` (global) · a guest-side default
route carrying `dev` · host-side masquerade for that `/30`. **The negative case was also measured**:
tightening `rp_filter` to 1 **has no effect** (the return route already goes out that veth), and
`proxy_arp` / `accept_local` / per-interface `forwarding` are **all unneeded** — don't add them to
the engine.

**Once these three are in place, `egress.default: allow` really does hand over the host LAN, so
the internal deny-safe tier is not a convenience — it is the delivery surface of that promise.** A
supplementary measurement (`spike-denysafe-tiers.sh`'s control arm): with all three mechanisms in
place, the floor installed and no tier, traffic from a sandbox to another host-LAN machine's sshd
(an RFC1918 address plus tcp/22) **leaves the netns, gets routed by the host, and on the `forward`
hook only a rule written in RFC1918 vocabulary catches it** — none of the floor's four items
(link-local / loopback / any address of the host itself / the sibling sandbox address space,
ordered per §2.4) covers that address. ⟹ **Without this tier, `default: allow` hands the entire
host LAN over to the sandbox**; whether this tier **exists** is therefore not an open question
(only its membership criteria are, see D8).

> **⚠ That supplementary measurement is a visibility control, not an independent proof that "the
> host LAN is reachable".** What it asserts is that **the tier rule saw that traffic** (the
> RFC1918 counter moved); the dial-test's outcome is recorded only as a note. **The reachability
> half's evidence comes from ⑤'s own runsc measurement** (a default route carrying `dev` is
> already enough to reach the host LAN). **The enforcement half has its own paired assertion**:
> after adding `ip daddr 192.168.0.0/16 drop` to the same rule set, the same LAN peer **cannot be
> dialed and the counter climbs**, so "the tier catches it" is measured, not inferred. The
> measurement rig is an ordinary netns peer (the same permitted reason as ⑩⑪⑫).

**⑥ Port-only redirection retains the original destination; UDP's danger lives on a different
interface.** Both halves' evidence and reasoning are in D3.3 / D3.2, not duplicated here.

**⑦ Under `default:deny`, DNS must be explicitly declared UDP by the caller, or it hangs first
and only then reports a resolution error.** With only TCP/53 allowed, that allow rule receives
**0 packets** while the UDP/53 drop counter climbs — the resolver **does not fall back to TCP**.
The observed symptom is `getent` hanging **20.06 s**, `curl`/`git` hanging **10.08 s** before
reporting **`Could not resolve host`** — **not** a policy refusal (an instance of D4.3's
diagnostic dislocation). **But before discussing policy, one more basic fact needs recording
first: the guest today has no resolver configuration at all.** `build-rootfs.sh` writes only
`/etc/passwd` / `/etc/group` / `/etc/hostname`, **never writes `resolv.conf`**, and nothing
anywhere in the repo mounts or generates one (measured, `grep -rn resolv internal/ cmd/` gives zero
hits). So today, inside the guest, `curl` reports `Could not resolve host` directly, and Go
programs fall back to `[::1]:53` and get refused — **DNS simply does not work inside the guest
today, and that predates any §5.D policy question**. Who writes that `resolv.conf` is a
**product/template-layer content choice** (the same class as open-decisions' "§5.A / Template
(content catalog)"), not engine semantics; but **it is a precondition for the act of "declaring
DNS"**, so it is recorded here.

Three more items on the relationship with the floor need recording precisely: **a loopback
resolver (`127.0.0.53`) is "structurally unreachable" rather than "blocked by policy" for a gVisor
guest** (with zero rules installed it lands on the guest's own netstack loopback and never leaves
the sandbox at all — the host-side `daddr 127.0.0.0/8` counts **0** while link-local counts 5 in
the same window), so the floor's loopback item is **redundant defense in depth** for a gVisor
guest; **the only stably-reachable host-local resolver address happens to be that sandbox's own
`/30` gateway**, which **falls inside the sibling CIDR** — a naive `drop 10.66.0.0/16` would kill
it too (measured), so the floor must allow "this sandbox's own gateway" **before** the sibling
drop; that gateway lands on `input`, so **§5.D semantics ③'s delivery path is the `input`-side
declaration allowance** (the hook question is already settled, see D4.2; before that it had no
delivery path); and **whether the resolver address falls inside the floor is deployment-specific**:
on this machine the resolver is `0.250.250.200` (inside `0.0.0.0/8`, neither loopback nor
link-local), **not** in any floor item, so **on this machine it is declarable** — but it would not
be declarable on a host whose resolver is `127.0.0.53` or a link-local address. **This is a
property of the deployment environment, not a defect of this engine**, and the conclusion cannot
be turned around into "the floor's design has a problem".

**⑧ The first hop splits into two hooks, `input` / `forward`, and the `input` side needs no address
list.** Anything to the host **itself** (veth IP, eth0 IP, host-local link-local) lands on `input`;
anything the host will **route** (sibling, transit link-local) lands on `forward`. On this basis, an
`iifname`-narrowed **pure ct-state** `input` floor statically covers every host address
(`iifname` is the entire reason it can be a **per-sandbox** rule — remove it and it stops being a
floor and becomes a host-level ingress kill switch, blocking the orchestrator's own `127.0.0.1`
listener and sshd along with it, because loopback also traverses `input`): once installed, the
guest cannot reach a `0.0.0.0` wildcard listener via **any** host address, and a host address added
**after the rule is already installed** is **equally blocked**. ⟹ **The host's own endpoints are
never enumerated.** A floor covering only `forward` **misses guest→host entirely** (measured: the
sibling is blocked while guest→host is `CONNECTED`). **The two failure symptoms are opposite**: a
naive daddr floor placed above an established accept → transport **dies outright** (loud); a wide
accept placed above the floor → **completely silent**, every reachability test still passes, and
the only signal is **the floor's counter reading 0**. So acceptance must assert both "unreachable
from inside the netns" **and** "the floor's counter is non-zero after a deliberate probe".

**⑨ gVisor honors ICMP errors, so `drop` and `reject` differ by orders of magnitude in the guest's
experience** (20.0 s timeout vs 0.002 s `no route to host`; **both arms have now been reproduced on
runsc** — `reject` 0 ms, `drop` ≥ 25 s; the reproduction conditions and why the assertion still goes
by magnitude are in the block at the end of D4.3, not duplicated here). The associated conclusion
"refusals facing the guest use `reject`" is in D4.3. The same round of measurement confirms
**gVisor changes nothing about conntrack or original-dst**: a gVisor-originated HTTP flow's return
traffic counts 7 packets on `established,related` (`invalid`=0, `untracked`=0), and removing the
ct-state allowance **breaks even an allowed flow** (proving it load-bearing); port-only redirection
recovers the real destination for a gVisor-originated connection just as well (three different
targets — `1.1.1.1:443` / `93.184.215.14:443` / `8.8.4.4:443` — each correct). **⑥ is therefore
independently reproduced on gVisor, not only a netns-level conclusion.**

**⑩ `via[]`'s narrow allowance must pin three things at once; miss one and there is a measured
counter-example.** The three: the declaration-authorized **pre-NAT destination**
(`ct original ip daddr` + `ct original proto-dst`), the **post-NAT port** it steers to
(`tcp dport <listener>`), and `iifname`. The final shape for the explicit-endpoint form:

```
iifname "<vh>" ct original ip daddr <declared_cidr> ct original proto-dst <declared_port> \
    tcp dport <listener_port> ct state new counter name <cnt> accept
```

The transparent-interception form (`match` is `cidr:any, port:N`) is **missing exactly one
clause**: `ct original ip daddr` is dropped — it must be dropped, because that declaration
authorizes "any destination on that port", and keeping the cidr clause would block legitimate
traffic. ⟹ **The generator emits that clause if and only if the declaration pins a cidr.**

**Four measured counter-examples showing why all three are load-bearing** (each one is "some
simpler way to write it", broken by one concrete flow):

| Simpler way to write it | Broken by what | Consequence |
| --- | --- | --- |
| `ct status dnat tcp dport <listener>` | An **undeclared original port** redirected to a **declared listener** (9200 → :18443) | Allowed, floor counts 0; the listener reads `ORIGDST=…:9200` |
| Pin only `ct original proto-dst` (no cidr) | The same port on a different destination address | Exceeds authorization under the explicit-endpoint form |
| Pin cidr + proto-dst, **no listener port** | A declared original port redirected to an **undeclared listener** (`9.9.9.9:443` → :19999) | The guest reaches a listener **the declaration never named** (banner read back: `hello-from-UNDECLARED`) |
| Drop `iifname` | The same traffic from **a different sandbox** | Sandbox 2's connection is allowed by sandbox 1's accept (both sides' floor counters stay 0) — **each sandbox's own `policy accept` base chain shares the same hook, so an accept with no `iifname` in one table is a hole in every sandbox's floor** |

**Used alone, `tcp dport <listener>` is exactly the "dial the listener port directly, bypassing
steering" side channel (covered by `TestViaSteersAndDoesNotReopenTheFloor`), and as the third clause
it is precisely what plugs the third row of the table above** — the same expression is both
dangerous and necessary, and the difference is that `ct original` has already pinned the pre-NAT
tuple (whereas traffic dialing the listener directly has no conntrack original entry that could
satisfy it). **Do not let anyone "simplify" it away.**

**This item also measured a new trap that would make a counter assertion read as healthy:** that
accept **must be in the same chain as the floor drop**. Placed in **a different base chain on the
same hook at a lower priority**, it **matches — the accept counter climbs — while the traffic is
still dropped by the floor and the connection still fails** — because a base chain's `accept` only
ends **that chain**; the packet continues into the next base chain. **Only a terminal verdict**
(`drop` / `reject`, the latter also returning `NF_DROP` after emitting its error packet)
**terminates across chains.** ⟹ **"the accept counter is non-zero" does not prove the allowance is
effective** — the working side must also assert reachability at the same time.

**⑪ A family-unqualified pure ct-state `inet` `input` rule covers IPv6 along with everything
else.** The conclusion, the strongest form ("the guest crafts its own global prefix and is still
blocked"), and "turning off the veth's IPv6 only has its host-side half hold up (guest root can
flip its own half back)" are all in D4.2, not duplicated here. Two incidental measurement facts:
this machine's **IPv6 conntrack is built into the kernel rather than a module** (so `lsmod` is the
wrong probe; `nf_conntrack_count` went 0 → 8 during the test run), and v6/v4 SYN sizes (80 B / 60
B) together with a family-tagged observation counter **attribute every drop to a definite family**,
rather than inferring from the total. **One more incidental fact**: the host-side link-local
address is derived from the MAC via EUI-64, so it is **deterministic and guest-predictable**, not
a random value.

**⑫ `iif` fails open silently after a veth is rebuilt under the same name; `iifname` does not — the
mechanism is in the bytecode, not an inference.** `iif` compiles to `cmp eq reg 1 0x00000008` (a
**frozen integer**); `iifname` compiles to the string bytes of the name. After a rebuild under the
same name, `iif`'s bytecode is **byte-for-byte unchanged**, still pointing at the now-dead ifindex.
Measured end to end: under an `iif`-only floor, the guest's `HELLO` **was delivered**, with the
drop counter at **0**. `iifname` kept matching normally across the same rebuild cycle. The race
explanation was ruled out: a routing conflict produces a **symmetric** hang (both counters equal),
while a stale ifindex's signature is the **asymmetric** `iif`=0 / `iifname`>0 pattern.

> **There is a trap here that ruins the measurement if missed, recorded for the record**: if the
> test rig does not **unload its own table** while rebuilding the netns/veth, the ifindex in the
> rule still resolves to the **previous** one — from the test's point of view no rebuild happened
> at all, and `iif` then presents as "never matches" instead of "does not match after a rebuild".
> `TestTheFloorStillHoldsAfterAVethRebuild` must assert at the point in time "after a rebuild", so
> it must genuinely reinstall the table.

**Two incidental facts, both affecting how this is written**: ① `nft list` normally **resolves
that frozen integer back to a name**, so the two spellings **look the same** in a listing — only
when no interface holds that index does the bare `iif 12` show through, **so review by reading the
listing cannot catch it**; ② `iif` **fails to load against a nonexistent interface** (rc=1
`Interface does not exist`), and mixing one such rule into an `-f` file makes the **whole atomic
load fail** (leaving no partial table) — the direction is safe, but it removes one degree of
freedom in the ordering of "rules installed before the guest starts running". **One risk that is
unmeasured but worse**: in this run ifindex only increased, never decreased (8→10→12→16), and
reuse was never observed; if, in the future, some **other** sandbox's veth is assigned a retired
index, a stale `iif` rule would start matching **that sandbox's traffic** — not a vulnerability but
a **silent cross-sandbox misattribution**. Unmeasured, but it only strengthens the case for this
guard.

**⑱ An nft interval set refuses nested members, and `auto-merge` silently folds a subnet into its
parent.** Writing a list of addresses containing nested entries (such as the IANA special-purpose
registry) verbatim into a `flags interval` set **fails to load**: `Error: conflicting intervals
specified` (`192.0.0.0/29` is inside `192.0.0.0/24`). Adding `auto-merge` **makes it load**, at the
cost of the subnet being folded away — `{10.0.0.0/8, 10.66.0.0/16}` becomes one entry. ⟹ **The L0
floor and the internal deny-safe tier cannot share one set**: the sibling `/16` falls inside `10/8`,
and sharing would either fail to load or silently fold the floor away (and "folded away" happens
silently). The two can only be **two rules, ordered**. The direction is safe (failing to load beats
loading wrong), but it removes the degree of freedom of "fit everything into one set".

**⑲ A v4-mapped destination is a plain IPv4 packet on the wire, so an `ip6 daddr ::ffff:0:0/96` arm
always counts zero.** A measurement subtlety: a **raw `AF_INET6` socket** must be used, not
`net.Dial` — the standard library normalizes the family away before the kernel ever sees it.
Measured, dialing `::ffff:<v4>`: the `ip daddr <v4>` counter climbs as expected while `meta
nfproto ipv6` counts **0**; putting `::ffff:0:0/96` on its own arm, the same dial counts **0** on
it while the v4 arm counts the full total. ⟹ **The matching layer does not need this member.** **But
the danger has not gone away, it has moved to a different layer**: `netip.Prefix.Contains` does
**not** unmap a v4-mapped address (`169.254.0.0/16` against `::ffff:169.254.169.254` returns
**false**, only true after `.Unmap()`; the older `net.IPNet.Contains`, via `To4()`, **does** unmap
and returns true directly). So a validation predicate that branches on family would judge
`::ffff:169.254.169.254/128` to be v6, compare it only against v6 members, and **let it through** —
and the packet then leaves as IPv4 and dies on the v4 floor. **Accepted by the API while silently
refused on the wire** — exactly D4.3's class of diagnostic dislocation. ⟹ Declaration validation
must **reject v4-mapped / v4-compatible literals before any containment test at all** (not
"normalize, then compare"): §5.D says the declaration only accepts IP literals, and
`::ffff:192.168.1.1` is a **second spelling** of an address that already has a spelling; accepting
it would mean the Binding stored differs from the caller's text, and the two spellings would then
compare unequal on every subsequent read-modify-write. **At the same time**, the predicate itself
still `.Unmap()`s internally, so that if someone ever lifts that refusal, the containment test does
not silently become family-blind.

**⑳ The host's own ULA is still reachable under `forwarding=0`, because that is local delivery to
the host; and the side that sysctl actually governs makes the counters lie.** This machine's
`eth0` has a ULA inside `fc00::/7`; with `net.ipv6.conf.all.forwarding=0` the sandbox **can still
connect to it and read back the payload**, counted on **`input`**. ⟹ **That sysctl was never the
mechanism guarding the host's own v6 addresses** (⑧'s pure ct-state `input` floor is). Only a ULA
that **needs to be routed** (one on another machine) is governed by it, and that side's symptom
must be recorded precisely: with `forwarding=0`, the packet is dropped **before it ever reaches the
`forward` hook**, so **neither the floor's nor the tier's counters move at all** — per D4.1's
discipline, "count 0 plus unreachable" here is **not a pass**, it is byte-for-byte indistinguishable
from "the rule was never installed". **So IPv6's acceptance assertion cannot rest on
"unreachable" alone**: either set `forwarding` to 1 first and then assert the rule blocked it, or
explicitly acknowledge that this cell is testing the sysctl, not the engine. **One design
conclusion attached**: `forwarding=0` must never be written into any obligation as a mechanism — it
is global host state the engine does not own, and any deployment that wants v6 egress at all must
set it to 1, so "protection" and "the feature working" are mutually exclusive here.

**㉓ NAT64's `64:ff9b::/96` is pure IPv6 on the wire, so a v4-vocabulary tier is family-blind to it;
but dropping the whole range is equally wrong.** Measured (inside the same `forwarding=1` window as
⑳ — ⑳'s mechanism established that with `forwarding=0` packets never reach the `forward` hook, so
this cell can only be measured inside that window): the same LAN peer's NAT64 form, under a
**live, counting** `ip daddr 192.168.0.0/16 drop`, is **never seen by that v4 drop**, while an
`ip6 daddr 64:ff9b::/96` arm does count it. ⟹ **When the v4 set is derived mechanically for the v6
side, the NAT64-image half of that derivation has measured grounds.** **But it does not license
"drop the whole `64:ff9b::/96`"**: per RFC 6052, that prefix maps **the entire v4 address space**,
public destinations included, so on a deployment genuinely doing NAT64, dropping the whole range
would refuse the public egress that `egress.default: allow` promises. ⟹ Only the **image of each
member** may be derived, never the prefix itself.

> **⚠ This cell's effect stops at "family and visibility"; an end-to-end bypass was not established
> on this machine.** The enforcement control (an address inside the same prefix with **no
> listener**) **also reports `connected`**: on this machine, `64:ff9b::/96`'s next hop answers the
> handshake on behalf of any address inside the prefix, so "connected" does not prove the LAN peer
> was actually reached, and the script marks this cell INVALID rather than passing, per D4.1. **The
> half that does hold is given by the counter**: the traffic is on-wire IPv6, matched by
> `ip6 daddr 64:ff9b::/96`, while the live v4 drop's count does not move at the same time — but
> **these two readings are printed only as a note in that branch and are not gated by any
> assertion** (the script even backfills the v6-side reading with a stale value so it does not feed
> the downstream drop assertion), so reproducing this means reading the numbers in the run's output
> directly.

**㉑ A destination inside the floor is a dead rule via `allow[]` but live via `via[]` — the same
address, two opposite outcomes; and a partial overlap is resolved by ordering.** Three measured
groups:
- The floor holds `169.254.0.0/16`; a declaration `allow 169.254.169.254:80` placed below it →
  **unreachable, the floor's count goes 0→3, while that declaration's own counter stays at 0.** ⟹
  It is a rule that is **never evaluated at all**.
- The same declaration, using `via[]` instead (a `prerouting` DNAT to a host-local listener) →
  **connected in 1–2 ms, real banner read back, ⑩'s narrow allowance counts 0→1, and the
  `forward`-side floor's count does not move.** Mechanism: DNAT is on `prerouting`, **earlier than**
  the filter floor, and post-translation traffic lands on `input`. ⟹ **`via[]` does not "lift the
  floor"** — the guest never reaches that in-floor address; what reaches it is a **host-local
  process**. So a "refused because inside the floor" validation **cannot be generalized to
  `via[].match[]`**, or it would kill the exact use case of "hand a destination inside the floor to
  a process outside the engine to judge" (a metadata proxy is exactly that). **These two fields
  being validated asymmetrically here is a mechanical fact, not an oversight.**
- **Partial overlap**: the floor holds `10.66.0.0/16`; a declaration `allow 10.0.0.0/8` placed below
  it → an internal address `10.77.250.2` is **reachable** (declaration's count 0→1), sibling
  `10.66.251.2` is **blocked** (floor's count 3→6, declaration's count unchanged), and the sibling's
  gateway is **blocked**. ⟹ Ordering resolves the overlapping supernet, **so any "refused because
  inside the floor" judgment must be written as whole-range containment, never as intersection** —
  using intersection would make a legitimate internal supernet undeclarable.

> **⑩⑪⑫'s measurement rig is an ordinary netns peer, not a real runsc guest** (⑨ already
> independently proved gVisor changes nothing about conntrack or original-dst, so this omission is
> permitted). ⑩'s two new findings — **the cross-chain accept trap** and **"the `forward` side
> needs its own default-deny"** — have already been rerun under runsc
> (`TestOneSandboxsRuleSetDoesNotAffectAnothers` /
> `TestDenyEmptyIsUnreachableEverywhereWithFloorCountersMoving`).

**㉔ The second hop lands on `output`, not on `forward`; and the only narrowing predicate `output`
offers is `skuid`, so that hook needs its own render guard.** Measured
(`test/preflight/spike-hop2-hook.sh`, an ordinary netns peer as the target): a host-local process's
egress **counts uniformly 0 in all three shapes on `forward`** (`skuid` / bare daddr / `iifname`
each 0), while the same flow is fully visible on `output` (`out_any=5`). `postrouting` also counts
0 (the nat hook only evaluates a flow's first packet, and that table has no NAT installed). ⟹
**D4.2's two-hook table covers only the first hop**; a floor written from it (`input` + `forward`,
`iifname`-narrowed) **sees not a single packet of the second hop**, and this leak's symptom is
**the floor's counter not moving** — the silent pass-through D4.1 named. **Three attached
conclusions, all affecting how this is written**:

| What was measured | Constraint on how it is written |
| --- | --- |
| `meta skuid` is evaluable on `output` and **genuinely blocks**: a probe by that uid fails, count 3, while the same probe **run as root gets the banner** | The second hop's enforcement point is a `skuid`-narrowed drop on `output` |
| **An un-narrowed drop on `output` is a host-level egress kill switch**: root's probe fails too (count 6) | This is guard ②'s danger **reproduced on the second hook**, and **here `iifname` cannot serve as that narrowing** (the traffic does not arrive on any interface) ⟹ the `output` hook needs **its own** narrowing guard, predicated on `skuid` |
| `skuid` **does not reliably match a whole flow**: a direct reverse measurement read `noown=0/5`, while Q1 read `skuid` matching only 4/5 in 2 of 3 runs | **Safe on the drop side, unsafe on the accept/`RETURN` side** (see the paragraph below D4.2's table); packets `skuid` fails to match pass straight through an allowance placed above the floor |

**One control arm**: an `output` base chain's `policy drop` **can** be held open by a `skuid !=`
accept (root gets the banner) — the **opposite** of what guard ② measured ("`iifname` cannot
narrow a chain's policy"). **Do not read this as "`policy drop` is fine to use here"**: it only
holds because a local packet almost always carries an owner, and the row above just said that
"almost" is not "always".

**Two incidental pure-rendering facts, in passing, both of which point the failure at the wrong
place**: ① in an `-f` file, **a named counter must be declared before the chain that references
it**; ② **`fwd` is a reserved word in nft 1.1.6 and cannot be used as a chain name**, and the
resulting error makes **every chain after it** each report a spurious syntax error — that is, one
chain-name collision presents as "my rule's syntax is wrong", with the real site several chains
away.

**The measurement rig is an ordinary netns peer** (the same permitted reason as ⑩⑪⑫: what is being
measured here is nft's hook ownership and predicate semantics, unrelated to whether the originator
is gVisor — the second hop's originator **is always a host process**, never the guest).

### D5.b Inbound / `publish`

**None of these five items follow from any item above** — inbound goes `prerouting` → translation
→ some hook, while the hook-split conclusion above (⑧) was measured for traffic **originated** by
the guest. **⑬–⑯** were measured on a real runsc guest plus a separate netns acting as "the
external caller" (`test/preflight/spike-publish-ingress.sh`); **⑰ used a different rig**, see that
item.

**⑬ publish's return path needs host-side SNAT, or it quietly depends on the guest having a
default route.** With only DNAT installed, a caller from **outside this sandbox's own `/30`** can
get a SYN to the guest, but the guest's reply packet **has nowhere to go** inside the netstack —
measured `to_guest=2 / from_guest=0`, the caller reads a timeout. After adding
`oifname <hveth> ip daddr <guest> snat to <gateway>`, the source address the guest sees becomes
**its own `/30` gateway** (on-link), the return path no longer needs any guest-side route, and the
same flow immediately becomes `connected`. ⟹ **For `publish` to really be "a purely host-side
rule"** (§5.D's own wording), the generator must emit this SNAT along with the DNAT; emitting only
the DNAT produces a feature that "works on a sandbox with a default route, times out on one
without".

> **⚠ This item's effect splits into two halves; do not treat the whole item as reproducible.**
> **The half with SNAT** is the path the checked-in script runs every time (it unconditionally
> installs that SNAT; the counters are named `pre_dnat` / `post_snat` / `fw_*`). **The "DNAT-only"
> control** was measured on a one-shot fixture (the names `to_guest` / `from_guest` come from it),
> and **the checked-in script does not run that cell** — there it exists only as a code comment.
> So **the direction of the conclusion is credible** (with no SNAT the return path has nowhere to
> go inside the netstack, and this has independent mechanical grounds), but **those two specific
> counts cannot be rerun by the checked-in script**. Making it reproducible requires the script to
> promote DNAT-only into a real cell.

> **The same measurement surfaced a more basic substrate fact: the sentry withdraws the L3 address
> from the kernel-side veth at boot.** After boot, `ip addr` inside that netns shows **only `lo`**
> (the veth link is still present and still UP), so `ip route add default via …` fails with
> `Nexthop has invalid gateway` — **the guest-side route can only be configured before boot, never
> after**. This is the other face of the same mechanism as "the engine assigns the IP itself, with
> no post-boot probing" — but that constrains **reads**, while this one constrains **writes**: of
> the three host-side egress mechanisms, "the guest-side default route carrying `dev`" must
> therefore be installed **before `runsc run`**, and cannot be patched in after the guest comes up
> — it falls inside the same atomic window as the rule set (§2.4).

**⑭ Publish traffic originated by the host itself does not traverse `prerouting`, and needs its
own DNAT on the `output` hook.** Under the same rule set with only a `prerouting` DNAT installed,
dialing from **the caller's netns** connects (`pre_dnat` counts 1); dialing the same `ip:port` from
**the host** is not translated at all (`pre_dnat` unchanged); after adding an `output`/`dstnat`
rule, the host side connects too (`out_dnat` counts 1). ⟹ **"A debug dial from the host works" is
not evidence that publish is effective, and neither is the reverse**: the two paths are carried by
two different rules, and acceptance must assert them separately —
`TestPublishIsReachableFromOutsideAndFromTheHostAndSurvivesResume` is split into two questions on
this basis.

**⑮ Translated publish traffic lands on `forward`, not on `input`.** Measured, the `fw_*` count
grows with every publish flow while `in_*` stays at 0. ⟹ **D5 ⑧⑩'s conclusion that "steered
traffic lands on `input`" does not carry over to inbound**: `via[]`'s target is a **host-local
endpoint** (so post-translation traffic is still local to this machine, landing on `input`), while
publish's target is **the guest IP** (so post-translation traffic must be routed onward, landing
on `forward`). **The same `prerouting` DNAT, two different downstream hooks — the difference is
only whether the post-translation destination is a local address.** The generator must decide
which chain carries the companion accept based on this.

**⑯ A guest bound to `127.0.0.1` and "no listener at all" are indistinguishable from the caller's
side — both are `refused`.** Measured, case B (guest bound to loopback) = `refused` @1 ms, case C
(no listener at all inside the guest) = `refused` @0 ms, **byte-for-byte identical**: the gVisor
netstack returns RST for both cases. ⟹ **§5.D's user-visible semantic ① therefore only promises
"refused and fails fast", never attributability.** **The direction is good** — neither case
**hangs**, so the half of that semantic that is actually load-bearing (not a timeout) holds and has
been reproduced; only "attributable" does not hold. **Attribution can only happen on the engine
side**: the engine knows which publish rule it installed and can read that rule's counter, while
the caller only ever sees one RST.

> **One thing this run could not effectively assert**: cell D wanted to also prove "publish's
> companion accept did not turn into an egress path" (D1 ④), but that guest had no default route,
> so its egress already got `net is unreachable` **before ever leaving the sandbox**, and the
> floor drop's count stayed at **0** — per D4.1's discipline this is an **invalid assertion**, not
> a pass. It is covered by `TestPublishDoesNotOpenAnEgressPathOnTheSubstrate` (whose sandbox **does**
> have a default route, so the same probe genuinely reaches the floor).

**⑰ Removing publish's DNAT only closes new connections; an already-established flow keeps moving
data as before — and removing only the companion SNAT produces a completely different failure
signature.** Both halves measured (netns peer rig, one-shot fixture, **not checked in**):
- **After removing both DNAT rules** (`prerouting` + `output`), a new connection is `refused`,
  while **the same already-established flow completes all 8 of 8 round-trip messages, each read
  back and verified**, with the ct entry count identical before and after removal. ⟹ **conntrack
  holds that NAT binding, and rule removal is never evaluated against it** (the same mechanism as
  ㉒'s, viewed from the "reinstall" side there). **This must be measured by reading data back, not
  by a write's return value**: a `write()` to a socket whose peer is already gone usually succeeds
  once before returning `EPIPE`, so "the write succeeded" proves nothing about delivery.
- **After removing only the companion SNAT** (DNAT left in place), an already-established flow
  still completes 6/6 round trips, while **the symptom for a new connection is "connects but reads
  time out"** (`connected_no_payload`, 2 s), **not `refused`**: the DNAT still steers the SYN to the
  guest and the handshake completes, but with no SNAT the return path cannot be routed (⑬'s
  mechanism), so the caller hangs waiting on data that will never arrive.

⟹ Two actionable conclusions. **One, revoking `publish` cannot be cleanly undone on a `running`
sandbox** — it can only promise "no new connections accepted", never "this port is closed", and the
gap between the two is exactly what a caller wants when revoking an inbound port (hence **there is
no dedicated un-publish endpoint**, and the reason is mechanical, not scheduling: revocation goes
through a `PATCH` of the whole declaration, and the port is only actually closed at the next
placement — `TestRevokingAPublishClosesThePortAtTheNextPlacement`). **Two, a half-torn-down publish
rule set breaks in the hardest-to-diagnose way possible**: missing the SNAT presents as "connects
then hangs", and is very easily misread as a guest-side application fault. So "publish does not
work" has **two distinct signatures**, and acceptance must assert them separately, never merged
into one "unreachable".

### D5.c Reconciliation / `Adopt`

**㉒ A control-plane restart does not break flows, but only "a whole atomic reinstall" is a safe
way to do it; flushing the ct table is a live-connection killer.** This measurement was completed
after installing `conntrack` (1.4.9). **This machine has no `/proc/net/nf_conntrack`**
(`CONFIG_NF_CONNTRACK_PROCFS` not enabled), so the netlink CLI is the only observation channel.
Three things a takeover "might do" to the rule set, with mutually different outcomes (a real runsc
guest plus a peer netns, `test/preflight/spike-ct-adopt.sh`):

| Approach | Result | Mechanism and measured numbers |
| --- | --- | --- |
| **Atomic reinstall** inside one `nft -f` file with bare-`table`+`delete`+definition (⑤'s idempotent three-step) | **Flows survive 5/5** | The ct entry's **id is unchanged across the operation**; `post_masq` stays **0** while bytes keep moving ⟹ the flow rides **the translation already stored in the old entry**, and the nat hook is never evaluated again; on the second-old table, `fw_est>0` and the `established` arm catches the live flow immediately |
| `delete`, then `install` (with an observable gap in between) | **Flows die 5/5** | The ct entry is **unchanged down to the byte** (same id, `ESTABLISHED`) ⟹ **the cause of death is not a lost ct entry**. A packet sent during the gap arrives at the peer with the **bare sandbox source address** (`p_raw +1`), the peer does not recognize this connection, and replies **RST** (`p_rst +1`) |
| `conntrack -F` | **The flow dies under `tcp_loose=0`** | `fw_gdrop +6`, `fw_rdrop +1` ⟹ **⑨'s mechanism hits a live flow**: the return traffic no longer satisfies `established,related`, and lands on the floor drop |

**Two controls that pin the conclusion down**: ① **deleting only the filter chain, leaving the nat
chain untouched**, the same delete-gap-reinstall sequence leaves **the flow alive** ⟹ **the danger
is the nat chain's absence, not the floor's absence**; ② `delete` and `install` **back to back**
(no sleep in between) gives **3/3 survival**, `p_raw +0` ⟹ the gap must be long enough for the flow
to happen to send a packet during it before it becomes fatal. **So this is a race, not a
deterministic failure — but the losing side is stably reachable at a second-scale gap, and when
the guest sends a packet is not something the engine controls**, so it cannot be excused as "small
enough gap is fine".

**The `conntrack -F` cell must be read together with a sysctl, or it measures a false
conclusion**: this machine's `nf_conntrack_tcp_loose=1` (the default) lets conntrack **adopt a flow
mid-stream even though it never saw the start** — so what is observed after a flush is **a new ct
entry** (id changed, `post_masq +1`, i.e. re-NAT'd) with the flow still alive; turning it off to 0
and then flushing kills the same flow immediately. ⟹ **"The flow is still alive after a flush" is a
property of that sysctl, not a property of this design** — the engine does not own it and must not
rely on it.

**Where the observation side actually sits**: the flow's guest-side half lives inside the sentry's
netstack and has no representation in any host ct table; measured, **the host's** ct table holds
this flow (keyed by guest IP plus port, `[ASSURED]`), while **the sandbox netns's own ct table
entry count stays at 0** (the same root cause as ②). ⟹ **The single ct entry carrying this flow
lives on the host side, and there is only one copy.**

> **⟹ Three hard constraints on §4.4-A `Adopt`**: **never flush the ct table** (in any form) ·
> **never "`delete` the table, then install"** · **if reinstalling at all, reinstall the whole
> table atomically.** The third is in fact a useful permission: `Adopt` can do **no reasoning at
> all** about a table it finds — it can just re-render the whole thing and install it, which is
> simpler than diffing. **A constraint attached to `ReclaimOrphans`**: it enumerates by table-name
> prefix, and if it runs before `Adopt` in the same reconciliation pass and ends up deleting a live
> sandbox's table in passing, that is **word for word** the second row of the table above — so the
> keep set must be computed **before** any deletion.
>
> **Already reproduced, `Adopt` included**: `TestALiveFlowSurvivesAControlPlaneRestartAndItsAdopt`
> runs a guest-side echo flow halfway through, then starts a second engine following the production
> boot sequence partway through, whose `Adopt` reinstalls the rule set, and asserts **the round-trip
> count read back across the restart** (not that the socket is still open — a `write()` to a dead
> peer usually succeeds once before EPIPE). What was measured above is the **three host-state
> changes `Adopt` might make**, so that conclusion is **conditional on `Adopt` doing only the
> first one**, which is exactly what this assertion guards. **One residual**: the second engine is
> started inside the same OS process (the same store and state directory, the production boot
> sequence), so the OS process itself was never actually killed — the difference lands on
> process-level state, while the host state this conclusion rests on (the ct table, the nft table)
> is all outside the process.

---

## D6. Interface shape vs registry plumbing: which half deserves to exist right now

The most honest cut against §1.3's criterion: **build the two shapes "rule generation is a pure
function" plus "the provider receives derived output and holds no authority over the declaration"
right now** — they are good design a single-node implementation should have anyway, and they
happen to become the future landing point too. **The concrete multi-implementation registration
slot is built only once a second implementation shows up**: when a slot has only one
implementation, the registration mechanism itself is a reservation (§1.3's cost criterion).

**Where the cut falls is fixed by D1 ②**: the engine-side half derives only an **abstract rule
set** from the declaration and the floor (placement-free, root-free, unit-testable); rendering the
nft text, and binding `iif` / the guest IP / publish's host endpoint, **all happen inside the
provider, at placement time** — because those three are host primitives only the provider holds,
and lifting them onto the cross-provider contract surface would be a leak. **The two halves meet
through §5.A's existing provider contract; `SteeringSpec` is the member carrying this half — no
new seam type is added.**

---

## D7. Litmus self-check (§1.3): everything reduces to one of the three kinds of addition

| Future need | Which kind of addition | Needs a new seam type? |
| --- | --- | --- |
| Extend to another scheduling backend (K8s…) | register a runtime provider (behavior) | No |
| Multi-tenancy | add a join table (**outside** the engine) | No |
| Add a new kind of allowance to the declaration | add rows (Data; the declaration is a Binding) | No |
| Egress audit feeding billing / observability | add a subscriber (Consumption, §4.1 event stream) | No |
| Choose a backend by SNI / swap credentials / terminate TLS | **not inside the engine** (an adapter outside it, §1.3's orthogonal outside-the-engine axis) | No |
| Access control in front of publish | **not inside the engine** (same as above); the half the engine can enforce (allow by source CIDR) is one rule predicate | No |

**All purely additive, inventing no new seam type** — exactly the shape §1.3 wants. Note that the
last two rows fall on the **outside-the-engine axis**: they do not even need "a seam reserved
inside the engine", so the engine side's count of additions is zero for them.

---

## D8. The agenda still to be refined (registered in open-decisions)

**The floor's membership list and rule text are already settled, and are not in this section**:
all four list items are static (D5 ④⑧); the shape of `via[]`'s narrow allowance and its IPv6
coverage are in D5 ⑩⑪; `iif`'s fail-open is in D5 ⑫. **"Whether the declaration's allowance is
also emitted on `input`" is settled** (yes, and that side carries its own sibling drop — D4.2).
**The internal deny-safe tier's membership is settled** (RFC1918 plus mechanical v6 derivation,
§2.4; this section leaves open only whether to widen it). **The second hop's hook and predicate
are also settled** (`output` plus `skuid`, D5 ㉔). **So §5.D has no pre-work measurement left to
do.** Three items remain open:

- **Whether the internal deny-safe tier is widened beyond RFC1918 in the future** (**the membership
  list itself is already settled into §2.4**: the three RFC1918 ranges plus mechanical v6
  derivation. What is open is only whether to widen it, and widening is purely additive).
- **Whether fail-closed emits a §4.1 audit event.**
- **`SteeringSpec`'s exact fields.**

> The measured evidence, candidates, and costs for each item are in [`open-decisions.md`](../open-decisions.md)'s
> "Design points still to be refined" section, not duplicated here. The normative statement for
> D0–D7 is in §2.1/§2.3/§2.4/§5.D; this document only develops the reasoning and the invariants
> that must be held.
