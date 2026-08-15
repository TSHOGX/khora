//go:build substrate

package test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/orchestrator"
	"github.com/TSHOGX/khora/internal/ruleset"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/test/harness"
)

// the documented invariant enforcement on the real substrate.
//
// # The acceptance discipline, which shapes every assertion in this file
//
// "IT IS UNREACHABLE" IS NOT A VALID ASSERTION ON ITS OWN (the documented invariant / the relevant internals page the documented invariant,
// the documented invariant ⑧). A rule set with the right rules in the WRONG ORDER behaves perfectly on
// ordinary traffic and differs only when it is being bypassed; and a floor whose
// counter reads 0 is INDISTINGUISHABLE from a floor that was bypassed. So every deny
// here is proved by TWO facts: unreachable from inside the netns, AND the floor
// counter moved after a deliberate probe. A counter that did not move is reported as
// an INVALID assertion, never as a pass.
//
// The paired direction matters just as much and for the opposite reason: an engine
// that could reach NOTHING would pass every deny assertion vacuously. That is why
// item 7 (public egress actually works under `allow`) has to pass before items 7b/7c
// mean anything: the order 8 → 7 → 8c → 7b/7c is a real dependency, not a listing.
//
// # The `item N` labels
//
// THIS FILE'S OWN numbering, declared by the `--- item N: … ---` banners below and
// used in failure messages so a failing assertion names itself. Keep a label stable
// once written — the sub-letters (7b, 9d, 14e) encode which assertion depends on
// which, as the paragraph above does for 7.

// forgerSrc adds an address to a guest interface using a raw netlink socket.
//
// It exists because the guest rootfs has NO `ip` binary — build-rootfs.sh installs
// dash, the loader and sandboxd, nothing else — so the forgery step this test is named
// for silently did nothing: exec of /sbin/ip failed with ENOENT, the error was
// swallowed, and the test degenerated into a second copy of the plain floor probe. It
// would then pass on an engine whose predicate WAS forgeable, which is the one thing
// pillar ④'s acceptance exists to rule out.
//
// Raw RTM_NEWADDR rather than a vendored iproute2: the guest is root in its own netns
// (the documented invariant), so the syscall is available and the dependency is zero.
const forgerSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

func main() {
	// forger <ipv4>. The target interface is DISCOVERED rather than passed: the guest
	// side of the veth is named vg<hash-of-SandboxID> (the documented invariant), which no test can know
	// statically, and hardcoding a guess is how this step silently did nothing before.
	ip := net.ParseIP(os.Args[1]).To4()
	if ip == nil {
		fmt.Println("FORGE_FAIL bad address")
		os.Exit(1)
	}
	all, err := net.Interfaces()
	if err != nil {
		fmt.Println("FORGE_FAIL enumerate:", err)
		os.Exit(1)
	}
	var ifi *net.Interface
	for i := range all {
		if all[i].Flags&net.FlagLoopback == 0 && all[i].Flags&net.FlagUp != 0 {
			ifi = &all[i]
			break
		}
	}
	if ifi == nil {
		fmt.Println("FORGE_FAIL no non-loopback interface")
		os.Exit(1)
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		fmt.Println("FORGE_FAIL socket:", err)
		os.Exit(1)
	}
	defer syscall.Close(fd)

	// struct ifaddrmsg + one IFA_LOCAL/IFA_ADDRESS attribute each.
	type ifAddrMsg struct {
		Family, Prefixlen, Flags, Scope uint8
		Index                           uint32
	}
	msg := ifAddrMsg{Family: syscall.AF_INET, Prefixlen: 32, Index: uint32(ifi.Index)}
	body := (*[8]byte)(unsafe.Pointer(&msg))[:]
	attr := func(typ uint16) []byte {
		out := make([]byte, 8)
		*(*uint16)(unsafe.Pointer(&out[0])) = 8 // rta_len
		*(*uint16)(unsafe.Pointer(&out[2])) = typ
		copy(out[4:], ip)
		return out
	}
	payload := append(append(append([]byte{}, body...), attr(syscall.IFA_LOCAL)...), attr(syscall.IFA_ADDRESS)...)

	hdr := make([]byte, 16)
	*(*uint32)(unsafe.Pointer(&hdr[0])) = uint32(16 + len(payload))
	*(*uint16)(unsafe.Pointer(&hdr[4])) = syscall.RTM_NEWADDR
	*(*uint16)(unsafe.Pointer(&hdr[6])) = syscall.NLM_F_REQUEST | syscall.NLM_F_CREATE | syscall.NLM_F_EXCL | syscall.NLM_F_ACK
	*(*uint32)(unsafe.Pointer(&hdr[8])) = 1 // seq
	req := append(hdr, payload...)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, req, 0, sa); err != nil {
		fmt.Println("FORGE_FAIL sendto:", err)
		os.Exit(1)
	}
	buf := make([]byte, 4096)
	n, _, rerr := syscall.Recvfrom(fd, buf, 0)
	if rerr != nil {
		fmt.Println("FORGE_FAIL recv:", rerr)
		os.Exit(1)
	}
	if n >= 20 {
		if code := *(*int32)(unsafe.Pointer(&buf[16])); code != 0 {
			fmt.Printf("FORGE_FAIL netlink errno %d\n", -code)
			os.Exit(1)
		}
	}
	// Read it back: the assertion is that the address IS on the interface, not that
	// the syscall returned 0.
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if in, ok := a.(*net.IPNet); ok && in.IP.To4() != nil && in.IP.To4().Equal(ip) {
			fmt.Println("FORGE_OK")
			return
		}
	}
	fmt.Println("FORGE_FAIL address not present after the add")
	os.Exit(1)
}
`

// guestServeSrc is the in-guest listener both the publish and the
// publish-does-not-open-egress arms inject: `serve <port> [addr]`, answering every
// connection with a banner. ONE source for both — they had drifted into a superset and
// a subset of each other (the subset simply always binding 0.0.0.0, which is this
// source's default), and two copies of a fixture is two things to keep in step for no
// assertion's benefit.
const guestServeSrc = `package main

import (
	"fmt"
	"net"
	"os"
)

