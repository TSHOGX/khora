package runtime

import (
	"net/netip"
	"strings"
	"testing"

	"engine/internal/ruleset"
)

// testCtx is a rendering context with every placement product bound, so a test can
// render any rule shape without a real placement.
func testCtx() renderCtx {
	return renderCtx{
		table:            "sbxnet_sbx_test",
		vethHost:         "vh0123456789ab",
		guestIP:          "10.66.250.2",
		gatewayIP:        "10.66.250.1",
		publishEndpoints: map[uint16]string{8080: "127.0.0.1:21000"},
	}
}

func mustRender(t *testing.T, rs ruleset.RuleSet, ctx renderCtx) string {
	t.Helper()
	text, err := renderRuleSet(rs, ctx)
	if err != nil {
		t.Fatalf("renderRuleSet: %v", err)
	}
	return text
}

// one builds a single-rule rule set with its counter declared.
func one(r ruleset.Rule) ruleset.RuleSet {
	if r.Counter == "" {
		r.Counter = "c"
	}
	return ruleset.RuleSet{Counters: []string{r.Counter}, Rules: []ruleset.Rule{r}}
}

// --- gate A item 4: the four render guards, on rendered TEXT ------------------

// GUARD 1: no `policy drop` on a per-sandbox base chain.
//
// A chain policy CANNOT be narrowed by any input-interface predicate — that is a
// property of the policy position itself, not of the predicate — so a `policy drop`
// in a per-sandbox table is a CROSS-SANDBOX kill switch. Measured: it killed
// another sandbox's traffic while its own rule had matched not one packet.
//
// Both halves: the renderer never emits it, AND the guard catches it if someone
// hand-writes it (which is what makes the property survive a future edit).
func TestGuardRefusesPolicyDropInAPerSandboxTable(t *testing.T) {
	// The renderer's own output must always be `policy accept`.
	rs := one(ruleset.Rule{
		Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Why: "default deny",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "policy accept") {
		t.Error("the rendered base chain is not `policy accept`")
	}
	if strings.Contains(text, "policy drop") {
		t.Error("the renderer emitted `policy drop`")
	}
	if err := renderGuards(text, true); err != nil {
		t.Errorf("the guard rejected the renderer's own output: %v", err)
	}

	// Reverse injection: source carrying the exact defect. A guard whose failure path
	// was never exercised is a guard nobody has checked.
	injected := strings.Replace(text, "policy accept", "policy drop", 1)
	err := renderGuards(injected, true)
	if err == nil {
		t.Fatal("the guard accepted `policy drop` in a per-sandbox table — that is a cross-sandbox " +
			"kill switch: a chain policy cannot be narrowed by an interface predicate, and it was " +
			"measured killing ANOTHER sandbox's traffic while its own rule matched no packet")
	}
	if !strings.Contains(err.Error(), "CROSS-SANDBOX") {
		t.Errorf("the guard rejected it for the wrong reason: %v", err)
	}
}

// GUARD 2: `iifname` / `oifname` (by NAME), never `iif` / `oif` (by INDEX).
//
// This is the single hardest property to catch by review, because BOTH failure
// modes are invisible: `iif` freezes an ifindex at load time and silently fails OPEN
// after a same-name veth rebuild (measured — the guest's packet was DELIVERED while
// the drop counter read 0), and `nft list` resolves that frozen integer back to a
// name, so the two spellings look IDENTICAL in a listing (network.md D5 ⑫).
func TestGuardRefusesTheByIndexInterfaceSpelling(t *testing.T) {
	for _, scope := range []ruleset.Scope{ruleset.ScopeGuestIn, ruleset.ScopeGuestOut} {
		rs := one(ruleset.Rule{
			Hook: ruleset.HookForward, Scope: scope, Verdict: ruleset.Reject, Why: "floor",
		})
		text := mustRender(t, rs, testCtx())
		// The by-NAME spelling is what the renderer emits.
		if !strings.Contains(text, "iifname") && !strings.Contains(text, "oifname") {
			t.Errorf("scope %s rendered without a by-name interface predicate:\n%s", scope, text)
		}
		if err := renderGuards(text, true); err != nil {
			t.Errorf("the guard rejected a by-name predicate: %v", err)
		}
	}

	// Reverse injection, both spellings.
	for _, bad := range []string{
		`iif "vh0123456789ab" counter name c drop`,
		`oif "vh0123456789ab" counter name c drop`,
		`iif 12 counter name c drop`,
	} {
		if err := renderGuards("table inet t {\n  chain c {\n    "+bad+"\n  }\n}", true); err == nil {
			t.Errorf("the guard accepted the by-INDEX spelling %q — it freezes an ifindex at load "+
				"time and fails OPEN after a same-name veth rebuild, with the drop counter reading 0 "+
				"(indistinguishable from `no traffic yet`), and `nft list` resolves it back to a name "+
				"so review cannot see it (D5 ⑫)", bad)
		}
	}
	// …and it must NOT false-positive on the legitimate longer words.
	for _, ok := range []string{
		`iifname "vh0" counter name c drop`,
		`oifname "vh0" counter name c accept`,
		`meta iifkind "veth" iifname "vh0" counter name c drop`,
	} {
		if err := renderGuards("table inet t {\n  chain c {\n    "+ok+"\n  }\n}", true); err != nil {
			t.Errorf("the guard false-positived on %q: %v", ok, err)
		}
	}
}

// GUARD 3: no un-narrowed rule in a per-sandbox table, accept AND drop alike.
//
// Every per-sandbox table is a `policy accept` base chain on a SHARED hook, so an
// un-narrowed accept is a hole in EVERY sandbox's floor (measured: sandbox 2's flow
// was let through by sandbox 1's accept, with both floors counting 0). An
// un-narrowed drop is worse, because drops terminate across chains.
func TestGuardRefusesAnUnNarrowedPerSandboxRule(t *testing.T) {
	for _, bad := range []string{
		`ct state new counter name c accept`,
		`counter name c drop`,
		`tcp dport 443 counter name c accept`,
	} {
		err := renderGuards("table inet t {\n  chain c {\n    "+bad+"\n  }\n}", true)
		if err == nil {
			t.Errorf("the guard accepted the un-narrowed per-sandbox rule %q — an un-narrowed accept "+
				"is a hole in EVERY sandbox's floor (measured: another sandbox's flow was let through "+
				"with both floors counting 0), and an un-narrowed drop terminates across chains "+
				"(D5 ⑩)", bad)
		}
	}
	// The renderer cannot produce one: an unscoped rule is refused upstream by
	// ruleset.Validate, and renderScope refuses it again.
	rs := ruleset.RuleSet{Counters: []string{"c"}, Rules: []ruleset.Rule{{
		Hook: ruleset.HookForward, Verdict: ruleset.Drop, Counter: "c", // Scope zero
	}}}
	if _, err := renderRuleSet(rs, testCtx()); err == nil {
		t.Error("renderRuleSet accepted an unscoped rule")
	}
}

// GUARD 4, and the SCOPE SPLIT that makes the four guards coherent.
//
// On the engine-level second-hop table the narrowing predicate is `skuid`, because
// that traffic arrives on NO INTERFACE (measured: a host-local process's egress
// counts zero in `forward` in all three shapes and is visible only in `output` —
// network.md D5 ㉔). An un-narrowed drop there is a HOST-LEVEL egress kill switch:
// measured, root's own probe failed too.
//
// So the guards are SCOPED BY TABLE, not one guard with an exemption. The paired
// assertion below is the one that matters: a legal second-hop rule (skuid-narrowed,
// carrying NO interface predicate) must NOT be rejected by the per-sandbox guard —
// otherwise the guard and §2.4 invariant 2 are mutually exclusive and whichever was
// written first wins silently.
func TestTheGuardScopesDoNotCross(t *testing.T) {
	uid := 4711
	ctx := renderCtx{table: SecondHopTable, secondHopUID: &uid}
	rs := one(ruleset.Rule{
		Hook: ruleset.HookOutputFilter, Scope: ruleset.ScopeSecondHopUID, Verdict: ruleset.Drop,
		Match: ruleset.Match{DestV4: ruleset.FloorV4}, Counter: "hop2", Why: "§2.4 invariant 2",
	})
	text := mustRender(t, rs, ctx)
	if !strings.Contains(text, "meta skuid 4711") {
		t.Errorf("the second-hop rule rendered without its uid narrowing:\n%s", text)
	}
	if strings.Contains(text, "iifname") {
		t.Errorf("the second-hop rule carries an interface predicate, which is unreachable there:\n%s", text)
	}
	// The second-hop guard accepts it…
	if err := renderGuards(text, false); err != nil {
		t.Fatalf("the second-hop guard rejected a legal second-hop rule: %v", err)
	}
	// …and this is the load-bearing pair: the PER-SANDBOX guard would reject it, which
	// is exactly why the guards must be scoped by table rather than unified.
	if err := renderGuards(text, true); err == nil {
		t.Error("the per-sandbox guard ACCEPTED a uid-narrowed rule — the two guards must have " +
			"disjoint scopes, and this direction is what proves they do: a single " +
			"`every rule carries iifname` guard would reject the very rule §2.4 invariant 2 requires")
	}

	// An un-narrowed drop in the second-hop table is refused.
	if err := renderGuards("table inet t {\n  chain c {\n    ip daddr 10.66.0.0/16 counter name c drop\n  }\n}", false); err == nil {
		t.Error("the second-hop guard accepted a drop with no `skuid` — on the output hook that is a " +
			"HOST-LEVEL egress kill switch (measured: root's own probe failed too), and `iifname` " +
			"cannot be the narrowing there because the traffic arrives on no interface (D5 ㉔)")
	}
	// A uid-narrowed ACCEPT is refused: skuid misses packets, so an accept narrowed
	// by it leaks them past the floor it sits above (the measured asymmetry).
	if err := renderGuards("table inet t {\n  chain c {\n    meta skuid 4711 counter name c accept\n  }\n}", false); err == nil {
		t.Error("the second-hop guard accepted a uid-narrowed ACCEPT — `skuid` does not reliably " +
			"match every packet of a flow, so the packets it misses fall through to whatever is " +
			"below (D5 ㉔ asymmetry); a uid narrowing is safe on a DROP only")
	}
	// Rendering a uid-scoped rule with no uid bound must FAIL rather than silently
	// drop the narrowing — that would produce precisely the host-level kill switch.
	if _, err := renderRuleSet(rs, renderCtx{table: SecondHopTable}); err == nil {
		t.Error("renderRuleSet emitted a uid-scoped rule with no uid bound — silently dropping the " +
			"narrowing produces the host-level egress kill switch the guard exists to prevent")
	}
}

// The guards must read the RULE, not its rationale. Several of these rules carry
// comments that necessarily mention the dangerous shape they avoid, and a text scan
// that tripped on the explanation would force the explanations out — which is the
// wrong direction (the same reason edge_test.go reads the AST rather than grepping).
func TestGuardsIgnoreTheirOwnRationaleText(t *testing.T) {
	line := `iifname "vh0" counter name c drop comment "not policy drop, and not iif: see D5 ⑫"`
	if err := renderGuards("table inet t {\n  chain c {\n    "+line+"\n  }\n}", true); err != nil {
		t.Errorf("a guard tripped on a rule's own COMMENT: %v — the comments have to be able to name "+
			"the shapes they avoid, so the guard must read the rule and not the prose", err)
	}
	// And the real rendered output, whose comments do exactly that.
	rs := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Match:   ruleset.Match{CTState: []string{"new"}},
		Counter: "in_floor",
		Why:     "L0 floor: NOT policy drop (cross-sandbox kill switch), and iifname not iif (D5 ⑫)",
	})
	if err := renderGuards(mustRender(t, rs, testCtx()), true); err != nil {
		t.Errorf("a guard tripped on the renderer's own comment: %v", err)
	}
}

