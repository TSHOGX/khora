package network

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/ruleset"
)

// --- helpers ------------------------------------------------------------------

func mustGenerate(t *testing.T, d Declaration) Spec {
	t.Helper()
	spec, err := Generate(d)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return spec
}

// indexOf returns the position of the rule with the given counter within one
// hook's rules, or -1. Positions are what the order assertions read (network.md D1 ④).
func indexOf(rules []ruleset.Rule, counter string) int {
	for i, r := range rules {
		if r.Counter == counter {
			return i
		}
	}
	return -1
}

// mustBefore asserts a strict ordering between two counters in one hook's chain,
// naming what breaks if the order flips.
func mustBefore(t *testing.T, rules []ruleset.Rule, first, second, why string) {
	t.Helper()
	i, j := indexOf(rules, first), indexOf(rules, second)
	if i < 0 {
		t.Fatalf("rule %q is absent (looking for %q before %q)", first, first, second)
	}
	if j < 0 {
		t.Fatalf("rule %q is absent (looking for %q before %q)", second, first, second)
	}
	if i >= j {
		t.Errorf("%q is at %d and %q at %d — %q must come FIRST: %s", first, i, second, j, first, why)
	}
}

func denyEmpty() Declaration { return Zero() }

func denyWithAllow(t *testing.T) Declaration {
	t.Helper()
	d := Zero()
	d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}}
	return d
}

func denyWithVia(t *testing.T) Declaration {
	t.Helper()
	d := Zero()
	d.Egress.Via = []Via{{
		Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
		Endpoint: "10.0.0.5:15443",
	}}
	return d
}

func allowPublic() Declaration {
	d := Zero()
	d.Egress.Default = AllowPublic
	return d
}

// --- the four declaration-value combinations ----------------------------------

// Each combination pins the rules it must produce AND the ones it must not. The
// negatives matter as much: the deny-safe tier appearing under `default: deny`
// would be a rule that never matches, i.e. one more legitimate zero in the
// counter signal every deny assertion reads (D4.1).
func TestTheFourDeclarationCombinations(t *testing.T) {
	cases := []struct {
		name       string
		d          Declaration
		wantHave   []string
		wantAbsent []string
	}{{
		name: "deny + empty",
		d:    denyEmpty(),
		wantHave: []string{
			"in_established", "in_sibling", "in_floor",
			"fwd_established_out", "fwd_established_in",
			"fwd_floor_v4", "fwd_floor_v6", "fwd_default_deny", "fwd_ingress_closed",
		},
		wantAbsent: []string{
			// No allows, no steering, no publish, and — the load-bearing one — NO
			// deny-safe tier: under `deny` the final refusal already covers every one of
			// those addresses, so a tier rule here would never match.
			"fwd_tier_v4", "fwd_tier_v6", "fwd_default_allow", "egress_masq",
			"allow0_in", "allow0_fwd", "via0_0_dnat", "pub0_in_dnat",
		},
	}, {
		name: "deny + allow[]",
		d:    denyWithAllow(t),
		wantHave: []string{
			// The allow is emitted on BOTH hooks — a forward-only allow cannot reach a
			// host-local destination at all (D4.2).
			"allow0_in", "allow0_fwd",
			// …and any allow means routed egress, so the masquerade goes on.
			"egress_masq",
			"in_floor", "fwd_floor_v4", "fwd_default_deny",
		},
		wantAbsent: []string{"fwd_tier_v4", "fwd_default_allow", "via0_0_dnat"},
	}, {
		name: "deny + via[]",
		d:    denyWithVia(t),
		wantHave: []string{
			"via0_0_dnat",   // the prerouting DNAT
			"via0_0_accept", // the narrow accept above the floor on input
			"in_floor", "fwd_default_deny",
		},
		wantAbsent: []string{
			// `via[]` alone needs NO routed egress: the steered flow is DNAT'd to a
			// host-local endpoint (a contract requirement) and delivered locally, so none
			// of the three host-side egress mechanisms is involved.
			"egress_masq",
			"allow0_in", "fwd_tier_v4", "fwd_default_allow",
		},
	}, {
		name: "allow",
		d:    allowPublic(),
		wantHave: []string{
			"fwd_default_allow",
			// The tier is the DELIVERY FACE of `allow`, not optional hardening: without
			// it this value hands the host's whole LAN to the sandbox (D5 ⑤).
			"fwd_tier_v4", "fwd_tier_v6",
			"egress_masq",
			"in_floor", "fwd_floor_v4", "fwd_floor_v6",
		},
		wantAbsent: []string{"fwd_default_deny"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustGenerate(t, tc.d)
			have := map[string]bool{}
			for _, r := range spec.Rules.Rules {
				have[r.Counter] = true
			}
			for _, c := range tc.wantHave {
				if !have[c] {
					t.Errorf("missing rule %q", c)
				}
			}
			for _, c := range tc.wantAbsent {
				if have[c] {
					t.Errorf("rule %q must NOT be emitted for this declaration", c)
				}
			}
		})
	}
}

// --- ORDER ---------------------------------------------------------------------

// Order is the mechanism, not the presentation. Every pair below has a measured
// counter-example on the wrong side.
func TestRuleOrderIsTheMechanism(t *testing.T) {
	t.Run("floor above the declaration's allows, tier below them", func(t *testing.T) {
		d := allowPublic()
		d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}}
		fwd := mustGenerate(t, d).Rules.ByHook(ruleset.HookForward)

		mustBefore(t, fwd, "fwd_floor_v4", "allow0_fwd",
			"no declaration lifts the floor, and that is materialized purely by ORDER (§2.4). "+
				"Below the allows, an `allow 10.0.0.0/8` would lift the floor's sibling item with it")
		mustBefore(t, fwd, "allow0_fwd", "fwd_tier_v4",
			"the deny-safe tier is LIFTABLE BY DECLARATION, which is the whole difference between "+
				"it and the floor — above the allows it would just be a second floor (§2.4)")
		mustBefore(t, fwd, "fwd_tier_v4", "fwd_default_allow",
			"the tier must refuse before the default admits, or `allow` hands over the host's LAN (D5 ⑤)")
	})

	t.Run("via[] wins over allow[]", func(t *testing.T) {
		// §6.2 fixes this: a flow matching both is STEERED. The two are observably
		// different outcomes, and the contract pins the choice so one declaration
		// yields one reachable set under any implementation.
		d := Zero()
		d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}}
		d.Egress.Via = []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "10.0.0.5:15443",
		}}
		spec := mustGenerate(t, d)
		// The steering DNAT is in `prerouting`, which the kernel evaluates BEFORE any
		// filter chain — so a matching flow is transformed before the allow is ever
		// reached. That is what "via wins" means mechanically.
		if len(spec.Rules.ByHook(ruleset.HookPrerouting)) == 0 {
			t.Fatal("no steering rule emitted")
		}
		in := spec.Rules.ByHook(ruleset.HookInput)
		mustBefore(t, in, "via0_0_accept", "allow0_in",
			"the steered flow's narrow accept must be reached before the plain allow, so the "+
				"observable outcome for a flow matching both is STEERED (§6.2)")
	})

	t.Run("multiple via[] entries are first-match-wins in declaration order", func(t *testing.T) {
		d := Zero()
		d.Egress.Via = []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "10.0.0.5:15443",
		}, {
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.0.0.0/8"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "10.0.0.5:15444",
		}}
		pre := mustGenerate(t, d).Rules.ByHook(ruleset.HookPrerouting)
		mustBefore(t, pre, "via0_0_dnat", "via1_0_dnat",
			"overlapping via[] entries are FIRST-MATCH-WINS in declaration order and later ones "+
				"are not evaluated — §6.2 pins it so one declaration means one reachable set")
		// And the endpoints are not swapped, which the ordering alone would not catch.
		if pre[0].ToEndpoint != "10.0.0.5:15443" || pre[1].ToEndpoint != "10.0.0.5:15444" {
			t.Errorf("steering endpoints in the wrong order: %q then %q", pre[0].ToEndpoint, pre[1].ToEndpoint)
		}
	})

	t.Run("the input side: self-gateway allow, sibling drop, then the allows", func(t *testing.T) {
		// The measured pair (D4.2): an `allow 10.0.0.0/8` must reach THIS sandbox's own
		// gateway (§5.D semantics ③'s only delivery path) and NOT a sibling's — which
		// is a question of order, and the `forward` side's sibling drop counts ZERO on
		// this path, so reading only that counter turns a real leak into "the floor
		// counter is 0" (an INVALID assertion, not a pass).
		d := Zero()
		d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}}
		in := mustGenerate(t, d).Rules.ByHook(ruleset.HookInput)

		mustBefore(t, in, "allow0_selfgw", "in_sibling",
			"a plain sibling-CIDR drop would kill this sandbox's OWN /30 gateway along with the "+
				"siblings — that gateway is the only stably-available host-local resolver address, "+
				"which §5.D semantics ③ requires a caller to be able to declare (D5 ⑦)")
		mustBefore(t, in, "in_sibling", "allow0_in",
			"without the sibling drop between them, an `allow 10.0.0.0/8` connects to a SIBLING's "+
				"gateway — measured, it reads the sibling's banner (D4.2)")
		mustBefore(t, in, "allow0_in", "in_floor",
			"the declaration's allows sit above the ct-state floor, or nothing declared could "+
				"reach a host-local address at all")
	})

	t.Run("the established accept is above the floor drop", func(t *testing.T) {
		in := mustGenerate(t, denyEmpty()).Rules.ByHook(ruleset.HookInput)
		mustBefore(t, in, "in_established", "in_floor",
			"below the floor, the guest's replies to the host are dropped and transport dies "+
				"outright — the one LOUD failure in this design (D5 ⑧)")
	})
}