// serve <port> [addr] — answer every connection with a banner.
func main() {
	bind := "0.0.0.0:" + os.Args[1]
	if len(os.Args) > 2 {
		bind = os.Args[2] + ":" + os.Args[1]
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Println("LISTEN_FAILED", err)
		return
	}
	fmt.Println("LISTENING", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("hello-from-GUEST"))
		_ = c.Close()
	}
}
`

// guestProbeSrc dials an address and prints a one-line verdict. The bare rootfs ships
// no coreutils, so anything the guest must run has to be injected
// (harness.InjectStaticBin). It reports the elapsed time because two assertions turn on
// "refused fast" vs "hung" (the documented invariant ⑨).
const guestProbeSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// probe <addr> [udp] [dial-timeout-seconds] — dial, read a banner, print
// OUTCOME=<verdict> MS=<elapsed>.
//
// The dial timeout is a PARAMETER because it is the entire cost of a deny probe; which
// value each caller passes and why is at denyDialTimeout / hangDialTimeout.
//
// (No backticks anywhere in this comment: it lives inside a raw string literal, and one
// would end the literal here rather than at the fixture's end.)
func main() {
	addr := os.Args[1]
	netw := "tcp"
	dial := 25 * time.Second
	// Order-independent, so a caller cannot silently transpose the two optional args.
	for _, a := range os.Args[2:] {
		if a == "udp" {
			netw = "udp"
			continue
		}
		if n, err := strconv.Atoi(a); err == nil {
			dial = time.Duration(n) * time.Second
		}
	}
	start := time.Now()
	c, err := net.DialTimeout(netw, addr, dial)
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
		// Connected but read nothing. For TCP that is a real distinction the assertions need
		// (a half-torn publish presents exactly this way, the documented invariant ⑰); for UDP it is how a
		// silently-dropped datagram looks.
		fmt.Printf("OUTCOME=connected_no_payload MS=%d ERR=%v\n", el, rerr)
		return
	}
	// PAYLOAD is emitted LAST because the harness reads it to end-of-line (see
	// runGuestProbeDialing).
	fmt.Printf("OUTCOME=connected MS=%d PAYLOAD=%s\n", el, strings.ReplaceAll(string(buf[:n]), "\n", " "))
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
	return runGuestProbeDialing(t, cl, addr, udp, denyDialTimeout)
}

// denyDialTimeout is what a probe waits for a dial that is expected NOT to complete.
//
// Short on purpose: it was the single largest cost in this file (measured: four tests
// spent 8-26 s each against a ~0.6 s per-sandbox baseline). Sound because the elapsed
// time is not what carries a deny here — "unreachable" AND "the counter moved" is
// (harness.NftCounter) — and a legitimate connect on these paths measures ~25 ms, a
// ~100x margin over the outcome this timeout must stay distinguishable from.
const denyDialTimeout = 3 * time.Second

// hangDialTimeout is for the two arms whose claim IS the elapsed time: `reject` refuses
// in milliseconds where `drop` hangs (the documented invariant ⑨). They must be able to OBSERVE the hang, so
// the timeout has to sit well above the bound they assert — with denyDialTimeout they
// would report 3 s, read as "fast", and pass without testing anything.
const hangDialTimeout = 25 * time.Second

func runGuestProbeDialing(t *testing.T, cl transport.Client, addr string, udp bool, dial time.Duration) probeResult {
	t.Helper()
	args := []string{addr}
	if udp {
		args = append(args, "udp")
	}
	args = append(args, strconv.Itoa(int(dial/time.Second)))
	// The outer bound is the dial plus slack for the probe's own 5 s read deadline and
	// the exec round-trip — derived from the dial rather than fixed, so a caller raising
	// the dial cannot have it cut off by a stale constant.
	ctx, cancel := context.WithTimeout(context.Background(), dial+20*time.Second)
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
	// PAYLOAD is read to END OF LINE rather than as a whitespace-delimited field: a
	// banner legitimately contains spaces (the via endpoint answers
	// "via-ok ORIGDST=<addr>"), and splitting on whitespace truncated it at "via-ok" —
	// which read exactly like the engine failing to recover the original destination.
	if i := strings.Index(raw, "PAYLOAD="); i >= 0 {
		rest := raw[i+len("PAYLOAD="):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[:nl]
		}
		res.payload = strings.TrimSpace(rest)
	}
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

// portOf returns a listener address's port BOTH ways: as the uint16 a declaration
// needs and as the string a probe target needs. Both, because every call site wants
// one or the other and the inline form is `SplitHostPort` + `Sscanf` with BOTH errors
// dropped — and a dropped error here is not cosmetic: an unexpected address shape
// yields port 0, which is the any-port WILDCARD in a tuple (the documented invariant), so a fixture slip
// would WIDEN the declaration under test rather than fail it.
func portOf(t *testing.T, addr string) (uint16, string) {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("listener address %q is not host:port: %v", addr, err)
	}
	n, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("listener port %q is not a uint16: %v", portStr, err)
	}
	if n == 0 {
		t.Fatalf("listener reported port 0 for %q — 0 is the any-port wildcard, which "+
			"would widen the declaration this test means to pin", addr)
	}
	return uint16(n), portStr
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

// mustCounterMoved is the acceptance discipline as a helper: it FAILS when the counter
// did not move, and distinguishes "did not move" from "does not exist" (-1).
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
			"indistinguishable from a floor that was bypassed (the documented invariant / the documented invariant / the documented invariant ⑧).\n%s",
			what, counter, before, after, harness.DumpRuleSet(id))
	}
}

// netEngineOpts is the shared engine configuration for this file: the guest probe
// injected, and the second-hop deployment inputs supplied so `via[]` is permitted.
func netEngineOpts(t *testing.T) harness.EngineOpts {
	t.Helper()
	uid := secondHopUID
	return harness.EngineOpts{
		Probes:        map[string]string{"probe": guestProbeSrc, "forger": forgerSrc},
		SecondHopUID:  &uid,
		HostPublishIP: hostIPv4(t),
	}
}

// secondHopUID is the uid the `via[]` tests run the endpoint process as. A
// fixed non-root uid: the whole point of item 9d is that the drop is narrowed to it
// and does NOT take out the host (measured: an un-narrowed drop on that hook is a
// host-level egress kill switch — the relevant internals page the documented invariant ㉔).
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

// declDeny is the minimal declaration: `deny` + empty (the documented invariant's "no network").
func declDeny() model.NetworkDecl {
	return model.NetworkDecl{Egress: model.NetEgress{Default: model.NetDeny}}
}

func declAllowPublic() model.NetworkDecl {
	return model.NetworkDecl{Egress: model.NetEgress{Default: model.NetAllow}}
}

// --- item 6: `deny` + empty is unreachable EVERYWHERE, with counters moving ------

// Four destination classes, and the COUNTERS ARE READ PER HOOK because
// the two hooks see different traffic: a destination that is an address of the host
// ITSELF lands on `input`, while something the host would ROUTE lands on `forward`
// (measured, the relevant internals page the documented invariant ⑧).
//
// Reading only the `input` counter would make every routed-destination probe look
// like "the floor counted 0", which by the discipline is an INVALID assertion rather
// than a pass — so each class below names the counter that must move for IT.
func TestDenyEmptyIsUnreachableEverywhereWithFloorCountersMoving(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())
	host := hostIPv4(t)

	// A host-side listener on a WILDCARD bind, so "unreachable" cannot be an artifact
	// of nothing listening. This is the the documented invariant ⑧ shape: with the floor installed, the guest
	// must not reach a 0.0.0.0 listener through ANY host address.
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, port := portOf(t, addr)

	cases := []struct {
		name    string
		target  string
		counter string
		why     string
	}{{
		name:    "an address of the host itself",
		target:  net.JoinHostPort(host, port),
		counter: "in_floor",
		why:     "the host's own address lands on `input`; the floor there is pure ct-state, so it covers every host address without enumerating one (the documented invariant ⑧)",
	}, {
		// Link-local is every cloud's metadata endpoint.
		name:    "link-local metadata address",
		target:  "169.254.169.254:80",
		counter: "fwd_floor_v4",
		why:     "link-local is routed, so it lands on `forward` and is an explicit daddr member of the floor there",
	}, {
		// The sibling item with NO second sandbox involved, which isolates A's own floor
		// from the cross-sandbox ordering question the sibling case below raises.
		name:    "an unallocated address in the sandbox address space",
		target:  "10.66.199.222:9000",
		counter: "fwd_floor_v4",
		why:     "the sibling CIDR is a floor daddr member, and with no sandbox holding this address only THIS sandbox's floor can refuse it",
	}, {
		// A different counter on purpose: refused by the DEFAULT, not by the floor.
		name:    "a public address",
		target:  "1.1.1.1:443",
		counter: "fwd_default_deny",
		why:     "a public destination is refused by the forward side's OWN default-deny — an input-only default-deny would never see it (the documented invariant ⑩)",
	}}

	// LOOPBACK IS DELIBERATELY NOT IN THIS TABLE, and it carries NO counter assertion: a
	// gVisor guest dialing 127.0.0.1 hits ITS OWN netstack's loopback, so the packet never
	// leaves the sandbox and the host's rules are never evaluated — measured, the
	// host-side `daddr 127.0.0.0/8` counter read 0 while link-local counted 5 in the same
	// run (the documented invariant ⑦). Demanding movement from traffic that constructively cannot arrive is an
	// unsatisfiable assertion, not a finding.
	if res := runGuestProbe(t, cl, net.JoinHostPort("127.0.0.1", port), false); res.reachable() {
		t.Errorf("the guest reached the HOST's loopback (%s) — that would mean the packet left the "+
			"sandbox, contradicting the documented invariant ⑦'s measurement that a gVisor guest's loopback dial stays in "+
			"its own netstack", res.raw)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := harness.NftCounter(t, id, tc.counter)
			res := runGuestProbe(t, cl, tc.target, false)
			after := harness.NftCounter(t, id, tc.counter)
			if res.reachable() {
				t.Fatalf("REACHED %s under `deny` + empty (payload %s) — the declared reachable set is "+
					"empty, so this is the the documented invariant ③ exhaustive-narrowing obligation broken: %s\n%s",
					tc.target, res.payload, tc.why, harness.DumpRuleSet(id))
			}
			mustCounterMoved(t, id, tc.counter, before, after, tc.name)
		})
	}

	// The sibling class needs a second sandbox to be a real target, and it is the one
	// the documented invariant calls an INDEPENDENT invariant: point-to-point veth does not deliver it,
	// because the host still ROUTES between two /30s — measured, sandbox A completed a
	// TCP handshake to sibling B and read its payload (the documented invariant ①).
	t.Run("a sibling sandbox's guest IP", func(t *testing.T) {
		sibID, _ := createRunning(t, eng, declAllowPublic())
		// THE TARGET IS THE SIBLING'S TRANSPORT ENDPOINT: a real listener (sandboxd) inside
		// that netns, which is exactly the "sibling endpoint" the documented invariant names. So "unreachable"
		// cannot be an artifact of nothing listening — the alternative reading this test has
		// to rule out.
		ep, err := eng.Mediator.Endpoint(sibID)
		if err != nil {
			t.Fatalf("sibling endpoint: %v", err)
		}
		before := harness.NftCounter(t, id, "fwd_floor_v4")
		res := runGuestProbe(t, cl, ep, false)
		after := harness.NftCounter(t, id, "fwd_floor_v4")
		if res.reachable() {
			t.Fatalf("REACHED sibling %s from another sandbox (payload %s) — the documented invariant's cross-sandbox "+
				"network isolation is an INDEPENDENT control-plane invariant that only this rule set "+
				"materializes: point-to-point veth does not deliver it, because the host still routes "+
				"between two /30s (measured, the documented invariant ①)\n%s", ep, res.payload, harness.DumpRuleSet(id))
		}
		mustCounterMoved(t, id, "fwd_floor_v4", before, after, "sibling guest IP")
	})
}

// --- item 9: the transport is not killed by the floor ---------------------------

// The LOUD counterpart to every silent failure in this file:
// the floor's `input` side must not drop the guest's replies to the host.
//
// It is asserted here rather than left implicit in the other tests' use of the
// transport, because a floor that killed it would fail everything with one confusing
// symptom (the documented invariant ⑧: a naive daddr floor placed above the established accept kills
// transport outright).
func TestTheFloorDoesNotKillTheTransport(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cl.Health(ctx); err != nil {
		t.Fatalf("Health failed with the floor installed: %v — the `input` floor must sit BELOW an "+
			"established/related accept, or it drops the guest's replies to the host and transport "+
			"dies outright (the documented invariant ⑧)\n%s", err, harness.DumpRuleSet(id))
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

// The order here is a HARD constraint rather than a preference: item 7 must pass
// BEFORE 7b/7c mean anything, because an engine that could reach nothing would pass
// every deny assertion vacuously (see the file header).
func TestAllowPublicDeliversEgressAndStillRefusesTheInternalTier(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)

	// ITEM 7: public egress actually works, so item 6 above is not passing on an engine
	// where nothing works at all. The three host-side mechanisms it proves materialized
	// are named in the failure message (measured necessary AND sufficient, the documented invariant ⑤).
	id, cl := createRunning(t, eng, declAllowPublic())
	pub := runGuestProbe(t, cl, "1.1.1.1:443", false)
	if !pub.reachable() && pub.outcome != "connected_no_payload" {
		t.Fatalf("`default: allow` did NOT deliver public egress (%s) — without this the deny "+
			"assertions elsewhere in this file would be passing vacuously on an engine that cannot "+
			"reach anything. The three host-side mechanisms are ip_forward, the guest default route "+
			"WITH `dev`, and the masquerade for this /30 (the documented invariant ⑤)\n%s", pub.raw, harness.DumpRuleSet(id))
	}
	t.Logf("item 7: public egress under `allow` → %s (%d ms)", pub.outcome, pub.ms)

	// ITEM 7b: the SAME declaration must NOT reach the host's LAN. This is the
	// deny-safe tier's acceptance, and it only means something now that item 7 passed:
	// measured, with the three mechanisms in place and no tier, this exact flow leaves
	// the netns and is routed by the host, and NONE of the floor's four items covers it
	// (the documented invariant ⑤). Without the tier, `allow` delivers a reachable set strictly WIDER than
	// the documented invariant defines.
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
			"reachable set being wider than the documented invariant's `allow` (public MINUS the floor MINUS the internal "+
			"deny-safe tier). Measured: with the three egress mechanisms in place and this tier "+
			"absent, this exact flow leaves the netns and is routed, and no floor item covers it "+
			"(the documented invariant ⑤)\n%s", lanTarget, harness.DumpRuleSet(id))
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
			"means nothing (the documented invariant)\n%s", lanTarget, allowBefore, allowAfter, harness.DumpRuleSet(liftedID))
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
			"purely by ORDER (the documented invariant)\n%s", sibEP, harness.DumpRuleSet(wideID))
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

// The declared `{cidr, port, proto}` works and NOTHING ADJACENT does.
//
// Both halves, because either alone is passable by a broken rule set: only the
// positive and a rule set that admits everything passes; only the negative and one
// that admits nothing does.
func TestAnAllowEntryAdmitsExactlyItsTuple(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-DECLARED")
	defer stop()
	port, portStr := portOf(t, addr)

	// The declared target is an address of the HOST, so this also exercises the rule
	// the documented invariant requires on BOTH hooks: a forward-only allow cannot reach a host-local
	// destination at all (the documented invariant).
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
			"(the documented invariant), which is why the documented invariant requires the allow on both hooks\n%s",
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
	_, otherPortStr := portOf(t, other)
	floorBefore := harness.NftCounter(t, id, "in_floor")
	res := runGuestProbe(t, cl, net.JoinHostPort(host, otherPortStr), false)
	floorAfter := harness.NftCounter(t, id, "in_floor")
	if res.reachable() {
		t.Errorf("an UNDECLARED port on the declared address was reachable (payload %s) — the "+
			"reachable set must be exactly the declared tuples, so the port dimension has to bite "+
			"(the documented invariant ③)\n%s", res.payload, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", floorBefore, floorAfter, "item 8: an undeclared port on a declared address")
}

// The UDP half, asserted separately because UDP is DNS's only declarable path (the documented invariant
// semantics ③) and because the assertion deliberately stops at the RULE layer:
// whether `curl` in the guest can RESOLVE depends on `resolv.conf`, which the engine
// does not write (the guest has no resolver configuration at all today — measured,
// the documented invariant ⑦).
func TestADeclaredUDPTupleIsAdmittedAtTheRuleLayer(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)

	// A host-side UDP echo, so "admitted" is observable rather than inferred.
	//
	// BOUND TO THE HOST ADDRESS, NOT THE WILDCARD, and this is load-bearing for the round
	// trip below rather than tidiness. A wildcard-bound UDP socket has no source address
	// pinned, so the kernel picks one per reply by route lookup — for a destination inside
	// the sandbox /30 that is the veth's host-side address, not the address the guest sent
	// to. The guest's socket is connected, so a reply from an unexpected source is
	// discarded and the probe times out: measured as `connected_no_payload` after 5 s with
	// the allow counter reading 0 → 1. That is a fixture artifact wearing the exact shape
	// of an engine defect ("the rule matched but traffic does not flow") — the hazard the
	// PAYLOAD-parsing note above is also about.
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host), Port: 0})
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
			"declarable path for DNS under `default: deny` (the documented invariant semantics ③), so this rule has to "+
			"bite\n%s", before, after, harness.DumpRuleSet(id))
	}
	// THE ROUND TRIP, which is what "observable rather than inferred" above means. The
	// counter alone proves the rule MATCHED, not that UDP works: the datagram could match
	// the input-side allow and still never come back, and the reply direction is carried
	// by a different rule from the one counted here. Both halves or neither.
	if !got.reachable() {
		t.Errorf("a declared UDP tuple matched the allow (counter %d → %d) but the datagram did not "+
			"round-trip: outcome=%q raw=%s\nUDP is the only declarable path for DNS under "+
			"`default: deny` (the documented invariant semantics ③), so a tuple that counts but does not carry traffic is "+
			"the feature being dead with its counter moving", before, after, got.outcome, got.raw)
	}
	if got.payload != "udp-banner" {
		t.Errorf("the UDP reply payload was %q, want %q — the banner is what distinguishes "+
			"\"the host answered\" from \"something answered\"", got.payload, "udp-banner")
	}
	t.Logf("item 8 (udp): %s", got.raw)
	// Deliberately NOT asserting name resolution: the guest ships no resolv.conf at
	// all, so DNS does not work there for reasons that precede any the documented invariant policy — that
	// is a template/product-layer content choice, outside the engine.
}

// --- item 8b: guest-facing refusals FAIL FAST rather than hang -----------------

// the documented invariant's fail-closed has a SECOND half — "the guest gets a clean
// connection failure" — and a pure-`drop` floor satisfies the security half while
// breaking this one. gVisor's netstack honors ICMP errors, so `reject` and `drop`
// differ by orders of magnitude in what the guest experiences.
//
// ASSERTED BY MAGNITUDE, NEVER BY A MILLISECOND FIGURE: this test IS the standing
// `reject` arm the documented invariant ⑨ cites (0 ms), and the `drop` arm is deliberately not kept as a
// standing test — every run would waste 25 s hanging.
//
// The source measurement was single-source and its first independent reproduction
// FAILED: the test bed had no guest default route, so nothing left the sandbox and all
// four verdicts looked alike — an invalid measurement, not a refutation. This test is
// that reproduction's second attempt. So the direction is trusted and the numbers are
// not (recorded here because no §-doc carries it: the documented invariant ⑨ states both arms as reproduced
// and defers the conditions to the documented invariant, which does not keep the failed attempt).
func TestGuestFacingRefusalsFailFastRatherThanHanging(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	// A refused destination that the floor rejects. Sub-second vs tens of seconds is
	// the claim; the threshold sits far from both.
	const fastEnough = 5 * time.Second
	// SAMPLED AROUND THE PROBE, because "refused in 2 ms" is produced identically by the
	// floor's `reject` and by the packet dying inside the netns before it ever reached a
	// rule — this test's own first attempt failed that way (no guest default route, so
	// nothing left the sandbox and all four verdicts looked alike).
	before := harness.NftCounter(t, id, "fwd_floor_v4")
	// hangDialTimeout, not the default: the assertion below is on the ELAPSED TIME, so a
	// short dial would report itself instead of the hang it exists to rule out.
	res := runGuestProbeDialing(t, cl, "169.254.169.254:80", false, hangDialTimeout)
	after := harness.NftCounter(t, id, "fwd_floor_v4")
	if res.reachable() {
		t.Fatalf("the probe REACHED a floor address: %s", res.raw)
	}
	mustCounterMoved(t, id, "fwd_floor_v4", before, after,
		"item 8b: the floor counter must move, or the fast refusal proves nothing about the ENGINE — "+
			"a packet dying in the netns is refused just as fast (this test's own first attempt "+
			"failed that way)")
	elapsed := time.Duration(res.ms) * time.Millisecond
	if elapsed >= fastEnough {
		t.Errorf("a guest-facing refusal took %s — the documented invariant's fail-closed promises the guest a CLEAN "+
			"connection failure, not a hang. A pure-`drop` floor meets the security half and breaks "+
			"this one; the fix is `reject`, and security is unaffected because neither forwards "+
			"(the documented invariant ⑨ / the documented invariant). Asserted by MAGNITUDE because that measurement is single-source and its "+
			"first reproduction was invalid\n%s", elapsed, harness.DumpRuleSet(id))
	}
	t.Logf("item 8b: refusal at %s (magnitude assertion: < %s). the documented invariant ⑨'s numbers were 20.0 s for "+
		"`drop` vs 0.002 s for `reject`; this run reproduces the DIRECTION", elapsed, fastEnough)
}

// --- item 10 / 11b: pillar ④ — the predicate binds to a fact the guest cannot ---
// --- change, and survives a veth rebuild ---------------------------------------

// Guest root FORGES a source address and the floor still holds.
//
// This is pillar ④'s acceptance: guest root can `ip addr add` (measured rc=0), so a
// SOURCE-address predicate is bypassable while an INPUT-INTERFACE one is not (the documented invariant ③).
// The counter is what proves it was the interface predicate that refused, and not
// `rp_filter` or a routing artifact.
func TestForgingASourceAddressDoesNotBypassTheFloor(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// guest root claims a SIBLING-SPACE address, which is what a source-address
	// predicate would be fooled by (it is inside the floor). NOT the host's own
	// address: adding that makes it locally owned inside the sandbox, so the probe
	// below never leaves the netns and every host counter stays 0 — measured, and by
	// the documented invariant that is an INVALID assertion rather than a pass.
	//
	// THE FORGERY MUST ACTUALLY HAPPEN, and both its RUN and its RESULT are asserted
	// below (see forgerSrc for the ENOENT that made it silently do nothing).
	evs, err := cl.Start(ctx, transport.StartSpec{
		Cmd: "/usr/bin/forger", Args: []string{"10.66.99.99"},
	})
	if err != nil {
		t.Fatalf("the forgery step did not RUN (%v) — without it this test cannot distinguish an "+
			"interface predicate from a source-address one, and would pass on a forgeable engine "+
			"(pillar ④ / the documented invariant ③)", err)
	}
	var forgeOut strings.Builder
	for e := range evs {
		if len(e.Data) > 0 {
			forgeOut.Write(e.Data)
		}
	}
	if !strings.Contains(forgeOut.String(), "FORGE_OK") {
		t.Fatalf("guest root could not add the forged address to its interface (%q) — the documented invariant ③ measured "+
			"this SUCCEEDING, so a failure here means the fixture broke, not that the engine is "+
			"safe: the whole point is that the forgery works and the floor holds anyway",
			forgeOut.String())
	}
	// THE PROBE TARGET IS A ROUTED FLOOR ADDRESS, for the same reason the forged address
	// is not the host's own (above): link-local is in the floor's forward daddr list AND
	// still routes, so the packet reaches the host and a counter moves.
	const floorTarget = "169.254.169.254:80"
	before := harness.NftCounter(t, id, "fwd_floor_v4")
	res := runGuestProbe(t, cl, floorTarget, false)
	after := harness.NftCounter(t, id, "fwd_floor_v4")
	if res.reachable() {
		t.Fatalf("after forging a floor-space source address the guest REACHED %s — the predicate "+
			"must bind to a fact the guest cannot change (pillar ④): a source-address predicate is "+
			"measurably forgeable, an input-interface one is not (the documented invariant ③)\n%s",
			floorTarget, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "fwd_floor_v4", before, after,
		"item 10: the floor counter must move, proving it was the INTERFACE predicate that refused "+
			"rather than rp_filter or a routing artifact — and that the guest's address manipulation "+
			"did not simply make the packet undeliverable inside the sandbox")
}

// After a pause→resume rebuilds the veth under the SAME NAME, the
// floor still holds.
//
// This is the behavioral face of the `iifname`-over-`iif` guard, and the TIME POINT
// is the assertion: `iif` freezes an ifindex at load time and fails OPEN only AFTER a
// same-name rebuild, with the drop counter reading 0 while the packet is delivered
// (the documented invariant ⑫). Asserting before the rebuild would pass with either spelling.
func TestTheFloorStillHoldsAfterAVethRebuild(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, cl := createRunning(t, eng, declDeny())
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, port := portOf(t, addr)

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
			"rebuild, and the packet is delivered while the drop counter reads 0 (the documented invariant ⑫). The "+
			"by-NAME spelling does not have this failure\n%s", host, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", before, after,
		"item 11b: after the rebuild the floor must both refuse AND count — a delivered packet with a zero counter is exactly `iif`'s signature")
}

// --- item 11: cross-sandbox independence ---------------------------------------

// Two sandboxes hold two tables, and one's rule set does not affect
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
	port, portStr := portOf(t, addr)

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
			"accept in B's table is a hole in EVERY sandbox's floor (measured, the documented invariant ⑩)\n%s",
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

// gatewayOf returns a sandbox's own /30 gateway — the host-side half of the pair the
// engine allocated for it.
//
// Derived from the transport endpoint rather than read off a link, because the
// allocation pins guest = host + 1 within the /30 (internal/runtime allocSubnet) and
// the transport endpoint IS the guest IP. Recomputing it from `ip addr` would need the
// veth name, which is a one-way hash of the SandboxID.
func gatewayOf(t *testing.T, eng *orchestrator.Engine, id model.SandboxID) string {
	t.Helper()
	ep, err := eng.Mediator.Endpoint(id)
	if err != nil {
		t.Fatalf("endpoint for %s: %v", id, err)
	}
	guest, _, err := net.SplitHostPort(ep)
	if err != nil {
		t.Fatalf("endpoint %q: %v", ep, err)
	}
	addr, err := netip.ParseAddr(guest)
	if err != nil || !addr.Is4() {
		t.Fatalf("endpoint %q is not an IPv4 guest address", ep)
	}
	if !ruleset.SandboxCIDR.Contains(addr) {
		t.Fatalf("guest %s is outside the sandbox address space %s — the whole premise of this "+
			"assertion is that both gateways sit inside the floor's sibling item", addr, ruleset.SandboxCIDR)
	}
	o := addr.As4()
	if o[3] == 0 {
		t.Fatalf("guest %s has no host-side neighbour in its /30", addr)
	}
	o[3]--
	return netip.AddrFrom4(o).String()
}

// The input-side allow did NOT open a SIBLING's gateway along with
// this sandbox's own.
//
// `allow 10.0.0.0/8` contains BOTH /30s, so nothing in the declaration separates the
// two gateways — ORDER does, and order is what this asserts: the self-gateway allow
// sits above the sibling drop, the declaration's own allows sit below it
// (internal/network/generate.go inputChain steps 3/4/5).
//
// BOTH HALVES ARE REQUIRED, for two different reasons.
//
//   - Without the reachable half, a rule set that never emitted the self-gateway allow
//     at all passes perfectly — and it has broken the documented invariant semantics ③, whose ONLY delivery
//     path is this one: the sole stably-available host-local resolver address is this
//     sandbox's own gateway, it is a host address so it lands on `input`, and a
//     `forward`-only allow cannot reach it (the documented invariant / the documented invariant ⑦).
//   - Without the counter, "cannot connect" is not an assertion at all. The counter that
//     must move is `in_sibling`, the sibling drop ON THE INPUT HOOK — the `forward` side's
//     own sibling item (`fwd_floor_v4`, same /16 in its daddr set) counts ZERO on this
//     path, because a sibling's gateway is an address of the HOST (the documented invariant). Reading that
//     one instead would turn a real leak (measured: the guest reads the sibling's
//     banner) into "the floor counter is 0".
func TestAnInputSideAllowReachesItsOwnGatewayButNotASiblings(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))

	// ONE listener on 0.0.0.0, and that is the point rather than a shortcut: it is bound
	// on EVERY host address, both gateways included, so at the socket layer the
	// sibling's gateway is genuinely serving. Nothing but the rule set's order stands
	// between the guest and that banner — which is exactly the leak the documented invariant measured.
	addr, stop := hostListener(t, "hello-from-GATEWAY")
	defer stop()
	port, portStr := portOf(t, addr)

	// The declaration names the WHOLE /8 with an explicit port: the assertion is a
	// precise reachable/unreachable pair on one tuple, so a port wildcard would blur it.
	d := declDeny()
	d.Egress.Allow = []model.NetTuple{tupleOf(t, "10.0.0.0/8", port, model.NetTCP)}
	aID, aCl := createRunning(t, eng, d)
	// B exists only so that a SECOND /30 gateway is a live host address. Its own
	// declaration is irrelevant — the rule under test is entirely in A's table.
	bID, _ := createRunning(t, eng, declDeny())

	aGW := gatewayOf(t, eng, aID)
	bGW := gatewayOf(t, eng, bID)
	if aGW == bGW {
		t.Fatalf("both sandboxes report the same gateway %s — the /30 allocator handed out one "+
			"subnet twice and this assertion would be vacuous", aGW)
	}

	// (1) A reaches its OWN gateway, and the SELF-GATEWAY rule is what carried it.
	// Attribution matters here: the declaration's ordinary input-side allow
	// (`allow0_in`) also matches this address, but it sits BELOW the sibling drop, so if
	// it were the rule doing the work the packet would already have been rejected.
	// Reading `allow0_selfgw` names the rule instead of inferring it.
	selfBefore := harness.NftCounter(t, aID, "allow0_selfgw")
	own := runGuestProbe(t, aCl, net.JoinHostPort(aGW, portStr), false)
	selfAfter := harness.NftCounter(t, aID, "allow0_selfgw")
	if !own.reachable() {
		t.Fatalf("the sandbox could NOT reach a host-local endpoint on its OWN gateway %s:%d (%s) — "+
			"the documented invariant semantics ③ (a caller declares its own resolver) has no other delivery path: that "+
			"gateway is an address of the host, so the allow must be on the INPUT hook and it must "+
			"sit ABOVE the sibling drop (the documented invariant / the documented invariant ⑦)\n%s", aGW, port, own.raw, harness.DumpRuleSet(aID))
	}
	if own.payload == "" || !strings.Contains(own.payload, "GATEWAY") {
		t.Errorf("reached the own-gateway endpoint but read %q, want the listener's banner", own.payload)
	}
	if selfAfter < 0 {
		t.Errorf("counter %q does not exist, so the self-gateway allow was never rendered — the "+
			"reachability just observed came from some other rule, and semantics ③ is being served "+
			"by accident\n%s", "allow0_selfgw", harness.DumpRuleSet(aID))
	} else if selfAfter <= selfBefore {
		t.Errorf("reached the own gateway but %q counted %d → %d — the traffic took a path the rule "+
			"set does not describe, so this pass says nothing about the ORDER under test\n%s",
			"allow0_selfgw", selfBefore, selfAfter, harness.DumpRuleSet(aID))
	}

	// (2)+(3) A does NOT reach the SIBLING's gateway, and the INPUT-side sibling drop is
	// the counter that moves. `fwd_floor_v4` is sampled too — not as the assertion but
	// to pin the measurement that makes it invalid: it must stay put.
	sibBefore := harness.NftCounter(t, aID, "in_sibling")
	fwdBefore := harness.NftCounter(t, aID, "fwd_floor_v4")
	sib := runGuestProbe(t, aCl, net.JoinHostPort(bGW, portStr), false)
	sibAfter := harness.NftCounter(t, aID, "in_sibling")
	fwdAfter := harness.NftCounter(t, aID, "fwd_floor_v4")
	if sib.reachable() {
		t.Errorf("the sandbox REACHED the SIBLING's gateway %s:%d and read %q — an `allow 10.0.0.0/8` "+
			"covers both /30s, so only ORDER separates them: the sibling drop must sit BETWEEN the "+
			"self-gateway allow and the declaration's own allows. Below them, this is the leak the documented invariant "+
			"measured (the guest reads the sibling's banner)\n%s",
			bGW, port, sib.payload, harness.DumpRuleSet(aID))
	}
	mustCounterMoved(t, aID, "in_sibling", sibBefore, sibAfter,
		"item 11c: a sibling's gateway is an address of the HOST, so the drop that refuses it can only "+
			"be the one on the INPUT hook")
	if fwdAfter > fwdBefore {
		t.Errorf("the forward-side floor counted %d → %d on this path — the measurement this "+
			"assertion rests on is that it counts ZERO here (a sibling's gateway is a host address, "+
			"so `forward` never sees it, the documented invariant). If it now moves, `in_sibling` may no longer be the "+
			"rule doing the refusing and this test is reading the wrong counter\n%s",
			fwdBefore, fwdAfter, harness.DumpRuleSet(aID))
	} else {
		t.Logf("forward-side floor stayed at %d across the sibling probe — the reason reading IT "+
			"would be an invalid assertion (the documented invariant)", fwdBefore)
	}
}

// --- item 12: atomicity — the rules are in place before the unit runs ----------

// No fail-open window, and destroy leaves NO table.
//
// The first half cannot be observed by polling from outside (the window it denies is
// exactly the one that must not exist), so it is asserted STRUCTURALLY: the very
// first thing the guest can possibly do is already refused. If the rules went on
// after the unit started, the guest's first probe would be the one that got through.
//
// THE FAILED-LAUNCH HALF IS NOT HERE, and the name deliberately does not claim it: what
// this test exercises is destroy, which reaches teardown with a COMPLETE placement.
// A failed launch reaches it half-built, which is the state `nft destroy`-over-`delete`
// and the table-before-veth ordering exist for. No control-plane knob fails a launch after
// the rules are installed, so that arm lives at the layer where it is reachable:
// runtime.TestAFailedLaunchLeavesNoRuleSetTable.
func TestTheRulesArePresentBeforeTheGuestRunsAndDestroyLeavesNoTable(t *testing.T) {
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
			"building the netns and starting the unit (the documented invariant), because any window where the network "+
			"is up and the rules are not is a fail-open window\n%s", res.raw, harness.DumpRuleSet(id))
	}

	// Teardown leaves nothing: a rule set is crash-surviving host state (the documented invariant), so it
	// leaks exactly the way a netns does.
	if err := eng.Mediator.Destroy(context.Background(), id); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if n := harness.CountRuleSetTables(t); n != before {
		t.Errorf("after destroy, rule-set tables are %d, want %d — the table leaked", n, before)
	}
}

// --- items 9b / 9c / 9d: `via[]` ------------------------------------------------

// viaListener runs a host-local TCP listener AS A GIVEN UID, recovering the ORIGINAL
// destination of each redirected connection and reporting it — the "process outside
// the engine" a `via[]` endpoint points at.
//
// It runs as secondHopUID because item 9d needs the second hop to be attributable: the
// engine-level table pins THAT uid's egress (see secondHopUID).
const viaListenerSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// vialisten <addr:port> — accept, recover SO_ORIGINAL_DST, answer with it.
//
// SO_ORIGINAL_DST is the interface that WORKS on TCP; the documented invariant excludes UDP from via[]
// precisely because the UDP equivalent returns the POST-NAT local endpoint, which is
// indistinguishable from "no NAT happened" (measured, the documented invariant / the documented invariant ⑥).
func main() {
	bind := os.Args[1]
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Println("LISTEN_FAILED", err)
		os.Exit(1)
	}
	fmt.Println("LISTENING", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		orig := "unknown"
		if tc, ok := c.(*net.TCPConn); ok {
			if raw, err := tc.SyscallConn(); err == nil {
				_ = raw.Control(func(fd uintptr) {
					if sa, err := syscall.GetsockoptIPv6Mreq(int(fd), syscall.IPPROTO_IP, 80); err == nil {
						ip := net.IPv4(sa.Multiaddr[4], sa.Multiaddr[5], sa.Multiaddr[6], sa.Multiaddr[7])
						p := int(sa.Multiaddr[2])<<8 | int(sa.Multiaddr[3])
						orig = ip.String() + ":" + strconv.Itoa(p)
					}
				})
			}
		}
		_, _ = c.Write([]byte("via-ok ORIGDST=" + orig))
		_ = c.Close()
	}
}
`