// --- the rendered file's shape -------------------------------------------------

// The idempotent atomic three-step, and the ordering inside the file.
//
// `delete table` on a table that does not exist returns rc=1 AND ABORTS THE WHOLE
// `-f` FILE, so the bare `table` line must come first (it creates it if absent) to
// make the delete always legal. And named counters MUST be declared before the
// chains referencing them, or the parse desynchronizes and the error is reported
// against a LATER line — several chains from its cause (measured, D5 ㉔).
func TestTheRenderedFileIsAnIdempotentAtomicThreeStep(t *testing.T) {
	rs := ruleset.RuleSet{
		Counters: []string{"a", "b"},
		Rules: []ruleset.Rule{
			{Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
				Match: ruleset.Match{CTState: []string{"established"}}, Counter: "a", Why: "return path"},
			{Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
				Counter: "b", Why: "default deny"},
		},
	}
	text := mustRender(t, rs, testCtx())

	iBare := strings.Index(text, "table inet sbxnet_sbx_test\n")
	iDelete := strings.Index(text, "delete table inet sbxnet_sbx_test")
	iDef := strings.Index(text, "table inet sbxnet_sbx_test {")
	if iBare < 0 || iDelete < 0 || iDef < 0 {
		t.Fatalf("the three-step is not all present:\n%s", text)
	}
	if !(iBare < iDelete && iDelete < iDef) {
		t.Errorf("the three-step is out of order (bare=%d delete=%d def=%d) — `delete table` on a "+
			"nonexistent table returns rc=1 and ABORTS THE WHOLE -f FILE, so the bare `table` line "+
			"must precede it", iBare, iDelete, iDef)
	}
	// Counters before chains.
	iCounter := strings.Index(text, "counter a {}")
	iChain := strings.Index(text, "chain c_input")
	if iCounter < 0 || iChain < 0 || iCounter > iChain {
		t.Errorf("named counters are not declared before the chains referencing them (counter=%d "+
			"chain=%d) — the parse then desynchronizes and reports the error several chains from "+
			"its cause (D5 ㉔)", iCounter, iChain)
	}
	// `fwd` is a RESERVED WORD in nft 1.1.6, and a chain-name collision presents as
	// "my rule syntax is wrong" several chains away from the real cause (D5 ㉔).
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "chain fwd") {
			t.Errorf("a chain is named `fwd`, which is reserved in nft 1.1.6 — the collision "+
				"desynchronizes the parser so EVERY LATER CHAIN reports a bogus syntax error of its "+
				"own (D5 ㉔): %q", line)
		}
	}
}

