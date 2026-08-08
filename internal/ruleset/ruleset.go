// Package ruleset is a LEAF: the two facts §5.D (Network) and §5.A (Runtime)
// must agree on, and nothing else. It imports no engine package, holds no
// mechanism, and abstracts no behavior — it is `guestport`'s precedent applied a
// second time (CLAUDE.md: when two subsystems need the same fact, put it in a
// leaf; never excuse a subsystem edge).
//
// The two facts:
//
//  1. THE STATIC ADDRESS FACTS the L0 floor and the internal deny-safe tier are
//     made of (§2.4). All four floor items are static (network.md D5 ④⑧) and
//     BOTH HOPS share one copy (§2.4 second invariant), so there is nothing to
//     distribute at runtime. The sandbox address space is here rather than in
//     local_runsc because the generator and the allocator must not be able to
//     disagree about it: the allocator hands out /30s from this /16, and the
//     floor's sibling item IS this /16.
//
//  2. THE ABSTRACT RULE VOCABULARY. §5.D generates `(declaration, L0 floor) →
//     abstract rule set` as a PURE FUNCTION — placement-free, root-free, unit
//     testable — and §5.A renders that into nft text and installs it at
//     placement time, binding the three things only it holds: the host-side
//     veth name, the guest IP, and the publish host endpoint (§5.A ❌ list /
//     network.md D1 ② / D6). Both halves need the same vocabulary. It cannot
//     live in either subsystem: `network` importing `runtime` or the reverse is
//     the dependency edge `internal/arch` fails on.
//
// NOTHING HERE MAY NAME A PLACEMENT PRODUCT. A rule that must reach "this
// sandbox's host veth" or "this sandbox's own /30 gateway" says so with a Scope
// or a Match flag, and the provider substitutes the concrete value. That is what
// keeps the generated rule set placement-free, and therefore what keeps §5.D's
// generator testable without root.
package ruleset

import (
	"fmt"
	"net/netip"
	"sort"
)

// SandboxCIDR is the single fixed CIDR every sandbox /30 is carved out of
// (network.md D5 ④). Two consumers, and the point of putting it here is that
// they cannot drift apart:
//
//   - §5.A's allocator hands out /30s from inside it.
//   - §5.D's L0 floor names it as the SIBLING item — the materialization of the
//     §2.3 cross-sandbox network isolation invariant, which point-to-point veth
//     does NOT deliver on its own (the host still ROUTES between two /30s;
//     measured, network.md D5 ①).
//
// Because it is one fixed CIDR the sibling item is a STATIC rule rather than a
// membership set maintained at runtime, which is why both hops can share one
// copy with no distribution mechanism at all (§2.4).
var SandboxCIDR = netip.MustParsePrefix("10.66.0.0/16")

// FloorV4 / FloorV6 are the L0 floor's ENUMERABLE items — the daddr list (§2.4).
//
// THE FOURTH FLOOR ITEM IS DELIBERATELY ABSENT FROM THESE LISTS. "Any address of
// the host itself" is covered by a single `iifname`-narrowed PURE CT-STATE rule
// on the `input` hook, never by enumeration: "not established" is exhaustive over
// every host address, so the engine does not need to know its own bind addresses
// and the whole class of "a host address changed, must we re-render" questions
// does not exist (§2.4 / network.md D5 ⑧). Putting host addresses in a list here
// would reintroduce exactly that problem.
//
// Two lists because nft's grammar is family-typed: `ip daddr` matches only IPv4
// packets and `ip6 daddr` only IPv6, so a set cannot mix them. This is NOT the
// generator "forking for IPv6" (which network.md D5 ⑪ measured as redundant for
// the ct-state floor): the ct-state rule carries no family qualifier and covers
// both families in one rule, while these are declaration-layer ADDRESS members
// that nft can only spell per family.
var (
	FloorV4 = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),    // loopback
		netip.MustParsePrefix("169.254.0.0/16"), // link-local (covers every cloud metadata endpoint)
		SandboxCIDR,                             // sibling sandbox address space (§2.3 / D5 ①)
	}
	FloorV6 = []netip.Prefix{
		netip.MustParsePrefix("::1/128"),   // loopback
		netip.MustParsePrefix("fe80::/10"), // link-local
	}
)