// buildUnprivBin compiles a one-file Go program into a fresh temp dir and makes both
// the dir and the binary world-executable, so an UNPRIVILEGED uid can exec it out of
// a root-owned temp dir. The chmods are the point and are easy to omit: without them
// the setpriv'd child fails with EACCES, which reads as "the rule set blocked it" —
// a false positive on exactly the assertion these helpers exist to make.
//
// GO111MODULE=off because the source is a bare file outside the module tree.
func buildUnprivBin(t *testing.T, name, src string) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", bin, srcPath)
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", name, err, out)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", bin, err)
	}
	return bin
}

// asUID wraps a command so it runs as uid (dropping supplementary groups), or runs it
// directly when uid is 0 — setpriv to the uid we already are would be a no-op that
// still needs the binary reachable, so the branch keeps the root arm honest.
func asUID(uid int, bin string, args ...string) *exec.Cmd {
	if uid == 0 {
		return exec.Command(bin, args...)
	}
	full := append([]string{"--reuid", fmt.Sprint(uid), "--regid", fmt.Sprint(uid),
		"--clear-groups", bin}, args...)
	return exec.Command("setpriv", full...)
}

// startViaListener builds and starts the via endpoint process as secondHopUID,
// returning its endpoint and a stop func.
//
// IT BINDS A NON-LOOPBACK HOST ADDRESS because the documented invariant's SIXTH 400 class refuses a
// `via[].endpoint` inside the L0 floor, loopback included. Measured on a loopback
// endpoint before this fixture moved off it: with `route_localnet=0` (this host's
// default) a 127/8 DNAT target is not routable for guest-originated traffic —
// `ip route get 127.0.0.9 from <guest>` answers "Network is unreachable".
func startViaListener(t *testing.T, port int) (string, func()) {
	t.Helper()
	bin := buildUnprivBin(t, "vialisten", viaListenerSrc)
	// Drops to the deployment's second-hop uid — the identity the engine-level table
	// narrows on.
	bind := net.JoinHostPort(hostIPv4(t), fmt.Sprint(port))
	cmd := asUID(secondHopUID, bin, bind)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start via listener: %v", err)
	}
	// Wait for the listening line so the test does not race the bind.
	buf := make([]byte, 128)
	n, _ := out.Read(buf)
	line := string(buf[:n])
	if !strings.Contains(line, "LISTENING") {
		_ = cmd.Process.Kill()
		t.Fatalf("via listener did not come up: %q", line)
	}
	stop := func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	t.Cleanup(stop)
	return bind, stop
}