// The three placement products the renderer binds, and the two that are refused when
// unbound. Rendering with an unbound product must FAIL rather than emit a rule
// missing a clause — a floor rule without its interface is the un-narrowed shape
// guard 3 exists to stop.
func TestRenderingRefusesAnUnboundPlacementProduct(t *testing.T) {
	cases := map[string]struct {
		rule ruleset.Rule
		ctx  renderCtx
		want string
	}{
		"interface narrowing with no veth": {
			rule: ruleset.Rule{Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Drop, Counter: "c"},
			ctx:  renderCtx{table: "t"},
			want: "veth",
		},
		"self-gateway match with no gateway": {
			rule: ruleset.Rule{Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
				Match: ruleset.Match{DestSelfGateway: true}, Counter: "c"},
			ctx:  renderCtx{table: "t", vethHost: "vh0"},
			want: "gateway",
		},
		"guest match with no guest IP": {
			rule: ruleset.Rule{Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestOut, Verdict: ruleset.Accept,
				Match: ruleset.Match{DestGuest: true}, Counter: "c"},
			ctx:  renderCtx{table: "t", vethHost: "vh0"},
			want: "guest IP",
		},
		"publish scope with no endpoint bound": {
			rule: ruleset.Rule{Hook: ruleset.HookPrerouting, Scope: ruleset.ScopeHostEndpoint, Verdict: ruleset.DNAT,
				Match: ruleset.Match{Proto: ruleset.TCP}, ToGuestPort: 9999, Counter: "c"},
			ctx:  renderCtx{table: "t", vethHost: "vh0", guestIP: "10.66.250.2"},
			want: "no endpoint was bound",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := renderRuleSet(one(tc.rule), tc.ctx)
			if err == nil {
				t.Fatalf("rendered a rule with an unbound placement product — emitting it without the " +
					"clause is exactly the un-narrowed shape the guards exist to stop")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name what was missing (%q): %v", tc.want, err)
			}
		})
	}
}

