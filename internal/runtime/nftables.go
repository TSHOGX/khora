package runtime

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"unicode/utf8"

	"engine/internal/ruleset"
)

// This file is the §5.A half of the §5.D split (network.md D1 ② / D6): render the
// abstract rule set into nft text and install it, binding the three things only
// this provider holds — the host-side veth name, the guest IP, and the publish
// host endpoint. Everything above the seam stays placement-free.
//
// # The table shape, and why it is this shape
//
// ONE TABLE PER SANDBOX. A table is an atomically-deletable named unit, which
// lands exactly on the shape `enumeratePlacements` / `teardownPlacement` already
// have. Base chains on `input` AND `forward`, because a forward-only rule set
// misses guest→host entirely (measured, network.md D5 ⑧).
//
// EVERY PER-SANDBOX BASE CHAIN IS `policy accept`, with the default refusal
// written as the last NARROWED rule instead. This is not a preference: a chain
// policy CANNOT be narrowed by any input-interface predicate — that is a property
// of the policy position itself — so a `policy drop` in a per-sandbox table is a
// CROSS-SANDBOX kill switch. Measured: it killed another sandbox's traffic while
// its own rule had not matched a single packet.
//
// The second-hop table is separate and engine-level (`SecondHopTable`): its
// predicate is a uid rather than an interface, one uid covers every sandbox, and it
// is installed once at startup.
const (
	// nftTablePrefix names per-sandbox tables. It carries the SandboxID verbatim so
	// enumeration is reversible, the same convention as the netns name (§4.4-A:
	// labeling placement with its SandboxID is the §3.3 reverse, and it is what lets
	// reconciliation both re-adopt a survivor's rules and identify orphan tables).
	nftTablePrefix = "sbxnet_"

	// SecondHopTable is the ENGINE-LEVEL table pinning the second hop (§2.4
	// invariant 2 / network.md D5 ㉔). Not per-sandbox: the predicate is the
	// deployment-supplied uid, so one rule set serves every `via[]` in the
	// deployment and it is installed once when the orchestrator starts.
	SecondHopTable = "sbxnet_hop2"
)

// nftTableName is the per-sandbox table for a SandboxID.
func nftTableName(sandbox string) string { return nftTablePrefix + sandbox }

// renderCtx is everything the renderer must substitute for a Scope or a Match
// flag — the placement products §5.D deliberately cannot name.
type renderCtx struct {
	table string
	// vethHost is the host-side veth. The narrowing predicate is `iifname` /
	// `oifname` BY NAME, never `iif` / `oif` by index — see renderScope.
	vethHost string
	// guestIP and gatewayIP are the two halves of this sandbox's /30.
	guestIP   string
	gatewayIP string
	// publishEndpoints maps a published guest port to the host `ip:port` bound for
	// it at this placement.
	publishEndpoints map[uint16]string
	// secondHopUID is the deployment-supplied uid, for the engine-level table only.
	secondHopUID *int
}

