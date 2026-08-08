package network

import (
	"fmt"
	"net/netip"

	"engine/internal/ruleset"
)

// Spec is what §5.D hands across the §5.A seam: the abstract rule set plus the
// publish descriptors whose host endpoints only the provider can bind.
//
// It is the DERIVED result, never the declaration itself — the provider installs
// and holds no authority (§5.A / §2.1 ③).
type Spec struct {
	// Rules is the abstract, placement-free rule set.
	Rules ruleset.RuleSet
	// Publish lists the `{guest_port, proto}` items a host endpoint must be bound
	// for, in declaration order. The provider allocates each endpoint at placement
	// and substitutes it into the rules that carry ScopeHostEndpoint.
	Publish []Publish
	// RoutedEgress reports whether this declaration can send anything out through
	// the host's router — i.e. whether the provider must put the host-side egress
	// plumbing in place (network.md D5 ⑤'s three mechanisms).
	//
	// It is a derived FACT rather than a knob: `via[]` alone does not need it (a
	// steered flow is DNAT'd to a host-local endpoint and never leaves), while any
	// `allow[]` entry or `default: allow` does.
	RoutedEgress bool
}

// Generate is §5.D's pure function: `(declaration, L0 floor) → abstract rule
// set`. Placement-free, root-free, unit-testable (§5.D / network.md D6).
//
// # Order is the security property
//
// Every position below is an assertion, not a style choice, because a rule set
// with the right rules in the wrong order behaves perfectly on ordinary traffic
// and differs only when it is being bypassed (network.md D1 ④ / D4.1). The three
// that carry the most weight:
//
//   - The FLOOR sits ABOVE the declaration's allows (no declaration lifts it), and
//     the internal deny-safe tier sits BELOW them (being liftable by declaration is
//     the entire difference between the tier and the floor, and it is materialized
//     purely by order — §2.4).
//   - The `via[]` narrow accept sits above the floor drop IN THE SAME CHAIN. A
//     cross-chain accept is measured to increment its own counter while the flow is
//     still dropped and the connection still fails, because a base chain's accept
//     ends only THAT chain (network.md D5 ⑩).
//   - On `input`, the sibling drop sits between the self-gateway allow and the
//     declaration's allows, so an `allow 10.0.0.0/8` reaches this sandbox's OWN
//     gateway (the declared-resolver path §5.D semantics ③ promises) and not a
//     SIBLING's (measured: it reads the sibling's banner without that drop —
//     network.md D4.2).
//
// # Why the first hop is two chains
//
// Traffic to an address of the host ITSELF lands on `input`; traffic the host
// ROUTES lands on `forward` (measured, network.md D5 ⑧). A floor covering only
// `forward` misses guest→host entirely. The two sides carry OPPOSITE predicate
// shapes: on `input` the floor is a pure ct-state rule with NO address list (`not
// established` is exhaustive over every host address, so the engine never has to
// know its own bind addresses, and the whole "a host address changed, must we
// re-render" class of question does not exist), while `forward` needs the explicit
// daddr list AND its own default-deny.
func Generate(d Declaration) (Spec, error) {
	d = d.Normalize()
	g := &gen{}

	// --- nat prerouting: steering and the inbound half of a publish -----------
	g.steering(d)
	g.publishInbound(d)

	// --- nat output: the host's own path to a published endpoint --------------
	// A SECOND, independent DNAT, and it is not redundancy: host-originated
	// traffic never traverses `prerouting`, so the prerouting rule is not
	// evaluated for it at all (measured, network.md D5 ⑭). Which is also why "I
	// dialed it from the host and it worked" is not evidence a publish works, and
	// why the reverse does not follow either — two paths, two rules, two
	// assertions.
	g.publishHostPath(d)

	// --- nat postrouting: the publish return SNAT and the egress masquerade ---
	g.publishReturn(d)
	routed := routedEgress(d)
	if routed {
		g.masquerade()
	}

	// --- filter input: guest → an address of the host itself ------------------
	if err := g.inputChain(d); err != nil {
		return Spec{}, err
	}

	// --- filter forward: guest → whatever the host routes, and back ----------
	if err := g.forwardChain(d); err != nil {
		return Spec{}, err
	}

	// --- filter output (ENGINE-LEVEL): the second hop -------------------------
	// Deliberately absent from this rule set. The predicate is one
	// deployment-supplied uid, so ONE rule set covers every sandbox regardless of
	// how many exist — hence one table, installed once at startup rather than per
	// placement (§2.4 / network.md D5 ㉔). SecondHopRules generates it, and
	// ValidateDeployment refuses a `via[]` declaration when the deployment did not
	// supply that uid: a declaration whose own invariant cannot be delivered is
	// refused rather than half-honored.

	if err := ruleset.Validate(g.rs); err != nil {
		return Spec{}, fmt.Errorf("network: generated rule set is invalid: %w", err)
	}
	return Spec{Rules: g.rs, Publish: d.Ingress.Publish, RoutedEgress: routed}, nil
}

