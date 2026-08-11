package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"warren/internal/ruleset"
)

// This file is the INSTALL side of §5.D enforcement: put the rendered rule set
// into the kernel, put the three host-side egress mechanisms in place, and tear
// both down again — all inside the window §2.4 requires.
//
// # The atomicity obligation
//
// The rules must be installed AFTER the netns/veth exist and BEFORE the isolated
// unit runs (§2.4). Any window in which the network is up and the rules are not is
// a fail-open window, and only the side creating the placement has that hook point
// at all (network.md D1 ①). A half-built placement is reclaimed by the same unwind
// every other launch failure uses (§4.4-A).
//
// # One further ordering constraint, from a different direction
//
// The guest-side default route must ALSO be installed before `runsc run`, and that
// is not a preference either: measured, the sentry TAKES THE L3 ADDRESS OFF the
// kernel-side veth when it boots (afterwards `ip addr` in that netns shows only
// `lo`, though the link is still up), so a post-boot `ip route add default via …`
// fails with `Nexthop has invalid gateway` (network.md D5 ⑬). So it lands in the
// SAME pre-launch window as the rules.

// egressSetup installs the host-side egress mechanisms.
//
// EXACTLY THREE, measured necessary AND sufficient for a real runsc guest to
// complete TLS and a `git ls-remote` (network.md D5 ⑤): global `ip_forward`, a
// guest-side default route carrying `dev`, and a masquerade for this /30. The
// masquerade is part of the generated rule set (it is a rule, and only emitted for a
// routed-egress declaration); this function does the other two.
//
// # THE GUEST DEFAULT ROUTE IS INSTALLED UNCONDITIONALLY, INCLUDING UNDER `deny`
//
// That looks backwards and is the opposite: it is what makes the deny ENFORCED rather
// than incidental.
//
// Without a default route, a `deny` sandbox's packet never leaves its netns — it dies
// at `net unreachable` inside gVisor's netstack, the host's rules are never evaluated,
// and every floor counter reads 0. That is precisely the state §2.4 calls a
// coincidence rather than an invariant: guest root can add the route itself with ONE
// command (measured, D5 ⑤ — `ip route add default via <host> dev <veth>`), so an
// engine that relies on its absence has enforced nothing. It is also unassertable —
// "unreachable with a zero counter" is indistinguishable from a bypassed floor
// (D4.1), so the acceptance discipline could not tell the two apart.
//
// With the route present, the guest's packet reaches the host, meets the floor, and is
// REJECTED by a rule that counts it. Same observable outcome for the guest, opposite
// epistemic status: a refusal the engine performs and can prove, rather than an
// accident it inherited. This is also what makes D5 ⑯'s failed assertion assertable
// at last (there, the guest had no default route, so its egress died before reaching
// the floor and the measurement was invalid rather than passing).
//
// THE MEASURED NEGATIVES MATTER AS MUCH, because each is a plausible thing to add:
// `rp_filter` tightened to 1 does NOT affect this (the return route already goes out
// that veth), and `proxy_arp`, `accept_local` and per-interface `forwarding` are ALL
// unnecessary. So the provider adds none of them — an unnecessary sysctl write is a
// host-global change the engine does not own.
func (p *LocalRunsc) egressSetup(h *placementHandle, gatewayIP string, routed bool) error {
	// (1) global ip_forward, only when this declaration can actually route. Note it is
	// NOT an engine guarantee: it is already 1 in this environment by default, which is
	// exactly why D5 ① could measure a sibling handshake succeeding. The engine writes
	// it because a routed-egress declaration cannot work without it, not because it
	// discovered it — and it does NOT write it for a `deny` sandbox, since that would
	// change host-global state for a sandbox that will not use it.
	if routed {
		if err := writeSysctl("net/ipv4/ip_forward", "1"); err != nil {
			return fmt.Errorf("runtime: enable ip_forward (one of the three host-side egress mechanisms, D5 ⑤): %w", err)
		}
	}
	// (2) the guest-side default route, carrying `dev`. WITHOUT `dev` the command
	// returns rc=0 and the route does not work (measured, D5 ⑤) — a silent failure,
	// so the flag is load-bearing rather than explicit-for-clarity.
	//
	// BEFORE `runsc run`, because the sentry takes the L3 address off the kernel-side
	// veth at boot and a post-boot add fails with `Nexthop has invalid gateway`
	// (measured, D5 ⑬).
	//
	// Unconditional — see the doc comment for why the `deny` case needs it MORE, not
	// less.
	vethGuest := vethGuestName(h.vethHost)
	if err := p.ipCmd("-n", h.netns, "route", "add", "default", "via", gatewayIP, "dev", vethGuest); err != nil {
		return fmt.Errorf("runtime: add the guest default route (D5 ⑤: it must carry `dev`, and it "+
			"must be added BEFORE runsc boots — the sentry takes the L3 address off the kernel-side "+
			"veth at boot, after which the add fails with `Nexthop has invalid gateway`, D5 ⑬): %w", err)
	}
	return nil
}