// FloorLiftableNever is the subset of the floor's daddr items that §6.2's seventh
// 400 class reads: loopback and link-local, in both families.
//
// It excludes the SIBLING item on purpose, and that exclusion is a decision with
// two independent reasons (§6.2): an `egress.allow[]` entry inside the sibling
// CIDR is NOT a dead rule — this sandbox's own /30 gateway sits in there and is
// the one stably-available host-local resolver address §5.D semantics ③ requires
// a caller to be able to declare (network.md D5 ⑦) — and a 400 that mentioned the
// sibling CIDR would put the sandbox address space on the wire, which §6.2's
// ID-addressing rule excludes.
var FloorLiftableNever = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
}

// TierV4 / TierV6 are the internal deny-safe tier (§2.4): deny by DEFAULT, but
// LIFTABLE BY DECLARATION — which is the whole difference between this tier and
// the floor, and it is materialized purely by ORDER (the floor sits above the
// declaration's allows, this tier below them).
//
// Members are RFC1918's three blocks plus a mechanical v6 derivation (§2.4). It
// is not optional hardening: with the three host-side egress mechanisms in place,
// `egress.default: allow` without this tier hands the host's entire LAN to the
// sandbox (measured, network.md D5 ⑤), i.e. it would deliver an `allow` STRICTLY
// WIDER than §2.4 defines.
//
// It must NOT share a set with the floor: the sibling /16 is nested inside 10/8,
// and an interval set either refuses to load with nested members or — with
// auto-merge — SILENTLY swallows the subnet, which would merge the floor away
// (measured, network.md D5 ⑱). Two rules, ordered.
var (
	TierV4 = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	// TierV6 is fc00::/7 plus the NAT64 IMAGE OF EACH MEMBER — never the
	// `64:ff9b::/96` prefix itself. Per RFC 6052 that prefix maps the ENTIRE IPv4
	// space, public destinations included, so dropping the whole /96 would refuse
	// exactly the public egress `egress.default: allow` promises (measured,
	// network.md D5 ㉓). Derived rather than transcribed so the two halves cannot
	// drift; the literals are pinned by a test.
	TierV6 = append([]netip.Prefix{netip.MustParsePrefix("fc00::/7")}, nat64Images(TierV4)...)
)

// NAT64Prefix is RFC 6052's well-known prefix. Present only as the base for
// deriving member images — it is never itself a rule member (see TierV6).
var NAT64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// nat64Images maps IPv4 prefixes to their RFC 6052 §2.2 images under the
// well-known /96 prefix: the v4 address occupies the low 32 bits, so the image's
// prefix length is 96 + the v4 prefix length.
func nat64Images(v4 []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(v4))
	base := NAT64Prefix.Addr().As16()
	for _, p := range v4 {
		var a [16]byte
		copy(a[:], base[:12])
		v := p.Addr().As4()
		copy(a[12:], v[:])
		out = append(out, netip.PrefixFrom(netip.AddrFrom16(a), 96+p.Bits()))
	}
	return out
}

// Proto is the transport protocol a tuple names. It is ALWAYS explicit — a
// format requirement, not an optional dimension: in the kernel mechanism that
// carries it, port matching itself depends on protocol matching (§5.D /
// network.md D3.1), so `{cidr, port}` is not well-formed.
type Proto string

const (
	TCP Proto = "tcp"
	UDP Proto = "udp"
)

