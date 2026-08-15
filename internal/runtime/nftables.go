package runtime

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/TSHOGX/khora/internal/ruleset"
)

// This file is the runtime provider contract half of the network declaration split (the network enforcement locus / network enforcement): render the
// abstract rule set into nft text and install it, binding the three things only
// this provider holds — the host-side veth name, the guest IP, and the publish
// host endpoint. Everything above the seam stays placement-free.
//
// # The table shape, and why it is this shape
//
// ONE TABLE PER SANDBOX. A table is an atomically-deletable named unit, which
// lands exactly on the shape `enumeratePlacements` / `teardownPlacement` already
// have. Base chains on `input` AND `forward`, because a forward-only rule set
// misses guest→host entirely (measured, network substrate measurements).
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
	// enumeration is reversible, the same convention as the netns name (atomic transition rollback:
	// labeling placement with its SandboxID is the ID-reference model reverse, and it is what lets
	// reconciliation both re-adopt a survivor's rules and identify orphan tables).
	nftTablePrefix = "sbxnet_"

	// SecondHopTable is the ENGINE-LEVEL table pinning the second hop, as measured by
	// the network substrate suite. It is not per-sandbox: the predicate is the
	// deployment-supplied uid, so one rule set serves every `via[]` in the
	// deployment and it is installed once when the orchestrator starts.
	SecondHopTable = "sbxnet_hop2"
)

// nftTableName is the per-sandbox table for a SandboxID.
func nftTableName(sandbox string) string { return nftTablePrefix + sandbox }