// `via[]` really steers, AND the narrow accept did not reopen the floor. BOTH halves,
// because each alone is passed by a broken rule set — and this is the easiest property
// in this file to silently pass, with TWO failure directions (the documented invariant ⑩): test only (a) and an accept wide enough to void the
// floor passes; read only the counter and a CROSS-CHAIN accept counts while the
// feature is dead.
func TestViaSteersAndDoesNotReopenTheFloor(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	endpoint, _ := startViaListener(t, 15443)

	// Steer a PUBLIC destination, so the declared target is something the floor would
	// not have refused anyway — isolating "steering happened" from "the floor refused".
	d := declDeny()
	d.Egress.Via = []model.NetVia{{
		Match:    []model.NetTuple{tupleOf(t, "1.1.1.1/32", 443, model.NetTCP)},
		Endpoint: endpoint,
	}}
	id, cl := createRunning(t, eng, d)

	// (a) WORKING: reachable AND the accept counted AND the endpoint saw the REAL
	// original destination (the documented invariant ⑥'s end-to-end payoff).
	accBefore := harness.NftCounter(t, id, "via0_0_accept")
	got := runGuestProbe(t, cl, "1.1.1.1:443", false)
	accAfter := harness.NftCounter(t, id, "via0_0_accept")
	if !got.reachable() {
		t.Fatalf("the steered flow did not reach the endpoint (%s) — `via[]` is dead. Note the "+
			"counter alone would not have told us: a CROSS-CHAIN accept increments while the flow is "+
			"still dropped (the documented invariant ⑩)\n%s", got.raw, harness.DumpRuleSet(id))
	}
	if !strings.Contains(got.payload, "via-ok") {
		t.Errorf("the steered flow reached something that is not the endpoint: %q", got.payload)
	}
	// The endpoint recovered the PRE-NAT destination, which is what makes `via[]` useful
	// at all: the process outside the engine sees the real target rather than a
	// guest-asserted alias (the documented invariant / the documented invariant ⑥).
	if !strings.Contains(got.payload, "ORIGDST=1.1.1.1:443") {
		t.Errorf("the endpoint did not recover the real original destination: %q — that recovery is "+
			"the whole point of steering by port (the documented invariant ⑥), and without it the process outside the "+
			"engine has only the guest's own claim about where it was going", got.payload)
	}
	if accAfter <= accBefore {
		t.Errorf("the narrow accept counted %d → %d while the flow worked — the traffic is taking a "+
			"path the rule set does not describe", accBefore, accAfter)
	}

	// (b) The SAME rule set must still refuse everything the narrow accept was not for.
	// Each of these four corresponds to a measured counter-example for a simpler
	// spelling of that accept (the documented invariant ⑩), so they are the proof its three clauses are
	// load-bearing — not extra coverage.
	host := hostIPv4(t)
	_, listenPortStr := portOf(t, endpoint)
	otherAddr, otherStop := hostListener(t, "hello-from-UNDECLARED")
	defer otherStop()
	_, otherPortStr := portOf(t, otherAddr)

	for _, tc := range []struct{ name, target, counter, why string }{{
		name:    "the guest dials the endpoint's port DIRECTLY, bypassing steering",
		target:  net.JoinHostPort(host, listenPortStr),
		counter: "in_floor",
		why: "`tcp dport <listener>` on its own is exactly this bypass; it is safe only as the " +
			"THIRD clause, because `ct original` has already pinned the pre-NAT tuple and a direct " +
			"dial has no conntrack original entry that satisfies it (the documented invariant ⑩)",
	}, {
		name:    "an UNDECLARED original port toward the declared destination",
		target:  "1.1.1.1:9200",
		counter: "fwd_default_deny",
		why: "the measured counter-example for `ct status dnat tcp dport <listener>`: 9200 → :18443 " +
			"was let through with the floor counting 0, and the listener read ORIGDST=…:9200 (the documented invariant ⑩)",
	}, {
		name:    "the declared port toward an UNDECLARED destination",
		target:  "9.9.9.9:443",
		counter: "fwd_default_deny",
		why: "the counter-example for pinning only `ct original proto-dst`: another address on the " +
			"same port would be steered, which is over-reach in the explicit-endpoint form (the documented invariant ⑩)",
	}, {
		name:    "an unrelated host port",
		target:  net.JoinHostPort(host, otherPortStr),
		counter: "in_floor",
		why:     "the floor still covers every host address the declaration did not name",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			before := harness.NftCounter(t, id, tc.counter)
			res := runGuestProbe(t, cl, tc.target, false)
			after := harness.NftCounter(t, id, tc.counter)
			if res.reachable() {
				t.Errorf("REACHED %s under a declaration that did not name it (payload %s) — the narrow "+
					"accept has reopened the floor: %s\n%s", tc.target, res.payload, tc.why,
					harness.DumpRuleSet(id))
			}
			mustCounterMoved(t, id, tc.counter, before, after, tc.name)
		})
	}

	// (c) THE `iifname` DIMENSION, separately: a SECOND sandbox's identical flow must
	// not be admitted by THIS sandbox's accept. Measured as the last row of the documented invariant ⑩ — an
	// accept without the interface clause is a hole in every sandbox's floor, and both
	// floors counted 0 when it happened.
	t.Run("another sandbox's identical flow is not admitted by this accept", func(t *testing.T) {
		otherID, otherCl := createRunning(t, eng, declDeny())
		before := harness.NftCounter(t, otherID, "fwd_default_deny")
		res := runGuestProbe(t, otherCl, "1.1.1.1:443", false)
		after := harness.NftCounter(t, otherID, "fwd_default_deny")
		if res.reachable() {
			t.Errorf("a SECOND sandbox's identical flow was admitted (payload %s) — every per-sandbox "+
				"table is a `policy accept` base chain on a SHARED hook, so an accept missing its "+
				"`iifname` is a hole in EVERY sandbox's floor (the documented invariant ⑩, last row)\n%s",
				res.payload, harness.DumpRuleSet(id))
		}
		mustCounterMoved(t, otherID, "fwd_default_deny", before, after,
			"item 9b(c): the second sandbox's OWN default-deny must be what refuses it")
	})
}

// When the `via[]` endpoint is DEAD, the steered connection is refused
// — it does NOT fall through to the next rule and leave.
//
// This is the documented invariant fail-closed's second named case, and it is a REQUIREMENT rather than an
// inherited measurement: the documented invariant has no "the listener died" cell.
//
// The counter half is the subtle part: it reads the INPUT-side narrow accept rather than
// the forward default-deny, for the hook-attribution reason spelled out at (b) below.
func TestADeadViaEndpointRefusesTheConnectionRatherThanLettingItOut(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	endpoint, stopListener := startViaListener(t, 15444)

	d := declDeny()
	d.Egress.Via = []model.NetVia{{
		Match:    []model.NetTuple{tupleOf(t, "1.1.1.1/32", 443, model.NetTCP)},
		Endpoint: endpoint,
	}}
	// A second, INDEPENDENT allowed egress, for the per-connection scope assertion.
	hostAddr, stopHost := hostListener(t, "hello-from-ALLOWED")
	defer stopHost()
	// The listener binds 0.0.0.0, so only its PORT is meaningful here; the address the
	// declaration must name is the host's own routable one, which hostIPv4 resolves.
	hostPort, hostPortStr := portOf(t, hostAddr)
	d.Egress.Allow = []model.NetTuple{tupleOf(t, hostIPv4(t)+"/32", hostPort, model.NetTCP)}
	id, cl := createRunning(t, eng, d)

	// Baseline: steering works while the endpoint is alive, so the failure below is
	// attributable to killing it rather than to the rule set.
	if res := runGuestProbe(t, cl, "1.1.1.1:443", false); !res.reachable() {
		t.Fatalf("steering did not work before the endpoint was killed: %s", res.raw)
	}

	// Kill the endpoint. The rule set is UNCHANGED — that is the point: this measures
	// what happens to a steered flow whose destination process is gone.
	stopListener()
	time.Sleep(300 * time.Millisecond)

	accBefore := harness.NftCounter(t, id, "via0_0_accept")
	// hangDialTimeout, not the default: (a) below asserts on the ELAPSED TIME.
	res := runGuestProbeDialing(t, cl, "1.1.1.1:443", false, hangDialTimeout)
	accAfter := harness.NftCounter(t, id, "via0_0_accept")

	// (a) refused, and FAST. The mechanism differs from the floor's `reject` and must
	// not be conflated: this RST comes from the host stack having no socket on that
	// port, not from a rule.
	if res.reachable() {
		t.Fatalf("the steered flow succeeded with the endpoint DEAD (%s) — something else answered", res.raw)
	}
	if el := time.Duration(res.ms) * time.Millisecond; el >= 5*time.Second {
		t.Errorf("a steered flow with a dead endpoint took %s — the documented invariant fail-closed promises a clean "+
			"failure for exactly this case, and the RST here comes from the host stack having no "+
			"socket rather than from a `reject` rule (do not conflate the two mechanisms)", el)
	}
	// (b) it did NOT leave. The narrow accept carries `ct state new`, so a re-dial must
	// increment it — positive evidence the packet took the steering path and was
	// consumed locally.
	mustCounterMoved(t, id, "via0_0_accept", accBefore, accAfter,
		"item 9c(b): the DNAT is in `prerouting` and does not check for a listener, so the packet is "+
			"still transformed and still lands on `input` — this counter rising proves it went down "+
			"the steering path rather than being routed out. (The forward default-deny is NOT usable "+
			"here: it counts zero on this path whether or not steering held, and `always zero` is "+
			"indistinguishable from `bypassed`)")

	// (c) PER-CONNECTION scope: another declared egress is unaffected. the documented invariant says
	// fail-closed applies to the failing connection and does not touch the rest.
	if res := runGuestProbe(t, cl, net.JoinHostPort(hostIPv4(t), hostPortStr), false); !res.reachable() {
		t.Errorf("an unrelated ALLOWED egress broke when the via endpoint died (%s) — fail-closed is "+
			"scoped per-connection (the documented invariant), so a dead endpoint must not touch other permitted "+
			"flows\n%s", res.raw, harness.DumpRuleSet(id))
	}
}

// The SECOND HOP is really pinned — the confused-deputy line (the documented invariant
// invariant 2).
//
// BOTH halves, and the reason is the same discipline as everywhere else: testing only
// "it cannot connect" would be passed by an engine that never installed the table at
// all, because hop 2 counts ZERO in `forward` (measured, the documented invariant ㉔) so the first-hop floor
// never evaluates it and "cannot connect" may have any other cause.
//
// The PAIRED direction is equally required: the same process reaching a permitted
// destination must still work. Without that, an UN-NARROWED drop would pass the deny
// half — while actually being a host-level egress kill switch (measured: root's own
// probe failed too).
func TestTheSecondHopIsPinnedUnderTheFloor(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	// A sibling sandbox, so the deputy has a real in-floor target to be pointed at:
	// its guest IP is inside the sandbox CIDR, which is the floor's sibling item.
	sibID, _ := createRunning(t, eng, declDeny())
	sibEP, err := eng.Mediator.Endpoint(sibID)
	if err != nil {
		t.Fatalf("sibling endpoint: %v", err)
	}
	sibIP, _, _ := net.SplitHostPort(sibEP)

	// The engine-level table must exist — it is installed once at boot by the harness,
	// in production's position.
	if n := harness.NftCounterIn(t, "sbxnet_hop2", "hop2_floor_v4"); n < 0 {
		t.Fatalf("the engine-level second-hop table has no v4 floor counter — the documented invariant invariant 2's " +
			"second half is not installed at all, and every FIRST-hop assertion in this file would " +
			"still pass (the documented invariant ㉔)")
	}

	// (a) DENY: the endpoint process's own egress to an in-floor target is refused, and
	// the SECOND-HOP table's counter is what moves.
	for _, tc := range []struct{ name, target string }{
		{"a sibling sandbox's guest IP", net.JoinHostPort(sibIP, "9000")},
		{"a link-local address", "169.254.169.254:80"},
	} {
		t.Run("deny: "+tc.name, func(t *testing.T) {
			before := harness.NftCounterIn(t, "sbxnet_hop2", "hop2_floor_v4")
			out := probeAsUID(t, secondHopUID, tc.target)
			after := harness.NftCounterIn(t, "sbxnet_hop2", "hop2_floor_v4")
			if strings.Contains(out, "CONNECTED") {
				t.Errorf("the second-hop process REACHED %s (%s) — that is the confused deputy the documented invariant's "+
					"second invariant names: an `upstream` pointed at a sibling or link-local address "+
					"walks straight through the side door, and the FIRST-hop floor never sees it "+
					"(measured: hop 2 counts zero in `forward`, the documented invariant ㉔)", tc.target, out)
			}
			if after <= before {
				t.Errorf("%s: refused (%s) but the second-hop counter did not move (%d → %d) — by the "+
					"acceptance discipline that is an INVALID assertion: an engine that never installed "+
					"the table would produce exactly this reading", tc.name, out, before, after)
			}
		})
	}

	// (b) ALLOW: the SAME process reaching a permitted destination still works. Without
	// this half, an un-narrowed drop passes (a) while being a host-level egress kill switch.
	t.Run("allow: the same process reaches a permitted destination", func(t *testing.T) {
		addr, stop := hostListener(t, "hello-from-HOST")
		defer stop()
		_, portStr := portOf(t, addr)
		out := probeAsUID(t, secondHopUID, net.JoinHostPort(hostIPv4(t), portStr))
		if !strings.Contains(out, "CONNECTED") {
			t.Errorf("the second-hop process could NOT reach a permitted destination (%s) — the drop "+
				"must be narrowed to the floor's addresses, not to everything: an un-narrowed drop on "+
				"the output hook is a host-level egress kill switch (measured: root's probe failed "+
				"too, the documented invariant ㉔), and it would pass every deny assertion above", out)
		}
	})

	// (c) ROOT IS UNAFFECTED — the control arm for the kill-switch hazard, and the whole
	// reason the drop must carry `skuid`. Without this arm, such a drop passes (b) above.
	//
	// ASSERTED POSITIVELY, on the counter rather than on reachability. Root dialing a
	// sibling guest IP is a HOST-ORIGINATED flow, so whether it connects depends on that
	// sibling's own rule set, not on this table — the honest assertion is that the
	// SECOND-HOP counter does not move for a uid the table does not name. (The previous
	// form looked for a "REFUSED_BY_TABLE" marker that probeAsUID never prints, so it
	// could not fail.)
	t.Run("root is unaffected by the second-hop drop", func(t *testing.T) {
		before := harness.NftCounterIn(t, runtime.SecondHopTable, "hop2_floor_v4")
		out := probeAsUID(t, 0, net.JoinHostPort(sibIP, "9000"))
		after := harness.NftCounterIn(t, runtime.SecondHopTable, "hop2_floor_v4")
		if after != before {
			t.Errorf("root's probe moved the second-hop counter (%d → %d) — the drop must be narrowed "+
				"to the deployment's uid (%d), and an un-narrowed one is a HOST-LEVEL egress kill "+
				"switch: measured, root's own probe failed too (the documented invariant ㉔). It would still pass the deny "+
				"half above, which is why this arm has to assert rather than log. out=%s",
				before, after, secondHopUID, out)
		}
	})
}

