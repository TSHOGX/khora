package network

import (
	"fmt"
	"net/netip"

	"github.com/TSHOGX/khora/internal/ruleset"
)

// Spec is what the documented invariant hands across the the documented invariant seam: the abstract rule set plus the
// publish descriptors whose host endpoints only the provider can bind.
//
// It is the DERIVED result, never the declaration itself — the provider installs
// and holds no authority (the documented invariant / the documented invariant ③).
type Spec struct {
	// Rules is the abstract, placement-free rule set.
	Rules ruleset.RuleSet
	// Publish lists the `{guest_port, proto}` items a host endpoint must be bound
	// for, in declaration order. The provider allocates each endpoint at placement
	// and substitutes it into the rules that carry ScopeHostEndpoint.
	Publish []Publish
	// RoutedEgress reports whether this declaration can send anything out through
	// the host's router — i.e. whether the provider must put the host-side egress
	// plumbing in place (the relevant internals page the documented invariant ⑤'s three mechanisms).
	//
	// It is a derived FACT rather than a knob: `via[]` alone does not need it (a
	// steered flow is DNAT'd to a host-local endpoint and never leaves), while any
	// `allow[]` entry or `default: allow` does.
	RoutedEgress bool

	// Counters maps each DECLARATION ENTRY to the counter names its rules carry, so a
	// caller reading counters back off a placement can attribute them to the entry the
	// user wrote (the documented invariant's per-entry `hit_packet`).
	//
	// Produced HERE, by the same expressions that mint the names. Re-deriving
	// `fmt.Sprintf("allow%d_fwd", i)` at the read site would be a second copy of a
	// naming convention living in this file: it drifts on the first rename and fails
	// SILENTLY, since an unmatched name reports no hits, which reads exactly like a
	// rule that was never hit.
	Counters CounterAttribution
}

// CounterAttribution maps declaration entries to the abstract counter names carrying
// their hit counts. Indices are declaration order, so Allow[i] belongs to
// `egress.allow[i]` and Via[i][j] to `egress.via[i].match[j]`.
type CounterAttribution struct {
	// Allow[i] are ALL the counters for `egress.allow[i]`, which a reader must SUM.
	// One entry legitimately emits up to three rules on different hooks — `input`,
	// `forward`, and the self-gateway accept — and they are mutually exclusive per
	// packet (a packet takes one hook, and within `input` an accept ends the chain), so
	// summing double-counts nothing.
	//
	// DROPPING THE SELF-GATEWAY ONE IS THE TEMPTING MISTAKE: it is the only delivery
	// path for a declared host-local resolver (the documented invariant semantics ③ / the relevant internals page the documented invariant ⑦), so
	// an entry that works entirely through it would report 0 hits while plainly
	// working — the "is this rule live" signal inverted on exactly the case operators
	// reach for first.
	Allow [][]string

	// Via[i][j] are the counters for `egress.via[i].match[j]`, and this is ONE name
	// rather than a set: the `prerouting` DNAT, which IS the match. The companion
	// accept on another hook acts on the same flow, so including it would count one
	// flow twice.
	//
	// The divergence dnat > 0 with accept == 0 is a known ENGINE defect signature
	// (the relevant internals page the documented invariant ⑩'s floor-kills-`via[]` case), not a number a caller can act on,
	// which is the other reason it stays out of this.
	Via [][]string

	// Default is the counter on the rule `egress.default` itself generates: the
	// catch-all refusal under `deny`, the catch-all accept under `allow`.
	//
	// The L0 floor and the deny-safe tier are deliberately NOT here. A floor refusal is
	// liftable by no declaration (the documented invariant), so attributing it to `egress.default` would
	// send a caller to widen an allow-list that cannot help.
	Default string

	// IngressClosed is the counter on the ingress default-deny — the rule that drops
	// inbound traffic to this guest which no `ingress.publish[]` entry opened (the documented invariant ③).
	//
	// DIRECTION-LEVEL, never per-publish: a publish entry's rules span three hooks and
	// the `forward` one matches other sandboxes' egress, so no per-entry number is
	// cleanly attributable. What this one answers is whether anything was refused
	// toward this guest at all, as opposed to nothing having arrived — otherwise
	// indistinguishable from outside the engine, since a dropped inbound packet is
	// visible from no vantage point there.
	//
	// It does NOT identify the sender: this is the one rule in a per-sandbox table that
	// can match another sandbox's egress (the reason it sits at its own
	// HookForwardIngress priority), so a nonzero value means "something was refused on
	// the way to this guest", never "who sent it".
	IngressClosed string
}