// routedEgress reports whether the declaration can send anything through the
// host's router. `via[]` alone cannot: a steered flow is DNAT'd to a HOST-LOCAL
// endpoint (a contract requirement, §5.D) and is delivered locally, so it needs
// none of the three host-side egress mechanisms.
func routedEgress(d Declaration) bool {
	return d.Egress.Default == AllowPublic || len(d.Egress.Allow) > 0
}

// SecondHopRules generates the ENGINE-LEVEL second-hop rule set (§2.4 invariant 2,
// second half). One table, installed once when the orchestrator starts.
//
// # Why this is a separate table rather than a per-sandbox rule
//
// The predicate is `skuid`, not an interface: hop 2 is a HOST-LOCAL process
// opening its own connection on the guest's behalf, so it arrives on no interface
// at all. Measured, its egress counts ZERO in `forward` in all three shapes and is
// visible only in `output` (network.md D5 ㉔). Reading §2.4's two-hook table as
// "the forward row covers hop 2 as well" yields a rule set in which hop 2 is never
// evaluated — and the symptom is a floor counter that does not move, which is
// D4.1's silent pass.
//
// One uid means one rule set regardless of how many sandboxes exist, hence one
// table, installed at startup.
//
// # Why there is no loop-avoidance accept
//
// D4.2 lists two needs for this predicate with OPPOSITE positions: "exempt the
// process from re-matching its own steering rule" (an accept ABOVE the steering
// rules) and "keep it under the L0 floor" (a drop). The first is not needed here
// at all, and that is a consequence of the shape rather than a shortcut: every
// steering DNAT this generator emits is narrowed by `iifname <this sandbox's
// veth>`, and hop 2's traffic arrives on no interface — so it CANNOT match a
// steering rule, and the loop is structurally impossible.
//
// That matters because the alternative is unsafe: `skuid` does not reliably match
// every packet of a flow, so a skuid-narrowed accept sitting above a floor drop
// leaks whatever it misses (measured asymmetry, network.md D5 ㉔). ruleset.Validate
// refuses such a rule outright, and this rule set contains ONLY drops.
//
// # What it pins, and what it does not
//
// It pins the L0 FLOOR against hop 2 — the confused deputy §2.4's second invariant
// names: an "upstream" pointed at a sibling or a link-local address would
// otherwise walk in through the side door. It does NOT apply the internal
// deny-safe tier, because that tier is liftable BY DECLARATION and hop 2 carries
// no declaration: there is no declaration to lift it and no sandbox to attribute
// it to (one uid serves every `via[]` in the deployment). §2.4's obligation for
// this hop is stated over the floor.
func SecondHopRules() (ruleset.RuleSet, error) {
	g := &gen{}
	// THE RETURN ARM, ABOVE THE FLOOR AND IN THE SAME CHAIN.
	//
	// Without it this table kills `via[]` outright, and the mechanism is worth stating
	// because it is not obvious: the endpoint's REPLY to the guest is host-originated
	// (so it traverses `output`) and its destination is the GUEST IP — which lies in the
	// sandbox address space, i.e. exactly the floor's sibling item. So the floor below
	// matches the reply to the very flow the declaration authorized.
	//
	// Measured: with the floor alone, a steered flow reached the endpoint and was
	// accepted (`via0_0_dnat=1`, `via0_0_accept=1`, conntrack SYN_RECV) while
	// `hop2_floor_v4` counted 9 — the reply dropped by this engine, and the guest hung
	// 25 s on a connection whose request had succeeded.
	//
	// It does not weaken the invariant. The confused deputy §2.4 invariant 2 names is
	// the endpoint process opening a NEW connection to an in-floor address; `ct state
	// new` is precisely what this arm does not match, so a new connection still meets
	// the floor. What passes is only the reverse direction of a flow the guest itself
	// initiated — traffic conntrack already attributes to an established connection.
	//
	// It also has to be an ACCEPT rather than a narrower shape, and it is safe here for
	// the reason a uid-narrowed accept is NOT: it carries no `skuid`. The measured
	// asymmetry (D5 ㉔) is about skuid missing packets; a ct-state arm has no such gap.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopReturn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "hop2_established",
		Why:     "the reply half of a flow the GUEST initiated: host-originated, destination = the guest IP, which lies in the floor's sibling item — so without this arm this table drops the reply to the very flow the declaration authorized (measured: hop2_floor counted 9 while the guest hung). `ct state new` is excluded, so a NEW connection to an in-floor address still meets the floor (§2.4 invariant 2)",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopUID,
		Verdict: ruleset.Drop,
		Match:   ruleset.Match{DestV4: ruleset.FloorV4},
		Counter: "hop2_floor_v4",
		Why:     "§2.4 invariant 2: hop 2 stays under the L0 floor (v4) — a confused deputy pointed at a sibling or link-local address is refused by the KERNEL, not by that process's own discipline",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopUID,
		Verdict: ruleset.Drop,
		Match:   ruleset.Match{DestV6: ruleset.FloorV6},
		Counter: "hop2_floor_v6",
		Why:     "§2.4 invariant 2: hop 2 stays under the L0 floor (v6)",
	})
	if err := ruleset.Validate(g.rs); err != nil {
		return ruleset.RuleSet{}, fmt.Errorf("network: second-hop rule set is invalid: %w", err)
	}
	return g.rs, nil
}