// probeAsUID dials an address as the given uid and reports CONNECTED / REFUSED, which
// is what the second-hop assertions read.
func probeAsUID(t *testing.T, uid int, target string) string {
	t.Helper()
	script := fmt.Sprintf(`
c, err := net.DialTimeout("tcp", %q, 3*time.Second)
if err != nil { fmt.Println("REFUSED", err); return }
c.Close(); fmt.Println("CONNECTED")`, target)
	src := "package main\n\nimport (\n\t\"fmt\"\n\t\"net\"\n\t\"time\"\n)\n\nfunc main() {\n" + script + "\n}\n"
	out, _ := asUID(uid, buildUnprivBin(t, "probe", src)).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// --- items 13 / 13b: the documented invariant reconciliation --------------------------------------

// The rule set is crash-surviving host state, so reconciliation must
// re-adopt a survivor's table and reclaim an orphan's — and the tables must not leak
// (the documented invariant / the documented invariant).
//
// Driven by building a SECOND engine on the same store, which is what a control-plane
// restart is from the host's point of view: the process is new, its live-map is empty,
// and the host still carries every placement the previous one built.
func TestReconciliationReadoptsARuleSetAndReclaimsAnOrphansTable(t *testing.T) {
	base := t.TempDir()
	opts := netEngineOpts(t)
	opts.BaseDir = base
	opts.StorePath = base + "/store.db"
	eng, _ := harness.Engine(t, opts)

	survivor, _ := createRunning(t, eng, declAllowPublic())
	tablesBefore := harness.CountRuleSetTables(t)
	if tablesBefore < 1 {
		t.Fatalf("no rule-set table for a running sandbox (%d)", tablesBefore)
	}

	// The restart: a second engine over the SAME store and state, running production's
	// boot sequence (second-hop install → reconcile).
	//
	// harness.Restart rather than harness.Engine, and the difference is load-bearing —
	// see harness.Restart's doc.
	eng2 := harness.Restart(t, base, opts)

	// The survivor's table is still there and still enforcing. Re-adoption must not have
	// gone through a delete-then-install, which is what would have killed live flows.
	if n := harness.NftCounter(t, survivor, "fwd_floor_v4"); n < 0 {
		t.Errorf("after a control-plane restart the survivor's rule set is GONE — a rule set is "+
			"crash-surviving host state exactly like the netns, so Adopt must rebuild it (the documented invariant); "+
			"without it the sandbox runs unenforced while every other adopt assertion passes\n%s",
			harness.DumpRuleSet(survivor))
	}
	// And it still refuses: the re-installed rule set is a working one, not an empty
	// table that merely exists.
	cl2, err := eng2.Mediator.Client(survivor)
	if err != nil {
		t.Fatalf("client after restart: %v", err)
	}
	before := harness.NftCounter(t, survivor, "fwd_tier_v4")
	res := runGuestProbe(t, cl2, "192.168.0.1:22", false)
	after := harness.NftCounter(t, survivor, "fwd_tier_v4")
	if res.reachable() {
		t.Errorf("after the restart the re-adopted sandbox reached the host LAN — the re-installed "+
			"rule set is not enforcing\n%s", harness.DumpRuleSet(survivor))
	}
	mustCounterMoved(t, survivor, "fwd_tier_v4", before, after,
		"item 13: the re-adopted rule set must be a WORKING one; a table that exists but counts "+
			"nothing is the same silent pass in a new disguise")

	// An ORPHAN table — one with no sandbox record at all — is reclaimed. Written
	// directly, because that is exactly the residue a crash between "install rules" and
	// "commit the record" leaves.
	orphan := "sbxnet_sbx_orphan0000000000"
	load := exec.Command("nft", "-f", "-")
	load.Stdin = strings.NewReader("table inet " + orphan + " {\n  chain c { type filter hook forward priority filter; policy accept; }\n}\n")
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("plant an orphan table: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("nft", "destroy", "table", "inet", orphan).Run() })

	if err := eng2.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	out, _ := exec.Command("nft", "list", "tables", "inet").Output()
	if strings.Contains(string(out), orphan) {
		t.Errorf("reconciliation left an ORPHAN rule-set table (%s) — it backs no sandbox record, and "+
			"a stale table is host state a later sandbox could inherit (the documented invariant / the documented invariant)", orphan)
	}
	// The survivor's table survived that same pass: the keep-set must be complete
	// BEFORE anything is deleted, or "reclaim then adopt" is literally the
	// delete-then-install sequence measured to kill every live flow (the documented invariant ㉒).
	if n := harness.NftCounter(t, survivor, "fwd_floor_v4"); n < 0 {
		t.Errorf("reconciliation reclaimed the SURVIVOR's table — the keep-set must be computed " +
			"before any deletion (the documented invariant ㉒)")
	}
}

// A LIVE FLOW keeps transferring bytes across a control-plane restart
// and its Adopt.
//
// THIS ONE CANNOT BE OMITTED, and the reason is that item 13 is completely insensitive
// to the failure it catches: an Adopt that quietly killed every live connection would
// leave the table counts exactly right, so 13 passes while every user's connection
// died (the documented invariant ㉒).
//
// Asserted BY BYTES, not by the socket still existing: a `write()` to a socket whose
// peer is gone typically succeeds once before returning EPIPE, so "the write worked" is
// not evidence of delivery. There has to be a read-back.
func TestALiveFlowSurvivesAControlPlaneRestartAndItsAdopt(t *testing.T) {
	base := t.TempDir()
	opts := netEngineOpts(t)
	opts.BaseDir = base
	opts.StorePath = base + "/store.db"
	eng, _ := harness.Engine(t, opts)

	// An echo listener on the host, and a declaration that allows exactly it.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, rerr := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(buf[:n]) // echo, so the guest can READ BACK and verify delivery
					}
					if rerr != nil {
						return
					}
				}
			}(c)
		}
	}()
	port, portStr := portOf(t, ln.Addr().String())

	d := declDeny()
	d.Egress.Allow = []model.NetTuple{tupleOf(t, hostIPv4(t)+"/32", port, model.NetTCP)}
	id, cl := createRunning(t, eng, d)

	// A guest-side long-running flow that reports a per-message ROUND-TRIP count, so the
	// assertion reads delivery rather than write success.
	const flowSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// flow <addr> — echo round trips for 20 s, printing a running count of VERIFIED ones.