// The ct-state clause must be FAMILY-UNQUALIFIED: one such rule in an `inet` table
// matches IPv6 too — including a global prefix the guest invented for itself, which
// the orchestrator could not enumerate even in principle — so an explicit
// `meta nfproto ipv6` second arm is redundant (measured: it counted exactly the
// packets the family-agnostic rule had already caught, network.md D5 ⑪).
func TestTheCtStateClauseCarriesNoFamilyQualifier(t *testing.T) {
	rs := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Match: ruleset.Match{CTState: []string{"new", "invalid", "untracked"}}, Counter: "in_floor",
		Why: "L0 floor, host-itself item",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "ct state new,invalid,untracked") {
		t.Errorf("the ct-state clause did not render as expected:\n%s", text)
	}
	for _, banned := range []string{"nfproto", "ip daddr", "ip6 daddr", "meta nfproto ipv6"} {
		if strings.Contains(text, banned) {
			t.Errorf("the ct-state floor carries %q — it must be family-UNQUALIFIED and carry no "+
				"address list, or the engine has to know its own bind addresses and the whole "+
				"`a host address changed, must we re-render` class of question comes back (D5 ⑧ ⑪):\n%s",
				banned, text)
		}
	}
}

// `via[]`'s narrow accept must render all three clauses — the pre-NAT tuple, the
// post-NAT port, and the interface. Each omission has a live measured
// counter-example (D5 ⑩), so the RENDERED text is checked clause by clause rather
// than trusting the abstract rule.
func TestTheViaAcceptRendersAllThreeClauses(t *testing.T) {
	cidr := netip.MustParsePrefix("1.1.1.1/32")
	rs := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
		Match: ruleset.Match{
			Proto: ruleset.TCP, DestPort: 15443,
			CTOriginalDest: &cidr, CTOriginalPort: 443,
			CTState: []string{"new"},
		},
		Counter: "via0_0_accept", Why: "the approved floor exception",
	})
	text := mustRender(t, rs, testCtx())
	for _, want := range []string{
		`iifname "vh0123456789ab"`,        // the interface
		"ct original ip daddr 1.1.1.1/32", // the pre-NAT destination
		"ct original proto-dst 443",       // the pre-NAT port
		"tcp dport 15443",                 // the post-NAT listener port
		"ct state new",
		"accept",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the narrow accept is missing %q — all three clauses are load-bearing, each "+
				"with a measured counter-example (D5 ⑩):\n%s", want, text)
		}
	}
}

