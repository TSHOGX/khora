package ruleset

import (
	"net/netip"
	"strings"
	"testing"
)

// The NAT64 derivation is MECHANICAL so the two halves of the v6 tier cannot
// drift, but a derivation nobody pinned can be silently wrong. These are the literals
// RFC 6052 the guest identity model requires for the three RFC1918 members under the well-known prefix.
//
// The negative half is the load-bearing one: `64:ff9b::/96` itself must NOT be a
// member. That prefix maps the ENTIRE IPv4 space — public destinations included —
// so dropping the whole /96 would refuse exactly the public egress
// `egress.default: allow` promises (measured, network substrate measurements).
func TestNAT64ImagesArePinnedAndThePrefixItselfIsNotAMember(t *testing.T) {
	want := map[string]bool{
		"fc00::/7":            true,
		"64:ff9b::a00:0/104":  true, // 10.0.0.0/8
		"64:ff9b::ac10:0/108": true, // 172.16.0.0/12
		"64:ff9b::c0a8:0/112": true, // 192.168.0.0/16
	}
	got := map[string]bool{}
	for _, p := range TierV6 {
		got[p.String()] = true
		if p == NAT64Prefix {
			t.Errorf("TierV6 contains the bare NAT64 prefix %s — per RFC 6052 it maps the whole "+
				"IPv4 space, so dropping it refuses the public egress `default: allow` promises (network enforcement)", p)
		}
	}
	for w := range want {
		if !got[w] {
			t.Errorf("TierV6 is missing %s", w)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("TierV6 has unexpected member %s", g)
		}
	}
}

// The floor's fourth item — "any address of the host itself" — must NOT be an
// address list (the provider security contract / network substrate measurements; see FloorV4).
//
// Asserted by shape rather than by reading a comment: the floor lists must hold
// only the three enumerable v4 items and the two v6 ones.
func TestFloorEnumeratesNoHostAddress(t *testing.T) {
	wantV4 := []string{"127.0.0.0/8", "169.254.0.0/16", "10.66.0.0/16"}
	wantV6 := []string{"::1/128", "fe80::/10"}
	assertPrefixes(t, "FloorV4", FloorV4, wantV4)
	assertPrefixes(t, "FloorV6", FloorV6, wantV6)

	// The sibling item IS the allocator's /16 — one copy, so the generator and the
	// allocator cannot disagree about the sandbox address space (network enforcement).
	found := false
	for _, p := range FloorV4 {
		if p == SandboxCIDR {
			found = true
		}
	}
	if !found {
		t.Errorf("FloorV4 does not name SandboxCIDR (%s) — the sibling item and the allocator's "+
			"range must be ONE fact, or a /30 can be handed out from outside the floor (the data-plane exposure boundary / network enforcement)", SandboxCIDR)
	}
}

// ClassAllowInsideFloor reads loopback and link-local ONLY — the sibling CIDR is
// excluded for the two reasons FloorLiftableNever gives.
func TestFloorLiftableNeverExcludesTheSiblingCIDR(t *testing.T) {
	assertPrefixes(t, "FloorLiftableNever", FloorLiftableNever,
		[]string{"127.0.0.0/8", "169.254.0.0/16", "::1/128", "fe80::/10"})
	for _, p := range FloorLiftableNever {
		if p == SandboxCIDR {
			t.Error("FloorLiftableNever names the sibling CIDR — an allow inside it is a LEGAL " +
				"declaration path (this sandbox's own /30 gateway), and " +
				"a 400 mentioning it would put the sandbox address space on the wire (the API error contract)")
		}
	}
}