// Generate is the documented invariant's pure function: `(declaration, L0 floor) → abstract rule
// set`. Placement-free, root-free, unit-testable (the documented invariant / the relevant internals page the documented invariant).
//
// # Order is the security property
//
// Every position below is an assertion: a rule set with the right rules in the wrong order
// behaves perfectly on ordinary traffic and differs only when it is being bypassed (the relevant internals page
// the documented invariant ④ / the documented invariant). Each position is argued where it is emitted — inputChain and forwardChain,
// numbered step by step.
//
// # Why the first hop is two chains
//
// Traffic to an address of the host ITSELF lands on `input`; traffic the host
// ROUTES lands on `forward` (measured, the relevant internals page the documented invariant ⑧), and the two sides carry
// OPPOSITE predicate shapes: see inputChain (6) for the `input` floor and
// forwardChain (3)(4)(7) for the `forward` daddr list and its own default-deny.
func Generate(d Declaration) (Spec, error) {
	d = d.Normalize()
	g := &gen{}

	// --- nat prerouting: steering and the inbound half of a publish -----------
	g.steering(d)
	g.publishInbound(d)

	// --- nat output: the host's own path to a published endpoint --------------
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
	// Deliberately absent from this rule set — SecondHopRules generates it as one
	// engine-level table (see there). ValidateDeployment refuses a `via[]`
	// declaration when the deployment supplied no uid: a declaration whose own
	// invariant cannot be delivered is refused rather than half-honored.

	if err := ruleset.Validate(g.rs); err != nil {
		return Spec{}, fmt.Errorf("network: generated rule set is invalid: %w", err)
	}
	return Spec{Rules: g.rs, Publish: d.Ingress.Publish, RoutedEgress: routed, Counters: g.attr}, nil
}

// routedEgress reports whether the declaration can send anything through the
// host's router — see Spec.RoutedEgress for why `via[]` alone cannot.
func routedEgress(d Declaration) bool {
	return d.Egress.Default == AllowPublic || len(d.Egress.Allow) > 0
}