func main() {
	c, err := net.DialTimeout("tcp", os.Args[1], 5*time.Second)
	if err != nil {
		fmt.Println("DIAL_FAILED", err)
		return
	}
	defer c.Close()
	buf := make([]byte, 64)
	ok := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := fmt.Sprintf("msg-%d", ok)
		if _, err := c.Write([]byte(msg)); err != nil {
			fmt.Printf("ROUNDTRIPS=%d WRITE_ERR=%v\n", ok, err)
			return
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, rerr := c.Read(buf)
		if rerr != nil {
			fmt.Printf("ROUNDTRIPS=%d READ_ERR=%v\n", ok, rerr)
			return
		}
		if string(buf[:n]) != msg {
			fmt.Printf("ROUNDTRIPS=%d MISMATCH=%q\n", ok, string(buf[:n]))
			return
		}
		ok++
		fmt.Printf("PROGRESS=%d\n", ok)
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("ROUNDTRIPS=%d DONE\n", ok)
}
`
	harness.InjectStaticBin(t, base+"/templates", "bare", "flow", flowSrc)
	// The template changed after the sandbox was created, so re-create to pick it up.
	id2, cl2 := createRunning(t, eng, d)
	_ = id
	_ = cl

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	evs, err := cl2.Start(ctx, transport.StartSpec{
		Cmd: "/usr/bin/flow", Args: []string{net.JoinHostPort(hostIPv4(t), portStr)},
	})
	if err != nil {
		t.Fatalf("start the guest flow: %v", err)
	}

	// Collect progress in the background; restart the control plane mid-flow.
	type result struct{ raw string }
	done := make(chan result, 1)
	go func() {
		var sb strings.Builder
		for ev := range evs {
			sb.WriteString(string(ev.Data))
		}
		done <- result{sb.String()}
	}()

	// Wait until the flow has verified a few round trips, so the restart really lands
	// mid-flow rather than before it started.
	time.Sleep(3 * time.Second)
	progressBefore := harness.NftCounter(t, id2, "allow0_in")

	// THE RESTART, mid-flow: a second engine over the same store and state, running
	// production's boot sequence. Its Adopt re-installs the rule set — as ONE atomic
	// file, which is what this test is really measuring.
	//
	// harness.Restart, not harness.Engine: see harness.Restart's doc.
	_ = harness.Restart(t, base, opts)

	res := <-done
	// The bytes that matter: round trips VERIFIED BY READ-BACK after the restart.
	var trips int
	last := 0
	for _, f := range strings.Fields(res.raw) {
		if k, v, ok := strings.Cut(f, "="); ok && k == "PROGRESS" {
			fmt.Sscanf(v, "%d", &last)
		}
	}
	trips = last
	if strings.Contains(res.raw, "READ_ERR") || strings.Contains(res.raw, "WRITE_ERR") || strings.Contains(res.raw, "MISMATCH") {
		t.Errorf("the live flow BROKE across the control-plane restart: %s\n"+
			"An Adopt that re-installs the rule set as one ATOMIC `nft -f` file leaves live flows "+
			"intact (measured 5/5); a delete-then-install kills them (5/5), because packets sent "+
			"during the gap leave with the raw sandbox source address and the peer RSTs them. "+
			"Flushing conntrack does the same wherever tcp_loose=0 (the documented invariant ㉒).\n"+
			"NOTE item 13's table-count assertions are INSENSITIVE to this failure — they pass "+
			"either way, which is why this assertion exists separately\n%s",
			strings.TrimSpace(res.raw), harness.DumpRuleSet(id2))
	}
	if trips < 5 {
		t.Errorf("only %d verified round trips across the restart (want ≥5) — asserted by BYTES "+
			"read back rather than by the socket surviving, because a write() to a dead peer "+
			"typically succeeds once before EPIPE (the documented invariant ⑰'s measurement discipline)\nraw: %s",
			trips, strings.TrimSpace(res.raw))
	}
	// THE COUNTER IS NOT COMPARED ACROSS THE RESTART: a freshly loaded table starts its
	// named counters at ZERO, so the reading legitimately goes 1 → 0 and a "must have
	// advanced" comparison would fail on a working engine while passing on one that never
	// re-installed anything. The round-trip count above carries the assertion; this
	// counter is read only as "the new table is counting", never as a delta.
	after := harness.NftCounter(t, id2, "allow0_in")
	if after < 0 {
		t.Errorf("after the restart the re-adopted rule set has no input-side allow counter — the "+
			"rule set was not re-installed at all, and the surviving flow proves nothing about "+
			"enforcement (the documented invariant: a rule set is crash-surviving host state)\n%s", harness.DumpRuleSet(id2))
	}
	t.Logf("item 13b: input-side allow counter reads %d after the restart (it was %d before). A reset "+
		"is EXPECTED: the atomic re-install that keeps the flow alive loads a fresh table, so the "+
		"assertion is the verified round-trip count, not a counter delta (the documented invariant ㉒)", after, progressBefore)
	t.Logf("item 13b: %d verified round trips across a control-plane restart + Adopt", trips)
}

// --- item 14: publish, in five separately-falsifiable questions ------------------

// Five questions rather than one "is it reachable", because each has a
// measured counter-example of its own (the documented invariant ⑬–⑯) and a single reachability check passes
// while four of the five are broken.
func TestPublishIsReachableFromOutsideAndFromTheHostAndSurvivesResume(t *testing.T) {
	eng, base := harness.Engine(t, netEngineOpts(t))
	_ = base
	ctx := context.Background()

	// A guest-side listener bound 0.0.0.0 — the documented invariant requires that, and semantic ① is
	// explicit that a loopback-bound service is NOT publishable.
	harness.InjectStaticBin(t, base+"/templates", "bare", "serve", guestServeSrc)

	d := declDeny()
	d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}
	id, cl := createRunning(t, eng, d)

	// Start the guest service.
	sctx, scancel := context.WithTimeout(ctx, 60*time.Second)
	defer scancel()
	if _, err := cl.Start(sctx, transport.StartSpec{Cmd: "/usr/bin/serve", Args: []string{"8080"}}); err != nil {
		t.Fatalf("start the guest service: %v", err)
	}
	time.Sleep(700 * time.Millisecond)

	// The engine's delivered address (the documented invariant: the engine delivers a `host ip:port`).
	netInfo, err := eng.Mediator.Network(ctx, id)
	endpoints := netInfo.PublishEndpoints
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0] == "" {
		t.Fatalf("no host endpoint reported for a running publish: %v", endpoints)
	}
	endpoint := endpoints[0]
	t.Logf("item 14: published endpoint = %s", endpoint)

	// (i) FROM OUTSIDE THE /30 — not merely from the host. Without the companion SNAT
	// this is precisely the case that times out: the guest's reply has nowhere to go
	// (measured to_guest=2 / from_guest=0, and adding the SNAT made the same flow
	// connect immediately — the documented invariant ⑬), and the host's own dial in (ii) would NOT have caught
	// it. Measured the confusion directly: a host-side dial succeeded and returned the
	// guest's banner while `pub0_in_dnat` counted ZERO, because host-originated traffic
	// does not traverse prerouting at all (the documented invariant ⑭). Hence the split into two questions, and
	// dialFromForeignNetns rather than this host.
	t.Run("(i) reachable from outside the sandbox's own /30, with the SNAT in place", func(t *testing.T) {
		out, derr := dialFromForeignNetns(t, endpoint)
		if derr != nil {
			t.Fatalf("an outside caller could not reach the published endpoint %s: %v (%s) — WITHOUT "+
				"the companion SNAT to this sandbox's own /30 gateway, a caller from outside the /30 "+
				"gets exactly this timeout while the guest's SYN arrives fine (measured to_guest=2 / "+
				"from_guest=0, the documented invariant ⑬)\n%s", endpoint, derr, out, harness.DumpRuleSet(id))
		}
		if !strings.Contains(out, "hello-from-GUEST") {
			t.Errorf("an outside caller read %q from the published endpoint, want the guest's banner. "+
				"`connected` with no payload is the HALF-TORN signature — DNAT present, companion SNAT "+
				"missing — which is the hardest publish failure to diagnose and is easily misread as a "+
				"guest application fault (the documented invariant ⑰)", out)
		}
		// NOW the prerouting DNAT is the rule that carried it, which is the whole point of
		// dialing from a foreign netns.
		if c := harness.NftCounter(t, id, "pub0_in_dnat"); c <= 0 {
			t.Errorf("the PREROUTING DNAT counted %d for a call from outside the /30 — that is the "+
				"rule this path depends on (a host-originated dial would not touch it at all, the documented invariant ⑭)", c)
		}
		if c := harness.NftCounter(t, id, "pub_return_snat"); c <= 0 {
			t.Errorf("the return SNAT counted %d — without it a publish works only on a sandbox that "+
				"happens to carry a default route, and times out on one that does not (the documented invariant ⑬)", c)
		}
	})

	// (iii) The companion accept is on FORWARD, and the input side stays at ZERO. Read
	// AFTER (i), because only an externally-originated flow is routed to the guest —
	// a host-originated one is delivered locally and never reaches `forward`.
	// Read as per-hook counters, because the hook assignment is the OPPOSITE of
	// `via[]`'s and copying one from the other is the mistake the documented invariant ⑮ warns about.
	t.Run("(iii) the companion accept is on forward, and the input side stays zero", func(t *testing.T) {
		if c := harness.NftCounter(t, id, "pub0_fwd"); c <= 0 {
			t.Errorf("the publish companion accept on FORWARD counted %d — post-DNAT the destination "+
				"is the GUEST IP, so the flow must be routed onward and lands on `forward`; a steered "+
				"flow's destination is host-local and lands on `input`. Same prerouting DNAT, two "+
				"different downstream hooks (the documented invariant ⑮)", c)
		}
		// AND THE INPUT SIDE STAYS ZERO — asserted, since it is half this subtest's claim.
		//
		// Zero is reachable rather than aspirational: this declaration is `deny` with no
		// `via[]` and no `allow[]`, the guest runs nothing that dials out, and the engine's
		// own transport replies are `established` and so met by the ct-state accept above the
		// floor, never by the floor's drop.
		if c := harness.NftCounter(t, id, "in_floor"); c != 0 {
			t.Errorf("the input-side floor counted %d during an inbound publish flow — post-DNAT the "+
				"destination must be the GUEST IP and land on `forward`; anything reaching the input "+
				"floor means the rewrite produced a host-local destination, and the forward-side "+
				"assertion above cannot see that (the documented invariant ⑮)\n%s", c, harness.DumpRuleSet(id))
		}
	})

	// (ii) FROM THE HOST ITSELF — a SECOND, independent rule, carried by the output-hook
	// DNAT alone. Neither of (i)/(ii) is evidence for the other (the documented invariant ⑭).
	t.Run("(ii) reachable from the host itself, via its own output-hook DNAT", func(t *testing.T) {
		before := harness.NftCounter(t, id, "pub0_out_dnat")
		conn, derr := net.DialTimeout("tcp", endpoint, 10*time.Second)
		if derr != nil {
			t.Fatalf("the HOST could not reach the published endpoint %s: %v — host-originated traffic "+
				"never traverses `prerouting`, so this path needs its OWN DNAT on the output hook "+
				"(the documented invariant ⑭)\n%s", endpoint, derr, harness.DumpRuleSet(id))
		}
		conn.Close()
		after := harness.NftCounter(t, id, "pub0_out_dnat")
		if after <= before {
			t.Errorf("the OUTPUT-hook DNAT counted %d → %d for a host-originated dial — if this stays "+
				"flat while the dial works, the traffic is taking some other path and the two "+
				"assertions (i) and (ii) are not independent after all (the documented invariant ⑭)", before, after)
		}
	})

	// (iv) A guest service bound to LOOPBACK is refused, fast. the documented invariant semantic ① promises
	// exactly this much and DELIBERATELY NOT attribution: measured, this case and "no
	// listener at all" are byte-identical to the caller (both RSTs), so a caller cannot
	// tell them apart and the contract does not pretend otherwise (the documented invariant ⑯).
	t.Run("(iv) a loopback-bound guest service is refused fast, without claiming attribution", func(t *testing.T) {
		d2 := declDeny()
		d2.Ingress.Publish = []model.NetPublish{{GuestPort: 9091, Proto: model.NetTCP}}
		id2, cl2 := createRunning(t, eng, d2)
		lctx, lcancel := context.WithTimeout(ctx, 60*time.Second)
		defer lcancel()
		// Bound to 127.0.0.1 INSIDE the guest: the sandbox has its own netns and its own
		// loopback, so the forwarded packet lands on the guest's netstack and nothing is
		// listening on the address it arrives at.
		if _, err := cl2.Start(lctx, transport.StartSpec{
			Cmd: "/usr/bin/serve", Args: []string{"9091", "127.0.0.1"},
		}); err != nil {
			t.Fatalf("start the loopback-bound guest service: %v", err)
		}
		time.Sleep(700 * time.Millisecond)
		ni2, nerr := eng.Mediator.Network(ctx, id2)
		eps := ni2.PublishEndpoints
		if nerr != nil || len(eps) == 0 || eps[0] == "" {
			t.Fatalf("no endpoint for the loopback case: %v %v", eps, nerr)
		}
		start := time.Now()
		conn, derr := net.DialTimeout("tcp", eps[0], 10*time.Second)
		el := time.Since(start)
		if derr == nil {
			conn.Close()
			t.Fatalf("a guest service bound to 127.0.0.1 was reachable through publish — the sandbox " +
				"has its own netns and its own loopback, so the forwarded packet cannot reach it (the documented invariant " +
				"semantic ①)")
		}
		if el >= 5*time.Second {
			t.Errorf("the refusal took %s — the contract is `connection refused, FAST`, not a hang "+
				"(the documented invariant semantic ①)", el)
		}
		// Deliberately NOT asserting that the error is attributable: measured, this is
		// byte-identical to "no listener in the guest at all" (both are gVisor netstack
		// RSTs, sub-millisecond), so the documented invariant promises refusal and explicitly not attribution.
		// Attribution is only possible ENGINE-side, by reading which rules were installed.
		t.Logf("item 14(iv): refused in %s (%v) — indistinguishable from `no listener`, which is why "+
			"the documented invariant promises refusal without attribution (the documented invariant ⑯)", el, derr)
	})

	// (v) The two failure signatures of a broken publish are DIFFERENT and must not be
	// merged into "unreachable": missing DNAT → refused FAST; missing companion SNAT →
	// CONNECTED THEN READ TIMEOUT. A half-torn rule set breaks in the second way, which
	// is the hardest to diagnose (the documented invariant ⑰).
	//
	// Asserted structurally rather than by dismantling a live rule set: the generator
	// emits all four parts as ONE unit, so the "missing SNAT" state is unreachable
	// through any engine path. The unit-level assertion that all four exist is in
	// TestOnePublishEmitsAllFourParts; here we pin that the WORKING case is the
	// connected-WITH-payload one, i.e. not the half-torn signature.
	t.Run("(v) the working case is `connected with payload`, not the half-torn signature", func(t *testing.T) {
		conn, derr := net.DialTimeout("tcp", endpoint, 10*time.Second)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			t.Fatalf("CONNECTED THEN READ TIMEOUT — that is the missing-SNAT signature exactly, and it " +
				"is the one that gets misread as a guest application fault. The four parts of a " +
				"publish are emitted as one unit precisely so this state is unreachable (the documented invariant ⑰)")
		}
		if n == 0 {
			t.Error("read zero bytes without an error")
		}
	})

	// 14: SURVIVES RESUME. The declaration is a Binding, so the next placement
	// regenerates the rules — with a NEW host endpoint, because that endpoint is a
	// placement product (the documented invariant).
	t.Run("survives pause/resume, with a rebound endpoint", func(t *testing.T) {
		if err := eng.Mediator.Pause(ctx, id); err != nil {
			t.Fatalf("pause: %v", err)
		}
		// While paused there is NO endpoint to report — the documented invariant omits the key rather than
		// reporting an empty string, because the endpoint is a placement product.
		niS, _ := eng.Mediator.Network(ctx, id)
		state, eps := niS.State, niS.PublishEndpoints
		if state != model.Paused {
			t.Errorf("state = %s, want paused", state)
		}
		for _, ep := range eps {
			if ep != "" {
				t.Errorf("a paused sandbox reported a host endpoint (%q) — the endpoint is a placement "+
					"product (the documented invariant) and a paused sandbox has none", ep)
			}
		}
		if err := eng.Mediator.Resume(ctx, id); err != nil {
			t.Fatalf("resume: %v", err)
		}
		ni2b, _ := eng.Mediator.Network(ctx, id)
		eps2 := ni2b.PublishEndpoints
		if len(eps2) != 1 || eps2[0] == "" {
			t.Fatalf("after resume there is no host endpoint: %v — the declaration is a Binding, so "+
				"the next placement must regenerate its rules from the record (the documented invariant / the documented invariant)", eps2)
		}
		// Restart the guest service on the new placement and check the NEW endpoint works.
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		defer rcancel()
		cl3, cerr := eng.Mediator.Client(id)
		if cerr != nil {
			t.Fatalf("client after resume: %v", cerr)
		}
		if _, err := cl3.Start(rctx, transport.StartSpec{Cmd: "/usr/bin/serve", Args: []string{"8080"}}); err != nil {
			t.Fatalf("restart the guest service: %v", err)
		}
		time.Sleep(700 * time.Millisecond)
		conn, derr := net.DialTimeout("tcp", eps2[0], 10*time.Second)
		if derr != nil {
			t.Fatalf("the publish did not survive resume: %v at %s\n%s", derr, eps2[0], harness.DumpRuleSet(id))
		}
		conn.Close()
		t.Logf("publish survived resume: %s → %s (the endpoint is rebound, the documented invariant)", endpoint, eps2[0])
	})
}

// A publish on a PAUSED sandbox records only — 200,
// `applies_at: next-resume`, no `host_endpoint`, NO rules on the host yet — and then
// appears after resume. Plus the the documented invariant discipline that it must not touch UpdatedAt.
func TestPublishOnAPausedSandboxRecordsWithoutInstallingRules(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, _ := createRunning(t, eng, declDeny())
	if err := eng.Mediator.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	rec, err := eng.Mediator.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	updatedBefore := rec.UpdatedAt

	// A paused sandbox holds NO placement, so it has no rule-set table at all.
	if n := harness.NftCounter(t, id, "in_floor"); n >= 0 {
		t.Errorf("a paused sandbox still has a rule-set table — pause releases the placement, and " +
			"the rule set is placement-local (the documented invariant)")
	}
	item, endpoint, now, perr := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if perr != nil {
		t.Fatalf("publish on a paused sandbox was refused: %v — `paused` is legal, and refusing it "+
			"would make publish the ONLY field harder to write while paused than while running, "+
			"pushing the caller onto the whole-binding PATCH that the documented invariant documents as dangerous (the documented invariant)", perr)
	}
	if now {
		t.Error("a paused publish reported `applies_at: now` — there is no placement to install into")
	}
	if endpoint != "" {
		t.Errorf("a paused publish reported a host endpoint (%q) — the documented invariant omits the key, because the "+
			"endpoint is a placement product and there is no placement", endpoint)
	}
	if item.GuestPort != 8080 {
		t.Errorf("item = %+v", item)
	}
	// It LANDED IN THE RECORD, which is what makes it survive to the next placement.
	rec2, err := eng.Mediator.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec2.Network.Ingress.Publish) != 1 {
		t.Fatalf("the declaration holds %d publish items after a paused publish, want 1 — it must "+
			"land in the record, or it would not survive to the next placement (the documented invariant)",
			len(rec2.Network.Ingress.Publish))
	}
	// the documented invariant discipline ② — the obligation exists ONLY because `paused` is allowed.
	if !rec2.UpdatedAt.Equal(updatedBefore) {
		t.Errorf("the paused publish moved UpdatedAt (%s → %s) — `retention` is measured from that "+
			"column (the documented invariant discipline ②), so this would silently extend the eviction deadline as a "+
			"side effect of adding a port. The obligation exists ONLY because `paused` is allowed",
			updatedBefore, rec2.UpdatedAt)
	}
	// And after resume the rules exist.
	if err := eng.Mediator.Resume(ctx, id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := harness.NftCounter(t, id, "pub0_in_dnat"); n < 0 {
		t.Errorf("after resume the publish has no DNAT rule — the declaration is a Binding and the "+
			"next placement regenerates from it (the documented invariant)\n%s", harness.DumpRuleSet(id))
	}
	niE, _ := eng.Mediator.Network(ctx, id)
	eps := niE.PublishEndpoints
	if len(eps) != 1 || eps[0] == "" {
		t.Errorf("no endpoint after resume: %v", eps)
	}
}

// Repeated publish of the same `{guest_port, proto}` is idempotent —
// one item in the declaration and ONE DNAT on the host.
//
// Both hazards — nft silently accepting a duplicate DNAT, and a later PATCH removing only
// one of two recorded items — are in the failure message below (measured, the documented invariant).
func TestRepeatedPublishIsIdempotentOnTheHostToo(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, _ := createRunning(t, eng, declDeny())

	for i := 0; i < 2; i++ {
		if _, _, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	rec, err := eng.Mediator.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Network.Ingress.Publish); n != 1 {
		t.Errorf("the declaration holds %d items after two identical publishes, want 1 — nft accepts "+
			"two identical DNATs silently and the second never matches, so the substrate will not "+
			"find this for us; and with two recorded items a later PATCH removing one leaves the "+
			"port open (the documented invariant)", n)
	}
	// One DNAT on the host, counted by listing the rules rather than the counters (a
	// duplicate would be a second rule with its own name).
	dump := harness.DumpRuleSet(id)
	if n := strings.Count(dump, "pub0_in_dnat"); n < 1 {
		t.Errorf("no publish DNAT installed:\n%s", dump)
	}
	if strings.Contains(dump, "pub1_in_dnat") {
		t.Errorf("a SECOND publish DNAT was installed for an idempotent repeat:\n%s", dump)
	}
}

// Item 14d, the half a declaration-layer assertion cannot reach: the repeat must return
// THE SAME host endpoint, and hot-adding a publish must not move the endpoints already
// bound at that placement.
//
// The idempotency the documented invariant promises is "return the existing item" — and the
// declaration layer alone cannot deliver it: `AddPublish` reports added=false and writes
// nothing, but the mediator still runs the install path (record and rules are one
// un-halvable write), so the provider re-binds. A provider that re-draws the endpoints on
// every reinstall therefore answers the second identical POST with a DIFFERENT
// `host_endpoint`, and hot-adding B silently moves A's — while the declaration, the
// counters and the reachability assertions all keep passing.
//
// Why this must be a substrate test rather than only a unit one: the endpoints asserted
// here are the ones a caller actually reads, produced by the real allocator across three
// real ReinstallRuleSet calls into real nft.
func TestHotAddingAPublishDoesNotMoveTheEndpointsAlreadyBound(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, _ := createRunning(t, eng, declDeny())

	_, epA, now, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish 8080/tcp: %v", err)
	}
	if !now || epA == "" {
		t.Fatalf("publish on a running sandbox: applies-now=%v endpoint=%q; want both set", now, epA)
	}

	// (1) The IDEMPOTENT REPEAT returns the same endpoint.
	_, epRepeat, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("repeat publish: %v", err)
	}
	if epRepeat != epA {
		t.Errorf("the idempotent repeat answered with %s, the first call with %s. the documented invariant makes a repeated "+
			"`{guest_port, proto}` return THE EXISTING one — a caller retrying a request whose 200 it "+
			"never saw would otherwise be told its port had moved\n%s", epRepeat, epA, harness.DumpRuleSet(id))
	}

	// (2) HOT-ADDING a different publish leaves the first one's endpoint alone. the documented invariant
	// endorses the hot-add on `running` precisely because a publish is PURELY ADDITIVE.
	_, epB, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 9090, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish 9090/tcp: %v", err)
	}
	if epB == epA {
		t.Fatalf("two publishes share the host endpoint %s", epA)
	}
	niX, err := eng.Mediator.Network(ctx, id)
	eps := niX.PublishEndpoints
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Fatalf("read back %d endpoints for two publishes: %v", len(eps), eps)
	}
	if eps[0] != epA {
		t.Errorf("hot-adding 9090 moved 8080's endpoint: %s → %s. That breaks the pure-additivity the documented invariant "+
			"rests on when it allows this write on a `running` sandbox — nothing in the record changed "+
			"for 8080, so a caller holding the endpoint it was handed must not lose it\n%s",
			epA, eps[0], harness.DumpRuleSet(id))
	}
	// The installed rules must agree with what the read face reports — a re-bind that
	// updated the handle but not the table would be the same bug pointing the other way.
	dump := harness.DumpRuleSet(id)
	for _, ep := range eps {
		ap, perr := netip.ParseAddrPort(ep)
		if perr != nil {
			t.Fatalf("endpoint %q is not ip:port: %v", ep, perr)
		}
		if !strings.Contains(dump, fmt.Sprintf("dport %d", ap.Port())) {
			t.Errorf("the reported endpoint %s has no matching dport clause installed — the read face "+
				"and the kernel disagree, which the documented invariant's biconditional says cannot persist\n%s", ep, dump)
		}
	}
}

// tcp/8080 and udp/8080 are TWO publishes, on the real substrate and across a resume.
//
// the documented invariant keys a publish on `{guest_port, proto}`: the declaration layer dedupes on the pair
// and the documented invariant defines idempotency on it, so this declaration is legal and the generator
// emits four DNATs for it. A provider binding endpoints per GUEST PORT refuses the second
// item as a duplicate — blaming the declaration layer for a key width only the provider
// chose. And because the declaration is durable, that refusal then repeats at EVERY
// placement: the resume below is the assertion that matters, since the sandbox's entry
// point into the broken state was a `POST …/publish` that returned 200.
func TestTCPAndUDPOnOneGuestPortAreTwoPublishesOnTheSubstrate(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, _ := createRunning(t, eng, declDeny())

	_, tcpEP, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish tcp/8080: %v", err)
	}
	_, udpEP, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetUDP})
	if err != nil {
		t.Fatalf("publish udp/8080 alongside tcp/8080: %v — both items pass the declaration layer "+
			"(the documented invariant keys on the PAIR) and Generate emits four DNATs, so a refusal here is the "+
			"provider's key width mistaken for a duplicate\n%s", err, harness.DumpRuleSet(id))
	}
	if tcpEP == udpEP {
		t.Errorf("tcp/8080 and udp/8080 got the same endpoint %s — one endpoint cannot carry both", tcpEP)
	}

	// Both protocols' DNATs are installed, each narrowed by its OWN endpoint port.
	dump := harness.DumpRuleSet(id)
	for _, tc := range []struct {
		proto, ep string
	}{{"tcp", tcpEP}, {"udp", udpEP}} {
		ap, perr := netip.ParseAddrPort(tc.ep)
		if perr != nil {
			t.Fatalf("endpoint %q: %v", tc.ep, perr)
		}
		want := fmt.Sprintf("%s dport %d", tc.proto, ap.Port())
		if !strings.Contains(dump, want) {
			t.Errorf("no %q clause installed — each item of the pair needs its own endpoint and its "+
				"own DNAT\n%s", want, dump)
		}
	}

	// THE RESUME: the declaration outlives this placement, so a pair-refusing binding
	// makes the sandbox permanently unresumable.
	if err := eng.Mediator.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := eng.Mediator.Resume(ctx, id); err != nil {
		t.Fatalf("resume with `[{8080,tcp},{8080,udp}]` recorded: %v — the declaration is durable and "+
			"every placement regenerates from it, so this sandbox would fail EVERY resume forever", err)
	}
	niX, err := eng.Mediator.Network(ctx, id)
	eps := niX.PublishEndpoints
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0] == "" || eps[1] == "" {
		t.Errorf("after resume the pair reports endpoints %v; want two bound\n%s", eps, harness.DumpRuleSet(id))
	}
}

// Adopt keeps a survivor's publish endpoints instead of re-issuing them — asserted with
// TWO survivors, because a single one cannot distinguish "recovered" from "re-issued in
// the same order by luck".
//
// the documented invariant promises Adopt yields the same endpoint with no new epoch: the isolation unit never
// restarted. A restarted process's port allocator starts EMPTY, and (unlike `ipSeq`) the
// publish cursor is not randomized, so without a read-back both survivors are re-issued
// the range's first ports in `ListSandboxes` order — and adopted in the other order they
// come back SWAPPED, which is worse than stale (the documented invariant licenses a NEW placement's endpoint
// differing, not a live placement's moving to a different sandbox).
func TestAdoptKeepsEachSurvivorsPublishEndpoints(t *testing.T) {
	base := t.TempDir()
	opts := netEngineOpts(t)
	opts.BaseDir = base
	opts.StorePath = base + "/store.db"
	eng, _ := harness.Engine(t, opts)
	ctx := context.Background()

	d := declDeny()
	d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}
	// THREE sandboxes, and the middle one is then DESTROYED. The gap that leaves in the
	// port sequence is what makes this test falsifiable, and it is worth the extra
	// sandbox: with two adjacent survivors, a re-issuing Adopt that happens to walk them
	// in creation order hands back the very same ports and the test passes on LUCK
	// (measured — the falsification run against a disabled read-back PASSED that way).
	// With a hole in the sequence, no re-issue can reproduce the survivors' ports: a
	// fresh cursor starts at the range base and allocates CONTIGUOUSLY, so the second
	// survivor cannot land on its old port in either adoption order.
	idA, _ := createRunning(t, eng, d)
	idGap, _ := createRunning(t, eng, d)
	idB, _ := createRunning(t, eng, d)
	if err := eng.Mediator.Destroy(ctx, idGap); err != nil {
		t.Fatalf("destroy the middle sandbox: %v", err)
	}

	epOf := func(t *testing.T, e *orchestrator.Engine, id model.SandboxID) string {
		t.Helper()
		niX, err := e.Mediator.Network(ctx, id)
		eps := niX.PublishEndpoints
		if err != nil {
			t.Fatalf("read network %s: %v", id, err)
		}
		if len(eps) != 1 || eps[0] == "" {
			t.Fatalf("no endpoint for %s: %v", id, eps)
		}
		return eps[0]
	}
	beforeA, beforeB := epOf(t, eng, idA), epOf(t, eng, idB)
	if beforeA == beforeB {
		t.Fatalf("two sandboxes were handed the same host endpoint %s", beforeA)
	}
	t.Logf("before the restart: %s → %s, %s → %s", idA, beforeA, idB, beforeB)

	// The restart: a new process over the same store and the same live placements.
	// harness.Restart rather than Engine — see harness.Restart's doc.
	eng2 := harness.Restart(t, base, opts)

	afterA, afterB := epOf(t, eng2, idA), epOf(t, eng2, idB)
	t.Logf("after the restart: %s → %s, %s → %s", idA, afterA, idB, afterB)
	if afterA != beforeA || afterB != beforeB {
		t.Errorf("Adopt re-issued the publish endpoints instead of recovering them:\n"+
			"  %s: %s → %s\n  %s: %s → %s\n"+
			"the documented invariant promises Adopt returns the SAME endpoint with no new epoch — the isolation unit "+
			"never restarted, so nothing about these placements is new. And the re-issue is not merely "+
			"stale: with the port cursor starting from the range base in ListSandboxes order, two "+
			"survivors SWAP, and a caller's cached endpoint then reaches the OTHER SANDBOX",
			idA, beforeA, afterA, idB, beforeB, afterB)
	}
	// A swap specifically — called out separately because it is the harmful shape and a
	// generic inequality message would not name it.
	if afterA == beforeB && afterB == beforeA {
		t.Errorf("the two survivors' endpoints were SWAPPED by adoption (%s now answers for %s) — this "+
			"is the cross-sandbox misdirection, not just a rebound endpoint", beforeB, idA)
	}
	// The recovered endpoints must also be the ones actually INSTALLED, and the allocator
	// must now hold them: a recovery that fixed the read face but left the ports free
	// would hand one to the next sandbox created.
	for _, tc := range []struct {
		id model.SandboxID
		ep string
	}{{idA, afterA}, {idB, afterB}} {
		ap, perr := netip.ParseAddrPort(tc.ep)
		if perr != nil {
			t.Fatalf("endpoint %q: %v", tc.ep, perr)
		}
		if dump := harness.DumpRuleSet(tc.id); !strings.Contains(dump, fmt.Sprintf("dport %d", ap.Port())) {
			t.Errorf("%s reports endpoint %s but no matching dport clause is installed\n%s",
				tc.id, tc.ep, dump)
		}
	}
	idC, _ := createRunning(t, eng2, d)
	epC := epOf(t, eng2, idC)
	if epC == afterA || epC == afterB {
		t.Errorf("a sandbox created after the restart was handed %s, which a SURVIVOR already holds — "+
			"the recovered ports must be marked in use, the same obligation markInUse discharges for "+
			"the /30 (the documented invariant)", epC)
	}
}

// --- item 15: the PATCH read-modify-write hazard --------------------------------

// Writing back a STALE declaration silently drops a publish added in
// between.
//
// the documented invariant makes the read-modify-write cycle a CONTRACT REQUIREMENT rather than advice for
// exactly this reason, and the contract accepts last-writer-wins rather than adding an
// optimistic-concurrency stamp (a record version would be the generation counter the documented invariant
// bans outright). So this test pins the HAZARD as real, which is what justifies the
// requirement — it is not asserting a bug.
func TestAStalePatchSilentlyDropsAHotAddedPublish(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()
	id, _ := createRunning(t, eng, declDeny())
	if err := eng.Mediator.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// (1) The caller reads the declaration.
	niStale, err := eng.Mediator.Network(ctx, id)
	stale := niStale.Decl
	if err != nil {
		t.Fatal(err)
	}
	// (2) Something adds a publish in between — the hot-add write point.
	if _, _, _, err := eng.Mediator.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// (3) The caller writes back the value it read at (1), changing something unrelated.
	stale.Egress.Default = model.NetAllow
	if err := patchDecl(eng, ctx, id, stale); err != nil {
		t.Fatalf("patch: %v", err)
	}

	rec, err := eng.Mediator.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Network.Ingress.Publish) != 0 {
		t.Fatalf("expected the stale PATCH to drop the publish (that is the documented hazard), but "+
			"the declaration still holds %d items — if the engine has started merging fields, the documented invariant's "+
			"read-modify-write requirement and this test both need revisiting",
			len(rec.Network.Ingress.Publish))
	}
	t.Log("item 15: a stale PATCH DID drop the hot-added publish, as the documented invariant documents. This is why " +
		"that GET is a contract requirement rather than a suggestion: the engine does not merge " +
		"fields and does not carry an optimistic-concurrency stamp (a record version would be the " +
		"generation counter the documented invariant bans). The safe sequence is pause → GET → PATCH → resume, and the " +
		"additive fix is a delete endpoint for publish, which removes the conflict CLASS")

	// The paired direction, which is the more dangerous one the documented invariant names: writing back a
	// declaration that still CONTAINS an already-removed publish would REOPEN an inbound
	// port. Same mechanism, opposite sign — and the engine cannot tell the two apart,
	// which is the whole reason the cycle is the caller's responsibility.
	niPub, _ := eng.Mediator.Network(ctx, id)
	withPub := niPub.Decl
	withPub.Ingress.Publish = []model.NetPublish{{GuestPort: 9099, Proto: model.NetTCP}}
	if err := patchDecl(eng, ctx, id, withPub); err != nil {
		t.Fatalf("patch: %v", err)
	}
	niTrim, _ := eng.Mediator.Network(ctx, id)
	trimmed := niTrim.Decl
	trimmed.Ingress.Publish = nil
	if err := patchDecl(eng, ctx, id, trimmed); err != nil {
		t.Fatalf("patch: %v", err)
	}
	rec2, _ := eng.Mediator.Get(ctx, id)
	if len(rec2.Network.Ingress.Publish) != 0 {
		t.Errorf("removing a publish through the whole-binding PATCH did not take effect (%d items) — "+
			"that PATCH is the ONLY revocation path today, since there is no un-publish endpoint "+
			"(deliberately: on a running sandbox a revocation could only promise `no new connections`, "+
			"never `the port is closed`, because conntrack holds the NAT binding — the documented invariant ⑰)",
			len(rec2.Network.Ingress.Publish))
	}
}

// --- item 10b: IPv6 is covered by the SAME floor ---------------------------------

// The IPv6 arm, which CANNOT be skipped even though the documented invariant ⑪ already measured an `inet`
// ct-state rule covering IPv6: that measurement was
// taken on a netns peer against a hand-written rule, while what has to be proved here
// is that THE RULE SET THIS ENGINE RENDERS covers v6 under runsc.
//
// The stake is concrete: the engine touches IPv6 nowhere, yet `disable_ipv6` defaults
// to 0, so the kernel gives every sandbox veth a host-side `fe80::/64` automatically
// and this host's eth0 carries real IPv6. A floor covering only IPv4 would pass every
// other assertion in this file while the whole family stayed open.
//
// Two targets, and the choice of the second is itself an assertion: eth0's GLOBAL
// address, never its link-local. A link-local on eth0 is unreachable from the sandbox
// anyway (different link), so asserting on it would assert something vacuously true
// (the documented invariant ⑪).
func TestIPv6IsCoveredByTheSameFloor(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	id, cl := createRunning(t, eng, declDeny())

	// The host-side veth's link-local, which the KERNEL assigned — not the engine.
	vethLL := hostVethLinkLocal(t, id)
	// eth0's GLOBAL v6 address, if this host has one.
	global := hostGlobalIPv6(t)

	// NO SKIP on the link-local: hostVethLinkLocal is fatal on every failure path, so the
	// counter-backed cell below always runs. The global address stays optional (guarded at
	// its own site) because it depends on this host's routed connectivity; the link-local
	// does not, since the kernel assigns one to every veth.

	// A v6 listener on the host, so "unreachable" is not an artifact of nothing being
	// there. Bound to the wildcard so it answers on every host v6 address.
	ln, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Skipf("cannot listen on IPv6 on this host: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_, _ = c.Write([]byte("hello-from-HOST-V6"))
			_ = c.Close()
		}
	}()
	_, portStr := portOf(t, ln.Addr().String())

	// THE GLOBAL-ADDRESS CELL IS NOT A COUNTER ASSERTION ON THIS HOST, and the reason is
	// a constructive limit rather than a gap in the floor — measured here rather than
	// assumed.
	//
	// The engine configures NO IPv6 address on the guest side of the veth (it touches
	// IPv6 nowhere), so a guest dial to a routed v6 address fails with
	// `network is unreachable` in 0 ms: there is no source address and no route, so the
	// packet never leaves the sandbox and no host rule is evaluated. Measured: the probe
	// failed instantly with every v6 counter flat at 0, including under
	// `net.ipv6.conf.all.forwarding=1`.
	//
	// the documented invariant ⑳ names exactly this shape and its consequence for acceptance: "unreachable with
	// a zero counter" is INDISTINGUISHABLE from "the floor was bypassed", so demanding
	// counter movement here would be an unsatisfiable assertion rather than a finding.
	// The honest report is that this cell measures the absence of guest v6 configuration,
	// not the engine's floor.
	//
	// The LINK-LOCAL cell below is what carries this item instead: the guest CAN reach
	// that address (on-link, no route needed), so its counter assertion is satisfiable.
	if global != "" {
		probeTarget := "[" + global + "]:" + portStr
		res := runGuestProbe(t, cl, probeTarget, false)
		if res.reachable() {
			t.Errorf("the guest REACHED the host's global IPv6 address %s (payload %s) — the engine "+
				"touches IPv6 nowhere while the kernel configures it anyway, so this would be the "+
				"whole family open with every IPv4 assertion still passing (the documented invariant ⑪)",
				global, res.payload)
		}
		t.Logf("item 10b: global v6 (%s) → %s in %d ms. NOT asserted by counter: the engine assigns "+
			"the guest no v6 address, so the packet never leaves the sandbox and every v6 counter is "+
			"flat — which the documented invariant ⑳ says is indistinguishable from a bypassed floor and therefore not a "+
			"valid assertion. The link-local cell below is the one that carries this item",
			global, res.outcome, res.ms)
	}

	for _, tc := range []struct{ name, addr, counter, why string }{{
		// The veth's link-local is an address of the HOST itself, so it lands on `input`
		// and is covered by the pure ct-state rule — the SAME rule that covers IPv4, with
		// no family qualifier and no `ip6` arm (the documented invariant ⑪).
		name:    "the host-side veth's kernel-assigned link-local",
		addr:    vethLL,
		counter: "in_floor",
		why: "the engine never configured this address — the kernel did, because disable_ipv6 " +
			"defaults to 0. It is an address of the host, so the family-UNQUALIFIED ct-state rule " +
			"on `input` is what must cover it (the documented invariant ⑧ ⑪)",
	}} {
		if tc.addr == "" {
			t.Logf("skipping %s: not present on this host", tc.name)
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			target := "[" + tc.addr + "]:" + portStr
			before := harness.NftCounter(t, id, tc.counter)
			res := runGuestProbe(t, cl, target, false)
			after := harness.NftCounter(t, id, tc.counter)
			if res.reachable() {
				t.Fatalf("the guest REACHED %s over IPv6 (payload %s) — the engine touches IPv6 nowhere "+
					"while the kernel configures it anyway, so a floor covering only IPv4 passes every "+
					"other assertion in this file with the whole family open: %s\n%s",
					target, res.payload, tc.why, harness.DumpRuleSet(id))
			}
			mustCounterMoved(t, id, tc.counter, before, after, tc.name)
		})
	}

	// And the structural half: the rendered floor carries NO `meta nfproto ipv6` arm, so
	// v6 rides the SAME rule as v4 rather than a mechanism that could rot independently.
	dump := harness.DumpRuleSet(id)
	if strings.Contains(dump, "nfproto ipv6") {
		t.Errorf("the rendered rule set carries a `meta nfproto ipv6` arm — one family-unqualified "+
			"ct-state rule already covers IPv6, and the explicit arm was measured counting exactly "+
			"the packets the agnostic rule had caught (the documented invariant ⑪)\n%s", dump)
	}
}

// hostVethLinkLocal returns the KERNEL-assigned fe80:: address on this sandbox's
// host-side veth. It never returns "" — every failure path is fatal (see below).
func hostVethLinkLocal(t *testing.T, id model.SandboxID) string {
	t.Helper()
	// EVERY failure here is FATAL, not an empty string. The kernel assigns this address
	// unconditionally (the engine never touches IPv6), so "not found" means the scrape
	// broke — and returning "" made the caller skip the only counter-backed IPv6
	// assertion in the suite while still reporting a pass. A silently-skipped assertion
	// is the failure mode this suite's whole discipline exists to rule out.
	//
	// The veth name is a one-way hash of the SandboxID, so it is found by listing the
	// engine's rule-set table rather than recomputed here.
	out, err := exec.Command("nft", "list", "table", "inet", "sbxnet_"+string(id)).Output()
	if err != nil {
		t.Fatalf("could not list the rule-set table for %s (%v) — the veth name is scraped from it, "+
			"and without it this test would silently skip its only counter-backed v6 assertion", id, err)
	}
	veth := ""
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, `"vh`) {
			veth = strings.Trim(f, `"`)
			break
		}
	}
	if veth == "" {
		t.Fatalf("no host veth name in the rule set for %s — every per-sandbox rule is narrowed by "+
			"`iifname`, so its absence means the rule set is not what this test assumes:\n%s", id, out)
	}
	addrs, err := exec.Command("ip", "-6", "-o", "addr", "show", "dev", veth, "scope", "link").Output()
	if err != nil {
		t.Fatalf("could not read %s's link-local address (%v) — the kernel assigns it "+
			"unconditionally, so this is a broken fixture, not a host without IPv6", veth, err)
	}
	for _, line := range strings.Split(string(addrs), "\n") {
		fs := strings.Fields(line)
		for i, f := range fs {
			if f == "inet6" && i+1 < len(fs) {
				return strings.SplitN(fs[i+1], "/", 2)[0]
			}
		}
	}
	t.Fatalf("%s carries no link-local IPv6 address — the kernel assigns one to every veth, so "+
		"this is a broken fixture rather than a host without IPv6:\n%s", veth, addrs)
	return ""
}