// gen accumulates rules in emission order.
type gen struct {
	rs ruleset.RuleSet
}

func (g *gen) emit(r ruleset.Rule) {
	for _, c := range g.rs.Counters {
		if c == r.Counter {
			g.rs.Rules = append(g.rs.Rules, r)
			return
		}
	}
	g.rs.Counters = append(g.rs.Counters, r.Counter)
	g.rs.Rules = append(g.rs.Rules, r)
}

// steering emits the `via[]` DNATs, in declaration order — which IS the semantics:
// overlapping `via[]` entries are FIRST-MATCH-WINS and later ones are not
// evaluated, and that order is fixed by the contract rather than left to the
// implementation, so one declaration yields one reachable set under any
// implementation (§6.2).
func (g *gen) steering(d Declaration) {
	for i, v := range d.Egress.Via {
		for j, t := range v.Match {
			m := ruleset.Match{Proto: protoOf(t.Proto), DestPort: t.Port}
			// The pre-NAT destination clause is emitted IF AND ONLY IF the declaration
			// pinned a cidr. Transparent interception (`cidr: any`) authorizes "any
			// destination on that port", so keeping the clause would block exactly the
			// traffic the declaration allowed; an explicit endpoint without it would
			// authorize every other address on that port (network.md D3.3 / D5 ⑩).
			if !t.AnyDest() {
				setDest(&m, t.CIDR)
			}
			g.emit(ruleset.Rule{
				Hook:       ruleset.HookPrerouting,
				Scope:      ruleset.ScopeGuestIn,
				Verdict:    ruleset.DNAT,
				Match:      m,
				ToEndpoint: v.Endpoint,
				Counter:    fmt.Sprintf("via%d_%d_dnat", i, j),
				Why:        fmt.Sprintf("egress.via[%d].match[%d] → %s (first-match-wins, §6.2)", i, j, v.Endpoint),
			})
		}
	}
}