// renderRuleSet renders an abstract rule set into ONE `nft -f` file.
//
// # The idempotent atomic three-step
//
// The file opens with bare `table` + `delete table` + the full definition. That
// combination is required rather than tidy: `delete table` on a table that does
// not exist returns rc=1 AND ABORTS THE WHOLE `-f` FILE, so a bare `table` line
// first (which creates it if absent) makes the delete always legal. The result is
// idempotent and atomic — which is precisely what §4.4-A's Adopt needs.
//
// # Why atomic reinstall is the ONLY safe way to re-install
//
// Measured on a live flow across a control-plane restart (network.md D5 ㉒):
//   - ONE atomic `nft -f` file: the flow SURVIVED 5/5. The conntrack entry's id is
//     unchanged and the masquerade rule counts ZERO while bytes keep moving — the
//     flow is running on the transform stored in the existing ct entry, and the nat
//     hook is not re-evaluated.
//   - `delete` then `install` as two steps: the flow DIED 5/5. Not from losing ct
//     state (the entry was untouched, same id, still ESTABLISHED) — packets sent
//     during the gap left with the RAW sandbox source address, the peer did not
//     recognize the connection, and sent an RST.
//
// So a two-step reinstall is a live-connection killer, and a control arm pins the
// cause: deleting only the FILTER chains across the same gap did NOT kill the flow.
// What is fatal is the NAT chain's absence, not the floor's.
//
// A third arm is a prohibition rather than a shape: `conntrack -F` kills live flows
// outright under `tcp_loose=0`, so reconciliation must NEVER flush conntrack. (On
// this host `tcp_loose=1` masks it — conntrack mid-stream-adopts the flow. That is
// a property of a sysctl the engine does not own and must not depend on.)
func renderRuleSet(rs ruleset.RuleSet, ctx renderCtx) (string, error) {
	if err := ruleset.Validate(rs); err != nil {
		return "", fmt.Errorf("runtime: refusing to render an invalid rule set: %w", err)
	}
	var b strings.Builder

	fmt.Fprintf(&b, "# engine per-sandbox rule set (§5.D generated, §5.A rendered)\n")
	// The idempotent atomic three-step (see the doc comment).
	fmt.Fprintf(&b, "table inet %s\n", ctx.table)
	fmt.Fprintf(&b, "delete table inet %s\n", ctx.table)
	fmt.Fprintf(&b, "table inet %s {\n", ctx.table)

	// NAMED COUNTERS MUST BE DECLARED BEFORE THE CHAINS REFERENCING THEM in one
	// `-f` file. Getting this wrong does not fail cleanly: the parse desynchronizes
	// and the error is reported against a later line, so the failure appears several
	// chains away from its cause (measured, network.md D5 ㉔).
	for _, c := range rs.Counters {
		fmt.Fprintf(&b, "  counter %s {}\n", c)
	}

	for _, h := range hookOrder(rs) {
		rules := rs.ByHook(h)
		if len(rules) == 0 {
			continue
		}
		spec, err := chainSpec(h)
		if err != nil {
			return "", err
		}
		// `policy accept` on every per-sandbox base chain, ALWAYS. A chain policy
		// cannot be narrowed by an interface predicate, so a `policy drop` here is a
		// cross-sandbox kill switch — measured, it killed another sandbox's traffic
		// while its own rule matched not one packet. The default refusal is the last
		// NARROWED rule instead, which the generator emits.
		//
		// Chain names avoid nft's reserved words: `fwd` is reserved in nft 1.1.6 and a
		// collision desynchronizes the parser so that EVERY LATER CHAIN reports a bogus
		// syntax error of its own — i.e. a chain-name collision presents as "my rule
		// syntax is wrong", several chains from the actual cause (measured, D5 ㉔).
		fmt.Fprintf(&b, "  chain %s {\n", spec.name)
		fmt.Fprintf(&b, "    type %s hook %s priority %s; policy accept;\n", spec.typ, spec.hook, spec.priority)
		for _, r := range rules {
			line, err := renderRule(r, ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
		fmt.Fprintf(&b, "  }\n")
	}
	fmt.Fprintf(&b, "}\n")
	return b.String(), nil
}

// hookOrder returns the hooks present, in a deterministic order.
func hookOrder(rs ruleset.RuleSet) []ruleset.Hook {
	hs := rs.Hooks()
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	return hs
}

type nftChain struct {
	name     string
	typ      string
	hook     string
	priority string
}

// chainSpec maps an abstract hook to its nft base-chain declaration.
//
// The nat/filter split is the kernel's, not ours: DNAT must be in a nat-type
// prerouting/output chain to be evaluated before routing, and the return SNAT and
// the masquerade must be in nat postrouting.
func chainSpec(h ruleset.Hook) (nftChain, error) {
	switch h {
	case ruleset.HookInput:
		return nftChain{"c_input", "filter", "input", "filter"}, nil
	case ruleset.HookForward:
		// NOT named `fwd`: that is a reserved word in nft 1.1.6, and the collision
		// desynchronizes the parser so every later chain reports a bogus error (D5 ㉔).
		return nftChain{"c_forward", "filter", "forward", "filter"}, nil
	case ruleset.HookPrerouting:
		return nftChain{"c_prerouting", "nat", "prerouting", "dstnat"}, nil
	case ruleset.HookPostrouting:
		return nftChain{"c_postrouting", "nat", "postrouting", "srcnat"}, nil
	case ruleset.HookOutputNAT:
		return nftChain{"c_output_nat", "nat", "output", "dstnat"}, nil
	case ruleset.HookOutputFilter:
		return nftChain{"c_output", "filter", "output", "filter"}, nil
	}
	return nftChain{}, fmt.Errorf("runtime: no nft chain for hook %s", h)
}

// renderRule renders one abstract rule.
//
// Clause order within the line follows nft's own convention (narrowing, then
// match, then counter, then verdict) and the counter sits IMMEDIATELY BEFORE the
// verdict so that what the counter measures is unambiguous: the packets this
// rule's verdict acted on.
func renderRule(r ruleset.Rule, ctx renderCtx) (string, error) {
	var cl []string

	scope, err := renderScope(r, ctx)
	if err != nil {
		return "", err
	}
	cl = append(cl, scope...)

	match, err := renderMatch(r.Match, ctx)
	if err != nil {
		return "", err
	}
	cl = append(cl, match...)

	cl = append(cl, "counter name "+r.Counter)

	verdict, err := renderVerdict(r, ctx)
	if err != nil {
		return "", err
	}
	cl = append(cl, verdict)

	line := strings.Join(cl, " ")
	if r.Why != "" {
		// The rationale goes on the wire as an nft comment. Not decoration: several of
		// these lines are one word away from being a security hole with no test firing,
		// so an operator reading `nft list` needs to see which promise each carries.
		line += fmt.Sprintf(" comment %q", truncateComment(r.Why))
	}
	return line, nil
}

// nftCommentMax is nft's comment length limit (128 bytes including the NUL). A
// comment over it makes the whole `-f` file fail to load, which would turn a
// rationale string into an outage.
const nftCommentMax = 127

// ellipsis marks a truncated comment. Counted in BYTES, not runes: the limit nft
// applies is a byte length, and "…" is three bytes in UTF-8 — reserving one
// character's worth would overshoot by two and fail the load.
const ellipsis = "…"

func truncateComment(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= nftCommentMax {
		return s
	}
	cut := nftCommentMax - len(ellipsis)
	// Do not slice inside a multi-byte rune: the rationales carry § and D-circle
	// characters, and a half rune would make the comment invalid UTF-8.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// renderScope turns the abstract narrowing into its concrete predicate.
//
// `iifname` / `oifname` BY NAME, never `iif` / `oif` by index, and this is the
// single most important line in the file to get right because BOTH failure modes
// are invisible:
//
//   - `iif` compiles the name to a FROZEN INTEGER ifindex at load time. After a
//     same-name veth rebuild the bytecode is byte-for-byte unchanged and still
//     points at the dead index, so the rule SILENTLY FAILS OPEN — measured, the
//     guest's packet was DELIVERED while the drop counter read 0 (network.md D5 ⑫).
//     And a zero counter is indistinguishable from "no traffic yet", so the symptom
//     looks like nothing at all.
//   - `nft list` normally resolves that frozen integer BACK to a name, so the two
//     spellings look identical in a listing. Review cannot catch this; only a guard
//     can. Hence renderGuards below, and hence this is not a style rule.
//
// A further un-measured but worse risk points the same way: ifindexes were only
// observed increasing, but if a RETIRED index were ever reused by ANOTHER
// sandbox's veth, a stale `iif` rule would start matching THAT sandbox's traffic —
// silent cross-sandbox misattribution rather than a mere hole.
func renderScope(r ruleset.Rule, ctx renderCtx) ([]string, error) {
	switch r.Scope {
	case ruleset.ScopeGuestIn:
		if ctx.vethHost == "" {
			return nil, fmt.Errorf("runtime: rule %q needs the host veth name", r.Counter)
		}
		return []string{fmt.Sprintf("iifname %q", ctx.vethHost)}, nil
	case ruleset.ScopeGuestOut:
		if ctx.vethHost == "" {
			return nil, fmt.Errorf("runtime: rule %q needs the host veth name", r.Counter)
		}
		return []string{fmt.Sprintf("oifname %q", ctx.vethHost)}, nil
	case ruleset.ScopeHostEndpoint:
		ep, ok := ctx.publishEndpoints[endpointKeyOf(r)]
		if !ok {
			return nil, fmt.Errorf("runtime: rule %q is scoped to a publish host endpoint, but no "+
				"endpoint was bound for guest port %d", r.Counter, endpointKeyOf(r))
		}
		addr, port, err := splitEndpoint(ep)
		if err != nil {
			return nil, fmt.Errorf("runtime: rule %q: %w", r.Counter, err)
		}
		fam := "ip"
		if !addr.Is4() {
			fam = "ip6"
		}
		proto := string(r.Match.Proto)
		if proto == "" {
			return nil, fmt.Errorf("runtime: rule %q is scoped to a host endpoint but names no protocol", r.Counter)
		}
		return []string{
			fmt.Sprintf("%s daddr %s", fam, addr),
			fmt.Sprintf("%s dport %d", proto, port),
		}, nil
	case ruleset.ScopeSecondHopUID:
		if ctx.secondHopUID == nil {
			return nil, fmt.Errorf("runtime: rule %q is narrowed by the second-hop uid, but the "+
				"deployment supplied none — an UN-NARROWED drop on this hook is a HOST-LEVEL egress "+
				"kill switch (measured: root's own probe failed too), so this must fail rather than "+
				"render without the narrowing", r.Counter)
		}
		return []string{fmt.Sprintf("meta skuid %d", *ctx.secondHopUID)}, nil
	}
	return nil, fmt.Errorf("runtime: rule %q has no scope — refusing to render an un-narrowed rule "+
		"(an un-narrowed accept is a hole in every sandbox's floor; an un-narrowed drop is a "+
		"host-level kill switch)", r.Counter)
}

// endpointKeyOf recovers which published guest port a host-endpoint-scoped rule
// belongs to. Both DNAT halves target the guest port, so that is the key.
func endpointKeyOf(r ruleset.Rule) uint16 { return r.ToGuestPort }

// renderMatch renders the selector clauses.
func renderMatch(m ruleset.Match, ctx renderCtx) ([]string, error) {
	var cl []string

	// CT-STATE FIRST and FAMILY-UNQUALIFIED. Deliberately carries no `meta nfproto`
	// arm: one such rule in an `inet` table matches IPv6 too — including a global
	// prefix the guest invented for itself, which the orchestrator could not
	// enumerate even in principle — so an explicit v6 second arm is redundant
	// (measured: it counted exactly the packets the family-agnostic rule had already
	// caught, network.md D5 ⑪).
	if len(m.CTState) > 0 {
		cl = append(cl, "ct state "+strings.Join(m.CTState, ","))
	}

	// The PRE-NAT tuple, for `via[]`'s narrow accept. `ct original` reads the
	// conntrack entry's original-direction tuple, which is established from the
	// packet's ORIGINAL header before any transform rule is evaluated — connection
	// tracking registers at a priority EARLIER than address translation, and the
	// transform only records the reverse direction, never overwriting the original.
	// That is the same mechanism every general-purpose transparent proxy relies on
	// (network.md D3.3 / D5 ⑥).
	if m.CTOriginalDest != nil {
		fam := "ip"
		if !m.CTOriginalDest.Addr().Is4() {
			fam = "ip6"
		}
		cl = append(cl, fmt.Sprintf("ct original %s daddr %s", fam, m.CTOriginalDest))
	}
	if m.CTOriginalPort != 0 {
		cl = append(cl, fmt.Sprintf("ct original proto-dst %d", m.CTOriginalPort))
	}

	// Destination address sets, per family — nft's grammar is family-typed, so `ip
	// daddr` matches only v4 packets and `ip6 daddr` only v6. Two rules where a set
	// would be needed is a GRAMMAR consequence, not the generator forking for IPv6.
	if len(m.DestV4) > 0 {
		set, err := renderPrefixSet(m.DestV4, true)
		if err != nil {
			return nil, err
		}
		cl = append(cl, "ip daddr "+set)
	}
	if len(m.DestV6) > 0 {
		set, err := renderPrefixSet(m.DestV6, false)
		if err != nil {
			return nil, err
		}
		cl = append(cl, "ip6 daddr "+set)
	}

	// The two placement-bound destinations.
	if m.DestSelfGateway {
		if ctx.gatewayIP == "" {
			return nil, fmt.Errorf("runtime: a rule needs this sandbox's own /30 gateway address")
		}
		cl = append(cl, "ip daddr "+ctx.gatewayIP)
		// The INTERSECTION with what the caller declared. Two `ip daddr` clauses in one
		// rule are ANDed by nft (verified), so this matches "the gateway, and only if
		// the declaration actually contained it".
		//
		// Without it the rule would reach the gateway on the declared port even for a
		// declaration that merely OVERLAPS the sandbox address space without containing
		// the gateway — lifting the floor's host-itself item with a declaration that
		// never named that address (see Match.DestSelfGatewayWithin).
		if within := m.DestSelfGatewayWithin; within != nil {
			fam := "ip"
			if !within.Addr().Is4() {
				fam = "ip6"
			}
			cl = append(cl, fmt.Sprintf("%s daddr %s", fam, within))
		}
	}
	if m.DestGuest {
		if ctx.guestIP == "" {
			return nil, fmt.Errorf("runtime: a rule needs this sandbox's guest IP")
		}
		cl = append(cl, "ip daddr "+ctx.guestIP)
	}

	// The post-NAT L4 selector. Port implies protocol — nft makes port matching
	// depend on protocol matching (D3.1) — and ruleset.Validate already refuses a
	// port without one.
	if m.DestPort != 0 {
		if m.Proto == "" {
			return nil, fmt.Errorf("runtime: a port match with no protocol reached the renderer")
		}
		cl = append(cl, fmt.Sprintf("%s dport %d", m.Proto, m.DestPort))
	} else if m.Proto != "" && m.CTOriginalPort == 0 && !hasPortClause(cl) {
		// A protocol on its own is still a narrowing worth emitting. Skipped when a
		// dport clause is already present — a `<proto> dport N` clause makes nft
		// generate the protocol dependency implicitly, so restating it as
		// `meta l4proto` is redundant. Not merely untidy: the redundant clause makes
		// two rules that mean the same thing render differently, which is the kind of
		// diff that hides a real change during review.
		cl = append(cl, "meta l4proto "+string(m.Proto))
	}
	return cl, nil
}

// hasPortClause reports whether a dport clause was already emitted — by the scope
// (the publish host-endpoint case) or by an earlier match clause.
func hasPortClause(cl []string) bool {
	for _, c := range cl {
		if strings.Contains(c, " dport ") {
			return true
		}
	}
	return false
}

// renderPrefixSet renders an address set, refusing anything but numeric literals.
//
// NUMERIC LITERALS ONLY IS A SECURITY REQUIREMENT, not input hygiene: nft resolves
// a BARE WORD in an address position THROUGH THE HOST RESOLVER, so a malformed
// address does not fail — it silently becomes a valid rule matching an unrelated
// IP. netip.Prefix cannot hold a name, so this is structurally satisfied; the check
// is here to keep it that way if the type ever loosens.
func renderPrefixSet(set []netip.Prefix, wantV4 bool) (string, error) {
	parts := make([]string, 0, len(set))
	for _, p := range set {
		if !p.IsValid() {
			return "", fmt.Errorf("runtime: invalid prefix in an address set")
		}
		if p.Addr().Is4() != wantV4 {
			return "", fmt.Errorf("runtime: prefix %s is in the wrong family set", p)
		}
		parts = append(parts, p.String())
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	// A brace set of prefixes is an interval set. It must NOT contain nested members:
	// nft either refuses to load ("conflicting intervals") or — with auto-merge —
	// SILENTLY swallows the subnet (measured, network.md D5 ⑱). ruleset.Validate
	// already rejects a nested set upstream; `auto-merge` is deliberately NOT set
	// here, so the failure direction stays loud.
	return "{ " + strings.Join(parts, ", ") + " }", nil
}

// renderVerdict renders the verdict clause.
func renderVerdict(r ruleset.Rule, ctx renderCtx) (string, error) {
	switch r.Verdict {
	case ruleset.Accept:
		return "accept", nil
	case ruleset.Drop:
		return "drop", nil
	case ruleset.Reject:
		// GUEST-FACING refusals use `reject`, and the per-protocol form matters:
		// gVisor's netstack honors ICMP errors, so `reject` gives the guest a clean,
		// immediate connection failure where `drop` gives it a multi-second hang
		// (network.md D5 ⑨). §2.4's fail-closed has two halves and a pure-`drop` floor
		// meets only the security one. Security is identical — neither forwards.
		//
		// On UDP the type matters too: `admin-prohibited` is IGNORED by gVisor while
		// `port-unreachable` is honored. This rule may cover both protocols, so the
		// per-protocol form is not available; nft's default for an `inet` filter chain
		// is `icmpx port-unreachable`, which maps to the honored type in both families.
		return "reject", nil
	case ruleset.DNAT:
		if r.ToEndpoint != "" {
			addr, port, err := splitEndpoint(r.ToEndpoint)
			if err != nil {
				return "", err
			}
			fam := "ip"
			if !addr.Is4() {
				fam = "ip6"
			}
			return fmt.Sprintf("dnat %s to %s", fam, joinHostPort(addr, port)), nil
		}
		if r.ToGuestPort != 0 {
			if ctx.guestIP == "" {
				return "", fmt.Errorf("runtime: rule %q DNATs to the guest but no guest IP is bound", r.Counter)
			}
			return fmt.Sprintf("dnat ip to %s:%d", ctx.guestIP, r.ToGuestPort), nil
		}
		return "", fmt.Errorf("runtime: rule %q is a DNAT with no target", r.Counter)
	case ruleset.SNAT:
		if !r.ToSelfGateway {
			return "", fmt.Errorf("runtime: rule %q is an SNAT with no target", r.Counter)
		}
		if ctx.gatewayIP == "" {
			return "", fmt.Errorf("runtime: rule %q SNATs to this sandbox's gateway but none is bound", r.Counter)
		}
		return fmt.Sprintf("snat ip to %s", ctx.gatewayIP), nil
	case ruleset.Masquerade:
		return "masquerade", nil
	}
	return "", fmt.Errorf("runtime: rule %q has no verdict", r.Counter)
}

func splitEndpoint(ep string) (netip.Addr, uint16, error) {
	ap, err := netip.ParseAddrPort(ep)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("endpoint %q is not `ip:port`: %w", ep, err)
	}
	return ap.Addr(), ap.Port(), nil
}

func joinHostPort(a netip.Addr, port uint16) string {
	if a.Is4() {
		return fmt.Sprintf("%s:%d", a, port)
	}
	return fmt.Sprintf("[%s]:%d", a, port)
}

// --- the render guards --------------------------------------------------------

// renderGuards is the TEXT half of the render guards (the abstract half is
// ruleset.Validate). It runs on the rendered file, immediately before install, and
// it is not defensive programming: each property below is broken by ONE WORD of
// editing, with no test firing and no visible difference in `nft list`. That is the
// same standing the §5.A seam token guard has.
//
// Three properties, each with a measured counter-example:
//
//  1. NO `policy drop` on a per-sandbox base chain. A chain policy cannot be
//     narrowed by any interface predicate — a property of the policy position
//     itself — so it is a CROSS-SANDBOX kill switch: measured, it killed another
//     sandbox's traffic while its own rule had matched no packet at all.
//
//  2. NO `iif` / `oif` (index), only `iifname` / `oifname` (name). `iif` freezes an
//     ifindex at load time and silently fails OPEN after a same-name veth rebuild
//     — measured, the packet was delivered with the drop counter at 0 — and `nft
//     list` resolves the frozen index back to a name, so the two spellings look
//     identical to a reviewer (network.md D5 ⑫).
//
//  3. NO un-narrowed rule in a per-sandbox table, for accept AND drop alike. Every
//     per-sandbox table is a `policy accept` base chain on a SHARED hook, so an
//     un-narrowed accept is a hole in EVERY sandbox's floor; an un-narrowed drop is
//     worse, since drops terminate across chains (measured both ways). Nat-type
//     verdicts are covered too — an un-narrowed DNAT would steer other sandboxes'
//     traffic.
//
// And the FOURTH, on the second-hop table only: no drop without `skuid`. An
// un-narrowed drop on the output hook is a HOST-LEVEL egress kill switch (measured:
// root's own probe failed too), and `iifname` cannot serve as that narrowing there
// because the traffic arrives on no interface. Two tables, two guards, two
// predicates — the guards are SCOPED BY TABLE rather than one guard with an
// exemption, because a single "every rule carries iifname" guard would reject the
// very rule §2.4 invariant 2 requires.
func renderGuards(text string, perSandbox bool) error {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Strip the trailing comment so a rationale mentioning e.g. "policy drop"
		// cannot trip the guard — the rules are what is being checked, not the prose.
		code := stripComment(trimmed)

		// GUARD 1.
		if strings.Contains(code, "policy drop") {
			return fmt.Errorf("runtime: rendered rule set carries `policy drop` (%q). A chain policy "+
				"CANNOT be narrowed by any input-interface predicate, so in a per-sandbox table it is a "+
				"CROSS-SANDBOX kill switch — measured, it killed another sandbox's traffic while its own "+
				"rule had matched no packet. Write the default refusal as the last NARROWED rule and "+
				"keep the chain `policy accept`", code)
		}
		// GUARD 2.
		if hasIndexInterfacePredicate(code) {
			return fmt.Errorf("runtime: rendered rule set uses `iif`/`oif` (by INDEX) instead of "+
				"`iifname`/`oifname` (by NAME) in %q. `iif` freezes an ifindex at load time and "+
				"silently FAILS OPEN after a same-name veth rebuild — measured, the guest's packet was "+
				"delivered while the drop counter read 0 — and `nft list` resolves the frozen index "+
				"back to a name, so a reviewer cannot see the difference (network.md D5 ⑫)", code)
		}
		// GUARDS 3 and 4, on rules only.
		if !isRuleLine(code) {
			continue
		}
		if perSandbox {
			if !strings.Contains(code, "iifname") && !strings.Contains(code, "oifname") &&
				!strings.Contains(code, "daddr") {
				return fmt.Errorf("runtime: rendered per-sandbox rule %q carries NO narrowing. Every "+
					"per-sandbox table is a `policy accept` base chain on a SHARED hook, so an "+
					"un-narrowed accept is a hole in EVERY sandbox's floor, and an un-narrowed drop is "+
					"worse — drops terminate across chains, making it a host-level kill switch "+
					"(measured both ways)", code)
			}
			if strings.Contains(code, "meta skuid") {
				return fmt.Errorf("runtime: rendered per-sandbox rule %q is narrowed by uid. That "+
					"predicate belongs to the ENGINE-LEVEL second-hop table (§2.4 invariant 2 / "+
					"network.md D5 ㉔) — a uid narrowing is not a per-sandbox narrowing at all", code)
			}
			continue
		}
		// The second-hop table: every DROP must carry `skuid`.
		if strings.Contains(code, "drop") && !strings.Contains(code, "meta skuid") {
			return fmt.Errorf("runtime: rendered second-hop rule %q drops WITHOUT a `skuid` narrowing. "+
				"On the output hook that is a HOST-LEVEL egress kill switch — measured, root's own "+
				"probe failed too — and `iifname` cannot serve as the narrowing here, because this "+
				"traffic arrives on no interface (network.md D5 ㉔)", code)
		}
		// …and nothing in this table may be an accept narrowed by uid: `skuid` misses
		// packets, so such an accept above a floor drop leaks whatever it misses.
		if strings.Contains(code, "accept") && strings.Contains(code, "meta skuid") {
			return fmt.Errorf("runtime: rendered second-hop rule %q is a uid-narrowed ACCEPT. `skuid` "+
				"does not reliably match every packet of a flow, so an accept narrowed by it leaks the "+
				"packets it misses past the floor it sits above (measured asymmetry, network.md D5 ㉔) "+
				"— a uid narrowing is safe on a DROP only", code)
		}
	}
	return nil
}

// isRuleLine reports whether a line is a rule rather than a chain/table/counter
// declaration or a brace.
//
// `counter <name> {}` (a DECLARATION) and `counter name <x>` (a REFERENCE inside a
// rule) both start with the same word, and the difference matters: an un-narrowed
// rule whose only clauses are the counter reference and the verdict —
// `counter name c drop`, i.e. the worst case guard 3 exists to catch — looks like a
// declaration to a naive prefix test and would be skipped. So the two are told
// apart by the `name` keyword rather than by the leading word.
func isRuleLine(code string) bool {
	switch {
	case code == "}" || code == "{":
		return false
	case strings.HasPrefix(code, "table "), strings.HasPrefix(code, "delete table "):
		return false
	case strings.HasPrefix(code, "chain "), strings.HasPrefix(code, "type "):
		return false
	case strings.HasPrefix(code, "counter ") && !strings.HasPrefix(code, "counter name "):
		return false // a counter DECLARATION, not a rule referencing one
	}
	return strings.Contains(code, "counter name ")
}

// stripComment removes a trailing `comment "…"` so the guards read the RULE and not
// its rationale. Without this, a comment explaining why `policy drop` is refused
// would itself trip guard 1 — and the fix must not be to stop explaining.
func stripComment(code string) string {
	if i := strings.Index(code, " comment \""); i >= 0 {
		return code[:i]
	}
	return code
}

// hasIndexInterfacePredicate finds `iif`/`oif` used as a bare keyword, without
// matching the legitimate `iifname`/`oifname`.
func hasIndexInterfacePredicate(code string) bool {
	for _, kw := range []string{"iif", "oif"} {
		idx := 0
		for {
			i := strings.Index(code[idx:], kw)
			if i < 0 {
				break
			}
			at := idx + i
			idx = at + len(kw)
			// A preceding letter means this is part of a longer word.
			if at > 0 && isWordByte(code[at-1]) {
				continue
			}
			// A following letter means `iifname` / `oifname` / `iiftype` — the by-NAME
			// forms, which are the correct ones.
			if idx < len(code) && isWordByte(code[idx]) {
				continue
			}
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