// hostGlobalIPv6 returns a GLOBAL IPv6 address of this host, or "".
func hostGlobalIPv6(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("ip", "-6", "-o", "addr", "show", "scope", "global").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, f := range strings.Fields(line) {
			if strings.Contains(f, ":") && strings.Contains(f, "/") && !strings.HasPrefix(f, "fe80") {
				addr := strings.SplitN(f, "/", 2)[0]
				if ip, err := netip.ParseAddr(addr); err == nil && ip.Is6() && !ip.IsLoopback() {
					return addr
				}
			}
		}
	}
	return ""
}

// A publish's reverse flow has NOT become an egress path (the documented invariant ④).
//
// THIS IS THE FIRST TIME THIS ASSERTION CAN BE VALID. The spike that tried it had a
// guest with no default route, so its egress died with `net unreachable` BEFORE
// leaving the sandbox and the floor counted 0 — by the acceptance discipline an invalid
// measurement rather than a pass (the documented invariant ⑯). The engine now installs that route for every
// declaration precisely so the probe reaches the floor, which is what makes this
// assertion mean something.
func TestPublishDoesNotOpenAnEgressPathOnTheSubstrate(t *testing.T) {
	eng, base := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()

	harness.InjectStaticBin(t, base+"/templates", "bare", "serve2", guestServeSrc)

	// `deny` + a publish: inbound open on one port, egress declared as nothing.
	d := declDeny()
	d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}
	id, cl := createRunning(t, eng, d)

	sctx, scancel := context.WithTimeout(ctx, 60*time.Second)
	defer scancel()
	if _, err := cl.Start(sctx, transport.StartSpec{Cmd: "/usr/bin/serve2", Args: []string{"8080"}}); err != nil {
		t.Fatalf("start the guest service: %v", err)
	}
	time.Sleep(700 * time.Millisecond)

	// The publish works — otherwise the negative below would be vacuous.
	niX, err := eng.Mediator.Network(ctx, id)
	eps := niX.PublishEndpoints
	if err != nil || len(eps) == 0 || eps[0] == "" {
		t.Fatalf("no published endpoint: %v %v", eps, err)
	}
	conn, derr := net.DialTimeout("tcp", eps[0], 10*time.Second)
	if derr != nil {
		t.Fatalf("the publish itself does not work (%v), so the egress assertion below would be "+
			"vacuous", derr)
	}
	conn.Close()

	// …and the guest's OWN outbound is still refused, with the floor counting it. The
	// companion accept is narrowed by `oifname` + the guest IP + the published port, so
	// it cannot serve as an egress path — but that is the property under test, not an
	// assumption.
	before := harness.NftCounter(t, id, "fwd_default_deny")
	res := runGuestProbe(t, cl, "1.1.1.1:443", false)
	after := harness.NftCounter(t, id, "fwd_default_deny")
	if res.reachable() {
		t.Errorf("with a publish installed the guest reached the public internet (%s) — the publish's "+
			"reverse flow must not become an egress path (the documented invariant ④)\n%s", res.raw, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "fwd_default_deny", before, after,
		"item 14b: the probe must REACH the floor and be counted. The spike that first tried this "+
			"could not assert it — its guest had no default route, so egress died inside the sandbox "+
			"and the counter read 0, which is an INVALID measurement rather than a pass (the documented invariant ⑯). The "+
			"engine now installs that route for every declaration, which is what makes this valid")

	// The same for a host address (the `input` side), since the two hooks are separate.
	addr, stop := hostListener(t, "hello-from-HOST")
	defer stop()
	_, portStr := portOf(t, addr)
	inBefore := harness.NftCounter(t, id, "in_floor")
	res2 := runGuestProbe(t, cl, net.JoinHostPort(hostIPv4(t), portStr), false)
	inAfter := harness.NftCounter(t, id, "in_floor")
	if res2.reachable() {
		t.Errorf("with a publish installed the guest reached a host address (%s)\n%s",
			res2.raw, harness.DumpRuleSet(id))
	}
	mustCounterMoved(t, id, "in_floor", inBefore, inAfter, "item 14b: the input side too")
}