// publishInbound emits the `prerouting` DNAT per publish item: the inbound half.
func (g *gen) publishInbound(d Declaration) {
	for i, p := range d.Ingress.Publish {
		g.emit(ruleset.Rule{
			Hook:        ruleset.HookPrerouting,
			Scope:       ruleset.ScopeHostEndpoint,
			Verdict:     ruleset.DNAT,
			Match:       ruleset.Match{Proto: protoOf(p.Proto)},
			ToGuestPort: p.GuestPort,
			Counter:     fmt.Sprintf("pub%d_in_dnat", i),
			Why:         fmt.Sprintf("ingress.publish[%d]: host endpoint → guest %s/%d (external callers)", i, p.Proto, p.GuestPort),
		})
	}
}

// publishHostPath emits the `output`-hook DNAT per publish item — the second,
// independent path (network.md D5 ⑭; see Generate's comment for why it is not
// redundant).
func (g *gen) publishHostPath(d Declaration) {
	for i, p := range d.Ingress.Publish {
		g.emit(ruleset.Rule{
			Hook:        ruleset.HookOutputNAT,
			Scope:       ruleset.ScopeHostEndpoint,
			Verdict:     ruleset.DNAT,
			Match:       ruleset.Match{Proto: protoOf(p.Proto)},
			ToGuestPort: p.GuestPort,
			Counter:     fmt.Sprintf("pub%d_out_dnat", i),
			Why:         fmt.Sprintf("ingress.publish[%d]: the HOST's own path — host-originated traffic never traverses prerouting (D5 ⑭)", i),
		})
	}
}

// publishReturn emits the return SNAT: rewrite the source to this sandbox's own
// /30 gateway.
//
// WITHOUT IT A PUBLISH IS NOT "SLIGHTLY WORSE", IT IS BROKEN FOR HALF THE
// SANDBOXES: the guest's reply has nowhere to go in its netstack, so a caller from
// OUTSIDE the /30 reads a timeout, while a sandbox that happens to carry a default
// route works — measured `to_guest=2 / from_guest=0`, and adding the SNAT made the
// same flow connect immediately (network.md D5 ⑬). Rewriting the source to the
// gateway makes it on-link, so the reply needs no guest-side route at all.
//
// One rule per sandbox rather than per publish item: it is keyed on the guest IP,
// not on a port.
func (g *gen) publishReturn(d Declaration) {
	if len(d.Ingress.Publish) == 0 {
		return
	}
	g.emit(ruleset.Rule{
		Hook:          ruleset.HookPostrouting,
		Scope:         ruleset.ScopeGuestOut,
		Verdict:       ruleset.SNAT,
		Match:         ruleset.Match{DestGuest: true},
		ToSelfGateway: true,
		Counter:       "pub_return_snat",
		Why:           "publish return path: source → this sandbox's own /30 gateway, so the reply is on-link and needs no guest route (D5 ⑬)",
	})
}

// masquerade is the third of the three host-side mechanisms egress needs
// (network.md D5 ⑤: ip_forward, a guest default route carrying `dev`, and a
// masquerade for this /30 — measured as necessary AND sufficient; rp_filter,
// proxy_arp, accept_local and per-interface forwarding are all measured
// unnecessary, so the provider adds none of them).
//
// IT WIDENS NOTHING, and that is worth stating because a masquerade looks like a
// permission: only packets that a FILTER chain already accepted ever reach
// postrouting, so this rewrites the source of traffic whose reachability was
// already decided by the floor, the allows, the tier and the default. What it
// buys is that the reply can find its way back. Narrowed by the input interface —
// the unforgeable predicate — rather than by source address, which guest root
// rewrites with one `ip addr add` (measured, network.md D5 ③).
func (g *gen) masquerade() {
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookPostrouting,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Masquerade,
		Counter: "egress_masq",
		Why:     "the third host-side egress mechanism (D5 ⑤); it permits nothing — only traffic a filter chain already accepted reaches postrouting",
	})
}