// SecondHopRules generates the ENGINE-LEVEL second-hop rule set (the documented invariant invariant 2,
// second half). One table, installed once when the orchestrator starts.
//
// A SEPARATE TABLE rather than a per-sandbox rule: hop 2 arrives on no interface at all, so the
// predicate is `skuid` and one uid covers every sandbox (see ruleset.HookOutputFilter for the
// measurement, and why reading the documented invariant's two-hook table as "the forward row covers hop 2 too"
// yields a rule set where hop 2 is never evaluated).
//
// NO LOOP-AVOIDANCE ACCEPT. the documented invariant lists two needs for this predicate at opposite positions —
// exempt the process from re-matching its own steering rule (an accept ABOVE them), and keep it
// under the L0 floor (a drop). The first is structurally unnecessary: every steering DNAT this
// generator emits is narrowed by `iifname <that sandbox's veth>`, and hop 2 arrives on no
// interface. That matters because the accept would be unsafe — `skuid` does not reliably match
// every packet, so one above a floor drop leaks whatever it misses (the documented invariant ㉔). ruleset.Validate
// refuses one outright; this set contains ONLY drops.
//
// IT PINS THE L0 FLOOR against hop 2 — the confused deputy the documented invariant's second invariant names, where
// an "upstream" pointed at a sibling or link-local address walks in the side door. It does NOT
// apply the internal deny-safe tier, which is liftable BY DECLARATION while hop 2 carries none
// and has no sandbox to attribute it to (one uid serves every `via[]`).
func SecondHopRules() (ruleset.RuleSet, error) {
	g := &gen{}
	// THE RETURN ARM, ABOVE THE FLOOR AND IN THE SAME CHAIN. Without it this table kills
	// `via[]` outright: the endpoint's REPLY to the guest is host-originated (so it traverses
	// `output`) and its destination is the GUEST IP, which lies in the sandbox address space —
	// exactly the floor's sibling item. So the floor below matches the reply to the very flow
	// the declaration authorized.
	//
	// Measured: with the floor alone, a steered flow reached the endpoint and was
	// accepted (`via0_0_dnat=1`, `via0_0_accept=1`, conntrack SYN_RECV) while
	// `hop2_floor_v4` counted 9 — the reply dropped by this engine, and the guest hung
	// 25 s on a connection whose request had succeeded.
	//
	// It does not weaken the invariant, and it is safe as an ACCEPT here for the reason a
	// uid-narrowed one is not — see ruleset.ScopeSecondHopReturn.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopReturn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "hop2_established",
		Why:     "the reply half of a flow the GUEST initiated: host-originated, destination = the guest IP, which lies in the floor's sibling item — so without this arm this table drops the reply to the very flow the declaration authorized (measured: hop2_floor counted 9 while the guest hung). `ct state new` is excluded, so a NEW connection to an in-floor address still meets the floor (the documented invariant invariant 2)",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopUID,
		Verdict: ruleset.Drop,
		Match:   ruleset.Match{DestV4: ruleset.FloorV4},
		Counter: "hop2_floor_v4",
		Why:     "the documented invariant invariant 2: hop 2 stays under the L0 floor (v4) — a confused deputy pointed at a sibling or link-local address is refused by the KERNEL, not by that process's own discipline",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookOutputFilter,
		Scope:   ruleset.ScopeSecondHopUID,
		Verdict: ruleset.Drop,
		Match:   ruleset.Match{DestV6: ruleset.FloorV6},
		Counter: "hop2_floor_v6",
		Why:     "the documented invariant invariant 2: hop 2 stays under the L0 floor (v6)",
	})
	if err := ruleset.Validate(g.rs); err != nil {
		return ruleset.RuleSet{}, fmt.Errorf("network: second-hop rule set is invalid: %w", err)
	}
	return g.rs, nil
}

// gen accumulates rules in emission order.
type gen struct {
	rs ruleset.RuleSet
	// attr accumulates which declaration entry each counter belongs to. Filled by the
	// allowCounter / viaCounter / defaultCounter helpers below rather than by the emit
	// sites, so the name and its attribution are produced by ONE expression and a
	// rename cannot separate them (see Spec.Counters).
	attr CounterAttribution
}

// defaultCounter records the counter on the rule `egress.default` itself generates. A
// pass-through that exists so the attribution is written where the name is chosen, the
// same discipline as the two helpers below.
func (g *gen) defaultCounter(name string) string {
	g.attr.Default = name
	return name
}

// allowCounter mints a counter name for `egress.allow[i]` and records it against that
// entry in the same step. Every allow-side name goes through here.
func (g *gen) allowCounter(i int, suffix string) string {
	name := fmt.Sprintf("allow%d_%s", i, suffix)
	for len(g.attr.Allow) <= i {
		g.attr.Allow = append(g.attr.Allow, nil)
	}
	g.attr.Allow[i] = append(g.attr.Allow[i], name)
	return name
}