// installRuleSet renders the abstract rule set and installs it as ONE atomic
// `nft -f` file.
//
// The atomicity is a hard requirement in three separate senses:
//   - vs the isolated unit: no fail-open window (§2.4).
//   - vs the kernel: a batch containing one illegal rule rolls back ENTIRELY
//     (measured), so "only part of the rules got installed" is not a state to
//     handle.
//   - vs LIVE CONNECTIONS on a re-install: a two-step delete-then-install kills
//     every live flow across the gap, while one atomic file does not (measured 5/5
//     both ways, network.md D5 ㉒ — see renderRuleSet's doc for the mechanism).
func (p *LocalRunsc) installRuleSet(ctx context.Context, h *placementHandle, spec SteeringSpec) error {
	if spec.Rules.Empty() {
		return nil
	}
	rctx := renderCtx{
		table:            nftTableName(h.sandbox),
		vethHost:         h.vethHost,
		guestIP:          h.guestIP,
		gatewayIP:        h.gatewayIP,
		publishEndpoints: h.publishEndpoints,
	}
	text, err := renderRuleSet(spec.Rules, rctx)
	if err != nil {
		return err
	}
	// The text-half render guards, immediately before install: each property they
	// check is broken by one word of editing with no test firing and no visible
	// difference in `nft list` (see renderGuards).
	if err := renderGuards(text, true, h.publishEndpoints); err != nil {
		return err
	}
	if err := p.nftLoad(ctx, text); err != nil {
		return fmt.Errorf("runtime: install rule set for %s: %w", h.sandbox, err)
	}
	// No `h.nftTable = rctx.table` here: namesFor derives the same name for every
	// handle, so the field is already set and assigning it again would suggest it
	// means "installed", which it does not.
	return nil
}

// InstallSecondHop installs the ENGINE-LEVEL second-hop table (§2.4 invariant 2,
// second half). Called ONCE when the orchestrator starts, not per placement.
//
// Not per-sandbox because the predicate is not per-sandbox: one deployment-supplied
// uid covers every `via[]` in the deployment, and the traffic arrives on no
// interface, so there is nothing sandbox-specific to narrow by (network.md D5 ㉔).
//
// A nil uid means the deployment did not supply one. Nothing is installed — and the
// STALE table from a PREVIOUS run is destroyed, which is the half a bare `return nil`
// silently skipped: an nft table is crash-surviving host state exactly like a netns
// (§2.4), so a run started without the flag would otherwise inherit an engine-level
// `output` table narrowed by a uid it does not know, dropping traffic for a reason
// absent from the running configuration and impossible for an operator to attribute.
//
// With no table installed there is no enforcement of §2.4's second invariant, so
// `via[]` is refused at both ends: kept out of the record at the write points
// (network.ValidateDeployment) and refused at placement for a row admitted by an
// earlier deployment (network.ValidatePlacement).
func (p *LocalRunsc) InstallSecondHop(ctx context.Context, rs ruleset.RuleSet, uid *int) error {
	if uid == nil {
		p.deleteRuleSet(SecondHopTable)
		return nil
	}
	text, err := renderRuleSet(rs, renderCtx{table: SecondHopTable, secondHopUID: uid})
	if err != nil {
		return err
	}
	// perSandbox=false selects the SECOND guard scope: every drop must carry
	// `skuid`, and no accept may. The per-sandbox interface guard would reject this
	// table's rules outright, which is why the guards are scoped by table rather
	// than one guard with an exemption.
	if err := renderGuards(text, false, nil); err != nil {
		return err
	}
	if err := p.nftLoad(ctx, text); err != nil {
		return fmt.Errorf("runtime: install the second-hop table (§2.4 invariant 2): %w", err)
	}
	return nil
}