// The floor and the tier must never share a set (measured, network enforcement — see TierV4).
//
// Asserted through Validate, i.e. through the mechanism that would actually stop
// someone from merging them, rather than as a standalone arithmetic claim.
func TestFloorAndTierCannotShareOneSet(t *testing.T) {
	merged := append(append([]netip.Prefix{}, FloorV4...), TierV4...)
	rs := RuleSet{
		Counters: []string{"c"},
		Rules: []Rule{{
			Hook: HookForward, Scope: ScopeGuestIn, Verdict: Drop, Counter: "c",
			Match: Match{DestV4: merged},
		}},
	}
	err := Validate(rs)
	if err == nil {
		t.Fatal("Validate accepted a set holding both the floor and the tier — nested members " +
			"make an nft interval set refuse to load, or silently merge the floor away (network enforcement)")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("Validate rejected the merged set for the wrong reason: %v", err)
	}
	// And each list on its own is legal — otherwise the check above would be
	// vacuous (it would reject everything).
	for name, set := range map[string][]netip.Prefix{"FloorV4": FloorV4, "TierV4": TierV4, "TierV6": TierV6, "FloorV6": FloorV6} {
		if err := validateNesting(set); err != nil {
			t.Errorf("%s is self-nesting, which it must not be: %v", name, err)
		}
	}
}