// The transparent-interception form renders exactly ONE CLAUSE FEWER.
func TestTheTransparentFormRendersOneFewerClause(t *testing.T) {
	base := ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
		Match:   ruleset.Match{Proto: ruleset.TCP, DestPort: 15443, CTOriginalPort: 443, CTState: []string{"new"}},
		Counter: "via", Why: "transparent interception",
	}
	transparent := mustRender(t, one(base), testCtx())
	if strings.Contains(transparent, "ct original ip daddr") {
		t.Errorf("the transparent form emitted a pre-NAT cidr clause — the declaration authorized "+
			"ANY destination on that port, so the clause would block exactly the traffic it "+
			"allowed (D3.3 / D5 ⑩):\n%s", transparent)
	}
	cidr := netip.MustParsePrefix("1.1.1.1/32")
	pinned := base
	pinned.Match.CTOriginalDest = &cidr
	explicit := mustRender(t, one(pinned), testCtx())
	if !strings.Contains(explicit, "ct original ip daddr 1.1.1.1/32") {
		t.Error("the explicit form is missing its pre-NAT cidr clause")
	}
	// Exactly one clause of difference, measured by line length rather than by eye.
	if len(explicit) <= len(transparent) {
		t.Error("the explicit form is not longer than the transparent one")
	}
}

// A brace set renders without `auto-merge`. With it, nft LOADS a set holding nested
// members but SILENTLY SWALLOWS the subnet — which would merge the floor away
// (measured, network.md D5 ⑱). The safe failure direction is refusing to load.
func TestAddressSetsAreRenderedWithoutAutoMerge(t *testing.T) {
	rs := one(ruleset.Rule{
		Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Match: ruleset.Match{DestV4: ruleset.FloorV4}, Counter: "fwd_floor_v4", Why: "L0 floor v4",
	})
	text := mustRender(t, rs, testCtx())
	if strings.Contains(text, "auto-merge") {
		t.Error("an address set was rendered with `auto-merge` — it makes nft accept a set holding " +
			"nested members while SILENTLY merging the subnet away, which is how the floor gets " +
			"merged out of existence without a word (D5 ⑱)")
	}
	if !strings.Contains(text, "ip daddr { ") {
		t.Errorf("the multi-member set did not render as a brace set:\n%s", text)
	}
	// A nested set is refused upstream, so it can never reach nft at all.
	nested := ruleset.RuleSet{Counters: []string{"c"}, Rules: []ruleset.Rule{{
		Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Drop, Counter: "c",
		Match: ruleset.Match{DestV4: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"), ruleset.SandboxCIDR,
		}},
	}}}
	if _, err := renderRuleSet(nested, testCtx()); err == nil {
		t.Error("renderRuleSet accepted a set with nested members")
	}
}

// Guest-facing refusals render as `reject`, not `drop`.
//
// gVisor's netstack honors ICMP errors, so the two spellings differ by orders of
// magnitude in what the guest experiences (network.md D5 ⑨). §2.4's fail-closed has
// two halves and a pure-`drop` floor meets only the security one, violating the
// half it wrote down itself: "the guest gets a clean connection failure". Security
// is identical — neither forwards the packet.
func TestGuestFacingRefusalsRenderAsReject(t *testing.T) {
	rs := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Match: ruleset.Match{CTState: []string{"new"}}, Counter: "in_floor", Why: "L0 floor",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "reject") {
		t.Errorf("a guest-facing refusal did not render as `reject`:\n%s", text)
	}
	// No explicit ICMP type: on UDP gVisor IGNORES `admin-prohibited` and honors only
	// `port-unreachable`, and a rule covering both protocols cannot name a
	// per-protocol type. nft's default for an `inet` filter chain is `icmpx
	// port-unreachable`, which maps to the honored type in both families.
	if strings.Contains(text, "admin-prohibited") {
		t.Error("the refusal names `admin-prohibited`, which gVisor IGNORES on UDP (measured: the " +
			"guest still hung ~5 s). Only `port-unreachable` is honored there, and nft's inet " +
			"default is exactly that (D5 ⑨)")
	}
}