// The `via[]` narrow accept must sit in the SAME chain as the floor drop it
// overrides, and above it.
//
// This is the trap that reads as healthy while the feature is dead: placed in a
// DIFFERENT base chain on the same hook, the accept MATCHES and its counter RISES
// while the flow is still dropped and the connection still fails (measured, D5 ⑩ —
// see ruleset.Accept).
func TestTheViaAcceptSharesTheFloorsChainAndSitsAboveIt(t *testing.T) {
	spec := mustGenerate(t, denyWithVia(t))
	in := spec.Rules.ByHook(ruleset.HookInput)
	mustBefore(t, in, "via0_0_accept", "in_floor",
		"the approved floor exception works by ORDER within one chain (§2.4)")

	// Same chain, asserted by hook identity rather than by position alone.
	var acc, floor *ruleset.Rule
	for i := range spec.Rules.Rules {
		switch spec.Rules.Rules[i].Counter {
		case "via0_0_accept":
			acc = &spec.Rules.Rules[i]
		case "in_floor":
			floor = &spec.Rules.Rules[i]
		}
	}
	if acc == nil || floor == nil {
		t.Fatal("the via accept or the floor rule is missing")
	}
	if acc.Hook != floor.Hook {
		t.Errorf("the via accept is on %s while the floor drop is on %s — a CROSS-CHAIN accept "+
			"increments its own counter while the flow is still dropped and the connection still "+
			"fails, so the counter reads healthy while the feature is dead (D5 ⑩)", acc.Hook, floor.Hook)
	}
	if !floor.Verdict.Terminal() {
		t.Error("the floor rule's verdict is not terminal — only drop/reject end traversal across chains")
	}
}

// The narrow accept must pin THREE things. Each omission has a live measured
// counter-example (D5 ⑩), so each is asserted separately rather than as "the rule
// looks about right".
func TestTheViaAcceptPinsAllThreeClauses(t *testing.T) {
	spec := mustGenerate(t, denyWithVia(t))
	var acc ruleset.Rule
	for _, r := range spec.Rules.ByHook(ruleset.HookInput) {
		if r.Counter == "via0_0_accept" {
			acc = r
		}
	}
	if acc.Counter == "" {
		t.Fatal("the via accept is missing")
	}

	// (1) The PRE-NAT destination. Without it, an explicit-endpoint declaration
	// authorizes every OTHER address on that port.
	if acc.Match.CTOriginalDest == nil {
		t.Error("no pre-NAT destination clause — the declaration named a cidr, so without it any " +
			"other address on that port is authorized (D5 ⑩)")
	} else if got := acc.Match.CTOriginalDest.String(); got != "1.1.1.1/32" {
		t.Errorf("pre-NAT destination = %s, want the declared 1.1.1.1/32", got)
	}
	// (2) The PRE-NAT port. Without it, a declared original port can reach a
	// listener the declaration never named (measured: 9.9.9.9:443 → :19999, and
	// the guest read a banner from `hello-from-UNDECLARED`).
	if acc.Match.CTOriginalPort != 443 {
		t.Errorf("pre-NAT port = %d, want the declared 443 (D5 ⑩)", acc.Match.CTOriginalPort)
	}
	// (3) The POST-NAT listener port. Without it — using only `ct status dnat` +
	// dport — an UNDECLARED original port redirected to a DECLARED listener is
	// allowed through with the floor counting 0 (measured: 9200 → :18443).
	if acc.Match.DestPort != 15443 {
		t.Errorf("post-NAT port = %d, want the endpoint's 15443 (D5 ⑩)", acc.Match.DestPort)
	}
	// (4) The interface. Without it, ANOTHER sandbox's identical flow is let
	// through by THIS sandbox's accept — each per-sandbox table is a `policy
	// accept` base chain on a shared hook, so an un-narrowed accept is a hole in
	// EVERY sandbox's floor (measured, D5 ⑩ last row).
	if acc.Scope != ruleset.ScopeGuestIn {
		t.Errorf("scope = %s, want guest-in: an accept without the interface narrowing is a hole "+
			"in every OTHER sandbox's floor (D5 ⑩)", acc.Scope)
	}
	if acc.Match.Proto != ruleset.TCP {
		t.Errorf("proto = %q, want tcp (a port match requires it, D3.1)", acc.Match.Proto)
	}
}

// The transparent-interception shape emits exactly ONE CLAUSE FEWER, and both
// directions are asserted — which is the point: keeping the cidr clause on a
// `cidr: any` declaration blocks the traffic the declaration authorized, while
// dropping it on an explicit-endpoint declaration authorizes every other address
// on that port (D3.3 / D5 ⑩).
func TestTheCidrClauseIsEmittedIffTheDeclarationPinnedOne(t *testing.T) {
	// Explicit endpoint → the clause is present (covered above, restated here so
	// the pair reads as one property).
	explicit := mustGenerate(t, denyWithVia(t))
	if got := findRule(explicit, "via0_0_accept").Match.CTOriginalDest; got == nil {
		t.Error("explicit-endpoint form: the pre-NAT cidr clause is missing")
	}
	if got := findRule(explicit, "via0_0_dnat").Match.DestV4; len(got) != 1 {
		t.Errorf("explicit-endpoint form: the steering DNAT has %d destination clauses, want 1", len(got))
	}

	// Transparent interception: `cidr: any`, i.e. "any destination on that port".
	d := Zero()
	d.Egress.Via = []Via{{
		Match:    []Tuple{{Port: 443, Proto: model.NetTCP}}, // no CIDR at all
		Endpoint: "10.0.0.5:15443",
	}}
	transparent := mustGenerate(t, d)
	if got := findRule(transparent, "via0_0_accept").Match.CTOriginalDest; got != nil {
		t.Errorf("transparent form: a pre-NAT cidr clause (%s) was emitted — the declaration "+
			"authorized ANY destination on that port, so this clause would block exactly the "+
			"traffic it allowed (D3.3 / D5 ⑩)", got)
	}
	if got := findRule(transparent, "via0_0_dnat").Match.DestV4; len(got) != 0 {
		t.Errorf("transparent form: the steering DNAT still carries a destination clause %v", got)
	}
	// The other two clauses survive: the shape differs by exactly one clause.
	acc := findRule(transparent, "via0_0_accept")
	if acc.Match.CTOriginalPort != 443 || acc.Match.DestPort != 15443 || acc.Scope != ruleset.ScopeGuestIn {
		t.Errorf("transparent form dropped more than the cidr clause: origPort=%d dport=%d scope=%s",
			acc.Match.CTOriginalPort, acc.Match.DestPort, acc.Scope)
	}
	// A /0 is the same thing spelled differently and must behave identically —
	// otherwise a caller writing `0.0.0.0/0` gets a rule that pins nothing while
	// carrying a clause.
	d0 := Zero()
	d0.Egress.Via = []Via{{
		Match:    []Tuple{{CIDR: mustCIDR(t, "0.0.0.0/0"), Port: 443, Proto: model.NetTCP}},
		Endpoint: "10.0.0.5:15443",
	}}
	if got := findRule(mustGenerate(t, d0), "via0_0_accept").Match.CTOriginalDest; got != nil {
		t.Errorf("a /0 emitted a cidr clause (%s) — a /0 pins nothing, so it is the transparent form", got)
	}
}

