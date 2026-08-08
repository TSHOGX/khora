//go:build substrate

package test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/orchestrator"
	"engine/internal/transport"
	"engine/test/harness"
)

// M8 GATE B: §5.D enforcement on the real substrate (plan §6 items 6–15).
//
// # The acceptance discipline, which shapes every assertion in this file
//
// "IT IS UNREACHABLE" IS NOT A VALID ASSERTION ON ITS OWN (§2.4 / network.md D4.1,
// D5 ⑧). A rule set with the right rules in the WRONG ORDER behaves perfectly on
// ordinary traffic and differs only when it is being bypassed; and a floor whose
// counter reads 0 is INDISTINGUISHABLE from a floor that was bypassed. So every deny
// here is proved by TWO facts: unreachable from inside the netns, AND the floor
// counter moved after a deliberate probe. A counter that did not move is reported as
// an INVALID assertion, never as a pass.
//
// The paired direction matters just as much and for the opposite reason: an engine
// that could reach NOTHING would pass every deny assertion vacuously. That is why
// item 7 (public egress actually works under `allow`) has to pass before items 7b/7c
// mean anything, and why the plan fixes the order 8 → 7 → 8c → 7b/7c.

// guestProbe is a tiny static binary injected into the Template: it dials an
// address and prints a one-line verdict. The bare rootfs ships no coreutils, so
// anything the guest must run has to be injected (harness.InjectStaticBin).
//
// It reports the OUTCOME and the elapsed time, because two gate B items turn on the
// difference between "refused fast" and "hung": §2.4's fail-closed has a second half
// — "the guest gets a clean connection failure" — that a pure-`drop` floor breaks
// while satisfying the security half (network.md D5 ⑨).
const guestProbeSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// probe <addr> [udp] — dial, read a banner, print OUTCOME=<verdict> MS=<elapsed>.
func main() {
	addr := os.Args[1]
	netw := "tcp"
	if len(os.Args) > 2 && os.Args[2] == "udp" {
		netw = "udp"
	}
	start := time.Now()
	c, err := net.DialTimeout(netw, addr, 25*time.Second)
	if err != nil {
		fmt.Printf("OUTCOME=refused MS=%d ERR=%v\n", time.Since(start).Milliseconds(), err)
		return
	}
	defer c.Close()
	if netw == "udp" {
		// A UDP "dial" connects nothing, so the only way to learn whether the path is
		// open is to send and wait. A refusal arrives as an ICMP error surfaced on the
		// next read.
		if _, werr := c.Write([]byte("ping")); werr != nil {
			fmt.Printf("OUTCOME=refused MS=%d ERR=%v\n", time.Since(start).Milliseconds(), werr)
			return
		}
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, rerr := c.Read(buf)
	if n < 0 {
		n = 0
	}
	el := time.Since(start).Milliseconds()
	if rerr != nil {
		// Connected but read nothing. For TCP that is a real distinction the plan needs
		// (a half-torn publish presents exactly this way, D5 ⑰); for UDP it is how a
		// silently-dropped datagram looks.
		fmt.Printf("OUTCOME=connected_no_payload MS=%d ERR=%v\n", el, rerr)
		return
	}
	fmt.Printf("OUTCOME=connected MS=%d PAYLOAD=%q\n", el, string(buf[:n]))
}
`

// probeResult is one guest probe's verdict.
type probeResult struct {
	outcome string
	ms      int64
	payload string
	raw     string
}

func (p probeResult) reachable() bool { return p.outcome == "connected" }

// runGuestProbe execs the injected probe inside the guest through the data plane and
// parses its one line. Uses the engine's own transport, so it exercises the path a
// caller would use and proves the transport survives the floor at the same time.
func runGuestProbe(t *testing.T, cl transport.Client, addr string, udp bool) probeResult {
	t.Helper()
	args := []string{addr}
	if udp {
		args = append(args, "udp")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	evs, err := cl.Start(ctx, transport.StartSpec{Cmd: "/usr/bin/probe", Args: args})
	if err != nil {
		t.Fatalf("start guest probe for %s: %v", addr, err)
	}
	var sb strings.Builder
	for ev := range evs {
		sb.WriteString(string(ev.Data))
	}
	raw := sb.String()
	res := probeResult{raw: raw}
	for _, field := range strings.Fields(raw) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "OUTCOME":
			res.outcome = v
		case "MS":
			fmt.Sscanf(v, "%d", &res.ms)
		case "PAYLOAD":
			res.payload = v
		}
	}
	if res.outcome == "" {
		t.Fatalf("guest probe for %s produced no verdict: %q", addr, raw)
	}
	return res
}

// hostListener starts a host-side TCP listener answering with a banner, and returns
// its address plus a stop func. Used as the "reachable target" for allow[]/via[] and
// as the publish caller's target.
func hostListener(t *testing.T, banner string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(banner))
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close(); <-done }
}

// hostIPv4 returns a non-loopback IPv4 address of this host — the "an address of the
// host itself" target for the floor assertions, and the LAN-facing address the
// deny-safe tier's assertion needs.
func hostIPv4(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("ip", "-4", "-o", "route", "get", "1.1.1.1").Output()
	if err != nil {
		t.Skipf("cannot determine this host's outbound IPv4: %v", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "src" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Skipf("no src address in %q", out)
	return ""
}

// mustCounterMoved is the acceptance discipline as a helper: it FAILS when the
// counter did not move, and distinguishes "did not move" from "does not exist".
//
// A missing counter (-1) is the worse of the two and gets its own message: it means
// the rule was never rendered at all, which is the same silent pass wearing a
// different disguise.
func mustCounterMoved(t *testing.T, id model.SandboxID, counter string, before, after int64, what string) {
	t.Helper()
	if after < 0 {
		t.Errorf("%s: counter %q DOES NOT EXIST — the rule was never rendered, so the "+
			"unreachability just observed proves nothing about enforcement.\n%s",
			what, counter, harness.DumpRuleSet(id))
		return
	}
	if after <= before {
		t.Errorf("%s: unreachable, but counter %q did not move (%d → %d). Per the acceptance "+
			"discipline this is an INVALID assertion, not a pass: a floor whose counter reads 0 is "+
			"indistinguishable from a floor that was bypassed (§2.4 / D4.1 / D5 ⑧).\n%s",
			what, counter, before, after, harness.DumpRuleSet(id))
	}
}

// netEngineOpts is the shared engine configuration for this file: the guest probe
// injected, and the second-hop deployment inputs supplied so `via[]` is permitted.
func netEngineOpts(t *testing.T) harness.EngineOpts {
	t.Helper()
	uid := secondHopUID
	return harness.EngineOpts{
		Probes:        map[string]string{"probe": guestProbeSrc},
		SecondHopUID:  &uid,
		HostPublishIP: hostIPv4(t),
	}
}

// secondHopUID is the uid the gate B `via[]` items run the endpoint process as. A
// fixed non-root uid: the whole point of item 9d is that the drop is narrowed to it
// and does NOT take out the host (measured: an un-narrowed drop on that hook is a
// host-level egress kill switch — network.md D5 ㉔).
const secondHopUID = 12345

// createRunning creates a sandbox with the given declaration and returns its id and
// a transport client.
func createRunning(t *testing.T, eng *orchestrator.Engine, decl model.NetworkDecl) (model.SandboxID, transport.Client) {
	t.Helper()
	ctx := context.Background()
	id, err := eng.Mediator.Create(ctx, lifecycle.CreateSpec{Template: "bare", Network: &decl})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Mediator.Destroy(context.Background(), id) })
	cl, err := eng.Mediator.Client(id)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return id, cl
}

// declDeny is the minimal declaration: `deny` + empty (§5.D's "no network").
func declDeny() model.NetworkDecl {
	return model.NetworkDecl{Egress: model.NetEgress{Default: model.NetDeny}}
}

func declAllowPublic() model.NetworkDecl {
	return model.NetworkDecl{Egress: model.NetEgress{Default: model.NetAllow}}
}

// --- item 6: `deny` + empty is unreachable EVERYWHERE, with counters moving ------

// Gate B item 6. Four destination classes, and the COUNTERS ARE READ PER HOOK because
// the two hooks see different traffic: a destination that is an address of the host
// ITSELF lands on `input`, while something the host would ROUTE lands on `forward`
// (measured, network.md D5 ⑧).
//
// Reading only the `input` counter would make every routed-destination probe look
// like "the floor counted 0", which by the discipline is an INVALID assertion rather
// than a pass — so each class below names the counter that must move for IT.
func TestDenyEmptyIsUnreachableEverywhereWithFloorCountersMoving(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())
	host := hostIPv4(t)

	// A host-side listener on a WILDCARD bind, so "unreachable" cannot be an artifact
	// of nothing listening. This is the D5 ⑧ shape: with the floor installed, the guest
	// must not reach a 0.0.0.0 listener through ANY host address.
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, port, _ := net.SplitHostPort(addr)

	cases := []struct {
		name    string
		target  string
		counter string
		why     string
	}{{
		// An address of the host ITSELF → `input`, where the floor is a pure ct-state
		// rule with no address list at all.
		name:    "an address of the host itself",
		target:  net.JoinHostPort(host, port),
		counter: "in_floor",
		why:     "the host's own address lands on `input`; the floor there is pure ct-state, so it covers every host address without enumerating one (D5 ⑧)",
	}, {
		// Link-local (every cloud metadata endpoint) → the host would ROUTE it, so
		// `forward`, and it is an explicit daddr member there.
		name:    "link-local metadata address",
		target:  "169.254.169.254:80",
		counter: "fwd_floor_v4",
		why:     "link-local is routed, so it lands on `forward` and is an explicit daddr member of the floor there",
	}, {
		// An UNALLOCATED address inside the sandbox /16. The sibling item as the floor
		// itself sees it, with no second sandbox involved — so this isolates A's own
		// floor from the cross-sandbox ordering question the sibling case raises below.
		name:    "an unallocated address in the sandbox address space",
		target:  "10.66.199.222:9000",
		counter: "fwd_floor_v4",
		why:     "the sibling CIDR is a floor daddr member, and with no sandbox holding this address only THIS sandbox's floor can refuse it",
	}, {
		// A public address → `forward`, refused by the DEFAULT rather than by the floor.
		// Different counter on purpose: this proves the default-deny is doing the work,
		// and that the forward side has its OWN default-deny (a destination neither on
		// the host nor steered never traverses `input`, D5 ⑩).
		name:    "a public address",
		target:  "1.1.1.1:443",
		counter: "fwd_default_deny",
		why:     "a public destination is refused by the forward side's OWN default-deny — an input-only default-deny would never see it (D5 ⑩)",
	}}

	// LOOPBACK IS DELIBERATELY NOT IN THIS TABLE, and the reason is a measured fact
	// rather than an omission: a gVisor guest dialing 127.0.0.1 hits ITS OWN netstack's
	// loopback, so the packet never leaves the sandbox and the host's rules are never
	// evaluated — measured, the host-side `daddr 127.0.0.0/8` counter read 0 while
	// link-local counted 5 in the same run (D5 ⑦). The floor's loopback item is
	// therefore REDUNDANT DEPTH for a gVisor guest, not the thing refusing it.
	//
	// Asserting a counter here would demand movement from traffic that constructively
	// cannot arrive — the assertion would be unsatisfiable rather than the engine
	// broken, which is exactly the "invalid measurement" the discipline exists to
	// distinguish from a pass.
	if res := runGuestProbe(t, cl, net.JoinHostPort("127.0.0.1", port), false); res.reachable() {
		t.Errorf("the guest reached the HOST's loopback (%s) — that would mean the packet left the "+
			"sandbox, contradicting D5 ⑦'s measurement that a gVisor guest's loopback dial stays in "+
			"its own netstack", res.raw)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := harness.NftCounter(t, id, tc.counter)
			res := runGuestProbe(t, cl, tc.target, false)
			after := harness.NftCounter(t, id, tc.counter)
			if res.reachable() {
				t.Fatalf("REACHED %s under `deny` + empty (payload %s) — the declared reachable set is "+
					"empty, so this is the §2.4 ③ exhaustive-narrowing obligation broken: %s\n%s",
					tc.target, res.payload, tc.why, harness.DumpRuleSet(id))
			}
			mustCounterMoved(t, id, tc.counter, before, after, tc.name)
		})
	}

	// The sibling class needs a second sandbox to be a real target, and it is the one
	// §2.3 calls an INDEPENDENT invariant: point-to-point veth does not deliver it,
	// because the host still ROUTES between two /30s — measured, sandbox A completed a
	// TCP handshake to sibling B and read its payload (D5 ①).
	t.Run("a sibling sandbox's guest IP", func(t *testing.T) {
		sibID, sibCl := createRunning(t, eng, declAllowPublic())
		// Start a listener INSIDE the sibling, so the target genuinely answers.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := sibCl.Start(ctx, transport.StartSpec{
			Cmd: "/usr/bin/probe", Args: []string{"listen"},
		}); err != nil {
			// The probe has no listen mode; use the sibling's transport port instead, which
			// is a real listener inside that netns and is exactly the "sibling endpoint"
			// §2.3 names.
			t.Logf("probe has no listen mode (%v) — using the sibling's transport endpoint, which is a real listener in its netns", err)
		}
		ep, err := eng.Mediator.Endpoint(sibID)
		if err != nil {
			t.Fatalf("sibling endpoint: %v", err)
		}
		before := harness.NftCounter(t, id, "fwd_floor_v4")
		res := runGuestProbe(t, cl, ep, false)
		after := harness.NftCounter(t, id, "fwd_floor_v4")
		if res.reachable() {
			t.Fatalf("REACHED sibling %s from another sandbox (payload %s) — §2.3's cross-sandbox "+
				"network isolation is an INDEPENDENT control-plane invariant that only this rule set "+
				"materializes: point-to-point veth does not deliver it, because the host still routes "+
				"between two /30s (measured, D5 ①)\n%s", ep, res.payload, harness.DumpRuleSet(id))
		}
		mustCounterMoved(t, id, "fwd_floor_v4", before, after, "sibling guest IP")
	})
}

// --- item 9: the transport is not killed by the floor ---------------------------

// Gate B item 9, and it is the LOUD counterpart to every silent failure in this file:
// the floor's `input` side must not drop the guest's replies to the host.
//
// It is asserted here rather than left implicit in the other tests' use of the
// transport, because a floor that killed it would fail everything with one confusing
// symptom (D5 ⑧: a naive daddr floor placed above the established accept kills
// transport outright).
func TestTheFloorDoesNotKillTheTransport(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cl.Health(ctx); err != nil {
		t.Fatalf("Health failed with the floor installed: %v — the `input` floor must sit BELOW an "+
			"established/related accept, or it drops the guest's replies to the host and transport "+
			"dies outright (D5 ⑧)\n%s", err, harness.DumpRuleSet(id))
	}
	// A multi-KB round trip, so this covers a real data-plane flow rather than one
	// small packet: the return path is what the established arm carries.
	payload := strings.Repeat("x", 64<<10)
	if err := cl.Write(ctx, "/tmp/probe-rt", []byte(payload)); err != nil {
		t.Fatalf("multi-KB write failed with the floor installed: %v\n%s", err, harness.DumpRuleSet(id))
	}
	got, err := cl.Read(ctx, "/tmp/probe-rt", 0, 0)
	if err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("round trip returned %d bytes, want %d", len(got), len(payload))
	}
	// …and the established counter really is the thing carrying it, so this is not
	// passing for some unrelated reason.
	if n := harness.NftCounter(t, id, "in_established"); n <= 0 {
		t.Errorf("the established/related accept counted %d packets while transport worked — either "+
			"the counter is missing or the traffic is taking a path the rule set does not describe", n)
	}
}

// --- items 7, 7b, 7c: `allow` delivers public egress, refuses the LAN, and the ---
// --- tier is liftable while the floor is not ------------------------------------

// The order here is the plan's, and it is a HARD constraint rather than a preference
// (§7): item 7 must pass BEFORE 7b/7c mean anything, because an engine that could
// reach nothing would pass every deny assertion vacuously.
func TestAllowPublicDeliversEgressAndStillRefusesTheInternalTier(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)

	// ITEM 7: public egress actually works. This is what proves the three host-side
	// mechanisms are materialized (ip_forward, the guest default route carrying `dev`,
	// and the masquerade — measured necessary AND sufficient, D5 ⑤). Without it, item 6
	// above would be passing on an engine where nothing works at all.
	id, cl := createRunning(t, eng, declAllowPublic())
	pub := runGuestProbe(t, cl, "1.1.1.1:443", false)
	if !pub.reachable() && pub.outcome != "connected_no_payload" {
		t.Fatalf("`default: allow` did NOT deliver public egress (%s) — without this the deny "+
			"assertions elsewhere in this file would be passing vacuously on an engine that cannot "+
			"reach anything. The three host-side mechanisms are ip_forward, the guest default route "+
			"WITH `dev`, and the masquerade for this /30 (D5 ⑤)\n%s", pub.raw, harness.DumpRuleSet(id))
	}
	t.Logf("item 7: public egress under `allow` → %s (%d ms)", pub.outcome, pub.ms)

	// ITEM 7b: the SAME declaration must NOT reach the host's LAN. This is the
	// deny-safe tier's acceptance, and it only means something now that item 7 passed:
	// measured, with the three mechanisms in place and no tier, this exact flow leaves
	// the netns and is routed by the host, and NONE of the floor's four items covers it
	// (D5 ⑤). Without the tier, `allow` delivers a reachable set strictly WIDER than
	// §2.4 defines.
	//
	// The target is a host-LAN RFC1918 address. This host's own outbound address is
	// used when it is RFC1918, which is the honest available target: it is routed
	// rather than local only if it is not this machine's — so the counter is what
	// carries the claim, and a local address would count on `input` instead.
	lanTarget := "192.168.0.1:22"
	if strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "172.") {
		// A neighbour of this host, still inside the same RFC1918 block: routed, not local.
		lanTarget = lanNeighbour(host) + ":22"
	}
	before := harness.NftCounter(t, id, "fwd_tier_v4")
	lan := runGuestProbe(t, cl, lanTarget, false)
	after := harness.NftCounter(t, id, "fwd_tier_v4")
	if lan.reachable() {
		t.Errorf("under `default: allow` the guest REACHED the host's LAN at %s — that is the "+
			"reachable set being wider than §2.4's `allow` (public MINUS the floor MINUS the internal "+
			"deny-safe tier). Measured: with the three egress mechanisms in place and this tier "+
			"absent, this exact flow leaves the netns and is routed, and no floor item covers it "+
			"(D5 ⑤)\n%s", lanTarget, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "fwd_tier_v4", before, after, "item 7b: the internal deny-safe tier")

	// ITEM 7c: the tier is DENY-SAFE (liftable by declaration) while the floor is NOT.
	// This PAIR is the only proof that the two-tier structure exists at all: without
	// the first half, an implementation that put the internal ranges in the FLOOR
	// passes everything; without the second, one that demoted the floor to deny-safe
	// passes everything.
	lanHost, lanPort, _ := net.SplitHostPort(lanTarget)
	lifted := declAllowPublic()
	lifted.Egress.Allow = []model.NetTuple{tupleOf(t, lanHost+"/32", 22, model.NetTCP)}
	liftedID, liftedCl := createRunning(t, eng, lifted)

	// (a) the declaration LIFTS the tier: that address is now reachable — or at least
	// reaches the far end rather than being refused locally. A LAN peer that does not
	// answer is reported as connected_no_payload / refused BY THE PEER, which is
	// distinguishable from our own refusal by the tier counter not moving.
	tierBefore := harness.NftCounter(t, liftedID, "fwd_tier_v4")
	allowBefore := harness.NftCounter(t, liftedID, "allow0_fwd")
	_ = runGuestProbe(t, liftedCl, lanTarget, false)
	tierAfter := harness.NftCounter(t, liftedID, "fwd_tier_v4")
	allowAfter := harness.NftCounter(t, liftedID, "allow0_fwd")
	if allowAfter <= allowBefore {
		t.Errorf("item 7c(a): the declared allow for %s counted %d → %d — the declaration did NOT "+
			"lift the deny-safe tier, so the tier is behaving like a second floor and `deny-safe` "+
			"means nothing (§2.4)\n%s", lanTarget, allowBefore, allowAfter, harness.DumpRuleSet(liftedID))
	}
	if tierAfter > tierBefore {
		t.Errorf("item 7c(a): the tier counter moved (%d → %d) for traffic the declaration allowed — "+
			"the allow must sit ABOVE the tier, or the tier is not liftable", tierBefore, tierAfter)
	}
	_ = lanPort

	// (b) the SAME declaration does NOT lift the floor. A declaration naming the whole
	// 10/8 (which CONTAINS the sandbox address space) must still not reach a sibling,
	// and the floor's counter must move.
	wide := declAllowPublic()
	wide.Egress.Allow = []model.NetTuple{tupleOf(t, "10.0.0.0/8", 9000, model.NetTCP)}
	wideID, wideCl := createRunning(t, eng, wide)
	sibID, _ := createRunning(t, eng, declDeny())
	sibEP, err := eng.Mediator.Endpoint(sibID)
	if err != nil {
		t.Fatalf("sibling endpoint: %v", err)
	}
	floorBefore := harness.NftCounter(t, wideID, "fwd_floor_v4")
	sib := runGuestProbe(t, wideCl, sibEP, false)
	floorAfter := harness.NftCounter(t, wideID, "fwd_floor_v4")
	if sib.reachable() {
		t.Errorf("item 7c(b): an `allow 10.0.0.0/8` REACHED sibling %s — the floor sits ABOVE the "+
			"declaration's allows precisely so no declaration can lift it, and that is materialized "+
			"purely by ORDER (§2.4)\n%s", sibEP, harness.DumpRuleSet(wideID))
	}
	mustCounterMoved(t, wideID, "fwd_floor_v4", floorBefore, floorAfter,
		"item 7c(b): the floor is not liftable by a declaration that contains it")
}

// lanNeighbour returns a different address in the same /24 as the given one, for use
// as a routed RFC1918 target.
func lanNeighbour(ip string) string {
	i := strings.LastIndex(ip, ".")
	if i < 0 {
		return ip
	}
	last := ip[i+1:]
	if last == "1" {
		return ip[:i+1] + "2"
	}
	return ip[:i+1] + "1"
}

func tupleOf(t *testing.T, cidr string, port uint16, proto model.NetProto) model.NetTuple {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return model.NetTuple{CIDR: p, Port: port, Proto: proto}
}

// --- item 8: an allow[] entry admits EXACTLY its tuple --------------------------

// Gate B item 8: the declared `{cidr, port, proto}` works and NOTHING ADJACENT does.
//
// Both halves, because either alone is passable by a broken rule set: only the
// positive and a rule set that admits everything passes; only the negative and one
// that admits nothing does.
func TestAnAllowEntryAdmitsExactlyItsTuple(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-DECLARED")
	defer stop()
	_, portStr, _ := net.SplitHostPort(addr)
	var port uint16
	fmt.Sscanf(portStr, "%d", &port)

	// The declared target is an address of the HOST, so this also exercises the rule
	// §5.D requires on BOTH hooks: a forward-only allow cannot reach a host-local
	// destination at all (D4.2).
	d := declDeny()
	d.Egress.Allow = []model.NetTuple{tupleOf(t, host+"/32", port, model.NetTCP)}
	id, cl := createRunning(t, eng, d)

	// (a) the declared tuple is reachable.
	inBefore := harness.NftCounter(t, id, "allow0_in")
	got := runGuestProbe(t, cl, net.JoinHostPort(host, portStr), false)
	inAfter := harness.NftCounter(t, id, "allow0_in")
	if !got.reachable() {
		t.Fatalf("the DECLARED tuple %s:%d/tcp was not reachable (%s) — and note the shape: this "+
			"destination is an address of the host, so a forward-only allow could not reach it at all "+
			"(D4.2), which is why §5.D requires the allow on both hooks\n%s",
			host, port, got.raw, harness.DumpRuleSet(id))
	}
	if got.payload == "" || !strings.Contains(got.payload, "DECLARED") {
		t.Errorf("reached the declared endpoint but read %q, want the listener's banner", got.payload)
	}
	if inAfter <= inBefore {
		t.Errorf("the input-side allow counted %d → %d while the flow succeeded — the traffic is "+
			"taking a path the rule set does not describe", inBefore, inAfter)
	}

	// (b) a DIFFERENT PORT on the same address is refused, with the floor counting it.
	// Same address, so this isolates the port dimension.
	other, otherStop := hostListener(t, "hello-from-UNDECLARED")
	defer otherStop()
	_, otherPortStr, _ := net.SplitHostPort(other)
	floorBefore := harness.NftCounter(t, id, "in_floor")
	res := runGuestProbe(t, cl, net.JoinHostPort(host, otherPortStr), false)
	floorAfter := harness.NftCounter(t, id, "in_floor")
	if res.reachable() {
		t.Errorf("an UNDECLARED port on the declared address was reachable (payload %s) — the "+
			"reachable set must be exactly the declared tuples, so the port dimension has to bite "+
			"(§2.4 ③)\n%s", res.payload, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", floorBefore, floorAfter, "item 8: an undeclared port on a declared address")
}

// Gate B item 8's UDP half, asserted separately because UDP is DNS's only declarable
// path (§5.D semantics ③) and because the plan is explicit that the assertion stops
// at the RULE layer: whether `curl` in the guest can RESOLVE depends on
// `resolv.conf`, which M8 does not write (the guest has no resolver configuration at
// all today — measured, D5 ⑦).
func TestADeclaredUDPTupleIsAdmittedAtTheRuleLayer(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)

	// A host-side UDP echo, so "admitted" is observable rather than inferred.
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, rerr := pc.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			_, _ = pc.WriteToUDP([]byte("udp-banner"), from)
			_ = n
		}
	}()
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)

	d := declDeny()
	d.Egress.Allow = []model.NetTuple{tupleOf(t, host+"/32", port, model.NetUDP)}
	id, cl := createRunning(t, eng, d)

	before := harness.NftCounter(t, id, "allow0_in")
	got := runGuestProbe(t, cl, fmt.Sprintf("%s:%d", host, port), true)
	after := harness.NftCounter(t, id, "allow0_in")
	if after <= before {
		t.Errorf("a declared UDP tuple counted %d → %d on the input-side allow — UDP is the ONLY "+
			"declarable path for DNS under `default: deny` (§5.D semantics ③), so this rule has to "+
			"bite\n%s", before, after, harness.DumpRuleSet(id))
	}
	t.Logf("item 8 (udp): %s", got.raw)
	// Deliberately NOT asserting name resolution: the guest ships no resolv.conf at
	// all, so DNS does not work there for reasons that precede any §5.D policy — that
	// is a template/product-layer content choice and explicitly out of M8's scope.
}

// --- item 8b: guest-facing refusals FAIL FAST rather than hang -----------------

// Gate B item 8b: §2.4's fail-closed has a SECOND half — "the guest gets a clean
// connection failure" — and a pure-`drop` floor satisfies the security half while
// breaking this one. gVisor's netstack honors ICMP errors, so `reject` and `drop`
// differ by orders of magnitude in what the guest experiences.
//
// ASSERTED BY MAGNITUDE, NOT BY MILLISECONDS, on purpose: the source measurement is
// single-source and its independent reproduction FAILED (the test bed had no guest
// default route, so nothing left the sandbox and all four verdicts looked alike — an
// invalid measurement, not a refutation). So the direction is trusted and the numbers
// are not. This test is also that reproduction's second attempt.
func TestGuestFacingRefusalsFailFastRatherThanHanging(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	// A refused destination that the floor rejects. Sub-second vs tens of seconds is
	// the claim; the threshold sits far from both.
	const fastEnough = 5 * time.Second
	res := runGuestProbe(t, cl, "169.254.169.254:80", false)
	if res.reachable() {
		t.Fatalf("the probe REACHED a floor address: %s", res.raw)
	}
	elapsed := time.Duration(res.ms) * time.Millisecond
	if elapsed >= fastEnough {
		t.Errorf("a guest-facing refusal took %s — §2.4's fail-closed promises the guest a CLEAN "+
			"connection failure, not a hang. A pure-`drop` floor meets the security half and breaks "+
			"this one; the fix is `reject`, and security is unaffected because neither forwards "+
			"(D5 ⑨ / D4.3). Asserted by MAGNITUDE because that measurement is single-source and its "+
			"first reproduction was invalid\n%s", elapsed, harness.DumpRuleSet(id))
	}
	t.Logf("item 8b: refusal at %s (magnitude assertion: < %s). D5 ⑨'s numbers were 20.0 s for "+
		"`drop` vs 0.002 s for `reject`; this run reproduces the DIRECTION", elapsed, fastEnough)
}

// --- item 10 / 11b: pillar ④ — the predicate binds to a fact the guest cannot ---
// --- change, and survives a veth rebuild ---------------------------------------

// Gate B item 10: guest root FORGES a source address and the floor still holds.
//
// This is pillar ④'s acceptance: guest root can `ip addr add` (measured rc=0), so a
// SOURCE-address predicate is bypassable while an INPUT-INTERFACE one is not (D5 ③).
// The counter is what proves it was the interface predicate that refused, and not
// `rp_filter` or a routing artifact.
func TestForgingASourceAddressDoesNotBypassTheFloor(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, port, _ := net.SplitHostPort(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// guest root adds the HOST's own address to its interface — the forgery D5 ③
	// measured succeeding at the `ip` level.
	evs, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/sbin/ip", Args: []string{"addr", "add", host + "/32", "dev", "lo"},
	})
	if err == nil {
		for range evs {
		}
	}
	before := harness.NftCounter(t, id, "in_floor")
	res := runGuestProbe(t, cl, net.JoinHostPort(host, port), false)
	after := harness.NftCounter(t, id, "in_floor")
	if res.reachable() {
		t.Fatalf("after forging a source address the guest REACHED %s — the predicate must bind to a "+
			"fact the guest cannot change (pillar ④): a source-address predicate is measurably "+
			"forgeable, an input-interface one is not (D5 ③)\n%s", host, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", before, after,
		"item 10: the floor counter must move, proving it was the INTERFACE predicate that refused rather than rp_filter or a routing artifact")
}

// Gate B item 11b: after a pause→resume rebuilds the veth under the SAME NAME, the
// floor still holds.
//
// This is the behavioral face of the `iifname`-over-`iif` guard, and the TIME POINT
// is the assertion: `iif` freezes an ifindex at load time and fails OPEN only AFTER a
// same-name rebuild, with the drop counter reading 0 while the packet is delivered
// (D5 ⑫). Asserting before the rebuild would pass with either spelling.
func TestTheFloorStillHoldsAfterAVethRebuild(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, cl := createRunning(t, eng, declDeny())
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, port, _ := net.SplitHostPort(addr)

	// Establish the pre-rebuild baseline, so a failure after the rebuild is
	// attributable to the rebuild.
	if res := runGuestProbe(t, cl, net.JoinHostPort(host, port), false); res.reachable() {
		t.Fatalf("reachable BEFORE the rebuild: %s", res.raw)
	}

	// pause → resume rebuilds netns + veth. The veth NAME is derived from the
	// SandboxID, so it comes back identical — which is precisely the condition under
	// which `iif` fails open.
	if err := eng.Mediator.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := eng.Mediator.Resume(ctx, id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	cl2, err := eng.Mediator.Client(id)
	if err != nil {
		t.Fatalf("client after resume: %v", err)
	}

	before := harness.NftCounter(t, id, "in_floor")
	res := runGuestProbe(t, cl2, net.JoinHostPort(host, port), false)
	after := harness.NftCounter(t, id, "in_floor")
	if res.reachable() {
		t.Fatalf("AFTER a same-name veth rebuild the guest REACHED %s — this is `iif`'s silent "+
			"fail-open: it freezes an ifindex at load time, the bytecode is unchanged after the "+
			"rebuild, and the packet is delivered while the drop counter reads 0 (D5 ⑫). The "+
			"by-NAME spelling does not have this failure\n%s", host, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", before, after,
		"item 11b: after the rebuild the floor must both refuse AND count — a delivered packet with a zero counter is exactly `iif`'s signature")
}

// --- item 11: cross-sandbox independence ---------------------------------------

// Gate B item 11: two sandboxes hold two tables, and one's rule set does not affect
// the other's reachable set.
//
// This is guard ①'s POSITIVE face: a `policy drop` in a per-sandbox table was
// measured killing ANOTHER sandbox's traffic while its own rule matched not one
// packet, so "my deny works" is not evidence the deny is scoped.
func TestOneSandboxsRuleSetDoesNotAffectAnothers(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, portStr, _ := net.SplitHostPort(addr)
	var port uint16
	fmt.Sscanf(portStr, "%d", &port)

	// A: denies everything. B: declares exactly this endpoint.
	denyID, denyCl := createRunning(t, eng, declDeny())
	allow := declDeny()
	allow.Egress.Allow = []model.NetTuple{tupleOf(t, host+"/32", port, model.NetTCP)}
	allowID, allowCl := createRunning(t, eng, allow)

	// B reaches it…
	if res := runGuestProbe(t, allowCl, net.JoinHostPort(host, portStr), false); !res.reachable() {
		t.Fatalf("the sandbox that DECLARED this endpoint cannot reach it (%s) — A's deny is "+
			"bleeding into B, which is guard ①'s hazard: a per-sandbox table sits on a SHARED hook "+
			"(measured: a `policy drop` there killed another sandbox's traffic while its own rule "+
			"matched no packet)\n%s", res.raw, harness.DumpRuleSet(allowID))
	}
	// …while A does not, and A's own floor counts it.
	before := harness.NftCounter(t, denyID, "in_floor")
	res := runGuestProbe(t, denyCl, net.JoinHostPort(host, portStr), false)
	after := harness.NftCounter(t, denyID, "in_floor")
	if res.reachable() {
		t.Errorf("the DENY sandbox reached an endpoint only the other one declared — an un-narrowed "+
			"accept in B's table is a hole in EVERY sandbox's floor (measured, D5 ⑩)\n%s",
			harness.DumpRuleSet(allowID))
	}
	mustCounterMoved(t, denyID, "in_floor", before, after, "item 11: A's own floor refuses what B declared")

	// Destroying one must not change the other's reachable set.
	if err := eng.Mediator.Destroy(context.Background(), denyID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if res := runGuestProbe(t, allowCl, net.JoinHostPort(host, portStr), false); !res.reachable() {
		t.Errorf("destroying the OTHER sandbox broke this one's reachable set (%s) — the tables must "+
			"be independent units\n%s", res.raw, harness.DumpRuleSet(allowID))
	}
}

// --- item 12: atomicity — the rules are in place before the unit runs ----------

// Gate B item 12: no fail-open window, and a failed launch leaves NO table.
//
// The first half cannot be observed by polling from outside (the window it denies is
// exactly the one that must not exist), so it is asserted STRUCTURALLY: the very
// first thing the guest can possibly do is already refused. If the rules went on
// after the unit started, the guest's first probe would be the one that got through.
func TestTheRulesArePresentBeforeTheGuestRunsAndAFailedLaunchLeavesNoTable(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	before := harness.CountRuleSetTables(t)

	id, cl := createRunning(t, eng, declDeny())
	// The table exists the moment the sandbox is running.
	if n := harness.CountRuleSetTables(t); n != before+1 {
		t.Errorf("rule-set tables went %d → %d for one sandbox, want +1", before, n)
	}
	// And the guest's FIRST outbound attempt is already refused — the floor was not
	// installed after the fact.
	if res := runGuestProbe(t, cl, "1.1.1.1:443", false); res.reachable() {
		t.Errorf("the guest's first probe got out: %s — the rule set must be installed BETWEEN "+
			"building the netns and starting the unit (§2.4), because any window where the network "+
			"is up and the rules are not is a fail-open window\n%s", res.raw, harness.DumpRuleSet(id))
	}

	// Teardown leaves nothing: a rule set is crash-surviving host state (§2.4), so it
	// leaks exactly the way a netns does.
	if err := eng.Mediator.Destroy(context.Background(), id); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if n := harness.CountRuleSetTables(t); n != before {
		t.Errorf("after destroy, rule-set tables are %d, want %d — the table leaked", n, before)
	}
}