// The counter sits immediately before the verdict, so what a counter measures is
// unambiguous: the packets this rule's verdict acted on. Every deny assertion in
// gate B reads a counter, so an ambiguous placement would undermine all of them.
func TestTheCounterSitsImmediatelyBeforeTheVerdict(t *testing.T) {
	rs := one(ruleset.Rule{
		Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Reject,
		Match:   ruleset.Match{DestV4: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}},
		Counter: "fwd_floor_v4", Why: "L0 floor",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "counter name fwd_floor_v4 reject") {
		t.Errorf("the counter is not immediately before the verdict:\n%s", text)
	}
}

// Comments are truncated to nft's limit. A comment over it makes the WHOLE `-f`
// file fail to load, which would turn a rationale string into an outage.
func TestLongRationalesAreTruncatedRatherThanBreakingTheLoad(t *testing.T) {
	long := strings.Repeat("this rule delivers a very specific measured property. ", 10)
	rs := one(ruleset.Rule{
		Hook: ruleset.HookForward, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Drop,
		Counter: "c", Why: long,
	})
	text := mustRender(t, rs, testCtx())
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, `comment "`)
		if i < 0 {
			continue
		}
		body := line[i+len(`comment "`):]
		body = strings.TrimSuffix(body, `"`)
		if len(body) > nftCommentMax {
			t.Errorf("a rendered comment is %d bytes, over nft's %d-byte limit — the whole -f file "+
				"would fail to load", len(body), nftCommentMax)
		}
	}
}

// Table names carry the SandboxID verbatim and are recoverable from it, which is
// what lets reconciliation identify orphan tables and re-adopt a survivor's rules
// (§4.4-A labeling / §2.4: the rule set is crash-surviving host state).
func TestTableNamesAreReversibleAndTheEngineTableIsNotASandbox(t *testing.T) {
	const sandbox = "sbx_0011223344556677"
	table := nftTableName(sandbox)
	if !strings.HasPrefix(table, nftTablePrefix) {
		t.Errorf("table name %q does not carry the enumeration prefix", table)
	}
	got, ok := sandboxOfTable(table)
	if !ok || got != sandbox {
		t.Errorf("sandboxOfTable(%q) = %q,%v; want %q,true — enumeration must be reversible or "+
			"reconciliation cannot tell which table backs which sandbox", table, got, ok, sandbox)
	}
	// The engine-level second-hop table must NEVER be read as a sandbox's table: it
	// belongs to the process, so treating it as an orphan would tear down §2.4
	// invariant 2's enforcement on every reconciliation pass.
	if _, ok := sandboxOfTable(SecondHopTable); ok {
		t.Errorf("sandboxOfTable(%q) claimed the ENGINE-LEVEL table belongs to a sandbox — "+
			"reconciliation would then reclaim it as an orphan and silently switch off the second "+
			"hop's enforcement (§2.4 invariant 2)", SecondHopTable)
	}
	if !strings.HasPrefix(SecondHopTable, nftTablePrefix) {
		// It shares the prefix so a stray engine table is still visible to an operator
		// listing engine tables; the reversibility check above is what keeps that safe.
		t.Errorf("the second-hop table %q does not share the engine prefix", SecondHopTable)
	}
}