// Revocation. What the engine actually offers is ONE path, and this
// asserts both of its halves.
//
// # Why the title is not "closes new connections but not established ones"
//
// That property is UNREACHABLE through any engine API: it describes deleting a DNAT rule
// under a live placement, and no endpoint does that — `publish` and the three egress
// fields are editable only from `paused` (the documented invariant). A test named for it would set up an echo
// service, an established connection and a round-trip check, never use them, and leave
// its failure branch unreachable. The conntrack asymmetry behind it is a property of
// nftables and is the MECHANISM REASON there is no un-publish endpoint (the documented invariant / the documented invariant ⑰).
//
// # What is asserted
//
// (a) The whole-binding PATCH is REFUSED on `running` — positively, as the the documented invariant rule it
// is, rather than logged as an acceptable outcome.
// (b) Revocation through the legal path — pause, PATCH, resume — genuinely CLOSES the
// port: the external caller is refused and the DNAT is gone from the rebuilt rule set.
// Nothing else in this file covers (b), and it is the half a caller revoking an inbound
// port actually cares about.
func TestRevokingAPublishClosesThePortAtTheNextPlacement(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	ctx := context.Background()

	d := declDeny()
	d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}
	id, _ := createRunning(t, eng, d)

	niX, err := eng.Mediator.Network(ctx, id)
	eps := niX.PublishEndpoints
	if err != nil || len(eps) == 0 {
		t.Fatalf("no endpoint: %v %v", eps, err)
	}
	endpoint := eps[0]

	// (a) The live edit is refused. ASSERTED, not narrated: this refusal is the whole
	// reason revocation is a placement-time operation, so if it ever stopped holding,
	// every argument resting on "materialized at placement" would be void.
	revoked := declDeny()
	if err := patchDecl(eng, ctx, id, revoked); err == nil {
		t.Fatal("the whole-binding PATCH edited the declaration on a RUNNING sandbox — `publish` and " +
			"the three egress fields are editable only from `paused` (the documented invariant), because the provider " +
			"materializes them at placement. Accepting it here would leave the record and the live " +
			"rule set disagreeing, with the record winning at the next placement")
	}

	// (b) The legal path. pause → PATCH → resume rebuilds the placement from the record,
	// which is what makes revocation unambiguous: there is no old rule to delete and no
	// old binding to outlive it, because the whole netns and table are new.
	if err := eng.Mediator.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := patchDecl(eng, ctx, id, revoked); err != nil {
		t.Fatalf("the whole-binding PATCH was refused on a PAUSED sandbox, which the documented invariant permits: %v", err)
	}
	if err := eng.Mediator.Resume(ctx, id); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The rule set no longer carries the DNAT. Structural half — checked first, because
	// if the rule is still there the reachability result below says nothing about
	// revocation.
	dump := harness.DumpRuleSet(id)
	if strings.Contains(dump, "dnat") {
		t.Errorf("the rebuilt rule set still carries a dnat after the publish was revoked — the "+
			"placement regenerates from the record (the documented invariant Binding), so this means the record kept the "+
			"item or Generate ignored its absence:\n%s", dump)
	}

	// And the external caller can no longer reach it. The endpoint is the one the engine
	// handed out before revocation — the address a caller would still be holding.
	// Reachability is the ABSENCE of DIAL_FAILED, not the presence of "CONNECTED": the
	// dialer's three markers are DIAL_FAILED / CONNECTED_NO_PAYLOAD / PAYLOAD, so matching
	// on "CONNECTED" would pass over PAYLOAD — the strongest evidence the port is open.
	out, derr := dialFromForeignNetns(t, endpoint)
	if derr == nil && !strings.Contains(out, "DIAL_FAILED") {
		t.Errorf("the revoked endpoint %s still accepted a connection from an external caller (%s) — "+
			"revocation at the next placement is the ONLY revocation this engine offers, so if the "+
			"rebuild does not close the port there is no way to close it at all", endpoint, out)
	}
	t.Logf("item 14e: after pause→PATCH→resume the revoked endpoint %s answers %q (err=%v) and the "+
		"rebuilt rule set carries no dnat", endpoint, out, derr)
}

// dialFromForeignNetns dials an address from a netns that is NOT the sandbox's own /30
// — the "external caller" every publish assertion needs.
//
// It exists because dialing from the host is a genuinely different path: host-originated
// traffic never traverses `prerouting`, so it is carried by the output-hook DNAT instead
// (measured, the documented invariant ⑭). Using the host as the "outside caller" would silently test that
// second rule twice and leave the external path unasserted — which is exactly the
// substitution the documented invariant warns is not evidence.
func dialFromForeignNetns(t *testing.T, endpoint string) (string, error) {
	t.Helper()
	ns := fmt.Sprintf("pubcaller%d", time.Now().UnixNano()%100000)
	vh := fmt.Sprintf("pcv%d", time.Now().UnixNano()%100000)
	vp := vh + "p"
	// A /30 in a DIFFERENT range from the sandbox space, so the caller is genuinely
	// foreign to it and its traffic must be routed by the host.
	const hostSide, callerSide = "10.99.99.1", "10.99.99.2"
	steps := [][]string{
		{"netns", "add", ns},
		{"link", "add", vh, "type", "veth", "peer", "name", vp},
		{"link", "set", vp, "netns", ns},
		{"addr", "add", hostSide + "/30", "dev", vh},
		{"link", "set", vh, "up"},
		{"-n", ns, "addr", "add", callerSide + "/30", "dev", vp},
		{"-n", ns, "link", "set", vp, "up"},
		{"-n", ns, "link", "set", "lo", "up"},
		{"-n", ns, "route", "add", "default", "via", hostSide, "dev", vp},
	}
	for _, st := range steps {
		if out, err := exec.Command("ip", st...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", st, err, out)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "del", ns).Run()
		_ = exec.Command("ip", "link", "del", vh).Run()
	})

	// A tiny dialer, run inside that netns.
	dir := t.TempDir()
	src := `package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	c, err := net.DialTimeout("tcp", os.Args[1], 8*time.Second)
	if err != nil {
		fmt.Println("DIAL_FAILED", err)
		os.Exit(1)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, rerr := c.Read(buf)
	if n < 0 {
		n = 0
	}
	if rerr != nil {
		fmt.Println("CONNECTED_NO_PAYLOAD", rerr)
		os.Exit(2)
	}
	fmt.Println("PAYLOAD", string(buf[:n]))
}
`
	if err := os.WriteFile(dir+"/main.go", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := dir + "/caller"
	build := exec.Command("go", "build", "-o", bin, dir+"/main.go")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the foreign-netns caller: %v: %s", err, out)
	}
	out, err := exec.Command("ip", "netns", "exec", ns, bin, endpoint).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// patchDecl drives the the documented invariant whole-binding PATCH with a declaration-only spec — what
// `PATCH /v1/sandboxes/{id}` carries when the request names only `network`.
//
// It goes through the ONE PATCH verb because that is all there is: the documented invariant requires one
// PATCH to be one acquisition of the transition lock, so the former declaration-only
// entry point was deleted rather than kept as a wrapper. Driving the mediator directly
// (rather than the HTTP face) is deliberate in this file — it is what proves the documented invariant's
// declaration classes are enforced at the WRITE POINT and not only in the API layer.
func patchDecl(eng *orchestrator.Engine, ctx context.Context, id model.SandboxID, d model.NetworkDecl) error {
	_, err := eng.Mediator.Rebind(ctx, id, lifecycle.RebindSpec{Binding: store.Binding{Network: &d}})
	return err
}

// TestRuleHitsAreReadableThroughTheReadFace is the acceptance half of the documented invariant's per-entry
// `hit_packet`: the number a caller reads must be the number the KERNEL is counting,
// attributed to the entry the caller actually wrote.
//
// What it pins that no unit test can: THE ATTRIBUTION JOIN. Unit tests prove the generator
// names counters it emitted and that the encoder sums them — but only a real placement
// proves those names are what nft installed. That is the half a rename breaks silently,
// because an unmatched name reports no hits, which reads exactly like a rule never hit.
func TestRuleHitsAreReadableThroughTheReadFace(t *testing.T) {
	eng, _ := harness.Engine(t, netEngineOpts(t))
	host := hostIPv4(t)
	addr, stop := hostListener(t, "hello-from-COUNTED")
	defer stop()
	port, portStr := portOf(t, addr)

	// The same shape as the declared-tuple test above: a real host listener the guest can
	// actually reach, so the counter has something to count.
	d := declDeny()
	d.Egress.Allow = []model.NetTuple{tupleOf(t, host+"/32", port, model.NetTCP)}
	id, cl := createRunning(t, eng, d)

	info, err := eng.Mediator.Network(context.Background(), id)
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	if info.Hits == nil {
		t.Fatalf("a RUNNING sandbox reports no counters at all — the read surface then cannot attribute a guest-side failure to the declaration, which is this key's entire consumer")
	}
	if len(info.Attribution.Allow) != 1 {
		t.Fatalf("attribution covers %d allow entries, declaration has 1", len(info.Attribution.Allow))
	}

	// EVERY attributed name must exist in what is installed. This is the join: a name the
	// generator invented but nft never rendered would report an absent key forever.
	for _, n := range info.Attribution.Allow[0] {
		if _, ok := info.Hits[n]; !ok {
			t.Errorf("counter %q is attributed to allow[0] but absent from the INSTALLED table — attribution and rendering have diverged, and a caller sees a permanently absent key rather than an error.\n%s", n, harness.DumpRuleSet(id))
		}
	}
	if info.Attribution.Default == "" {
		t.Errorf("no counter is attributed to egress.default")
	} else if _, ok := info.Hits[info.Attribution.Default]; !ok {
		t.Errorf("the counter attributed to egress.default (%q) is absent from the installed table", info.Attribution.Default)
	}

	// Cross-checked against the harness's INDEPENDENT read of the same counters, which is
	// what makes this more than an internally-consistent fiction: the API's number and
	// nft's must agree.
	for _, n := range info.Attribution.Allow[0] {
		viaNft := harness.NftCounter(t, id, n)
		if viaNft < 0 {
			continue // already reported above
		}
		if uint64(viaNft) != info.Hits[n] {
			t.Errorf("counter %q reads %d through the read surface and %d through nft — the API is not reporting the kernel's number", n, info.Hits[n], viaNft)
		}
	}

	// And it MOVES. A static number is worse than an absent one: the documented invariant's only signal for
	// "is this rule live" is that this count changes under a deliberate probe (the documented invariant's
	// acceptance discipline / the documented invariant).
	before := allowHits(t, eng, id)
	got := runGuestProbe(t, cl, net.JoinHostPort(host, portStr), false)
	if !got.reachable() {
		t.Fatalf("the DECLARED tuple was not reachable (%s), so this test cannot distinguish a dead counter from a dead rule: %s", got.outcome, harness.DumpRuleSet(id))
	}
	after := allowHits(t, eng, id)
	if after <= before {
		t.Errorf("allow[0]'s hit_packet did not move across a probe that DID get through (%d → %d) — the number is decorative, and the allow-list rollout this key exists for cannot attribute anything with it.\n%s", before, after, harness.DumpRuleSet(id))
	}
}

// allowHits sums what the documented invariant reports for allow[0], through the same path the read surface
// uses.
func allowHits(t *testing.T, eng *orchestrator.Engine, id model.SandboxID) uint64 {
	t.Helper()
	info, err := eng.Mediator.Network(context.Background(), id)
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	var total uint64
	for _, n := range info.Attribution.Allow[0] {
		total += info.Hits[n]
	}
	return total
}