func findRule(s Spec, counter string) ruleset.Rule {
	for _, r := range s.Rules.Rules {
		if r.Counter == counter {
			return r
		}
	}
	return ruleset.Rule{}
}

// The self-gateway allow must not reach a gateway the declaration never CONTAINED.
//
// The hole this closes, concretely: a sandbox on 10.66.250.0/30 (gateway
// 10.66.250.1) declaring `allow 10.66.99.0/24:8080/tcp` OVERLAPS the sandbox
// address space, so the rule is emitted — but 10.66.250.1 is not in the declared
// CIDR. With the gateway clause alone the caller reaches one address they never
// named, on the declared port, and it is a HOST address — so any host process bound
// `0.0.0.0:8080` becomes reachable (see ruleset.Match.DestSelfGatewayWithin). The
// rule must pin the intersection, which the renderer materializes as two ANDed
// daddr clauses.
func TestTheSelfGatewayAllowCannotReachAGatewayTheDeclarationDidNotContain(t *testing.T) {
	// The dangerous shape: overlaps the /16, excludes any plausible gateway of the
	// sandbox that declared it.
	d := Zero()
	d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.66.99.0/24"), Port: 8080, Proto: model.NetTCP}}
	gw := findRule(mustGenerate(t, d), "allow0_selfgw")
	if gw.Counter == "" {
		t.Fatal("no self-gateway rule emitted for a declaration overlapping the sandbox space")
	}
	within := gw.Match.DestSelfGatewayWithin
	if within == nil {
		t.Fatal("the self-gateway rule carries no declared-prefix clause — a sandbox on " +
			"10.66.250.0/30 would then reach 10.66.250.1:8080, an address this declaration does " +
			"NOT contain, and with it any host process bound 0.0.0.0:8080 (§2.4's host-itself floor " +
			"item, measured D5 ⑧)")
	}
	// The prefix is the DECLARED one, verbatim — not the sandbox space, and not
	// widened. Widening it to 10.66.0.0/16 would restore the whole hole.
	if within.String() != "10.66.99.0/24" {
		t.Errorf("the clause pins %s, want the declared 10.66.99.0/24 — anything wider reopens the "+
			"hole this clause exists to close", within)
	}
	// And the arithmetic the rule now expresses: this gateway is NOT in that prefix,
	// so the intersection is empty and the rule cannot match.
	gwAddr := netip.MustParseAddr("10.66.250.1")
	if within.Contains(gwAddr) {
		t.Fatalf("fixture is wrong: %s contains %s", within, gwAddr)
	}
	// The converse, so this is not just "a clause is present": a declaration that
	// genuinely CONTAINS the gateway still reaches it — otherwise the fix would have
	// broken §5.D semantics ③, which is the whole reason the rule exists.
	d2 := Zero()
	d2.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}}
	gw2 := findRule(mustGenerate(t, d2), "allow0_selfgw")
	if gw2.Match.DestSelfGatewayWithin == nil {
		t.Fatal("the containing declaration lost its self-gateway rule")
	}
	if !gw2.Match.DestSelfGatewayWithin.Contains(gwAddr) {
		t.Errorf("%s does not contain the gateway %s — §5.D semantics ③ has no delivery path left, "+
			"which is the failure the fix must not introduce", gw2.Match.DestSelfGatewayWithin, gwAddr)
	}
}

// --- the render guards, on real generator output -------------------------------

// Every declaration the generator accepts must produce a rule set that passes the
// abstract guards. Generate calls ruleset.Validate itself, so this asserts the
// guards are actually reached rather than that they exist.
func TestEveryGeneratedRuleSetPassesTheGuards(t *testing.T) {
	pubUDP := Zero()
	pubUDP.Ingress.Publish = []Publish{{GuestPort: 5353, Proto: model.NetUDP}}
	allowAll := allowPublic()
	allowAll.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}}
	allowAll.Egress.Via = []Via{{
		Match:    []Tuple{{Port: 443, Proto: model.NetTCP}},
		Endpoint: "[fd00::1]:15443",
	}}
	allowAll.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}, {GuestPort: 5353, Proto: model.NetUDP}}

	for name, d := range map[string]Declaration{
		"deny+empty": denyEmpty(), "deny+allow": denyWithAllow(t), "deny+via": denyWithVia(t),
		"allow": allowPublic(), "publish-udp": pubUDP, "everything": allowAll,
	} {
		t.Run(name, func(t *testing.T) {
			spec := mustGenerate(t, d)
			if err := ruleset.Validate(spec.Rules); err != nil {
				t.Errorf("generated rule set fails the guards: %v", err)
			}
			// Every rule carries a counter, because a deny assertion is only valid when
			// a counter moved — a floor whose counter reads 0 is indistinguishable from
			// a floor that was bypassed (§2.4 / D5 ⑧).
			for _, r := range spec.Rules.Rules {
				if r.Counter == "" {
					t.Errorf("rule %s/%s has no counter", r.Hook, r.Verdict)
				}
				if r.Why == "" {
					t.Errorf("rule %s (%s) carries no rationale — these lines are one word away from "+
						"being a hole with no test firing, so an operator reading `nft list` needs to "+
						"see which promise each carries", r.Counter, r.Hook)
				}
			}
			// Counter names must be unique per rule set, or two rules share one counter
			// and every counter-based assertion over them becomes ambiguous.
			seen := map[string]int{}
			for _, r := range spec.Rules.Rules {
				seen[r.Counter]++
			}
			for c, n := range seen {
				if n > 1 {
					t.Errorf("counter %q is shared by %d rules — a shared counter makes every "+
						"assertion reading it ambiguous", c, n)
				}
			}
		})
	}
}

// The two guards' SCOPES must not cross: a legal second-hop rule (uid-narrowed, NO
// interface narrowing) must not be rejected by the per-sandbox interface guard.
//
// Otherwise the guard and §2.4 invariant 2 are mutually exclusive and whichever
// was written first wins silently.
func TestTheTwoGuardScopesDoNotCross(t *testing.T) {
	rs, err := SecondHopRules()
	if err != nil {
		t.Fatalf("SecondHopRules: %v", err)
	}
	if err := ruleset.Validate(rs); err != nil {
		t.Fatalf("the second-hop rule set — uid-narrowed and deliberately carrying no interface "+
			"predicate — was rejected by the guards: %v. The guard must be scoped BY TABLE, or it is "+
			"mutually exclusive with §2.4 invariant 2", err)
	}
	for _, r := range rs.Rules {
		switch r.Scope {
		case ruleset.ScopeSecondHopUID:
			// A uid-narrowed rule may only DROP: skuid misses packets, so an accept narrowed
			// by it leaks them past the floor it sits above (the measured asymmetry, D5 ㉔).
			if r.Verdict != ruleset.Drop {
				t.Errorf("second-hop rule %q is a uid-narrowed %s — only DROP is safe under a "+
					"predicate that misses packets (D5 ㉔)", r.Counter, r.Verdict)
			}
		case ruleset.ScopeSecondHopReturn:
			// The ONE accept this table may carry, and the distinction from the case above is
			// the whole point: it is narrowed by CT-STATE, not by identity, and the measured
			// leak is a property of `skuid` specifically. Without it this table drops the reply
			// to the very flow a `via[]` declaration authorized — see the dedicated test below.
			if r.Verdict != ruleset.Accept {
				t.Errorf("the second-hop return arm %q has verdict %s, want accept", r.Counter, r.Verdict)
			}
		default:
			t.Errorf("second-hop rule %q is narrowed by %s — on that hook the only predicates are "+
				"the deployment uid and, for the return arm, ct-state (D5 ㉔)", r.Counter, r.Scope)
		}
	}
}