// viaCounter mints a counter name for `egress.via[i].match[j]`. Only the DNAT name is
// recorded as attribution (see CounterAttribution.Via for why the companion accept is
// excluded), so `record` is explicit rather than implied by the call.
func (g *gen) viaCounter(i, j int, suffix string, record bool) string {
	name := fmt.Sprintf("via%d_%d_%s", i, j, suffix)
	if !record {
		return name
	}
	for len(g.attr.Via) <= i {
		g.attr.Via = append(g.attr.Via, nil)
	}
	for len(g.attr.Via[i]) <= j {
		g.attr.Via[i] = append(g.attr.Via[i], "")
	}
	g.attr.Via[i][j] = name
	return name
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
// implementation (the documented invariant).
func (g *gen) steering(d Declaration) {
	for i, v := range d.Egress.Via {
		for j, t := range v.Match {
			// t.Port == 0 (AnyPort) needs no branch: the zero already IS the wildcard
			// encoding in Match (see ruleset.Match.DestPort), and the assertion that it
			// renders as an absent `dport` lives in the runtime tests.
			m := ruleset.Match{Proto: protoOf(t.Proto), DestPort: t.Port}
			// The pre-NAT destination clause is emitted IF AND ONLY IF the declaration
			// pinned a cidr. Transparent interception (`cidr: any`) authorizes "any
			// destination on that port", so keeping the clause would block exactly the
			// traffic the declaration allowed; an explicit endpoint without it would
			// authorize every other address on that port (the relevant internals page the documented invariant / the documented invariant ⑩).
			if !t.AnyDest() {
				setDest(&m, t.CIDR)
			}
			g.emit(ruleset.Rule{
				Hook:       ruleset.HookPrerouting,
				Scope:      ruleset.ScopeGuestIn,
				Verdict:    ruleset.DNAT,
				Match:      m,
				ToEndpoint: v.Endpoint,
				Counter:    g.viaCounter(i, j, "dnat", true),
				Why:        fmt.Sprintf("egress.via[%d].match[%d] → %s (first-match-wins, the documented invariant)", i, j, v.Endpoint),
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
// independent path (the relevant internals page the documented invariant ⑭; see Generate's comment for why it is not
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
			Why:         fmt.Sprintf("ingress.publish[%d]: the HOST's own path — host-originated traffic never traverses prerouting (the documented invariant ⑭)", i),
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
// same flow connect immediately (the relevant internals page the documented invariant ⑬). Rewriting the source to the
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
		Why:           "publish return path: source → this sandbox's own /30 gateway, so the reply is on-link and needs no guest route (the documented invariant ⑬)",
	})
}

// masquerade is the third of the three host-side mechanisms egress needs
// (the relevant internals page the documented invariant ⑤: ip_forward, a guest default route carrying `dev`, and a
// masquerade for this /30 — measured as necessary AND sufficient; rp_filter,
// proxy_arp, accept_local and per-interface forwarding are all measured
// unnecessary, so the provider adds none of them).
//
// IT WIDENS NOTHING, though a masquerade looks like a permission: only packets a FILTER chain
// already accepted ever reach postrouting, so this rewrites the source of traffic whose
// reachability the floor, the allows, the tier and the default already decided. What it buys is
// a reply that can find its way back. Narrowed by the input interface, never by source address
// (ruleset.ScopeGuestIn).
func (g *gen) masquerade() {
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookPostrouting,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Masquerade,
		Counter: "egress_masq",
		Why:     "the third host-side egress mechanism (the documented invariant ⑤); it permits nothing — only traffic a filter chain already accepted reaches postrouting",
	})
}

// inputChain builds the per-sandbox `input` chain: guest → an address of the host
// itself. Order below IS the semantics; see Generate's comment.
func (g *gen) inputChain(d Declaration) error {
	// (1) The return path. FIRST, above the floor drop, and its absence is the one
	// failure in this whole design that is LOUD: the floor would drop the guest's
	// replies to the host and transport would die outright (the relevant internals page the documented invariant ⑧). Two
	// narrowings would both work here; the input interface is the unforgeable one.
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "in_established",
		Why:     "return path for host→guest flows (transport included). Family-UNQUALIFIED on purpose: one such rule covers IPv6 too, so the generator does not fork for it (the documented invariant ⑪)",
	})

	// (2) The `via[]` narrow accepts. ABOVE the floor drop and IN THIS SAME CHAIN.
	//
	// They are required because a steered flow is DNAT'd in `prerouting` — before
	// the filter floor — and therefore lands in `input` as `ct state new`, which is
	// precisely what the pure ct-state floor below drops (measured: 8 packets on
	// `input`, 0 on `forward`). So without this the floor kills `via[]` outright.
	//
	// Each accept pins THREE things — the pre-NAT destination tuple, the post-NAT
	// listener port, and the interface — and every omission has a live measured
	// counter-example (the relevant internals page the documented invariant ⑩'s four-row table).
	for i, v := range d.Egress.Via {
		_, endpointPort, err := ParseEndpoint(v.Endpoint)
		if err != nil {
			return fmt.Errorf("egress.via[%d].endpoint: %w", i, err)
		}
		for j, t := range v.Match {
			// A PORT WILDCARD (`t.Port == 0`) leaves the pre-NAT PORT clause off, exactly as
			// `cidr: any` leaves the pre-NAT DESTINATION clause off. That does not weaken the
			// accept below what the documented invariant ⑩ requires: what the measurement forbids is admitting MORE
			// than the declaration authorized, and the clause dropped here is the one the
			// declaration itself declined to pin — "any original port, arriving on this
			// sandbox's veth, destined for this endpoint's listener port" IS the authorized
			// set. `iifname` is untouched in every case.
			m := ruleset.Match{
				Proto:          protoOf(t.Proto),
				DestPort:       endpointPort, // post-NAT: the listener's port
				CTOriginalPort: t.Port,       // pre-NAT: the declared port, 0 = any (the documented invariant)
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
				Counter: g.viaCounter(i, j, "accept", false),
				Why:     fmt.Sprintf("egress.via[%d].match[%d]: the approved floor exception — pre-NAT tuple ∧ post-NAT port ∧ interface, all three load-bearing (the documented invariant ⑩), and in THIS chain because a cross-chain accept counts while the flow still dies", i, j),
			})
		}
	}

	// (3) The self-gateway allow, ABOVE the sibling drop.
	//
	// This is how the documented invariant semantics ③ gets a delivery path at all: under
	// `default: deny` a caller must be able to declare its own resolver, and the
	// only stably-available host-local resolver address is THIS SANDBOX'S OWN /30
	// gateway — which sits inside the sibling CIDR, so a plain
	// `drop <sandbox space>` kills it along with everything else (measured,
	// the relevant internals page the documented invariant ⑦). Without this rule that promise has no way to be kept.
	//
	// Emitted for any allow[] tuple whose cidr INTERSECTS the sandbox address
	// space, carrying that tuple's port and protocol. The generator cannot test
	// containment: the gateway address is a placement product it does not know
	// (the documented invariant / the documented invariant ❌ list). Having the provider test it instead would move behavior
	// out of the unit-testable generator, the property that makes every
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
				// AND the declared prefix, which the renderer ANDs into the intersection.
				// Without it a declaration merely OVERLAPPING the sandbox space reaches a
				// gateway it never contained — see ruleset.Match.DestSelfGatewayWithin.
				DestSelfGatewayWithin: &cidr,
				Proto:                 protoOf(t.Proto),
				DestPort:              t.Port,
			},
			Counter: g.allowCounter(i, "selfgw"),
			Why:     fmt.Sprintf("egress.allow[%d] ∩ this sandbox's own /30 gateway — the documented invariant semantics ③'s only delivery path, and it must sit above the sibling drop (the documented invariant ⑦)", i),
		})
	}

	// (4) The sibling drop, BETWEEN the self-gateway allow and the declaration's
	// allows. Both positions are assertions with measured counter-examples:
	//
	//   - below the declaration's allows, an `allow 10.0.0.0/8` reaches a SIBLING's
	//     gateway (measured: it reads the sibling's banner) — and the `forward`
	//     side's sibling drop counts ZERO on this path, so reading only that
	//     counter turns a real leak into "the floor counter is 0", which by the
	//     acceptance discipline is an INVALID assertion rather than a pass (the documented invariant).
	//   - above the self-gateway allow, it kills semantics ③ (the case in (3)).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV4: []netip.Prefix{ruleset.SandboxCIDR}},
		Counter: "in_sibling",
		Why:     "L0 floor, sibling item on the input side: a SIBLING's /30 gateway is a host address, so only this chain sees it — the forward-side sibling drop counts 0 on this path (the documented invariant)",
	})

	// (5) The declaration's allows, on THIS hook too. `forward`-only allows cannot
	// reach a host-local address at all, which is why the documented invariant requires both sides
	// (the documented invariant).
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
			Counter: g.allowCounter(i, "in"),
			Why:     fmt.Sprintf("egress.allow[%d] on the input hook — a forward-only allow cannot reach a host-local destination (the documented invariant)", i),
		})
	}

	// (6) THE FLOOR on this side: one `iifname`-narrowed PURE CT-STATE rule, with
	// NO address list.
	//
	// "Not established" is exhaustive over every address of the host, so the host's
	// own endpoints are NEVER computed: the engine does not need to know its bind
	// addresses, and a host address added AFTER the rules were installed is covered
	// too (measured, the relevant internals page the documented invariant ⑧). The `iifname` narrowing is the ENTIRE reason
	// this is a per-sandbox rule and not a host-level inbound kill switch — without
	// it, it would take out the orchestrator's own 127.0.0.1 listener and sshd
	// along with the guest, because loopback traverses `input` as well.
	//
	// `reject`, not `drop`: this refusal faces the GUEST (ruleset.Reject).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookInput,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{CTState: []string{"new", "invalid", "untracked"}},
		Counter: "in_floor",
		Why:     "L0 floor, host-itself item: a pure ct-state rule, so host addresses are NEVER enumerated and one added later is still covered (the documented invariant ⑧). Covers IPv6 with no ip6 arm (the documented invariant ⑪)",
	})
	return nil
}