// Published endpoints are drawn from the engine's own port range, and the allocator
// does not hand the same port out twice — the same property the /30 allocator has,
// for the same reason: two sandboxes must not share a host endpoint.
func TestPublishPortAllocationDoesNotCollide(t *testing.T) {
	base := t.TempDir()
	p, err := NewLocalRunsc(base+"/root", base+"/state")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint16]bool{}
	for i := 0; i < 200; i++ {
		port := p.allocPublishPort()
		if port < publishPortBase || port >= publishPortBase+publishPortSpan {
			t.Fatalf("allocPublishPort returned %d, outside the engine's range [%d,%d)",
				port, publishPortBase, publishPortBase+publishPortSpan)
		}
		if seen[port] {
			t.Fatalf("allocPublishPort handed out %d twice — two sandboxes would share one host "+
				"endpoint, and one of the two publishes would silently never match", port)
		}
		seen[port] = true
	}
	// Freeing returns a port to circulation, or a long-running process leaks its port
	// range one publish at a time. Asserted by REWINDING the cursor, the same way the
	// /30 allocator's test does and for the same reason: the cursor walks the whole
	// range before wrapping, so "free one and allocate again" would pass whether or
	// not the free took effect — the allocator simply would not have come back around.
	for port := range seen {
		p.freePublishPort(port)
	}
	p.mu.Lock()
	p.pubSeq = publishPortBase
	p.mu.Unlock()
	reissued := p.allocPublishPort()
	if reissued != publishPortBase {
		t.Errorf("after freeing every port and rewinding, allocPublishPort returned %d rather than "+
			"the range's first port %d — a released port must return to circulation", reissued, publishPortBase)
	}

	// The exhaustion path must not spin forever: with the whole range held, the
	// allocator would loop looking for a free port. That is a real operational shape
	// (a deployment publishing thousands of ports), so it must fail rather than hang.
	p.mu.Lock()
	for port := publishPortBase; port < publishPortBase+publishPortSpan; port++ {
		p.pubInUse[port] = true
	}
	p.mu.Unlock()
	if got := p.allocPublishPort(); got != 0 {
		t.Errorf("allocPublishPort returned %d with the whole range held; want 0 (exhausted) — "+
			"anything else means it either spun or handed out a port already in use", got)
	}
}

// The default publish address is LOOPBACK, and that default is a decision: publish
// is an EXPLICIT inbound exception (§2.1 ③), so binding it to every host interface
// by default would make "this sandbox is reachable from outside" a consequence of
// omitting a flag rather than of a decision.
func TestThePublishAddressDefaultsToLoopback(t *testing.T) {
	base := t.TempDir()
	p, err := NewLocalRunsc(base+"/root", base+"/state")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.publishIP(); got != "127.0.0.1" {
		t.Errorf("publishIP() = %q by default, want loopback — external reachability must come from "+
			"a deployment DECISION, not from omitting a flag (§2.1 ③)", got)
	}
	p.HostPublishIP = "192.168.1.10"
	if got := p.publishIP(); got != "192.168.1.10" {
		t.Errorf("publishIP() = %q after the deployment named an address", got)
	}
}

// The publish DNAT halves render with the host endpoint as their narrowing and the
// guest as their target — the two clauses that make them per-sandbox rules despite
// having no interface predicate available (prerouting sees an arbitrary caller
// interface; the output copy is locally generated).
func TestThePublishDNATsRenderEndpointNarrowedAndGuestTargeted(t *testing.T) {
	for _, hook := range []ruleset.Hook{ruleset.HookPrerouting, ruleset.HookOutputNAT} {
		rs := one(ruleset.Rule{
			Hook: hook, Scope: ruleset.ScopeHostEndpoint, Verdict: ruleset.DNAT,
			Match: ruleset.Match{Proto: ruleset.TCP}, ToGuestPort: 8080,
			Counter: "pub0", Why: "publish",
		})
		text := mustRender(t, rs, testCtx())
		for _, want := range []string{
			"ip daddr 127.0.0.1", // the bound host endpoint's address
			"tcp dport 21000",    // …and its port: the per-sandbox narrowing
			"dnat ip to 10.66.250.2:8080",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("hook %s: the publish DNAT is missing %q:\n%s", hook, want, text)
			}
		}
		if err := renderGuards(text, true); err != nil {
			t.Errorf("hook %s: the guards rejected a publish DNAT: %v", hook, err)
		}
	}
}