// --- the second hop, generator side -------------------------------------------

// The second hop must be emitted on `output` and NOWHERE ELSE.
//
// Measured, hop 2's egress counts ZERO in `forward` in all three shapes and is
// visible only in `output` (D5 ㉔). So a rule set that puts it on the wrong hook
// passes every FIRST-hop assertion while the second hop is never evaluated at all
// — and the symptom is a floor counter that does not move, D4.1's silent pass.
func TestTheSecondHopLandsOnOutputAndNotOnForward(t *testing.T) {
	rs, err := SecondHopRules()
	if err != nil {
		t.Fatalf("SecondHopRules: %v", err)
	}
	if len(rs.Rules) == 0 {
		t.Fatal("no second-hop rules — §2.4 invariant 2's second half would be undelivered")
	}
	for _, r := range rs.Rules {
		if r.Hook != ruleset.HookOutputFilter {
			t.Errorf("second-hop rule %q is on %s: measured, a host-local process's egress counts "+
				"ZERO in forward in all three shapes and is visible only in output — a rule on the "+
				"wrong hook passes every first-hop assertion while hop 2 is never evaluated (D5 ㉔)",
				r.Counter, r.Hook)
		}
	}
	// Both families, and the floor items are the ones the confused deputy would be
	// pointed at (a sibling's guest IP, a link-local address).
	var haveV4, haveV6 bool
	for _, r := range rs.Rules {
		if len(r.Match.DestV4) > 0 {
			haveV4 = true
		}
		if len(r.Match.DestV6) > 0 {
			haveV6 = true
		}
	}
	if !haveV4 || !haveV6 {
		t.Errorf("second-hop floor covers v4=%v v6=%v — both are needed: the deputy is pointed at "+
			"an address, and either family reaches a sibling", haveV4, haveV6)
	}
	// The sibling CIDR specifically: that is the confused-deputy target §2.4 names.
	found := false
	for _, r := range rs.Rules {
		for _, p := range r.Match.DestV4 {
			if p == ruleset.SandboxCIDR {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the second-hop floor does not cover the sandbox address space (%s) — that is "+
			"exactly the confused deputy §2.4 invariant 2 names: an `upstream` pointed at a sibling "+
			"walks in through the side door", ruleset.SandboxCIDR)
	}
}

// The loop-avoidance rule must NOT be a bare uid-narrowed accept.
//
// D4.2 lists two needs for this predicate with opposite positions, and the accept
// half is unsafe: `skuid` misses packets, so an accept sitting above a floor drop
// leaks whatever it fails to match (D5 ㉔). This generator avoids the loop
// STRUCTURALLY instead — every steering DNAT is narrowed by the sandbox's input
// interface, and hop 2 arrives on no interface, so it cannot match one.
func TestLoopAvoidanceIsNotABareUIDAccept(t *testing.T) {
	rs, err := SecondHopRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs.Rules {
		if r.Verdict != ruleset.Accept {
			continue
		}
		// The return arm is the one legal accept (ct-state narrowed). Anything else —
		// especially a uid-narrowed one — is the unsafe shape D4.2's first row would have
		// needed and D5 ㉔ measured leaking.
		if r.Scope != ruleset.ScopeSecondHopReturn {
			t.Errorf("second-hop rule %q is an accept narrowed by %s: a uid-narrowed accept above a "+
				"floor drop leaks the packets skuid misses (D5 ㉔). Loop avoidance here is STRUCTURAL "+
				"— every steering rule is interface-narrowed and hop 2 arrives on no interface",
				r.Counter, r.Scope)
		}
		if len(r.Match.CTState) == 0 {
			t.Errorf("the second-hop accept %q carries no ct-state clause — it would then admit NEW "+
				"connections to in-floor addresses, which IS the confused deputy (D4.2)", r.Counter)
		}
		for _, st := range r.Match.CTState {
			if st == "new" {
				t.Errorf("the second-hop accept %q matches `new` — a new connection to an in-floor "+
					"address is exactly what §2.4 invariant 2 refuses", r.Counter)
			}
		}
	}
	// The structural claim, asserted rather than asserted-in-prose: every steering
	// DNAT really is interface-narrowed, which is what makes hop 2 unable to match
	// one and therefore what makes the accept unnecessary.
	for _, r := range mustGenerate(t, denyWithVia(t)).Rules.ByHook(ruleset.HookPrerouting) {
		if r.Scope != ruleset.ScopeGuestIn {
			t.Errorf("steering rule %q is narrowed by %s, not by the input interface — then hop 2 "+
				"COULD match it and the loop becomes reachable, which is what the (unsafe) "+
				"uid accept would have had to prevent", r.Counter, r.Scope)
		}
	}
}

// The second-hop table must carry a RETURN arm above its floor, or it kills `via[]`
// (the mechanism is in SecondHopRules; it was found on the substrate).
//
// Measured with the floor alone: the steered flow reached the endpoint and was accepted
// (`via0_0_dnat=1`, `via0_0_accept=1`, conntrack SYN_RECV) while `hop2_floor_v4`
// counted 9 — the reply dropped by this engine, and the guest hung 25 s on a connection
// whose request had succeeded.
//
// The arm does not weaken §2.4 invariant 2, and the assertion below is what pins that:
// it must NOT match `new`, so a new connection to an in-floor address — the confused
// deputy itself — still meets the floor.
func TestTheSecondHopCarriesAReturnArmAboveItsFloor(t *testing.T) {
	rs, err := SecondHopRules()
	if err != nil {
		t.Fatal(err)
	}
	ret := indexOf(rs.Rules, "hop2_established")
	v4 := indexOf(rs.Rules, "hop2_floor_v4")
	v6 := indexOf(rs.Rules, "hop2_floor_v6")
	if ret < 0 {
		t.Fatal("the second-hop table has NO return arm — it therefore drops the reply to every " +
			"steered flow, because that reply's destination is the guest IP and the guest IP lies in " +
			"the floor's sibling item (measured: hop2_floor counted 9 while the guest hung 25 s)")
	}
	if ret > v4 || ret > v6 {
		t.Errorf("the return arm is at %d but the floor drops are at %d/%d — it must sit ABOVE them "+
			"IN THIS CHAIN: an accept ends only its own chain, so below the drop it would count "+
			"while the reply still died (D5 ⑩'s cross-chain trap, same shape)", ret, v4, v6)
	}
	arm := rs.Rules[ret]
	// It admits only ALREADY-ESTABLISHED flows. This is the line between "the reply to
	// what the guest asked for" and "the deputy opening its own connection".
	var established bool
	for _, st := range arm.Match.CTState {
		if st == "new" {
			t.Error("the return arm matches `new` — that admits the endpoint process opening a NEW " +
				"connection to an in-floor address, which is precisely the confused deputy §2.4 " +
				"invariant 2 exists to refuse (D4.2)")
		}
		if st == "established" || st == "related" {
			established = true
		}
	}
	if !established {
		t.Error("the return arm has no established/related clause, so it is not narrowed to a reply " +
			"at all")
	}
	// And it carries NO uid predicate: the measured leak that forbids a uid-narrowed
	// accept is a property of `skuid` missing packets, and a ct-state arm has no such
	// gap. Using uid here would reintroduce exactly that leak.
	if arm.Scope != ruleset.ScopeSecondHopReturn {
		t.Errorf("the return arm is narrowed by %s, want the ct-state return scope", arm.Scope)
	}
}

// --- the deny-safe tier, generator side ---------------------------------------

// The tier's position is the assertion, and the v6 half's MEMBERSHIP is too.
func TestTheDenySafeTierIsLiftableAndSeparateFromTheFloor(t *testing.T) {
	d := allowPublic()
	d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "192.168.139.3/32"), Port: 22, Proto: model.NetTCP}}
	spec := mustGenerate(t, d)
	fwd := spec.Rules.ByHook(ruleset.HookForward)

	// Liftable: the declaration sits ABOVE it.
	mustBefore(t, fwd, "allow0_fwd", "fwd_tier_v4",
		"deny-SAFE means liftable by declaration, and that is materialized purely by the "+
			"declaration sitting above it (§2.4)")
	// …but the floor is NOT liftable: it sits above the same declaration.
	mustBefore(t, fwd, "fwd_floor_v4", "allow0_fwd",
		"an `allow 10.0.0.0/8` must not lift the floor's sibling item with it (§2.4)")

	// Separate rules, never one shared set: the sibling /16 nests inside 10/8, and
	// an nft interval set either refuses to load or silently merges the subnet away
	// (D5 ⑱). Asserted as "no single rule holds members of both", which is the form
	// a merge would take.
	for _, r := range fwd {
		hasFloorItem, hasTierItem := false, false
		for _, p := range r.Match.DestV4 {
			for _, f := range ruleset.FloorV4 {
				if p == f {
					hasFloorItem = true
				}
			}
			for _, ti := range ruleset.TierV4 {
				if p == ti {
					hasTierItem = true
				}
			}
		}
		if hasFloorItem && hasTierItem {
			t.Errorf("rule %q holds BOTH floor and tier members in one set — the sibling /16 nests "+
				"inside 10/8, so the set either fails to load or SILENTLY merges the floor away (D5 ⑱)",
				r.Counter)
		}
	}

	// v6 membership: fc00::/7 plus each member's NAT64 IMAGE, never the
	// `64:ff9b::/96` prefix itself (which maps the whole v4 space and would refuse
	// exactly the public egress `allow` promises — D5 ㉓).
	var tier6 ruleset.Rule
	for _, r := range fwd {
		if r.Counter == "fwd_tier_v6" {
			tier6 = r
		}
	}
	if tier6.Counter == "" {
		t.Fatal("no v6 tier rule under `default: allow`")
	}
	for _, p := range tier6.Match.DestV6 {
		if p == ruleset.NAT64Prefix {
			t.Error("the v6 tier includes the bare 64:ff9b::/96 prefix — per RFC 6052 that maps the " +
				"ENTIRE v4 space, public destinations included, so dropping it refuses the public " +
				"egress `default: allow` promises (D5 ㉓). Only each MEMBER's image may be derived")
		}
	}
	if len(tier6.Match.DestV6) != len(ruleset.TierV6) {
		t.Errorf("the v6 tier has %d members, want %d", len(tier6.Match.DestV6), len(ruleset.TierV6))
	}
}