// nftLoad feeds one file to `nft -f -`. Everything in it applies atomically.
func (p *LocalRunsc) nftLoad(ctx context.Context, text string) error {
	argv := []string{"nft", "-f", "-"}
	if p.Sudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Surface the real output AND the file, numbered: nft reports a parse error
		// against a line number, and a desynchronized parse reports it several chains
		// from the actual cause (measured, network.md D5 ㉔), so the numbered file is
		// what makes the message usable at all.
		return fmt.Errorf("%v: %w: %s\n--- rule set ---\n%s", argv, err, out, numberLines(text))
	}
	return nil
}

// deleteRuleSet removes a sandbox's table. Idempotent and best-effort, like every
// other step of teardown.
//
// `destroy table` rather than `delete table`: `delete` on a nonexistent table
// returns rc=1, and teardown runs on half-built placements where the table may
// never have been installed.
func (p *LocalRunsc) deleteRuleSet(table string) {
	if table == "" {
		return
	}
	_ = p.run(context.Background(), []string{"nft", "destroy", "table", "inet", table})
}

// enumerateRuleSetTables lists engine per-sandbox tables present on this host, by
// name prefix — the §4.4-A reconciliation face of the rule set (§2.4: a rule set is
// crash-surviving host state exactly like a netns, so reconciliation must rebuild
// or tear it down, and it cannot be added afterwards).
func (p *LocalRunsc) enumerateRuleSetTables(ctx context.Context) ([]string, error) {
	out, err := p.runOut(ctx, []string{"nft", "list", "tables", "inet"})
	if err != nil {
		// No tables at all (or nft absent) is not an error for enumeration: an empty
		// host has no orphans. A REAL failure here would be indistinguishable from
		// "nothing is installed", and the safe direction is to reclaim nothing.
		return nil, nil
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		// `table inet <name>`
		if len(f) == 3 && f[0] == "table" && f[1] == "inet" && strings.HasPrefix(f[2], nftTablePrefix) {
			found = append(found, f[2])
		}
	}
	return found, nil
}

// CountRuleSetTables reports how many engine per-sandbox rule-set tables exist —
// the leak assertion's read side, shaped like the netns count the substrate suites
// already use.
func (p *LocalRunsc) CountRuleSetTables(ctx context.Context) (int, error) {
	tables, err := p.enumerateRuleSetTables(ctx)
	return len(tables), err
}

// sandboxOfTable recovers the SandboxID from a table name. Reversible by
// construction, the same convention the netns name uses (§4.4-A SandboxID
// labeling).
func sandboxOfTable(table string) (string, bool) {
	if table == SecondHopTable {
		// The engine-level table is not a placement resource and is never an orphan:
		// it belongs to the process, not to a sandbox.
		return "", false
	}
	return strings.CutPrefix(table, nftTablePrefix)
}

