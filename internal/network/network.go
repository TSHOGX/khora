// Package network is subsystem §5.D. It does EXACTLY TWO THINGS: it holds the
// reachability declaration, and it generates the abstract rule set that delivers
// it. It moves no bytes, terminates no connection, and contributes no process
// (§5.D) — connection termination, protocol recognition and content rewriting do
// not exist here and there is no seam that could carry them.
//
// The boundary is not a preference, it is `enforceability locus` (network.md D0 /
// D1): a control that ONLY the side creating the placement can enforce belongs in
// the engine; everything else does not. Four things pass that test — installing
// rules atomically with placement, binding the predicate to the input interface,
// the L0 floor on BOTH hops, and rule ORDER — and they are exactly what this
// package plus §5.A deliver. Choosing an upstream by SNI, swapping a credential,
// terminating TLS: none of them need the namespace-creating side, so none of them
// are here (§1.4 excludes the credential family permanently).
//
// # Why the generator is a pure function
//
// `(declaration, L0 floor) → abstract rule set` is placement-free and root-free
// (§5.D / network.md D6), which is what puts EVERY difference between declaration
// values under a unit test instead of behind a root-only substrate run. The three
// things it cannot know — the host-side veth name, the guest IP, the publish host
// endpoint — are placement products held only by the provider; naming them here
// would both fail to run (no placement exists yet) and lift a host primitive onto
// the cross-provider contract (§5.A ❌ list).
//
// The signature has two inputs but the output is grouped BY HOOK, and that is
// load-bearing rather than presentational: one declaration produces rules on
// several hooks with OPPOSITE predicate shapes (network.md D5 ⑧ ㉔), so flattening
// the two sides into one list is exactly how the hook-assignment difference falls
// out of the part a test pins.
package network

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"engine/internal/ruleset"
)

// Default is `egress.default` — the only two values (§5.D wire schema). There is
// NO posture enum: a posture is a COMBINATION of the four fields' values
// (`deny` + no allow + no via = "no egress"; `deny` + via = "only through this
// rendezvous"), and every enum would be one more coupling point between the
// engine and product semantics (§1.1 / §5.D).
type Default string

const (
	// DenyAll is the fail-closed side, and the value an omitted `network` object
	// normalizes to (see Zero).
	DenyAll Default = "deny"
	// AllowPublic is "the public internet, MINUS the L0 floor and the internal
	// deny-safe tier" (§2.4). It is emphatically not "everything": without the
	// deny-safe tier this value would hand the host's whole LAN to the sandbox
	// (measured, network.md D5 ⑤), i.e. it would deliver an `allow` strictly wider
	// than §2.4 defines.
	AllowPublic Default = "allow"
)

// Tuple is `{cidr, port, proto}` — ONE tuple, not three independent options
// (§5.D). `proto` is always explicit because it is a FORMAT requirement rather
// than an optional dimension: in the kernel mechanism carrying it, port matching
// itself depends on protocol matching, so `{cidr, port}` is not well-formed
// (network.md D3.1). One consequence worth naming: the classic "port 80 and 443
// do not stack, you need two rules" trap is structurally absent here, because
// port and protocol are always both spelled out.
type Tuple struct {
	// CIDR is the destination prefix. Only IP LITERALS — no domain names, and the
	// reason is COST, not feasibility: domain-level control would require a DNS
	// proxy, a TTL refresher coupled to connection tracking, and a churning
	// product list inside the engine, and the third breaks §1.1 outright
	// (network.md D2). To narrow to a domain, hand the flow to a process outside
	// the engine with `via[]`.
	//
	// An INVALID (zero) prefix means ANY destination, which is only meaningful
	// inside `via[].match[]`: that is the "transparent interception" value of this
	// same field, and the generator emits one clause FEWER for it (network.md
	// D3.3 / D5 ⑩). A prefix with Bits()==0 is treated identically — a /0 pins
	// nothing.
	CIDR netip.Prefix
	// Port is the destination port. Zero means any port.
	Port uint16
	// Proto is mandatory (see the type doc).
	Proto ruleset.Proto
}

// AnyDest reports whether this tuple pins no destination address. The generator
// emits the pre-NAT destination clause IF AND ONLY IF this is false, and both
// directions are asserted: keeping a cidr clause on a transparent-interception
// declaration would block the traffic the declaration authorized, while dropping
// it on an explicit-endpoint declaration would authorize every other address on
// that port (network.md D5 ⑩).
func (t Tuple) AnyDest() bool { return !t.CIDR.IsValid() || t.CIDR.Bits() == 0 }

// Via is one `egress.via[]` entry: flows matching any of `Match` are sent to
// `Endpoint`.
//
// WHAT SITS BEHIND THAT ENDPOINT, THE ENGINE DOES NOT KNOW AND DOES NOT ASK
// (§5.D). It is an address; the engine installs a rule sending matched traffic
// there. So it is a declaration VALUE, never an engine constant or an engine
// config key.
type Via struct {
	Match []Tuple
	// Endpoint is a HOST-LOCAL `ip:port`. Host-local is a contract requirement,
	// not a convenience: the second hop — that process's OWN egress on the
	// sandbox's behalf — must be pinned by the KERNEL rather than by the process's
	// own discipline, and the kernel's reach stops at this host. Accepting a remote
	// endpoint would rest the floor on that process's self-restraint, which is
	// precisely the confused deputy §2.4's second invariant rejects (network.md
	// D4.2).
	Endpoint string
}