// forwardChain builds the per-sandbox `forward` chain: guest → whatever the host
// routes, and traffic routed toward the guest.
func (g *gen) forwardChain(d Declaration) error {
	// (1)(2) The two return-path arms. Both directions of an established flow pass
	// through `forward`, so both need an arm, each narrowed to this sandbox's veth
	// — outbound by the interface it arrives on, inbound by the interface it leaves
	// by. Family-unqualified, so IPv6 rides the same arm (the documented invariant ⑪).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Accept,
		Match:   ruleset.Match{CTState: []string{"established", "related"}},
		Counter: "fwd_established_out",
		Why:     "established/related, guest→out. Measured load-bearing: remove it and even an ALLOWED flow breaks, because the reverse direction has no other way in (the documented invariant ⑨)",
	})
	// THE INBOUND RETURN ARM LIVES ON THE INGRESS CHAIN, not beside its outbound twin,
	// and that placement is forced by the cross-chain rule (the documented invariant ⑩): an accept ends only
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
		Why:     "established/related, in→guest (the reply half of any allowed egress, and of a published flow). On the INGRESS chain because an accept ends only its own chain while the ingress drop crosses chains (the documented invariant ⑩)",
	})

	// (3)(4) THE FLOOR's daddr list, ABOVE the declaration's allows — this is what
	// "no declaration lifts the floor" IS, materialized purely by order (the documented invariant).
	// Two rules because nft's grammar is family-typed, NOT because the generator
	// forks for IPv6 (see ruleset.FloorV4).
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV4: ruleset.FloorV4},
		Counter: "fwd_floor_v4",
		Why:     "L0 floor daddr items, v4: loopback / link-local (every cloud metadata endpoint) / sibling sandbox address space — the documented invariant's cross-sandbox isolation, which point-to-point veth does NOT deliver on its own (the host routes between two /30s, measured the documented invariant ①)",
	})
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForward,
		Scope:   ruleset.ScopeGuestIn,
		Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV6: ruleset.FloorV6},
		Counter: "fwd_floor_v6",
		Why:     "L0 floor daddr items, v6: loopback / link-local. Two rules only because `ip daddr` and `ip6 daddr` are family-typed — not a v6 fork of the mechanism (the documented invariant ⑪)",
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
			Counter: g.allowCounter(i, "fwd"),
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
	// to the sandbox (measured, the relevant internals page the documented invariant ⑤) — an `allow` strictly wider than
	// the documented invariant defines.
	//
	// Emitted only under `default: allow`, because under `deny` the final refusal
	// below already covers every one of these addresses; a tier rule there would be
	// a rule that never matches, i.e. one more legitimate zero in the counter
	// signal every deny assertion reads.
	//
	// `drop`, not `reject`, and unlike the ingress default-deny below the reason is
	// not secrecy: this refusal DOES face the guest. the documented invariant scopes the guest-facing
	// half of fail-closed to denials the engine owes an error for — a target the
	// DECLARATION did not reach — and these addresses are not that. Under
	// `default: allow` the caller asked for the public internet; the tier is telling
	// it that the host's own LAN was never part of that, which is the the documented invariant boundary
	// itself rather than a denial within the declared set.
	//
	// Separate rules from the floor, never a shared set (see ruleset.TierV4).
	if d.Egress.Default == AllowPublic {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Drop,
			Match:   ruleset.Match{DestV4: ruleset.TierV4},
			Counter: "fwd_tier_v4",
			Why:     "internal deny-safe tier, v4 (RFC1918): liftable BY DECLARATION, which is materialized purely by sitting BELOW the allows (the documented invariant). Without it `default: allow` would hand over the host's LAN (the documented invariant ⑤)",
		})
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Drop,
			Match:   ruleset.Match{DestV6: ruleset.TierV6},
			Counter: "fwd_tier_v6",
			Why:     "internal deny-safe tier, v6: fc00::/7 plus the NAT64 IMAGE of each v4 member — never the 64:ff9b::/96 prefix itself, which maps the whole v4 space and would refuse the public egress `allow` promises (the documented invariant ㉓)",
		})
	}

	// (7) The default. Under `allow` an explicit accept rather than falling through
	// to the chain policy: every per-sandbox base chain must be `policy accept`
	// (measured — see the provider's renderer), so falling through would work, but
	// it would leave the "this declaration permits public egress" decision with no
	// counter of its own and nothing for an acceptance test to read.
	if d.Egress.Default == AllowPublic {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Accept,
			Counter: g.defaultCounter("fwd_default_allow"),
			Why:     "egress.default: allow — public egress, i.e. everything the floor above and the deny-safe tier above did not refuse (the documented invariant)",
		})
	} else {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForward,
			Scope:   ruleset.ScopeGuestIn,
			Verdict: ruleset.Reject,
			Counter: g.defaultCounter("fwd_default_deny"),
			Why:     "egress.default: deny — the forward side needs its OWN default-deny: a destination that is not on the host and was not steered never traverses `input` at all (the documented invariant ⑩)",
		})
	}

	// (8) The publish companion accepts, on `forward` — NOT `input`.
	//
	// The hook assignment is the OPPOSITE of `via[]`'s, and the difference is
	// mechanical: after the DNAT, a steered flow's destination is a HOST-LOCAL
	// endpoint (so it stays on this machine and lands on `input`), while a
	// published flow's destination is the GUEST IP (so it must be routed onward and
	// lands on `forward`). Measured both ways — the forward counters rise per
	// published flow while the input ones stay at zero (the relevant internals page the documented invariant ⑮). Same
	// prerouting DNAT, two different downstream hooks, decided only by whether the
	// post-NAT destination is a local address.
	for i, p := range d.Ingress.Publish {
		g.emit(ruleset.Rule{
			Hook:    ruleset.HookForwardIngress,
			Scope:   ruleset.ScopeGuestOut,
			Verdict: ruleset.Accept,
			Match:   ruleset.Match{DestGuest: true, Proto: protoOf(p.Proto), DestPort: p.GuestPort},
			Counter: fmt.Sprintf("pub%d_fwd", i),
			Why:     fmt.Sprintf("ingress.publish[%d] companion accept, on FORWARD (post-DNAT the destination is the guest IP, so it is routed onward — the opposite of via[]'s hook, the documented invariant ⑮)", i),
		})
	}

	// (9) The inbound default-deny: ingress is closed unless published (the documented invariant ③).
	//
	// On a SEPARATE chain at a later priority, because this drop is the one rule of a
	// per-sandbox table that can match ANOTHER sandbox's egress — traffic from A to B's
	// guest IP is `oifname B-veth`, and at one shared priority the two chains race
	// (measured; see ruleset.HookForwardIngress).
	//
	// `drop`, not `reject`, and the asymmetry with the guest-facing refusals above
	// is deliberate: this one faces an EXTERNAL caller, to whom the engine promises
	// nothing, and an ICMP error would confirm the sandbox exists.
	const ingressClosedCounter = "fwd_ingress_closed"
	g.emit(ruleset.Rule{
		Hook:    ruleset.HookForwardIngress,
		Scope:   ruleset.ScopeGuestOut,
		Verdict: ruleset.Drop,
		Counter: ingressClosedCounter,
		Why:     "ingress default-deny (the documented invariant ③): inbound is closed unless published. Without it, anything the host routes could reach the guest IP directly",
	})
	// Attributed so the read surface can report it without naming an nft counter itself
	// (the documented invariant reads the declaration's view, and a member named after nft would not be
	// seam-clean). Direction-level, never per-publish — see CounterAttribution.
	g.attr.IngressClosed = ingressClosedCounter
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