// inputChain builds the per-sandbox `input` chain: guest → an address of the host
// itself. Order below IS the semantics; see Generate's comment.
func (g *gen) inputChain(d Declaration) error {
	// (1) The return path. FIRST, above the floor drop, and its absence is the one
	// failure in this whole design that is LOUD: the floor would drop the guest's
	// replies to the host and transport would die outright (network.md D5 ⑧). Two
	// narrowings would both work here; the input interface is the unforgeable one.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "in_established",
		Why:     "return path for host→guest flows (transport included). Family-UNQUALIFIED on purpose: one such rule covers IPv6 too, so the generator does not fork for it (D5 ⑪)",
	})

	// (2) The `via[]` narrow accepts. ABOVE the floor drop and IN THIS SAME CHAIN.
	//
	// They are required because a steered flow is DNAT'd in `prerouting` — before
	// the filter floor — and therefore lands in `input` as `ct state new`, which is
	// precisely what the pure ct-state floor below drops (measured: 8 packets on
	// `input`, 0 on `forward`). So without this the floor kills `via[]` outright.
	//
	// Each accept pins THREE things, and every omission has a live measured
	// counter-example (network.md D5 ⑩): the pre-NAT destination tuple, the
	// post-NAT listener port, and the interface. Dropping the listener port lets a
	// declared original port reach a listener the declaration never named; dropping
	// the pre-NAT tuple lets an UNDECLARED original port reach a declared listener;
	// dropping the interface makes it a hole in every other sandbox's floor.
	for i, v := range d.Egress.Via {
		_, endpointPort, err := ParseEndpoint(v.Endpoint)
		if err != nil {
			return fmt.Errorf("egress.via[%d].endpoint: %w", i, err)
		}
		for j, t := range v.Match {
			m := ruleset.Match{
				Proto:          protoOf(t.Proto),
				DestPort:       endpointPort, // post-NAT: the listener's port
				CTOriginalPort: t.Port,       // pre-NAT: the declared port
				CTState:        []string{"new"},
			}
			if !t.AnyDest() {
				cidr := t.CIDR
				m.CTOriginalDest = &cidr
			}
			g.emit(ruleset.Rule{
				Hook:    ruleset.HookInput,
				Scope:   ruleset.ScopeGuestIn,
				Verdict: ruleset.Accept,
				Match:   m,
				Counter: fmt.Sprintf("via%d_%d_accept", i, j),
				Why:     fmt.Sprintf("egress.via[%d].match[%d]: the approved floor exception — pre-NAT tuple ∧ post-NAT port ∧ interface, all three load-bearing (D5 ⑩), and in THIS chain because a cross-chain accept counts while the flow still dies", i, j),
			})
		}
	}

	// (3) The self-gateway allow, ABOVE the sibling drop.
	//
	// This is how §5.D semantics ③ gets a delivery path at all: under
	// `default: deny` a caller must be able to declare its own resolver, and the
	// only stably-available host-local resolver address is THIS SANDBOX'S OWN /30
	// gateway — which sits inside the sibling CIDR, so a plain
	// `drop <sandbox space>` kills it along with everything else (measured,
	// network.md D5 ⑦). Without this rule that promise has no way to be kept.
	//
	// Emitted for any allow[] tuple whose cidr INTERSECTS the sandbox address
	// space, carrying that tuple's port and protocol. The generator cannot test
	// containment: the gateway address is a placement product it does not know
	// (§5.D / §5.A ❌ list). The residual imprecision is bounded and stated: a
	// declaration naming a sandbox-space subnet that does not actually contain this
	// sandbox's gateway still reaches its own gateway on the declared port. The
	// alternative — having the provider test containment — would move behavior out
	// of the unit-testable generator, which is the property that makes every
	// declaration-value difference assertable without root.
	for i, t := range d.Egress.Allow {
		if !intersectsSandboxSpace(t.CIDR) {
			continue
		}
		cidr := t.CIDR
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookInput,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Accept,
			Match: ruleset.Match{
				DestSelfGateway: true,
				// AND the declared prefix. The emission predicate above can only test
				// OVERLAP — the gateway address is a placement product the generator does
				// not know — so without this clause a declaration merely overlapping the
				// sandbox space (`allow 10.66.99.0/24`) would also reach a gateway it never
				// contained, i.e. lift the floor's host-itself item on an address the caller
				// never named. The renderer ANDs the two into the intersection.
				DestSelfGatewayWithin: &cidr,
				Proto:                 protoOf(t.Proto),
				DestPort:              t.Port,
			},
			Counter: fmt.Sprintf("allow%d_selfgw", i),
			Why:     fmt.Sprintf("egress.allow[%d] ∩ this sandbox's own /30 gateway — §5.D semantics ③'s only delivery path, and it must sit above the sibling drop (D5 ⑦)", i),
		})
	}

	// (4) The sibling drop, BETWEEN the self-gateway allow and the declaration's
	// allows. Both positions are assertions with measured counter-examples:
	//
	//   - below the declaration's allows, an `allow 10.0.0.0/8` reaches a SIBLING's
	//     gateway (measured: it reads the sibling's banner) — and the `forward`
	//     side's sibling drop counts ZERO on this path, so reading only that
	//     counter turns a real leak into "the floor counter is 0", which by the
	//     acceptance discipline is an INVALID assertion rather than a pass (D4.2).
	//   - above the self-gateway allow, it kills semantics ③ (the case in (3)).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV4: []netip.Prefix{ruleset.SandboxCIDR}},
		Counter: "in_sibling",
		Why:     "L0 floor, sibling item on the input side: a SIBLING's /30 gateway is a host address, so only this chain sees it — the forward-side sibling drop counts 0 on this path (D4.2)",
	})

	// (5) The declaration's allows, on THIS hook too. `forward`-only allows cannot
	// reach a host-local address at all, which is why §5.D requires both sides
	// (D4.2).
	for i, t := range d.Egress.Allow {
		m := ruleset.Match{Proto: protoOf(t.Proto), DestPort: t.Port}
		if !t.AnyDest() {
			setDest(&m, t.CIDR)
		}
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookInput,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Accept,
			Match:   m,
			Counter: fmt.Sprintf("allow%d_in", i),
			Why:     fmt.Sprintf("egress.allow[%d] on the input hook — a forward-only allow cannot reach a host-local destination (D4.2)", i),
		})
	}

	// (6) THE FLOOR on this side: one `iifname`-narrowed PURE CT-STATE rule, with
	// NO address list.
	//
	// "Not established" is exhaustive over every address of the host, so the host's
	// own endpoints are NEVER computed: the engine does not need to know its bind
	// addresses, and a host address added AFTER the rules were installed is covered
	// too (measured, network.md D5 ⑧). The `iifname` narrowing is the ENTIRE reason
	// this is a per-sandbox rule and not a host-level inbound kill switch — without
	// it, it would take out the orchestrator's own 127.0.0.1 listener and sshd
	// along with the guest, because loopback traverses `input` as well.
	//
	// `reject`, not `drop`: this refusal faces the GUEST, and gVisor's netstack
	// honors ICMP errors, so the two spellings differ by orders of magnitude in
	// what the guest experiences. A pure-`drop` floor satisfies §2.4 fail-closed's
	// security half while violating the half it wrote down itself — "the guest gets
	// a clean connection failure". Security is identical either way (network.md
	// D5 ⑨ / D4.3).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{CTState: []string{"new", "invalid", "untracked"}},
		Counter: "in_floor",
		Why:     "L0 floor, host-itself item: a pure ct-state rule, so host addresses are NEVER enumerated and one added later is still covered (D5 ⑧). Covers IPv6 with no ip6 arm (D5 ⑪)",
	})
	return nil
}

