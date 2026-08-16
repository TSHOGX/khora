// Package network is subsystem the network declaration. It does EXACTLY TWO THINGS: it holds the
// reachability declaration, and it generates the abstract rule set that delivers
// it. It moves no bytes, terminates no connection, and contributes no process
// (the network declaration) — connection termination, protocol recognition and content rewriting do
// not exist here and there is no seam that could carry them.
//
// The boundary is not a preference, it is an enforceability locus: a control that ONLY the side
// creating the placement can enforce belongs in the engine; everything else does not. Four things
// pass that test — installing
// rules atomically with placement, binding the predicate to the input interface,
// the L0 floor on BOTH hops, and rule ORDER — and they are exactly what this
// package plus the runtime provider contract deliver. Choosing an upstream by SNI, swapping a credential,
// terminating TLS: none of them need the namespace-creating side, so none of them
// are here (the engine non-goals excludes the credential family permanently).
//
// # Why the generator is a pure function
//
// `(declaration, L0 floor) → abstract rule set` is placement-free and root-free
// (the network declaration / the network seam boundary), which is what puts EVERY difference between declaration
// values under a unit test instead of behind a root-only substrate run. The three
// things it cannot know — the host-side veth name, the guest IP, the publish host
// endpoint — are placement products held only by the provider; naming them here
// would both fail to run (no placement exists yet) and lift a host primitive onto
// the cross-provider contract (the runtime provider contract list).
//
// The input is the declaration ALONE — the L0 floor is a static fact the `ruleset`
// leaf holds, never a parameter, so no caller can pass a different floor. The output
// is one ORDERED list in which each rule names its own hook, because order across
// hooks is itself a security property (a `via[]` accept must sit above the floor drop
// IN THE SAME CHAIN — network enforcement). Hooks are grouped only downstream, by the renderer and
// by `ruleset.RuleSet.ByHook`, which is what a unit test uses to pin "what came out on
// each hook".
package network

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/ruleset"
)

// The DECLARATION TYPES live in `model`, not here, and that placement follows the
// same rule Mount and Limits follow: the store persists this value, and `model`
// depends on no subsystem (the subsystem boundaries preamble). What lives HERE is everything that carries
// the network declaration SEMANTICS — the normalization decision, the deduplication requirement, the
// nine 400 classes, and the generator.
//
// These aliases exist so this package reads in its own vocabulary rather than
// spelling `model.Net…` on every line. They are true aliases, not conversions:
// there is ONE type, so no translation step can drift.
type (
	Declaration = model.NetworkDecl
	Egress      = model.NetEgress
	Ingress     = model.NetIngress
	Tuple       = model.NetTuple
	Via         = model.NetVia
	Publish     = model.NetPublish
	Default     = model.NetEgressDefault
)

const (
	// DenyAll is the fail-closed side, and the value an omitted `network` object
	// normalizes to (see Zero).
	DenyAll = model.NetDeny
	// AllowPublic is "the public internet, MINUS the L0 floor and the internal
	// deny-safe tier" (the provider security contract). It is emphatically not "everything": without the
	// deny-safe tier this value would hand the host's whole LAN to the sandbox
	// (measured, network substrate measurements), i.e. it would deliver an `allow` strictly wider
	// than the provider security contract defines.
	AllowPublic = model.NetAllow
)

// Zero is what an OMITTED `network` object normalizes to, and the engine writes
// this value into the record EXPLICITLY at the write point (the network declaration / the control-plane API).
//
// Two things this resolves, and they are different things: "the whole `network`
// object omitted" is LEGAL (this value), while "a `network` object given but
// missing `egress.default`" is the first of the API error contract's nine 400 classes. Because the
// normalization happens at the write point, the record always holds a chosen
// value and the "a null cannot be told from a choice" problem it guards against
// is structurally absent.
//
// `deny` rather than `allow`: it is the fail-closed side, and it happens to be
// exactly the minimal declaration — "no network" (the network declaration has no posture enum, so a
// posture just IS a combination of values).
func Zero() Declaration {
	return Declaration{
		Egress:  Egress{Default: DenyAll, Allow: []Tuple{}, Via: []Via{}},
		Ingress: Ingress{Publish: []Publish{}},
	}
}

// AddPublish is the `POST …/publish` write, applied to the declaration:
// idempotent on `{guest_port, proto}`.
//
// DEDUPLICATION MUST HAPPEN HERE, at the declaration layer, because THE SUBSTRATE
// WILL NOT FIND IT FOR US: nft accepts two byte-identical DNAT rules without
// complaint and the second one simply never matches (measured, the network declaration). And with
// two identical items recorded, a later `PATCH` removing one leaves the other —
// "the publish was revoked and the port is still open", a silent failure on a
// security-relevant field.
//
// Returns the declaration and whether anything was added (false = the idempotent
// no-op, which the API answers with the existing item).
func AddPublish(d Declaration, p Publish) (Declaration, bool) {
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

// protoOf translates the RECORD's protocol enum into the enforcement layer's.
//
// The two are separate types on purpose, and the translation lives HERE — the same
// shape DeathCause uses (the runtime provider contract: the provider seam must not depend on the
// control-plane record shape, and the mediator is the one place that legitimately
// knows both). `model` therefore depends on no subsystem and `ruleset` on no
// record, while the network declaration — which owns both halves of this particular conversion — pays
// one switch statement for it.
func protoOf(p model.NetProto) ruleset.Proto {
	switch p {
	case model.NetTCP:
		return ruleset.TCP
	case model.NetUDP:
		return ruleset.UDP
	}
	// Unreachable for a validated declaration: Validate refuses anything but the two
	// values. Returning the empty protocol makes the failure LOUD if it ever is
	// reached — ruleset.Validate then rejects the rule for carrying a port with no
	// protocol, rather than rendering a rule that matches the wrong traffic.
	return ""
}

// ParseEndpoint splits a `via[].endpoint` into its address and port.
//
// A port is REQUIRED, and that is mechanical rather than pedantic: the narrow
// accept that keeps `via[]` working without reopening the floor must pin the
// POST-NAT port, which is this endpoint's port (network substrate measurements). An endpoint
// without one cannot be steered to and cannot be guarded.
func ParseEndpoint(ep string) (netip.Addr, uint16, error) {
	host, portStr, err := splitHostPort(ep)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("endpoint address %q is not an IP literal: the engine "+
			"resolves no names (the network declaration)", host)
	}
	// the API error contract's v4-mapped class applies HERE TOO, and for a reason of its own rather
	// than by analogy with the tuple's `cidr`: `dnat` is family-typed. A v4-mapped
	// endpoint renders `dnat ip6 to [::ffff:10.0.0.5]:1080` onto a flow whose packets
	// are IPv4, because `netip.Addr.Is4` is FALSE for the mapped form — the rule
	// installs, matches nothing, and `via[]` is silently dead with NO counter moving.
	// The containment checks happen to survive the spelling (they unmap), which is
	// exactly what makes this gap invisible from the validation side.
	if verr := rejectV4Mapped(addr, host); verr != nil {
		return netip.Addr{}, 0, verr
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
			"`via[]` from reopening the L0 floor must pin the post-NAT port (network substrate measurements), "+
			"so an endpoint without one cannot be enforced", ep)
	}
	return ep[:i], ep[i+1:], nil
}