// The self-gateway allow renders as the INTERSECTION of the gateway and the
// declared prefix — two ANDed `ip daddr` clauses.
//
// One clause alone is the hole: a declaration that merely OVERLAPS the sandbox
// address space (the generator's emission test, since it does not know the gateway
// address) would reach a gateway it never contained — a HOST address, and with it
// any host process bound `0.0.0.0:<declared port>`, i.e. §2.4's host-itself floor
// item lifted by a declaration that never named it (measured exposure: D5 ⑧).
func TestTheSelfGatewayAllowRendersTheIntersection(t *testing.T) {
	// Declared prefix EXCLUDES this context's gateway (10.66.250.1).
	excluded := netip.MustParsePrefix("10.66.99.0/24")
	rs := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
		Match: ruleset.Match{
			DestSelfGateway: true, DestSelfGatewayWithin: &excluded,
			Proto: ruleset.TCP, DestPort: 8080,
		},
		Counter: "allow0_selfgw", Why: "declared ∩ own gateway",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "ip daddr 10.66.250.1") {
		t.Errorf("the gateway clause is missing:\n%s", text)
	}
	if !strings.Contains(text, "ip daddr 10.66.99.0/24") {
		t.Errorf("the DECLARED-prefix clause is missing — with the gateway clause alone this rule "+
			"reaches 10.66.250.1:8080, an address the declaration does not contain, and with it any "+
			"host process bound 0.0.0.0:8080 (§2.4 host-itself floor item, D5 ⑧):\n%s", text)
	}
	// Two clauses, ANDed by nft — which is what makes the match the intersection
	// rather than a union. Asserted by count so a future edit that replaces one with
	// the other (rather than keeping both) fails here.
	if n := strings.Count(text, "ip daddr "); n != 2 {
		t.Errorf("the rule carries %d `ip daddr` clauses, want exactly 2 (the gateway AND the "+
			"declared prefix):\n%s", n, text)
	}
	if err := renderGuards(text, true); err != nil {
		t.Errorf("the guards rejected the intersection form: %v", err)
	}

	// And the empty-clause direction: a rule with no declared prefix renders the
	// gateway alone. That shape is now only reachable by hand-construction — the
	// generator always supplies the prefix — but it must not silently become the
	// intersection form, or a test could pass while asserting nothing.
	bare := one(ruleset.Rule{
		Hook: ruleset.HookInput, Scope: ruleset.ScopeGuestIn, Verdict: ruleset.Accept,
		Match:   ruleset.Match{DestSelfGateway: true, Proto: ruleset.TCP, DestPort: 8080},
		Counter: "bare", Why: "gateway only",
	})
	if n := strings.Count(mustRender(t, bare, testCtx()), "ip daddr "); n != 1 {
		t.Errorf("a gateway-only rule rendered %d daddr clauses, want 1", n)
	}
}

// The publish return SNAT rewrites the source to this sandbox's own /30 gateway.
// Without it a publish works only on a sandbox that happens to carry a default
// route and TIMES OUT on one that does not — measured `to_guest=2 / from_guest=0`,
// and adding the SNAT made the same flow connect immediately (network.md D5 ⑬).
func TestThePublishReturnSNATTargetsTheOwnGateway(t *testing.T) {
	rs := one(ruleset.Rule{
		Hook: ruleset.HookPostrouting, Scope: ruleset.ScopeGuestOut, Verdict: ruleset.SNAT,
		Match: ruleset.Match{DestGuest: true}, ToSelfGateway: true,
		Counter: "pub_return_snat", Why: "publish return path",
	})
	text := mustRender(t, rs, testCtx())
	if !strings.Contains(text, "snat ip to 10.66.250.1") {
		t.Errorf("the return SNAT does not target this sandbox's own gateway:\n%s", text)
	}
	if !strings.Contains(text, `oifname "vh0123456789ab"`) {
		t.Errorf("the return SNAT is not narrowed to this sandbox's veth:\n%s", text)
	}
}

// The nat chains land on the right hooks with the right priorities — DNAT must be
// in a nat-type prerouting/output chain to be evaluated before routing, and the
// return SNAT and masquerade in nat postrouting.
func TestChainSpecsMapHooksToTheKernelsOwnSplit(t *testing.T) {
	want := map[ruleset.Hook]nftChain{
		ruleset.HookInput:        {"c_input", "filter", "input", "filter"},
		ruleset.HookForward:      {"c_forward", "filter", "forward", "filter"},
		ruleset.HookPrerouting:   {"c_prerouting", "nat", "prerouting", "dstnat"},
		ruleset.HookPostrouting:  {"c_postrouting", "nat", "postrouting", "srcnat"},
		ruleset.HookOutputNAT:    {"c_output_nat", "nat", "output", "dstnat"},
		ruleset.HookOutputFilter: {"c_output", "filter", "output", "filter"},
	}
	for hook, exp := range want {
		got, err := chainSpec(hook)
		if err != nil {
			t.Errorf("chainSpec(%s): %v", hook, err)
			continue
		}
		if got != exp {
			t.Errorf("chainSpec(%s) = %+v, want %+v", hook, got, exp)
		}
	}
	if _, err := chainSpec(ruleset.Hook(99)); err == nil {
		t.Error("chainSpec accepted an unknown hook")
	}
}