// Hook is where a rule attaches. Six values, and the split is a MEASURED
// structural fact rather than an implementation convenience:
//
//   - The first hop (the guest's own egress) splits across TWO hooks with
//     OPPOSITE predicate shapes: traffic to an address of the host ITSELF lands
//     on `input`, traffic the host would ROUTE lands on `forward` (network.md
//     D5 ⑧). A floor that only covers `forward` misses guest→host entirely.
//   - The SECOND hop (a host-local process opening its own connection on the
//     guest's behalf, §2.4 invariant 2) lands on NEITHER: measured, it counts
//     zero in `forward` in all three shapes and is visible only in `output`
//     (network.md D5 ㉔). Reading it as "the forward row covers it too" yields a
//     rule set in which the second hop is never evaluated, and the symptom is a
//     floor counter that does not move — D4.1's silent pass.
type Hook uint8

const (
	// HookInput is the per-sandbox filter input chain: guest → an address of the
	// host itself.
	HookInput Hook = iota + 1
	// HookForward is the per-sandbox filter forward chain: guest → anything the
	// host routes, plus (in the other direction) traffic routed TO the guest.
	HookForward
	// HookPrerouting is the per-sandbox nat prerouting chain: `via[]` steering and
	// the inbound half of a publish.
	HookPrerouting
	// HookPostrouting is the per-sandbox nat postrouting chain: the publish return
	// SNAT and the egress masquerade.
	HookPostrouting
	// HookOutputNAT is the per-sandbox nat output chain: the SECOND, independent
	// DNAT a publish needs so the host can dial its own published endpoint —
	// host-originated traffic never traverses prerouting, so "it works from the
	// host" is not evidence a publish works, and vice versa (network.md D5 ⑭).
	HookOutputNAT
	// HookOutputFilter is the ENGINE-LEVEL (not per-sandbox) filter output chain
	// that pins the second hop (§2.4 invariant 2 / network.md D5 ㉔). It is a
	// separate table because its predicate is the deployment-supplied uid/gid
	// rather than an interface: one uid means one rule set, independent of how
	// many sandboxes exist, so it is installed once at startup and not per
	// placement.
	HookOutputFilter
)

func (h Hook) String() string {
	switch h {
	case HookInput:
		return "input"
	case HookForward:
		return "forward"
	case HookPrerouting:
		return "prerouting"
	case HookPostrouting:
		return "postrouting"
	case HookOutputNAT:
		return "output-nat"
	case HookOutputFilter:
		return "output-filter"
	}
	return "invalid"
}

// PerSandbox reports whether this hook belongs to a per-sandbox table. The
// negation is exactly HookOutputFilter, and the distinction is what gives the
// render guards their SCOPES: the per-sandbox guard demands an interface
// narrowing, the second-hop guard demands a uid narrowing, and a single "every
// rule must carry iifname" guard would reject the very rule §2.4 invariant 2
// requires (the second hop arrives on no interface at all).
func (h Hook) PerSandbox() bool { return h != HookOutputFilter && h != 0 }

// Verdict is what a matching rule does.
type Verdict uint8

const (
	// Accept ends evaluation of THIS CHAIN only. That is a trap worth naming
	// here: a base chain's accept does not end the packet's traversal, so the
	// packet continues into the next base chain on the same hook and can still be
	// dropped there — measured, an accept placed in a different base chain
	// incremented its counter while the connection still failed (network.md D5
	// ⑩). Only a terminal verdict (drop / reject) ends traversal across chains.
	// Consequence for the generator: an approved exception must sit in the SAME
	// chain as the floor drop it overrides, above it.
	Accept Verdict = iota + 1
	// Reject is the GUEST-FACING refusal. gVisor's netstack honors ICMP errors, so
	// the same denial written as `drop` versus `reject` differs by orders of
	// magnitude in what the guest experiences (network.md D5 ⑨ / D4.3). §2.4's
	// fail-closed has two halves — the security half AND "the guest gets a clean
	// connection failure" — and a pure-`drop` floor satisfies only the first.
	// Security is unaffected: neither verdict forwards the packet.
	Reject
	// Drop is for denials with no guest to inform: the second hop's own egress,
	// whose originator is a host-local process, and the internal deny-safe tier's
	// refusals where an ICMP error to the guest is not owed.
	Drop
	// DNAT rewrites the destination (via[] steering, publish inbound).
	DNAT
	// SNAT rewrites the source to this sandbox's own /30 gateway (publish return).
	SNAT
	// Masquerade rewrites the source to the outbound interface's address (the
	// third of the three host-side mechanisms egress needs, network.md D5 ⑤).
	Masquerade
)