// Each guard, driven over source that carries the exact defect. A guard whose
// failure path was never exercised is a guard nobody has checked (the
// volume/seam_test.go reverse-injection precedent).
func TestValidateCatchesEachSilentlyBrokenShape(t *testing.T) {
	base := func(mut func(*Rule)) RuleSet {
		r := Rule{
			Hook: HookForward, Scope: ScopeGuestIn, Verdict: Drop, Counter: "c",
			Why: "test",
		}
		mut(&r)
		return RuleSet{Counters: []string{"c"}, Rules: []Rule{r}}
	}
	cases := []struct {
		name string
		rs   RuleSet
		want string
	}{{
		// An un-narrowed accept is a hole in EVERY sandbox's floor; an un-narrowed
		// drop terminates across chains and is a host-level kill switch (network enforcement ).
		name: "unscoped rule",
		rs:   base(func(r *Rule) { r.Scope = ScopeNone }),
		want: "unscoped",
	}, {
		// The two guard scopes must not cross: uid on a per-sandbox rule is not a
		// per-sandbox narrowing at all.
		name: "uid narrowing on a per-sandbox rule",
		rs:   base(func(r *Rule) { r.Scope = ScopeSecondHopUID }),
		want: "second-hop scope",
	}, {
		// THE RETURN SCOPE ON A PER-SANDBOX HOOK, the worse half of the same crossing,
		// and it WAS accepted while only the uid direction was guarded — caught then only
		// by the renderer's text guard, i.e. on the substrate at install time instead of
		// here, root-free.
		name: "second-hop return scope on a per-sandbox rule",
		rs: base(func(r *Rule) {
			r.Scope = ScopeSecondHopReturn
			r.Verdict = Accept
			r.Match = Match{CTState: []string{"established", "related"}}
		}),
		want: "second-hop scope",
	}, {
		// …and the reverse: the second hop arrives on no interface, so an interface
		// predicate there is simply unreachable (network enforcement).
		name: "interface narrowing on the second-hop chain",
		rs:   base(func(r *Rule) { r.Hook = HookOutputFilter; r.Scope = ScopeGuestIn }),
		want: "narrowed by",
	}, {
		// skuid misses packets, so an accept narrowed by it leaks them past the
		// floor it sits above — the measured asymmetry (network enforcement).
		name: "uid-narrowed accept",
		rs:   base(func(r *Rule) { r.Hook = HookOutputFilter; r.Scope = ScopeSecondHopUID; r.Verdict = Accept }),
		want: "may only DROP",
	}, {
		// A deny assertion is only valid when a counter moved; a rule with no
		// counter cannot participate in one (the provider security contract / network enforcement).
		name: "rule with no counter",
		rs:   base(func(r *Rule) { r.Counter = "" }),
		want: "no named counter",
	}, {
		// A named counter referenced before declaration fails the `nft -f` load and
		// reports the error against the wrong line (network enforcement).
		name: "undeclared counter",
		rs:   base(func(r *Rule) { r.Counter = "nowhere" }),
		want: "not declared",
	}, {
		name: "port without protocol",
		rs:   base(func(r *Rule) { r.Match.DestPort = 443 }),
		want: "requires an explicit protocol",
	}, {
		name: "both families in one rule",
		rs: base(func(r *Rule) {
			r.Match.DestV4 = FloorV4
			r.Match.DestV6 = FloorV6
		}),
		want: "both address families",
	}, {
		name: "no hook",
		rs:   base(func(r *Rule) { r.Hook = 0 }),
		want: "no hook",
	}, {
		name: "no verdict",
		rs:   base(func(r *Rule) { r.Verdict = 0 }),
		want: "no verdict",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.rs)
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate(%s) = %q, want it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

// The positive control for the guard suite above: a legal per-sandbox rule and a
// legal second-hop rule both pass, and the second-hop one passes WITHOUT an
// interface narrowing — the whole reason the guard is scoped by table, since a single
// "every rule carries iifname" rule would reject the one rule the second-hop floor
// requires and whichever check was written first would silently win.
func TestValidateAcceptsBothTablesLegalShapes(t *testing.T) {
	rs := RuleSet{
		Counters: []string{"floor", "hop2"},
		Rules: []Rule{{
			Hook: HookForward, Scope: ScopeGuestIn, Verdict: Drop, Counter: "floor",
			Match: Match{DestV4: FloorV4, CTState: nil}, Why: "L0 floor, v4 daddr items",
		}, {
			Hook: HookOutputFilter, Scope: ScopeSecondHopUID, Verdict: Drop, Counter: "hop2",
			Match: Match{DestV4: FloorV4}, Why: "second hop stays under the floor",
		}},
	}
	if err := Validate(rs); err != nil {
		t.Fatalf("Validate rejected a legal two-table rule set: %v", err)
	}
	if got := len(rs.ByHook(HookForward)); got != 1 {
		t.Errorf("ByHook(forward) = %d rules, want 1", got)
	}
	if got := len(rs.ByHook(HookOutputFilter)); got != 1 {
		t.Errorf("ByHook(output-filter) = %d rules, want 1", got)
	}
}

// Only drop and reject end traversal across base chains — the abstract form of the
// trap measured in network enforcement (see Accept). So "the accept counter moved" is not evidence a
// rule works, and the generator must place an approved exception in the SAME chain as
// the drop it overrides.
func TestOnlyTerminalVerdictsEndCrossChainTraversal(t *testing.T) {
	for v, wantTerminal := range map[Verdict]bool{
		Accept: false, Reject: true, Drop: true,
		DNAT: false, SNAT: false, Masquerade: false,
	} {
		if got := v.Terminal(); got != wantTerminal {
			t.Errorf("%s.Terminal() = %v, want %v", v, got, wantTerminal)
		}
	}
}

// PerSandbox is what gives the two render guards their disjoint scopes, so the
// mapping is asserted rather than assumed: exactly one hook is engine-level.
func TestExactlyOneHookIsEngineLevel(t *testing.T) {
	// Every hook, INCLUDING HookForwardIngress. Enumerating by hand is why that one
	// was missing: it is the hook whose priority carries the measured cross-sandbox
	// ordering fix, so leaving it out left exactly the arm that matters unchecked.
	all := []Hook{HookInput, HookForward, HookForwardIngress, HookPrerouting,
		HookPostrouting, HookOutputNAT, HookOutputFilter}
	engineLevel := 0
	for _, h := range all {
		if !h.PerSandbox() {
			engineLevel++
			if h != HookOutputFilter {
				t.Errorf("hook %s is engine-level; only the second-hop output filter chain may be (network enforcement)", h)
			}
		}
		if h.String() == "invalid" {
			t.Errorf("hook %d has no name", h)
		}
	}
	if engineLevel != 1 {
		t.Errorf("%d engine-level hooks, want exactly 1 (the second hop's output chain)", engineLevel)
	}
}

func assertPrefixes(t *testing.T, name string, got []netip.Prefix, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d members %v, want %d %v", name, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("%s[%d] = %s, want %s", name, i, got[i], want[i])
		}
	}
}