// forwardChain builds the per-sandbox `forward` chain: guest → whatever the host
// routes, and traffic routed toward the guest.
func (g *gen) forwardChain(d Declaration) error {
	// (1)(2) The two return-path arms. Both directions of an established flow pass
	// through `forward`, so both need an arm, each narrowed to this sandbox's veth
	// — outbound by the interface it arrives on, inbound by the interface it leaves
	// by. Family-unqualified, so IPv6 rides the same arm (D5 ⑪).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "fwd_established_out",
		Why:     "established/related, guest→out. Measured load-bearing: remove it and even an ALLOWED flow breaks, because the reverse direction has no other way in (D5 ⑨)",
	})
	// THE INBOUND RETURN ARM LIVES ON THE INGRESS CHAIN, not beside its outbound twin,
	// and that placement is forced by the cross-chain rule (D5 ⑩): an accept ends only
	// ITS OWN chain, while a drop terminates across chains. Put this arm on the main
	// chain and the ingress default-deny below — on a later chain — still drops every
	// reply it just accepted.
	//
	// Measured exactly that way: an allowed public egress flow had `fwd_established_in`
	// counting 9 accepted packets AND `fwd_ingress_closed` dropping the same 9, so the
	// guest hung 25 s on a flow whose request left and whose reply was refused by this
	// engine. The accept must sit in the SAME chain as the drop it overrides, above it —
	// the same rule that puts `via[]`'s narrow accept in the floor's chain.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForwardIngress,
		Scope:   ruleset.ScopeGuestOut,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "fwd_established_in",
		Why:     "established/related, in→guest (the reply half of any allowed egress, and of a published flow). On the INGRESS chain because an accept ends only its own chain while the ingress drop crosses chains (D5 ⑩)",
	})

	// (3)(4) THE FLOOR's daddr list, ABOVE the declaration's allows — this is what
	// "no declaration lifts the floor" IS, materialized purely by order (§2.4).
	// Two rules because nft's grammar is family-typed, NOT because the generator
	// forks for IPv6: the ct-state arms above carry no family qualifier and cover
	// both families in one rule each (D5 ⑪). These are declaration-layer ADDRESS
	// members, which nft can only spell per family.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV4: ruleset.FloorV4},
		Counter: "fwd_floor_v4",
		Why:     "L0 floor daddr items, v4: loopback / link-local (every cloud metadata endpoint) / sibling sandbox address space — §2.3's cross-sandbox isolation, which point-to-point veth does NOT deliver on its own (the host routes between two /30s, measured D5 ①)",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV6: ruleset.FloorV6},
		Counter: "fwd_floor_v6",
		Why:     "L0 floor daddr items, v6: loopback / link-local. Two rules only because `ip daddr` and `ip6 daddr` are family-typed — not a v6 fork of the mechanism (D5 ⑪)",
	})

	// (5) The declaration's allows.
	for i, t := range d.Egress.Allow {
		m := ruleset.Match{Proto: protoOf(t.Proto), DestPort: t.Port}
		if !t.AnyDest() {
			setDest(&m, t.CIDR)
		}
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Accept,
			Match:   m,
			Counter: fmt.Sprintf("allow%d_fwd", i),
			Why:     fmt.Sprintf("egress.allow[%d] — below the floor, above the deny-safe tier", i),
		})
	}

	// (6) THE INTERNAL DENY-SAFE TIER, BELOW the allows and ABOVE the default.
	//
	// Both positions are assertions. Above the allows it would stop being
	// deny-SAFE (no declaration could lift it, making it a second floor); above the
	// floor, an `allow 10.0.0.0/8` would lift the floor's sibling item along with
	// it. It is NOT optional hardening: with the three host-side egress mechanisms
	// in place and this tier missing, `default: allow` hands the host's entire LAN
	// to the sandbox (measured, network.md D5 ⑤) — an `allow` strictly wider than
	// §2.4 defines.
	//
	// Emitted only under `default: allow`, because under `deny` the final refusal
	// below already covers every one of these addresses; a tier rule there would be
	// a rule that never matches, i.e. one more legitimate zero in the counter
	// signal every deny assertion reads.
	//
	// Separate rules from the floor, never a shared set: the sibling /16 nests
	// inside 10/8, and an nft interval set either refuses to load or silently
	// merges the subnet away (measured, D5 ⑱). ruleset.Validate enforces it.
	if d.Egress.Default == AllowPublic {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Drop,
			Match:   ruleset.Match{DestV4: ruleset.TierV4},
			Counter: "fwd_tier_v4",
			Why:     "internal deny-safe tier, v4 (RFC1918): liftable BY DECLARATION, which is materialized purely by sitting BELOW the allows (§2.4). Without it `default: allow` would hand over the host's LAN (D5 ⑤)",
		})
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Drop,
			Match:   ruleset.Match{DestV6: ruleset.TierV6},
			Counter: "fwd_tier_v6",
			Why:     "internal deny-safe tier, v6: fc00::/7 plus the NAT64 IMAGE of each v4 member — never the 64:ff9b::/96 prefix itself, which maps the whole v4 space and would refuse the public egress `allow` promises (D5 ㉓)",
		})
	}

	// (7) The default. Under `allow` an explicit accept rather than falling through
	// to the chain policy: the base chain must be `policy accept` (a `policy drop`
	// in a per-sandbox table is a CROSS-SANDBOX kill switch — measured, it killed
	// another sandbox's traffic while its own rule matched not one packet), so
	// falling through would work, but it would leave the "this declaration permits
	// public egress" decision with no counter of its own and nothing for an
	// acceptance test to read.
	if d.Egress.Default == AllowPublic {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Accept,
			Counter: "fwd_default_allow",
			Why:     "egress.default: allow — public egress, i.e. everything the floor above and the deny-safe tier above did not refuse (§2.4)",
		})
	} else {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Reject,
			Counter: "fwd_default_deny",
			Why:     "egress.default: deny — the forward side needs its OWN default-deny: a destination that is not on the host and was not steered never traverses `input` at all (D5 ⑩)",
		})
	}

	// (8) The publish companion accepts, on `forward` — NOT `input`.
	//
	// The hook assignment is the OPPOSITE of `via[]`'s, and the difference is
	// mechanical: after the DNAT, a steered flow's destination is a HOST-LOCAL
	// endpoint (so it stays on this machine and lands on `input`), while a
	// published flow's destination is the GUEST IP (so it must be routed onward and
	// lands on `forward`). Measured both ways — the forward counters rise per
	// published flow while the input ones stay at zero (network.md D5 ⑮). Same
	// prerouting DNAT, two different downstream hooks, decided only by whether the
	// post-NAT destination is a local address.
	for i, p := range d.Ingress.Publish {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForwardIngress,
			Scope:   ruleset.ScopeGuestOut,
			Verdict: ruleset.Accept,
			Match:   ruleset.Match{DestGuest: true, Proto: protoOf(p.Proto), DestPort: p.GuestPort},
			Counter: fmt.Sprintf("pub%d_fwd", i),
			Why:     fmt.Sprintf("ingress.publish[%d] companion accept, on FORWARD (post-DNAT the destination is the guest IP, so it is routed onward — the opposite of via[]'s hook, D5 ⑮)", i),
		})
	}

	// (9) The inbound default-deny: ingress is closed unless published (§2.1 ③).
	//
	// On a SEPARATE chain at a later priority (HookForwardIngress), because this drop
	// is the one rule of a per-sandbox table that can match ANOTHER sandbox's egress —
	// traffic from A to B's guest IP is `oifname B-veth`. At one shared priority, B's
	// drop raced A's floor and won, which cost the guest a 25-second hang instead of a
	// clean refusal AND made the counter that moves depend on table creation order
	// (measured; see ruleset.HookForwardIngress).
	//
	// `drop`, not `reject`, and the asymmetry with the guest-facing refusals above
	// is deliberate: this one faces an EXTERNAL caller, to whom the engine promises
	// nothing, and an ICMP error would confirm the sandbox exists. The guest-facing
	// half of fail-closed (§2.4: "the guest gets a clean connection failure") is
	// about the guest, and this refusal is not aimed at it.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForwardIngress,
		Scope:   ruleset.ScopeGuestOut,
		Verdict: ruleset.Drop,
		Counter: "fwd_ingress_closed",
		Why:     "ingress default-deny (§2.1 ③): inbound is closed unless published. Without it, anything the host routes could reach the guest IP directly",
	})
	return nil
}

// setDest puts a prefix into the family-appropriate slot. nft's grammar is
// family-typed, so a rule carries one family or the other, never both
// (ruleset.Validate refuses a rule holding both).
func setDest(m *ruleset.Match, p netip.Prefix) {
	if p.Addr().Is4() {
		m.DestV4 = []netip.Prefix{p}
		return
	}
	m.DestV6 = []netip.Prefix{p}
}

// intersectsSandboxSpace reports whether a declared prefix overlaps the sandbox
// address space at all — the trigger for the self-gateway allow (see
// inputChain (3)). Intersection rather than containment in EITHER direction,
// because both readings occur in practice: `10.0.0.0/8` is a supernet of the
// sandbox space, while a caller pinning a narrow subnet inside it is a subset.
func intersectsSandboxSpace(p netip.Prefix) bool {
	if !p.IsValid() || !p.Addr().Is4() {
		return false
	}
	return p.Overlaps(ruleset.SandboxCIDR)
}