// Publish is one `ingress.publish[]` entry: a guest port exposed as a host
// `ip:port`. It is the explicit inbound exception and the ONE hot-addable item
// (§5.D) — a pure host-side rule set, purely additive, touching no isolation
// unit.
type Publish struct {
	GuestPort uint16
	Proto     ruleset.Proto
}

// Egress is the outbound half of the declaration: the three fields that change
// only at a resume boundary (§5.D).
type Egress struct {
	// Default is MANDATORY on the wire when a `network` object is given, and the
	// reason is not strictness for its own sake: if "unspecified" were read as some
	// default, a null in the record could no longer be told apart from "the caller
	// chose that value" (§5.D, the same shape as idle_timeout / retention).
	Default Default
	Allow   []Tuple
	Via     []Via
}

// Ingress is the inbound half: closed by default, opened item by item.
type Ingress struct {
	Publish []Publish
}

// Declaration is §5.D's four-field declaration — a Binding (§3.4), recorded on
// the sandbox's own control-plane record and regenerated into a rule set at every
// placement from the CURRENT record. That is what makes it stable across resume
// and rebuildable by §4.4-A reconciliation after a crash: a rule set is
// crash-surviving host state exactly like a netns.
type Declaration struct {
	Egress  Egress
	Ingress Ingress
}

// Zero is what an OMITTED `network` object normalizes to, and the engine writes
// this value into the record EXPLICITLY at the write point (§5.D / §6.1).
//
// Two things this resolves, and they are different things: "the whole `network`
// object omitted" is LEGAL (this value), while "a `network` object given but
// missing `egress.default`" is the first of §6.2's eight 400 classes. Because the
// normalization happens at the write point, the record always holds a chosen
// value and the "a null cannot be told from a choice" problem it guards against
// is structurally absent.
//
// `deny` rather than `allow`: it is the fail-closed side, and it happens to be
// exactly the minimal declaration — "no network" (§5.D has no posture enum, so a
// posture just IS a combination of values).
func Zero() Declaration {
	return Declaration{
		Egress:  Egress{Default: DenyAll, Allow: []Tuple{}, Via: []Via{}},
		Ingress: Ingress{Publish: []Publish{}},
	}
}

// Normalize fills in the empty-slice spelling so a read-back never has to choose
// between `[]` and null (§6.1: the three arrays always appear, empty is `[]`,
// never omitted and never null — they are DEFINITE values in the record, and the
// omit-when-unreadable rule governs facts that cannot be read, of which a
// declaration has none).
func (d Declaration) Normalize() Declaration {
	if d.Egress.Allow == nil {
		d.Egress.Allow = []Tuple{}
	}
	if d.Egress.Via == nil {
		d.Egress.Via = []Via{}
	}
	if d.Ingress.Publish == nil {
		d.Ingress.Publish = []Publish{}
	}
	return d
}

// AddPublish is the `POST …/publish` write, applied to the declaration:
// idempotent on `{guest_port, proto}`.
//
// DEDUPLICATION MUST HAPPEN HERE, at the declaration layer, because THE SUBSTRATE
// WILL NOT FIND IT FOR US: nft accepts two byte-identical DNAT rules without
// complaint and the second one simply never matches (measured, §5.D). And with
// two identical items recorded, a later `PATCH` removing one leaves the other —
// "the publish was revoked and the port is still open", a silent failure on a
// security-relevant field.
//
// Returns the declaration and whether anything was added (false = the idempotent
// no-op, which the API answers with the existing item).
func (d Declaration) AddPublish(p Publish) (Declaration, bool) {
	d = d.Normalize()
	for _, cur := range d.Ingress.Publish {
		if cur.GuestPort == p.GuestPort && cur.Proto == p.Proto {
			return d, false
		}
	}
	next := make([]Publish, len(d.Ingress.Publish), len(d.Ingress.Publish)+1)
	copy(next, d.Ingress.Publish)
	d.Ingress.Publish = append(next, p)
	return d, true
}

// ParseEndpoint splits a `via[].endpoint` into its address and port.
//
// A port is REQUIRED, and that is mechanical rather than pedantic: the narrow
// accept that keeps `via[]` working without reopening the floor must pin the
// POST-NAT port, which is this endpoint's port (network.md D5 ⑩). An endpoint
// without one cannot be steered to and cannot be guarded.
func ParseEndpoint(ep string) (netip.Addr, uint16, error) {
	host, portStr, err := splitHostPort(ep)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("endpoint address %q is not an IP literal: the engine "+
			"resolves no names (§5.D)", host)
	}
	port, perr := strconv.ParseUint(portStr, 10, 16)
	if perr != nil || port == 0 {
		return netip.Addr{}, 0, fmt.Errorf("endpoint port %q is not a port number", portStr)
	}
	return addr, uint16(port), nil
}

// splitHostPort accepts `1.2.3.4:8080` and `[::1]:8080`.
func splitHostPort(ep string) (host, port string, err error) {
	if ep == "" {
		return "", "", fmt.Errorf("endpoint is empty")
	}
	if strings.HasPrefix(ep, "[") {
		end := strings.Index(ep, "]")
		if end < 0 || end+2 > len(ep) || ep[end+1] != ':' {
			return "", "", fmt.Errorf("endpoint %q is not `[ipv6]:port`", ep)
		}
		return ep[1:end], ep[end+2:], nil
	}
	i := strings.LastIndexByte(ep, ':')
	if i < 0 {
		return "", "", fmt.Errorf("endpoint %q carries no port — the narrow accept that keeps "+
			"`via[]` from reopening the L0 floor must pin the post-NAT port (network.md D5 ⑩), "+
			"so an endpoint without one cannot be enforced", ep)
	}
	return ep[:i], ep[i+1:], nil
}