func (v Verdict) String() string {
	switch v {
	case Accept:
		return "accept"
	case Reject:
		return "reject"
	case Drop:
		return "drop"
	case DNAT:
		return "dnat"
	case SNAT:
		return "snat"
	case Masquerade:
		return "masquerade"
	}
	return "invalid"
}

// Terminal reports whether this verdict ends traversal across base chains. Only
// these two do (network.md D5 ⑩) — see Accept's doc for why that matters.
func (v Verdict) Terminal() bool { return v == Reject || v == Drop }

// Scope is the NARROWING every rule must carry, and it is the abstract form of
// the render guards (§4 of the M8 plan / network.md D5 ⑩ ㉔).
//
// Why every rule needs one: each per-sandbox table puts a `policy accept` base
// chain on a SHARED hook, so a rule in one sandbox's table is evaluated against
// EVERY sandbox's traffic. An un-narrowed accept is therefore a hole in every
// sandbox's floor, and an un-narrowed drop is worse — drops terminate across
// chains, so it is a host-level kill switch (measured in both directions,
// network.md D5 ⑩ / the hop-2 spike Q5).
//
// The concrete clause each Scope becomes is the provider's business: these name
// placement products (§5.A ❌ list), which is precisely why they appear here as
// an enum rather than as strings.
type Scope uint8

const (
	// ScopeNone is ILLEGAL in a rule. It is the zero value on purpose: a rule
	// somebody forgot to scope fails Validate rather than rendering into a
	// host-wide rule.
	ScopeNone Scope = iota
	// ScopeGuestIn narrows to traffic ARRIVING on this sandbox's host-side veth →
	// `iifname <veth>`. This is pillar ④'s materialization: the predicate binds to
	// a fact the guest cannot change, unlike a source address, which guest root
	// forges with one `ip addr add` (measured, network.md D5 ③).
	//
	// BY NAME, never by index. `iif` freezes an ifindex at load time and silently
	// fails OPEN after a same-name veth rebuild — measured, the guest's packet was
	// delivered while the drop counter read 0 — and the two spellings render
	// identically in `nft list`, so review cannot catch it (network.md D5 ⑫).
	ScopeGuestIn
	// ScopeGuestOut narrows to traffic LEAVING toward this sandbox's guest →
	// `oifname <veth>`. Same veth, same by-name discipline, same unforgeability:
	// the guest cannot change which interface leads to it.
	//
	// It exists because two required rules cannot be spelled with iifname at all:
	// a publish's companion accept (the traffic arrives on the CALLER's interface,
	// network.md D5 ⑮) and the inbound default-deny that makes "ingress closed
	// unless published" true (§2.1 ③). A guard that demanded iifname literally
	// would reject both.
	ScopeGuestOut
	// ScopeHostEndpoint narrows to this sandbox's PUBLISHED host endpoint →
	// `ip daddr <host ip> <proto> dport <host port>`. Used by the two publish
	// DNATs, where no interface predicate is available: prerouting sees an
	// arbitrary caller interface, and the output-hook copy is locally generated.
	// The host port is allocated per placement and per publish item, so it is a
	// per-sandbox narrowing in the sense the guard cares about.
	ScopeHostEndpoint
	// ScopeSecondHopUID narrows to the deployment-supplied uid/gid of the process
	// sitting behind `via[].endpoint` → `meta skuid <uid>`. Legal ONLY in the
	// engine-level output table.
	//
	// It may narrow a DROP and never an ACCEPT, and that asymmetry is measured
	// rather than stylistic: skuid does not reliably match every packet of a flow
	// (one packet occasionally carries no socket owner), so a skuid-narrowed drop
	// is safe — losing the SYN kills the flow — while a skuid-narrowed accept
	// above a floor drop leaks whatever it fails to match (network.md D5 ㉔).
	// Validate enforces it.
	ScopeSecondHopUID
)