// --- the floor's four items, and no IPv6 fork ---------------------------------

// The floor's four items must all be delivered, each in the shape §2.4 gives it.
func TestAllFourFloorItemsAreDelivered(t *testing.T) {
	spec := mustGenerate(t, denyEmpty())
	fwd := spec.Rules.ByHook(ruleset.HookForward)
	in := spec.Rules.ByHook(ruleset.HookInput)

	// Items 1–3 (loopback / link-local / sibling) as daddr members on `forward`.
	want := map[string]bool{"127.0.0.0/8": false, "169.254.0.0/16": false, "10.66.0.0/16": false}
	for _, r := range fwd {
		if r.Counter != "fwd_floor_v4" {
			continue
		}
		for _, p := range r.Match.DestV4 {
			if _, ok := want[p.String()]; ok {
				want[p.String()] = true
			}
		}
	}
	for item, got := range want {
		if !got {
			t.Errorf("floor item %s is not in the forward-side daddr list", item)
		}
	}
	// v6 half.
	want6 := map[string]bool{"::1/128": false, "fe80::/10": false}
	for _, r := range fwd {
		if r.Counter != "fwd_floor_v6" {
			continue
		}
		for _, p := range r.Match.DestV6 {
			if _, ok := want6[p.String()]; ok {
				want6[p.String()] = true
			}
		}
	}
	for item, got := range want6 {
		if !got {
			t.Errorf("floor item %s is not in the forward-side v6 daddr list", item)
		}
	}

	// Item 4 — "any address of the host itself" — must be a PURE CT-STATE rule with
	// NO address list. That is what makes host addresses never enumerated, so the
	// engine need not know its bind addresses and a host address added AFTER
	// installation is still covered (D5 ⑧).
	var floor ruleset.Rule
	for _, r := range in {
		if r.Counter == "in_floor" {
			floor = r
		}
	}
	if floor.Counter == "" {
		t.Fatal("no input-side floor rule — a forward-only floor misses guest→host entirely (D5 ⑧)")
	}
	if len(floor.Match.DestV4) > 0 || len(floor.Match.DestV6) > 0 {
		t.Errorf("the input-side floor carries an address list (%v / %v) — it must be PURE ct-state, "+
			"or the engine has to know its own bind addresses and the whole `a host address changed, "+
			"must we re-render` class of question comes back (§2.4 / D5 ⑧)",
			floor.Match.DestV4, floor.Match.DestV6)
	}
	if len(floor.Match.CTState) == 0 {
		t.Error("the input-side floor is not a ct-state rule")
	}
	// The `iifname` narrowing is the ENTIRE reason this is a per-sandbox rule
	// rather than a host-level inbound kill switch: without it it takes out the
	// orchestrator's own 127.0.0.1 listener and sshd too, because loopback also
	// traverses `input` (D5 ⑧).
	if floor.Scope != ruleset.ScopeGuestIn {
		t.Errorf("the input-side floor is narrowed by %s — without the interface narrowing this "+
			"same rule is a HOST-level inbound kill switch that takes out the orchestrator's own "+
			"listener and sshd (D5 ⑧)", floor.Scope)
	}
	// The forward side needs its OWN default-deny: a destination not on the host
	// and not steered never traverses `input` at all (D5 ⑩).
	if indexOf(fwd, "fwd_default_deny") < 0 {
		t.Error("the forward chain has no default-deny — a destination that is neither on the host " +
			"nor steered never traverses `input`, so an input-only default-deny does not cover it (D5 ⑩)")
	}
}

// The generator must NOT fork for IPv6: a single family-UNQUALIFIED ct-state rule
// covers both families, and an explicit `meta nfproto ipv6` second arm is
// redundant — measured, it counted exactly the packets the family-agnostic drop
// had already caught (D5 ⑪).
//
// Asserted structurally: every ct-state rule must be family-unqualified, i.e.
// carry no address list at all.
func TestTheGeneratorDoesNotForkForIPv6(t *testing.T) {
	for name, d := range map[string]Declaration{
		"deny": denyEmpty(), "allow": allowPublic(), "via": denyWithVia(t), "allow[]": denyWithAllow(t),
	} {
		t.Run(name, func(t *testing.T) {
			for _, r := range mustGenerate(t, d).Rules.Rules {
				if len(r.Match.CTState) == 0 {
					continue
				}
				// A ct-state rule that also carries a family-typed address list would BE
				// the fork: it forces a second arm for the other family.
				if len(r.Match.DestV4) > 0 || len(r.Match.DestV6) > 0 {
					t.Errorf("ct-state rule %q also carries a family-typed address list — that is the "+
						"IPv6 fork D5 ⑪ measured as redundant: one unqualified ct-state rule covers "+
						"both families, including a global prefix the guest invented for itself", r.Counter)
				}
			}
			// The v4/v6 floor daddr pair is NOT a fork: those are declaration-layer
			// ADDRESS members, and nft's grammar can only spell them per family. Assert
			// the count stays at exactly two so a third arm would be noticed.
			families := 0
			for _, r := range mustGenerate(t, d).Rules.ByHook(ruleset.HookForward) {
				if strings.HasPrefix(r.Counter, "fwd_floor_") {
					families++
				}
			}
			if families != 2 {
				t.Errorf("%d forward-side floor rules, want exactly 2 (one per family, because "+
					"`ip daddr` and `ip6 daddr` are family-typed — not a mechanism fork)", families)
			}
		})
	}
}