// renderCtx is everything the renderer must substitute for a Scope or a Match
// flag — the placement products the network declaration deliberately cannot name.
type renderCtx struct {
	table string
	// vethHost is the host-side veth. The narrowing predicate is `iifname` /
	// `oifname` BY NAME, never `iif` / `oif` by index — see renderScope.
	vethHost string
	// guestIP and gatewayIP are the two halves of this sandbox's /30.
	guestIP   string
	gatewayIP string
	// publishEndpoints maps a published `{guest_port, proto}` to the host `ip:port`
	// bound for it at this placement.
	publishEndpoints map[publishKey]string
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
// idempotent and atomic — which is precisely what atomic transition rollback's Adopt needs.
//
// # Why atomic reinstall is the ONLY safe way to re-install
//
// Measured on a live flow across a control-plane restart (network substrate measurements):
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

	fmt.Fprintf(&b, "# engine per-sandbox rule set (the network declaration generated, the runtime provider contract rendered)\n")
	// The idempotent atomic three-step (see the doc comment).
	fmt.Fprintf(&b, "table inet %s\n", ctx.table)
	fmt.Fprintf(&b, "delete table inet %s\n", ctx.table)
	fmt.Fprintf(&b, "table inet %s {\n", ctx.table)

	// NAMED COUNTERS MUST BE DECLARED BEFORE THE CHAINS REFERENCING THEM in one
	// `-f` file. Getting this wrong does not fail cleanly: the parse desynchronizes
	// and the error is reported against a later line, so the failure appears several
	// chains away from its cause (measured, network substrate measurements).
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
		// `policy accept` ALWAYS (see the file header); the default refusal is the last
		// NARROWED rule, which the generator emits. Chain names come from chainSpec,
		// which states why they avoid nft's reserved words.
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
		// desynchronizes the parser so every later chain reports a bogus error (network enforcement).
		return nftChain{"c_forward", "filter", "forward", "filter"}, nil
	case ruleset.HookForwardIngress:
		// A LATER priority than the floor chain, so EVERY sandbox's floor is evaluated
		// before ANY sandbox's ingress default-deny (see ruleset.HookForwardIngress).
		return nftChain{"c_forward_in", "filter", "forward", "filter + 10"}, nil
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
	// Do not slice inside a multi-byte rune: rationale text may contain Unicode markers
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
//     guest's packet was DELIVERED while the drop counter read 0, and a zero counter
//     is indistinguishable from "no traffic yet".
//   - `nft list` normally resolves that frozen integer BACK to a name, so the two
//     spellings look identical in a listing. Review cannot catch this; only a guard
//     can — hence renderGuards below.
//
// Both mechanisms, down to the bytecode: network substrate measurements.
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
		// The protocol is checked FIRST because it is half the endpoint's KEY: without
		// it the lookup below would miss and report "no endpoint was bound", pointing at
		// the binding for what is really a malformed rule.
		proto := string(r.Match.Proto)
		if proto == "" {
			return nil, fmt.Errorf("runtime: rule %q is scoped to a host endpoint but names no protocol "+
				"— the protocol is half of a publish's identity (the network declaration keys on `{guest_port, proto}`), so "+
				"there is no endpoint to look up without it", r.Counter)
		}
		ep, ok := ctx.publishEndpoints[endpointKeyOf(r)]
		if !ok {
			return nil, fmt.Errorf("runtime: rule %q is scoped to a publish host endpoint, but no "+
				"endpoint was bound for %s", r.Counter, endpointKeyOf(r))
		}
		addr, port, err := splitEndpoint(ep)
		if err != nil {
			return nil, fmt.Errorf("runtime: rule %q: %w", r.Counter, err)
		}
		fam := "ip"
		if !addr.Is4() {
			fam = "ip6"
		}
		return []string{
			fmt.Sprintf("%s daddr %s", fam, addr),
			fmt.Sprintf("%s dport %d", proto, port),
		}, nil
	case ruleset.ScopeSecondHopReturn:
		// No identity predicate: the narrowing IS the ct-state clause renderMatch emits.
		// Legal only in the engine-level table, and only for an established/related
		// accept — ruleset.Validate enforces both, so this arm carries no check of its own.
		return nil, nil
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

// publishKey identifies a published endpoint. IT IS THE PAIR, not the guest port
// alone, because that is what a publish IS at the declaration layer: the network declaration's
// idempotency and deduplication are both defined on `{guest_port, proto}`, so
// `[{8080,tcp},{8080,udp}]` is two legal items and network.AddPublish appends both.
// Keying the binding on the port alone silently narrows the provider's notion of
// identity below the declaration's, and the failure is not a missing rule: the
// generator emits all four DNATs, and the BINDING refuses the pair as a duplicate —
// blaming the declaration layer for a key width only the provider chose. Because the
// declaration is already in the store by then, that refusal repeats at every resume.
type publishKey struct {
	port  uint16
	proto ruleset.Proto
}

func (k publishKey) String() string { return fmt.Sprintf("%s/%d", k.proto, k.port) }

// endpointKeyOf recovers which published item a host-endpoint-scoped rule belongs
// to. Both DNAT halves target the guest port and carry the protocol in Match.Proto,
// which is exactly the declaration-layer identity (see publishKey).
func endpointKeyOf(r ruleset.Rule) publishKey {
	return publishKey{port: r.ToGuestPort, proto: r.Match.Proto}
}

// renderMatch renders the selector clauses.
func renderMatch(m ruleset.Match, ctx renderCtx) ([]string, error) {
	var cl []string

	// CT-STATE FIRST and FAMILY-UNQUALIFIED. Deliberately carries no `meta nfproto`
	// arm: one such rule in an `inet` table matches IPv6 too — including a global
	// prefix the guest invented for itself, which the orchestrator could not
	// enumerate even in principle — so an explicit v6 second arm is redundant
	// (measured: it counted exactly the packets the family-agnostic rule had already
	// caught, network substrate measurements).
	if len(m.CTState) > 0 {
		cl = append(cl, "ct state "+strings.Join(m.CTState, ","))
	}

	// The PRE-NAT tuple, for `via[]`'s narrow accept. `ct original` reads the
	// conntrack entry's original-direction tuple, which is established from the
	// packet's ORIGINAL header before any transform rule is evaluated — connection
	// tracking registers at a priority EARLIER than address translation, and the
	// transform only records the reverse direction, never overwriting the original.
	// That is the same mechanism every general-purpose transparent proxy relies on
	// (via intent forms / network enforcement).
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
	// depend on protocol matching (network enforcement) — and ruleset.Validate already refuses a
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
	// SILENTLY swallows the subnet (measured, network substrate measurements). ruleset.Validate
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
		// (network substrate measurements). the provider security contract's fail-closed has two halves and a pure-`drop` floor
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

// renderGuards is the TEXT half of the render guards (the abstract half is ruleset.Validate),
// run on the rendered file immediately before install. Each property below is broken by ONE
// WORD of editing, with no test firing and no visible difference in `nft list` — the same
// standing the runtime provider contract seam token guard has.
//
// Four properties. Each one's danger, and the measurement that pins it, is stated
// where the renderer makes the choice — 1 in the file header, 2 in renderScope, 3 and
// 4 in the refusals below — and argued in full in network substrate measurements / :
//
//  1. NO `policy drop` on a per-sandbox base chain.
//  2. NO `iif` / `oif` (index), only `iifname` / `oifname` (name).
//  3. NO un-narrowed rule in a per-sandbox table, for accept AND drop alike —
//     nat-type verdicts included, since an un-narrowed DNAT steers other sandboxes.
//  4. On the second-hop table only: no terminal verdict without `skuid`.
//
// THE GUARDS ARE SCOPED BY TABLE, not one guard with an exemption: `iifname` cannot
// narrow anything on the output hook (that traffic arrives on no interface), so a
// single "every rule carries iifname" guard would reject the second-hop rule.
func renderGuards(text string, perSandbox bool, endpoints map[publishKey]string) error {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Strip the trailing comment so a rationale mentioning e.g. "policy drop"
		// cannot trip the guard — the rules are what is being checked, not the prose.
		code := stripComment(trimmed)

		// Per-sandbox base chains may not use drop policy.
		if strings.Contains(code, "policy drop") {
			return fmt.Errorf("runtime: rendered rule set carries `policy drop` (%q). A chain policy "+
				"CANNOT be narrowed by any input-interface predicate, so in a per-sandbox table it is a "+
				"CROSS-SANDBOX kill switch — measured, it killed another sandbox's traffic while its own "+
				"rule had matched no packet. Write the default refusal as the last NARROWED rule and "+
				"keep the chain `policy accept`", code)
		}
		// Interface matches must bind by name, never by index.
		if hasIndexInterfacePredicate(code) {
			return fmt.Errorf("runtime: rendered rule set uses `iif`/`oif` (by INDEX) instead of "+
				"`iifname`/`oifname` (by NAME) in %q. `iif` freezes an ifindex at load time and "+
				"silently FAILS OPEN after a same-name veth rebuild — measured, the guest's packet was "+
				"delivered while the drop counter read 0 — and `nft list` resolves the frozen index "+
				"back to a name, so a reviewer cannot see the difference (network substrate measurements)", code)
		}
		// GUARDS 3 and 4, on rules only.
		if !isRuleLine(code) {
			continue
		}
		if perSandbox {
			if !hasPerSandboxNarrowing(code, endpoints) {
				return fmt.Errorf("runtime: rendered per-sandbox rule %q carries NO narrowing. Every "+
					"per-sandbox table is a `policy accept` base chain on a SHARED hook, so an "+
					"un-narrowed accept is a hole in EVERY sandbox's floor, and an un-narrowed drop is "+
					"worse — drops terminate across chains, making it a host-level kill switch "+
					"(measured both ways). The three legal narrowings are `iifname` (ScopeGuestIn), "+
					"`oifname` (ScopeGuestOut) and a host endpoint's `daddr` AND `dport` TOGETHER "+
					"(ScopeHostEndpoint)", code)
			}
			if strings.Contains(code, "meta skuid") {
				return fmt.Errorf("runtime: rendered per-sandbox rule %q is narrowed by uid. That "+
					"predicate belongs to the ENGINE-LEVEL second-hop table (the second-hop floor / "+
					"network substrate measurements) — a uid narrowing is not a per-sandbox narrowing at all", code)
			}
			continue
		}
		// The second-hop table. An ACCEPT is stated as a WHITELIST rather than as a set
		// of enumerated refusals: the return arm is the only accept this table may
		// carry, so an accept shape nobody anticipated must FAIL rather than fall
		// through every branch. Enumerating refusals is how `ct state new … accept` —
		// the confused deputy itself — passed: it matched no refusal and reached the
		// end of the loop.
		if strings.Contains(code, "accept") {
			legal := strings.Contains(code, "ct state") &&
				!strings.Contains(code, "new") &&
				!strings.Contains(code, "meta skuid")
			if !legal {
				return fmt.Errorf("runtime: rendered second-hop rule %q is not the RETURN arm — the "+
					"only accept this table may carry is `ct state established,related` with no "+
					"identity predicate. An accept matching `new` admits NEW connections to in-floor "+
					"addresses, the confused deputy the second-hop floor refuses (network enforcement); one narrowed by "+
					"`skuid` leaks the packets skuid misses past the floor it sits above (measured "+
					"asymmetry, network substrate measurements) — a uid narrowing is safe on a DROP only", code)
			}
			continue
		}
		// EVERY TERMINAL VERDICT, not just the word `drop`: `reject` terminates
		// traversal across chains identically (ruleset.Verdict.Terminal), so an
		// un-narrowed reject here is the same host-level egress kill switch.
		if (strings.Contains(code, "drop") || strings.Contains(code, "reject")) &&
			!strings.Contains(code, "meta skuid") {
			return fmt.Errorf("runtime: rendered second-hop rule %q carries a terminal verdict WITHOUT "+
				"a `skuid` narrowing. On the output hook that is a HOST-LEVEL egress kill switch — "+
				"measured, root's own probe failed too — and `iifname` cannot serve as the narrowing "+
				"here, because this traffic arrives on no interface (network substrate measurements)", code)
		}
		// A nat verdict has no business in this table at all: it pins the second hop
		// under the L0 floor and does nothing else, so a DNAT here would steer
		// host-originated traffic.
		if strings.Contains(code, "dnat ") || strings.Contains(code, "snat ") ||
			strings.Contains(code, "masquerade") {
			return fmt.Errorf("runtime: rendered second-hop rule %q carries a nat verdict. The "+
				"engine-level table pins the second hop under the floor and does nothing else "+
				"(the second-hop floor); a nat verdict there would steer host-originated traffic", code)
		}
	}
	return nil
}

// hasPerSandboxNarrowing reports whether a per-sandbox rule line carries one of the
// THREE narrowings renderScope can actually produce for a per-sandbox table. It is
// the narrowed-verdict criterion, and it is written against renderScope's real output rather than
// against "some address clause is present".
//
// One entry per legal scope, and no fourth:
//
//   - `iifname` — ScopeGuestIn. The unforgeable one (network enforcement).
//   - `oifname` — ScopeGuestOut. It cannot be spelled with `iifname` at all: a
//     publish's companion accept sees the CALLER's interface, and the inbound
//     default-deny narrows by where the traffic is going (network enforcement / the isolation model).
//   - `daddr` AND `dport` TOGETHER — ScopeHostEndpoint, whose whole clause is
//     `ip daddr <host ip> <proto> dport <host port>`. The host port is allocated per
//     placement and per publish item, and it is the PORT that makes the pair
//     per-sandbox: the address alone is a host address every sandbox's table names.
//
// WHY THE ENDPOINT SET AND NOT A `daddr`+`dport` TEXT SHAPE: that shape is not unique
// to ScopeHostEndpoint. `renderMatch` emits `ip daddr … <proto> dport …` for ANY
// declared tuple carrying a port, which is most of the declaration-derived rule set —
// so keying on the shape let `ip daddr 10.66.0.0/16 tcp dport 22 … drop` through, i.e.
// a floor drop that lost its `iifname`, which is exactly the host-level kill switch
// this guard exists to catch, in the rule class where it does the most damage.
// Matching against the endpoints ACTUALLY BOUND at this placement is what makes the
// check structural: only a real published endpoint can satisfy it.
//
// The abstract half (ruleset.Validate's ScopeNone check) and renderScope each refuse an
// unscoped rule today, so neither shape was a live hole — but the text guard's ENTIRE
// reason to exist is being the line that still holds when the abstract half is edited
// wrong, and against that standard keying on a shared text shape was no line at all.
// The property itself: refuse ANY rule in a per-sandbox table that carries no
// narrowing, accept and drop alike (the provider security contract / network substrate measurements ).
func hasPerSandboxNarrowing(code string, endpoints map[publishKey]string) bool {
	if strings.Contains(code, "iifname") || strings.Contains(code, "oifname") {
		return true
	}
	// ScopeHostEndpoint is the one interfaceless per-sandbox narrowing, and it is
	// identified by a BOUND endpoint: the host port is allocated per placement and per
	// publish item, so the address and port together are what make the pair
	// per-sandbox (the address alone is a host address every sandbox's table names).
	for _, ep := range endpoints {
		addr, port, err := splitEndpoint(ep)
		if err != nil {
			continue
		}
		if strings.Contains(code, fmt.Sprintf("daddr %s ", addr)) &&
			strings.Contains(code, fmt.Sprintf(" dport %d ", port)) {
			return true
		}
	}
	return false
}

// isRuleLine reports whether a line is a rule rather than a chain/table/counter
// declaration or a brace.
//
// `counter <name> {}` (a DECLARATION) and `counter name <x>` (a REFERENCE inside a
// rule) both start with the same word, and the difference matters: an un-narrowed
// rule whose only clauses are the counter reference and the verdict —
// `counter name c drop`, i.e. the worst unqualified verdict — looks like a
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
	// A rule is identified by carrying a VERDICT, not by carrying a counter.
	//
	// Keying on `counter name ` let the worst case through: `drop` on its own, or
	// `ip daddr 10.66.0.0/16 drop`, has no counter reference, so guards 3 and 4
	// skipped it entirely — and an un-narrowed drop in a per-sandbox table is exactly
	// the host-level kill switch the narrowed-verdict guard exists to catch. The counter requirement is
	// ruleset.Validate's to enforce; this guard's whole job is to hold when that half
	// is edited wrong, so it must not depend on that half having held.
	for _, v := range ruleVerdicts {
		if strings.Contains(code, v) {
			return true
		}
	}
	return false
}

// ruleVerdicts is every verdict a rendered rule can carry. `masquerade` and the two
// nat verdicts are here because a rule can end in one of those with no filter verdict
// at all, and an un-narrowed DNAT would steer another sandbox's traffic.
var ruleVerdicts = []string{"accept", "drop", "reject", "dnat ", "snat ", "masquerade"}

// stripComment removes a trailing `comment "…"` so the guards read the RULE and not
// its rationale. Without this, a comment explaining why `policy drop` is refused
// would itself trip the no-drop-policy guard — and the fix must not be to stop explaining.
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