// writeSysctl writes one sysctl through /proc. Idempotent.
func writeSysctl(path, val string) error {
	full := "/proc/sys/" + path
	cur, err := os.ReadFile(full)
	if err == nil && strings.TrimSpace(string(cur)) == val {
		return nil // already set; do not write host-global state needlessly
	}
	return os.WriteFile(full, []byte(val), 0o644)
}

// allocPublishPort hands out a host port for a published guest port.
//
// The engine chooses the host port rather than reusing the guest port, and that is
// forced: two sandboxes may both publish port 8080, and the host side must
// distinguish them. It is a placement product, rebound every resume — which is
// exactly why §6.1 reports it as `host_endpoint` only while a placement exists and
// warns against caching it (§3.3).
// Returns 0 when the range is EXHAUSTED, and that bound is required rather than
// cautious: without it the scan spins forever once every port is held, and
// "publishing thousands of ports" is a reachable deployment shape rather than a
// hypothetical. A caller turns the 0 into a launch failure, which unwinds the
// half-built placement (§4.4-A) — the same disposition as any other placement
// resource that cannot be materialized.
func (p *LocalRunsc) allocPublishPort() uint16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tried := 0; tried < int(publishPortSpan); tried++ {
		if p.pubSeq < publishPortBase || p.pubSeq >= publishPortBase+publishPortSpan {
			p.pubSeq = publishPortBase
		}
		port := p.pubSeq
		p.pubSeq++
		if !p.pubInUse[port] {
			p.pubInUse[port] = true
			return port
		}
	}
	return 0
}

func (p *LocalRunsc) freePublishPort(port uint16) {
	if port == 0 {
		return
	}
	p.mu.Lock()
	delete(p.pubInUse, port)
	p.mu.Unlock()
}

func (p *LocalRunsc) markPublishPortInUse(port uint16) {
	if port == 0 {
		return
	}
	p.mu.Lock()
	p.pubInUse[port] = true
	p.mu.Unlock()
}

// The host port range published endpoints are drawn from. Above the ephemeral
// range's usual start so a published port does not collide with an outgoing
// connection's source port.
const (
	publishPortBase uint16 = 21000
	publishPortSpan uint16 = 4000
)

// numberLines prefixes each line with its number, so an nft parse error's line
// number can be located. Worth the few lines: a chain-name collision reports a
// bogus syntax error against a LATER chain, so the reported number is often not
// where the mistake is, and having the whole numbered file in the error is what
// makes that diagnosable at all (network.md D5 ㉔).
func numberLines(s string) string {
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		b.WriteString(strconv.Itoa(i+1) + "\t" + line + "\n")
	}
	return b.String()
}

// ruleHits reads this table's named counters, keyed by the abstract counter name §5.D
// minted (see runtime.Provider.RuleHits).
//
// A MISSING KEY IS NOT A ZERO, and that distinction is the whole reason this returns a
// map rather than a slice of values: "the rule is installed and matched nothing" and "the
// rule is not there at all" are different facts, and §2.4's acceptance discipline rests on
// telling them apart — a bypassed rule's only symptom is a counter reading 0, so
// manufacturing a zero for an absent counter destroys the one signal §5.D has.
//
// Read-only: it lists, never loads. A failure returns the error rather than an empty map,
// so a caller cannot mistake "could not read" for "everything is at zero".
func (p *LocalRunsc) ruleHits(ctx context.Context, table string) (map[string]uint64, error) {
	if table == "" {
		return nil, fmt.Errorf("runtime: no rule-set table for this placement")
	}
	out, err := p.runOut(ctx, []string{"nft", "-j", "list", "counters", "table", "inet", table})
	if err != nil {
		return nil, fmt.Errorf("runtime: read rule counters for %s: %w", table, err)
	}
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("runtime: parse rule counters for %s: %w", table, err)
	}
	hits := make(map[string]uint64, len(doc.Nftables))
	for _, o := range doc.Nftables {
		if o.Counter != nil {
			hits[o.Counter.Name] = o.Counter.Packets
		}
	}
	return hits, nil
}