func (s Scope) String() string {
	switch s {
	case ScopeGuestIn:
		return "guest-in"
	case ScopeGuestOut:
		return "guest-out"
	case ScopeHostEndpoint:
		return "host-endpoint"
	case ScopeSecondHopUID:
		return "second-hop-uid"
	}
	return "unscoped"
}

// Match is a rule's selector, in IP / port / protocol vocabulary ONLY. The
// engine's enforcement evaluates nothing else — no domain names, no TLS, no
// application protocol (§2.4 / network.md D2).
type Match struct {
	// DestV4 / DestV6 are destination prefix sets. Family-separated because nft's
	// grammar is: one set cannot hold both families. Both empty = any destination.
	DestV4 []netip.Prefix
	DestV6 []netip.Prefix

	// DestSelfGateway matches "this sandbox's own /30 gateway address" — a
	// placement product the provider substitutes.
	//
	// It exists for §5.D semantics ③: the only stably-available host-local
	// resolver address is this sandbox's own gateway, and it sits INSIDE the
	// sibling CIDR (network.md D5 ⑦). So the declaration's allow must be able to
	// reach it while the same declaration must NOT reach a sibling's gateway —
	// which is a question of ORDER, not of set membership, and needs this
	// placement-bound spelling to express.
	DestSelfGateway bool
	// DestGuest matches "this sandbox's guest IP" — the destination a publish DNAT
	// produces, used by the companion accept and the return SNAT.
	DestGuest bool

	// Proto / DestPort are the post-NAT L4 selector. DestPort requires Proto (nft
	// makes port matching depend on protocol matching, D3.1).
	Proto    Proto
	DestPort uint16

	// CTState is a connection-tracking state list, e.g. {"established","related"}.
	// Deliberately family-UNQUALIFIED: one such rule in an `inet` table matches
	// IPv6 as well, including a global prefix the guest invented for itself, so an
	// explicit `meta nfproto ipv6` second arm is redundant (measured, network.md
	// D5 ⑪). The generator therefore does NOT fork for IPv6 here.
	CTState []string

	// CTOriginalDestV4 / CTOriginalPort pin the PRE-NAT tuple of a steered flow.
	// Both are required for `via[]`'s narrow accept together with the post-NAT
	// DestPort, and all three are load-bearing: each omission has a measured
	// counter-example, from "an undeclared original port reaching a declared
	// listener" to "a declared port reaching a listener the declaration never
	// named" (network.md D5 ⑩).
	CTOriginalDestV4 *netip.Prefix
	CTOriginalPort   uint16

	// NotOutSelf adds `oifname != <this sandbox's veth>` — used by the egress
	// masquerade so it cannot rewrite traffic staying inside the /30.
	NotOutSelf bool
}