// --- publish: the four-part unit, and the hook assignment ---------------------

// A publish is a RULE SET, not a rule: four parts, and each missing part is a
// BROKEN publish rather than a smaller feature (§5.D).
func TestOnePublishEmitsAllFourParts(t *testing.T) {
	d := Zero()
	d.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}}
	spec := mustGenerate(t, d)

	// (1) prerouting DNAT — external callers.
	pre := findRule(spec, "pub0_in_dnat")
	if pre.Hook != ruleset.HookPrerouting || pre.Verdict != ruleset.DNAT {
		t.Errorf("the inbound DNAT is %s/%s", pre.Hook, pre.Verdict)
	}
	// (2) output-hook DNAT — the HOST's own path. A second, independent rule:
	// host-originated traffic never traverses prerouting, so "it worked from the
	// host" is not evidence, and neither is the reverse (D5 ⑭).
	out := findRule(spec, "pub0_out_dnat")
	if out.Hook != ruleset.HookOutputNAT || out.Verdict != ruleset.DNAT {
		t.Errorf("the host-path DNAT is %s/%s — host-originated traffic never traverses prerouting, "+
			"so it needs its OWN rule on the output hook (D5 ⑭)", out.Hook, out.Verdict)
	}
	// (3) the return SNAT to this sandbox's own /30 gateway. Without it a publish
	// works only on a sandbox that happens to carry a default route and TIMES OUT
	// on one that does not (measured to_guest=2 / from_guest=0, D5 ⑬).
	snat := findRule(spec, "pub_return_snat")
	if snat.Verdict != ruleset.SNAT || !snat.ToSelfGateway {
		t.Errorf("the return SNAT is %s (toSelfGateway=%v) — without it the guest's reply has "+
			"nowhere to go and an outside caller reads a timeout (D5 ⑬)", snat.Verdict, snat.ToSelfGateway)
	}
	if snat.Hook != ruleset.HookPostrouting {
		t.Errorf("the return SNAT is on %s, want postrouting", snat.Hook)
	}
	// (4) the companion accept, on the FORWARD hook — the OPPOSITE hook from `via[]`'s.
	// After the DNAT a published flow's destination is the GUEST IP, so it must be
	// routed onward and lands on `forward`; a steered flow's destination is
	// host-local and lands on `input`. Measured both ways (D5 ⑮).
	//
	// On that hook's INGRESS chain specifically — a later priority, so every sandbox's
	// floor is evaluated before any sandbox's inbound rules (see
	// TestTheIngressDenyIsOnALaterChainThanTheFloor for the measured race).
	acc := findRule(spec, "pub0_fwd")
	if acc.Hook != ruleset.HookForwardIngress {
		t.Errorf("the publish companion accept is on %s, want the forward hook's ingress chain: "+
			"post-DNAT the destination is the guest IP so it is ROUTED onward — the opposite of "+
			"via[]'s hook assignment, and the two must not be copied from each other (D5 ⑮)", acc.Hook)
	}
	if acc.Verdict != ruleset.Accept || !acc.Match.DestGuest || acc.Match.DestPort != 8080 {
		t.Errorf("the companion accept does not pin the guest destination and port: %+v", acc.Match)
	}
}

// The publish return SNAT is emitted ONCE per sandbox, not once per item: it is
// keyed on the guest IP, not on a port.
func TestThePublishReturnSNATIsOnePerSandbox(t *testing.T) {
	d := Zero()
	d.Ingress.Publish = []Publish{
		{GuestPort: 8080, Proto: model.NetTCP},
		{GuestPort: 9090, Proto: model.NetTCP},
		{GuestPort: 5353, Proto: model.NetUDP},
	}
	spec := mustGenerate(t, d)
	n := 0
	for _, r := range spec.Rules.Rules {
		if r.Verdict == ruleset.SNAT {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d return SNAT rules for 3 publish items, want 1 — it is keyed on the guest IP, "+
			"not on a port", n)
	}
	// …while the DNATs and companion accepts ARE per item.
	for i := range d.Ingress.Publish {
		for _, c := range []string{"pub%d_in_dnat", "pub%d_out_dnat", "pub%d_fwd"} {
			name := fmt.Sprintf(c, i)
			if findRule(spec, name).Counter == "" {
				t.Errorf("publish item %d is missing rule %q", i, name)
			}
		}
	}
	// Spec.Publish carries the items in declaration order for the provider to bind
	// host endpoints to.
	if len(spec.Publish) != 3 {
		t.Errorf("Spec.Publish carries %d items, want 3", len(spec.Publish))
	}
}

// Ingress is closed by default (§2.1 ③): without an inbound default-deny anything
// the host routes could reach the guest IP directly.
func TestIngressIsClosedWhenNothingIsPublished(t *testing.T) {
	ing := mustGenerate(t, denyEmpty()).Rules.ByHook(ruleset.HookForwardIngress)
	if indexOf(ing, "fwd_ingress_closed") < 0 {
		t.Fatal("no inbound default-deny")
	}
	// It must be LAST among the guest-out rules, or it would pre-empt the publish
	// companion accepts.
	d := Zero()
	d.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}}
	withPub := mustGenerate(t, d).Rules.ByHook(ruleset.HookForwardIngress)
	mustBefore(t, withPub, "pub0_fwd", "fwd_ingress_closed",
		"the inbound default-deny must sit BELOW the publish companion accepts, or it refuses "+
			"the very flows the publish opened")
}

// The ingress default-deny must live on a LATER-priority chain than the floor, and
// this is a CROSS-SANDBOX ordering requirement rather than organization: measured,
// sandbox B's ingress drop won over sandbox A's floor at one shared priority, A's floor
// counted ZERO, and the guest got a 25-SECOND HANG instead of the clean failure §2.4
// promises (see ruleset.HookForwardIngress).
func TestTheIngressDenyIsOnALaterChainThanTheFloor(t *testing.T) {
	spec := mustGenerate(t, denyEmpty())
	floorHooks := map[string]ruleset.Hook{}
	for _, r := range spec.Rules.Rules {
		floorHooks[r.Counter] = r.Hook
	}
	if got := floorHooks["fwd_floor_v4"]; got != ruleset.HookForward {
		t.Errorf("the floor is on %s, want the forward hook's main chain", got)
	}
	if got := floorHooks["fwd_ingress_closed"]; got != ruleset.HookForwardIngress {
		t.Errorf("the ingress default-deny is on %s, want the LATER forward-ingress chain — at one "+
			"shared priority another sandbox's ingress drop races this sandbox's floor, and measured, "+
			"the ingress drop won: the guest hung 25 s instead of failing fast, and the floor counted "+
			"zero (§2.4 fail-closed's second half / D4.1's counter signal)", got)
	}
	// The two must be different hooks, which is what produces two chains and therefore
	// the deterministic priority ordering.
	if floorHooks["fwd_floor_v4"] == floorHooks["fwd_ingress_closed"] {
		t.Error("the floor and the ingress default-deny share a chain — then their relative order " +
			"ACROSS sandboxes is undefined, and the counter that moves depends on which table was " +
			"created first")
	}
}