// Rule is one abstract rule. Order within a hook is the order in RuleSet.Rules,
// and ORDER IS A SECURITY PROPERTY rather than a style question: the floor sits
// above the declaration's allows, the deny-safe tier below them, and an approved
// exception in the same chain above the drop it overrides. A rule set with the
// right rules in the wrong order behaves perfectly on ordinary traffic and
// differs only when it is being bypassed (network.md D1 ④ / D4.1).
type Rule struct {
	Hook    Hook
	Scope   Scope
	Match   Match
	Verdict Verdict

	// Counter is the NAMED nft counter this rule increments. It is REQUIRED on
	// every rule, because the acceptance discipline for every deny assertion is
	// "unreachable inside the netns AND the floor counter moved after a
	// deliberate probe" — a floor whose counter reads 0 is indistinguishable from
	// a floor that was bypassed (§2.4 / network.md D5 ⑧ / D4.1).
	Counter string

	// ToGuestPort is the DNAT target port on this sandbox's guest IP (publish).
	ToGuestPort uint16
	// ToEndpoint is the DNAT target for `via[]` steering: a DECLARED host-local
	// "ip:port". It is a declaration value, not a placement product, so it
	// legitimately appears as a literal here.
	ToEndpoint string
	// ToSelfGateway makes an SNAT rewrite the source to this sandbox's own /30
	// gateway (the publish return path). Without it a publish works only on a
	// sandbox that happens to have a default route and times out on one that does
	// not (measured, network.md D5 ⑬).
	ToSelfGateway bool

	// Why documents what this rule delivers, and it is rendered into the nft file
	// as a comment. Not decoration: several of these rules are one word away from
	// being a security hole with no test firing, so an operator reading
	// `nft list` needs to see which promise each line carries.
	Why string
}

// RuleSet is the abstract rule set — §5.A's `SteeringSpec` payload, produced by
// §5.D's pure generator and consumed by the provider's renderer.
type RuleSet struct {
	// Rules is the ORDERED rule list. Rules for different hooks may interleave
	// here; the renderer groups by hook and preserves relative order within each.
	Rules []Rule

	// Counters is every named counter the rules reference, in declaration order.
	// It is carried explicitly because a single `nft -f` file must DECLARE a named
	// counter before the chain that references it, or the load fails in a way that
	// points at the wrong line (network.md D5 ㉔).
	Counters []string
}

// Empty reports whether this rule set installs nothing.
func (rs RuleSet) Empty() bool { return len(rs.Rules) == 0 }

// ByHook returns this rule set's rules for one hook, in order.
func (rs RuleSet) ByHook(h Hook) []Rule {
	var out []Rule
	for _, r := range rs.Rules {
		if r.Hook == h {
			out = append(out, r)
		}
	}
	return out
}