// The publish return path is not an EGRESS path (D1 ④): a declaration that
// publishes a port but declares no egress must still have its floor and its
// default-deny intact.
func TestPublishDoesNotBecomeAnEgressPath(t *testing.T) {
	d := Zero() // default: deny, no allow[], no via[]
	d.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}}
	spec := mustGenerate(t, d)
	fwd := spec.Rules.ByHook(ruleset.HookForward)
	if indexOf(fwd, "fwd_default_deny") < 0 {
		t.Error("publishing a port removed the egress default-deny")
	}
	if indexOf(fwd, "fwd_floor_v4") < 0 || indexOf(spec.Rules.ByHook(ruleset.HookInput), "in_floor") < 0 {
		t.Error("publishing a port removed part of the floor")
	}
	// And it does not turn on routed egress: a publish needs no masquerade.
	if spec.RoutedEgress {
		t.Error("a publish-only declaration reported RoutedEgress — publish is a pure host-side " +
			"inbound rule set and needs none of the three egress mechanisms")
	}
	if indexOf(spec.Rules.ByHook(ruleset.HookPostrouting), "egress_masq") >= 0 {
		t.Error("a publish-only declaration emitted the egress masquerade")
	}
}

// RoutedEgress is a derived FACT, not a knob: it decides whether the provider must
// put the three host-side egress mechanisms in place (D5 ⑤). `via[]` alone does
// not need them — a steered flow is DNAT'd to a host-local endpoint and delivered
// locally.
func TestRoutedEgressIsDerivedFromTheDeclaration(t *testing.T) {
	for name, tc := range map[string]struct {
		d    Declaration
		want bool
	}{
		"deny+empty": {denyEmpty(), false},
		"deny+via":   {denyWithVia(t), false},
		"deny+allow": {denyWithAllow(t), true},
		"allow":      {allowPublic(), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mustGenerate(t, tc.d).RoutedEgress; got != tc.want {
				t.Errorf("RoutedEgress = %v, want %v", got, tc.want)
			}
		})
	}
}

// Guest-facing refusals use `reject`, external-facing ones use `drop` (the measured
// magnitude gap is in ruleset.Reject).
//
// The inbound default-deny is the deliberate exception: it faces an EXTERNAL
// caller the engine promises nothing, and an ICMP error would confirm the sandbox
// exists.
func TestGuestFacingRefusalsRejectWhileInboundDrops(t *testing.T) {
	spec := mustGenerate(t, allowPublic())
	for _, r := range spec.Rules.Rules {
		guestFacing := r.Scope == ruleset.ScopeGuestIn
		switch r.Counter {
		case "in_floor", "in_sibling", "fwd_floor_v4", "fwd_floor_v6", "fwd_default_deny":
			if r.Verdict != ruleset.Reject {
				t.Errorf("%q uses %s, want reject: this refusal faces the GUEST, and gVisor honors "+
					"ICMP errors — a pure-drop floor meets fail-closed's security half while breaking "+
					"the `clean connection failure` half it wrote down itself (D5 ⑨)", r.Counter, r.Verdict)
			}
		case "fwd_ingress_closed":
			if r.Verdict != ruleset.Drop {
				t.Errorf("%q uses %s, want drop: it faces an EXTERNAL caller, and an ICMP error "+
					"would confirm the sandbox exists", r.Counter, r.Verdict)
			}
		case "fwd_tier_v4", "fwd_tier_v6":
			// The tier faces the guest but is a policy refusal rather than a floor
			// breach; drop is what §2.4 describes for it, and either verdict is
			// security-equivalent. Pinned so a change is deliberate.
			if r.Verdict != ruleset.Drop {
				t.Errorf("%q uses %s, want drop", r.Counter, r.Verdict)
			}
		}
		_ = guestFacing
	}
}

// A rule whose Why mentions a measured fact must carry the citation, so the reason
// survives contact with a future editor who only sees the code.
func TestEveryRuleCitesTheSectionItDelivers(t *testing.T) {
	d := allowPublic()
	d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}}
	d.Egress.Via = []Via{{
		Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
		Endpoint: "10.0.0.5:15443",
	}}
	d.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}}
	for _, r := range mustGenerate(t, d).Rules.Rules {
		if len(r.Why) < 20 {
			t.Errorf("rule %q has a rationale too terse to be useful: %q", r.Counter, r.Why)
		}
	}
	// The second-hop rules too.
	rs, _ := SecondHopRules()
	for _, r := range rs.Rules {
		if !strings.Contains(r.Why, "§2.4") {
			t.Errorf("second-hop rule %q does not name the invariant it delivers: %q", r.Counter, r.Why)
		}
	}
}

// A declaration must produce a DETERMINISTIC rule set: the same input twice yields
// the same rules in the same order. Otherwise "the rules are equivalent" replaces
// "the rules are the same", and §6.2's promise that one declaration means one
// reachable set stops being checkable.
func TestGenerationIsDeterministic(t *testing.T) {
	d := allowPublic()
	d.Egress.Allow = []Tuple{
		{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP},
		{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP},
	}
	d.Ingress.Publish = []Publish{{GuestPort: 8080, Proto: model.NetTCP}, {GuestPort: 9090, Proto: model.NetTCP}}
	a, b := mustGenerate(t, d), mustGenerate(t, d)
	if len(a.Rules.Rules) != len(b.Rules.Rules) {
		t.Fatalf("rule counts differ: %d vs %d", len(a.Rules.Rules), len(b.Rules.Rules))
	}
	for i := range a.Rules.Rules {
		x, y := a.Rules.Rules[i], b.Rules.Rules[i]
		if x.Counter != y.Counter || x.Hook != y.Hook || x.Verdict != y.Verdict {
			t.Errorf("rule %d differs: %v vs %v", i, x, y)
		}
	}
	// Counters are declared before use, in a stable order — a named counter
	// referenced before its declaration fails the `nft -f` load and reports the
	// error against the wrong line (D5 ㉔).
	if len(a.Rules.Counters) != len(b.Rules.Counters) {
		t.Error("counter declaration order is not deterministic")
	}
	for i := range a.Rules.Counters {
		if a.Rules.Counters[i] != b.Rules.Counters[i] {
			t.Errorf("counter %d differs: %q vs %q", i, a.Rules.Counters[i], b.Rules.Counters[i])
		}
	}
}

// A declaration's `via[]` with an unparseable endpoint must fail generation rather
// than emit a rule set missing its narrow accept — that rule set would install a
// steering DNAT with nothing above the floor to let the steered flow through,
// which is `via[]` silently dead.
func TestGenerateRefusesAnEndpointItCannotPin(t *testing.T) {
	d := Zero()
	d.Egress.Via = []Via{{
		Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
		Endpoint: "10.0.0.5", // no port
	}}
	if _, err := Generate(d); err == nil {
		t.Error("generated a rule set for an endpoint with no port — the narrow accept must pin the " +
			"POST-NAT port (D5 ⑩), so without one the steering DNAT would be installed with nothing " +
			"above the floor to admit the steered flow: via[] silently dead")
	}
}