// Hooks returns the hooks this rule set touches, in a stable order.
func (rs RuleSet) Hooks() []Hook {
	seen := map[Hook]bool{}
	var out []Hook
	for _, r := range rs.Rules {
		if !seen[r.Hook] {
			seen[r.Hook] = true
			out = append(out, r.Hook)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Validate is the ABSTRACT half of the render guards — the half that runs
// root-free, in a unit test, over the generator's output (M8 plan §4 / §6 gate A
// item 4).
//
// These are not defensive programming. Each one is a property that ONE WORD OF
// EDITING breaks with no test firing and no visible difference in `nft list`
// (network.md D5 ⑧ ⑩ ⑫ ㉔), which is the same standing the §5.A seam token guard
// has. The text half — no `policy drop`, no ifindex spelling — lives in the
// provider's renderer, because those are properties of the rendered file.
func Validate(rs RuleSet) error {
	declared := map[string]bool{}
	for _, c := range rs.Counters {
		declared[c] = true
	}
	for i, r := range rs.Rules {
		where := fmt.Sprintf("rule %d (%s/%s %q)", i, r.Hook, r.Verdict, r.Why)
		if r.Hook == 0 {
			return fmt.Errorf("%s: no hook", where)
		}
		if r.Verdict == 0 {
			return fmt.Errorf("%s: no verdict", where)
		}
		if r.Counter == "" {
			return fmt.Errorf("%s: no named counter — every rule must count, because "+
				"a deny assertion is only valid when a counter moved (§2.4 / D5 ⑧)", where)
		}
		if !declared[r.Counter] {
			return fmt.Errorf("%s: counter %q is not declared in RuleSet.Counters — a named "+
				"counter must be declared before the chain referencing it in one `nft -f` file (D5 ㉔)",
				where, r.Counter)
		}
		// GUARD: every rule carries a narrowing. An un-narrowed accept is a hole in
		// EVERY sandbox's floor (each per-sandbox table is a `policy accept` base
		// chain on a shared hook); an un-narrowed drop is worse, because drops
		// terminate across chains — measured as a host-level kill switch on both
		// the input side (D5 ⑧) and the output side (D5 ㉔).
		if r.Scope == ScopeNone {
			return fmt.Errorf("%s: unscoped — every rule must be narrowed to its own "+
				"placement, or it is evaluated against every sandbox's traffic (D5 ⑩ ㉔)", where)
		}
		// GUARD: the two guard SCOPES do not cross. A per-sandbox rule narrowed by
		// uid would not be per-sandbox at all; a second-hop rule narrowed by
		// interface is unreachable, because that traffic arrives on no interface
		// (D5 ㉔). This is the "two tables, two guards, two predicates" split — it
		// is not an exemption from one guard, it is two guards with disjoint scopes.
		if r.Hook.PerSandbox() && r.Scope == ScopeSecondHopUID {
			return fmt.Errorf("%s: a per-sandbox rule may not be narrowed by uid — that "+
				"predicate belongs to the engine-level second-hop table (§2.4 / D5 ㉔)", where)
		}
		if !r.Hook.PerSandbox() && r.Scope != ScopeSecondHopUID {
			return fmt.Errorf("%s: the engine-level output chain can only be narrowed by "+
				"uid/gid — no interface predicate is available there, because the traffic "+
				"arrives on no interface (D5 ㉔)", where)
		}
		// GUARD: a uid-narrowed rule may only DROP. skuid does not reliably match
		// every packet of a flow, so an accept narrowed that way leaks whatever it
		// misses down to the floor it was meant to sit above (D5 ㉔). Dropping is
		// safe in the same conditions: losing the SYN kills the flow.
		if r.Scope == ScopeSecondHopUID && r.Verdict != Drop {
			return fmt.Errorf("%s: a uid-narrowed rule may only DROP — `skuid` misses "+
				"packets, so an accept/return narrowed by it leaks them past the floor "+
				"it sits above (D5 ㉔ asymmetry)", where)
		}
		if len(r.Match.DestV4) > 0 && len(r.Match.DestV6) > 0 {
			return fmt.Errorf("%s: one rule cannot carry both address families — nft's "+
				"`ip daddr` / `ip6 daddr` are family-typed and a set cannot mix them", where)
		}
		for _, p := range r.Match.DestV4 {
			if !p.Addr().Is4() {
				return fmt.Errorf("%s: %s is not an IPv4 prefix but sits in DestV4", where, p)
			}
		}
		for _, p := range r.Match.DestV6 {
			if p.Addr().Is4() {
				return fmt.Errorf("%s: %s is not an IPv6 prefix but sits in DestV6", where, p)
			}
		}
		if r.Match.DestPort != 0 && r.Match.Proto == "" {
			return fmt.Errorf("%s: a port match requires an explicit protocol — nft makes "+
				"port matching depend on protocol matching (D3.1)", where)
		}
		if err := validateNesting(r.Match.DestV4); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if err := validateNesting(r.Match.DestV6); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// validateNesting rejects a set whose members nest. An nft interval set REFUSES
// to load with nested members, and `auto-merge` instead loads it while SILENTLY
// swallowing the subnet (measured, network.md D5 ⑱) — which is how a floor gets
// merged away without a word. The floor's sibling /16 nests inside the tier's
// 10/8, which is exactly why those two are separate ordered rules and never one
// set; this check is what makes a future attempt to merge them fail loudly.
func validateNesting(set []netip.Prefix) error {
	for i, a := range set {
		for j, b := range set {
			if i == j {
				continue
			}
			if a.Bits() <= b.Bits() && a.Contains(b.Addr()) {
				return fmt.Errorf("set member %s is nested inside %s — an nft interval set "+
					"either refuses to load or silently merges the subnet away (D5 ⑱); keep "+
					"nested members in separate ordered rules", b, a)
			}
		}
	}
	return nil
}