// The self-gateway allow is emitted for any allow[] entry overlapping the sandbox
// address space, in EITHER direction of overlap — a supernet like 10.0.0.0/8 and a
// narrow subnet inside the space both occur (§5.D semantics ③ / D5 ⑦).
func TestTheSelfGatewayAllowFollowsTheDeclaredSandboxSpaceOverlap(t *testing.T) {
	for name, tc := range map[string]struct {
		cidr string
		want bool
	}{
		"supernet of the sandbox space": {"10.0.0.0/8", true},
		"a subnet inside it":            {"10.66.250.0/24", true},
		"the exact space":               {"10.66.0.0/16", true},
		"a public address":              {"1.1.1.1/32", false},
		"another RFC1918 block":         {"192.168.0.0/16", false},
		"a v6 address":                  {"fd00::1/128", false},
	} {
		t.Run(name, func(t *testing.T) {
			d := Zero()
			d.Egress.Allow = []Tuple{{CIDR: mustCIDR(t, tc.cidr), Port: 53, Proto: model.NetUDP}}
			in := mustGenerate(t, d).Rules.ByHook(ruleset.HookInput)
			got := indexOf(in, "allow0_selfgw") >= 0
			if got != tc.want {
				t.Errorf("self-gateway allow emitted = %v, want %v for %s. It is §5.D semantics ③'s "+
					"ONLY delivery path: the sole stably-available host-local resolver address is this "+
					"sandbox's own /30 gateway, which lives inside the sibling CIDR (D5 ⑦)",
					got, tc.want, tc.cidr)
			}
			if got {
				gw := findRule(mustGenerate(t, d), "allow0_selfgw")
				if !gw.Match.DestSelfGateway {
					t.Error("the self-gateway rule does not name the gateway")
				}
				// It carries the DECLARED port and protocol, not a wildcard: the caller
				// declared a resolver on a port, not "everything on my gateway".
				if gw.Match.DestPort != 53 || gw.Match.Proto != ruleset.UDP {
					t.Errorf("the self-gateway rule does not carry the declared port/proto: %+v", gw.Match)
				}
				// …AND the declared prefix, so the rule matches the INTERSECTION. The
				// emission predicate above can only test OVERLAP (the gateway address is a
				// placement product the generator does not know), so without this clause a
				// declaration overlapping the sandbox space WITHOUT containing the gateway
				// would still reach it — see the dedicated test below.
				if gw.Match.DestSelfGatewayWithin == nil {
					t.Error("the self-gateway rule does not carry the declared prefix — the emission " +
						"test is OVERLAP, so without the prefix ANDed in, a declaration that never " +
						"contained the gateway still reaches it")
				} else if got := gw.Match.DestSelfGatewayWithin.String(); got != tc.cidr {
					t.Errorf("the self-gateway rule pins prefix %s, want the declared %s", got, tc.cidr)
				}
			}
		})
	}
}

// TestCounterAttributionCoversEveryDeclarationEntry is what makes §6.1's per-entry
// `hit_packet` trustworthy: every entry the caller wrote must map to counter names that
// actually EXIST in the emitted set. Attribution is produced where the names are minted
// (see Spec.Counters for why), and this asserts the join.
func TestCounterAttributionCoversEveryDeclarationEntry(t *testing.T) {
	d := Zero()
	d.Egress.Allow = []Tuple{
		{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP},
		{CIDR: mustCIDR(t, "8.8.8.8/32"), Port: 53, Proto: model.NetUDP},
	}
	d.Egress.Via = []Via{{
		Endpoint: "10.0.0.5:15443",
		Match: []Tuple{
			{CIDR: mustCIDR(t, "2.2.2.2/32"), Port: 80, Proto: model.NetTCP},
			{CIDR: mustCIDR(t, "3.3.3.3/32"), Port: 8443, Proto: model.NetTCP},
		},
	}}
	spec := mustGenerate(t, d)

	live := map[string]bool{}
	for _, c := range spec.Rules.Counters {
		live[c] = true
	}

	if got := len(spec.Counters.Allow); got != len(d.Egress.Allow) {
		t.Fatalf("attribution covers %d allow entries, declaration has %d — an entry with no counters reports 0 hits forever, which reads as \"never matched\" rather than \"not measured\"", got, len(d.Egress.Allow))
	}
	for i, names := range spec.Counters.Allow {
		if len(names) == 0 {
			t.Errorf("allow[%d] has no attributed counters", i)
		}
		for _, n := range names {
			if !live[n] {
				t.Errorf("allow[%d] is attributed counter %q, absent from the emitted rule set — attribution and names have diverged, and the symptom is a permanent 0", i, n)
			}
		}
	}
	for i, row := range spec.Counters.Via {
		for j, n := range row {
			if n == "" {
				t.Errorf("via[%d].match[%d] has no attributed counter", i, j)
				continue
			}
			if !live[n] {
				t.Errorf("via[%d].match[%d] is attributed %q, absent from the emitted set", i, j, n)
			}
		}
	}
	if spec.Counters.Default == "" || !live[spec.Counters.Default] {
		t.Errorf("the default catch-all counter is %q — empty or absent from the emitted set", spec.Counters.Default)
	}
}

// TestAllowAttributionIncludesTheSelfGatewayRule pins the aggregation mistake that would
// break the case operators reach for FIRST.
//
// An `allow[]` entry inside the sandbox address space also emits a self-gateway accept,
// and that rule is the ONLY delivery path for a declared host-local resolver (§5.D
// semantics ③ / network.md D5 ⑦). Summing only the input/forward counters would report 0
// hits for an entry that is plainly working — inverting the one signal §5.D has for "is
// this rule live".
func TestAllowAttributionIncludesTheSelfGatewayRule(t *testing.T) {
	d := Zero()
	d.Egress.Allow = []Tuple{{CIDR: ruleset.SandboxCIDR, Port: 53, Proto: model.NetUDP}}
	spec := mustGenerate(t, d)

	var sawSelfGw bool
	for _, n := range spec.Counters.Allow[0] {
		if strings.HasSuffix(n, "_selfgw") {
			sawSelfGw = true
		}
	}
	if !sawSelfGw {
		t.Errorf("allow[0] attribution is %v, with no self-gateway counter — for a declared host-local resolver that rule is the only delivery path, so this entry would report 0 hits while working (network.md D5 ⑦)", spec.Counters.Allow[0])
	}
}

// TestViaAttributionExcludesTheCompanionAccept pins the opposite mistake: counting both
// of a `via[]` entry's rules double-counts, because the DNAT and its companion accept act
// on the SAME flow at different hooks.
//
// The DNAT is the rule that IS the match, which is the semantics §6.1 fixes for this
// number. dnat > 0 with accept == 0 is additionally a known engine defect signature
// (D5 ⑩'s floor-kills-via case) rather than something a caller can act on.
func TestViaAttributionExcludesTheCompanionAccept(t *testing.T) {
	spec := mustGenerate(t, denyWithVia(t))
	got := spec.Counters.Via[0][0]
	if !strings.HasSuffix(got, "_dnat") {
		t.Errorf("via[0].match[0] is attributed %q; want the DNAT counter — that rule IS the match, and the companion accept acts on the same flow, so counting both reports one flow twice", got)
	}
	// The accept IS emitted; it is simply not what this entry reports. Without this the
	// test would pass on a fixture that never produced the shape it is about.
	var acceptEmitted bool
	for _, c := range spec.Rules.Counters {
		if strings.HasPrefix(c, "via0_0_") && strings.HasSuffix(c, "_accept") {
			acceptEmitted = true
		}
	}
	if !acceptEmitted {
		t.Fatalf("no via0_0_*_accept counter was emitted, so this test is not distinguishing the two cases")
	}
}

// TestDefaultAttributionExcludesTheFloorAndTheTier: the aggregate reports the
// DECLARATION's own catch-all and nothing else.
//
// Folding the L0 floor or the deny-safe tier in would be actively misleading rather than
// merely imprecise: a floor refusal is liftable by no declaration (§2.4), so a caller
// seeing those hits attributed to `egress.default` would widen an allow-list that cannot
// help.
func TestDefaultAttributionExcludesTheFloorAndTheTier(t *testing.T) {
	for name, tc := range map[string]struct {
		d    Declaration
		want string
	}{
		"deny":  {denyEmpty(), "fwd_default_deny"},
		"allow": {allowPublic(), "fwd_default_allow"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := mustGenerate(t, tc.d)
			if spec.Counters.Default != tc.want {
				t.Errorf("default attribution = %q, want %q", spec.Counters.Default, tc.want)
			}
			for _, bad := range []string{"floor", "tier", "sibling", "established"} {
				if strings.Contains(spec.Counters.Default, bad) {
					t.Errorf("the default aggregate names a %s counter (%q) — not liftable by any declaration, so attributing it here sends a caller to widen an allow-list that cannot help (§2.4)", bad, spec.Counters.Default)
				}
			}
		})
	}
}
