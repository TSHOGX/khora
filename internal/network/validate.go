package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"engine/internal/ruleset"
)

// ErrDeclarationInvalid is the one verdict §6.2's eight 400 classes share: a
// declaration that is decidable-invalid WITHOUT reading any other row, and that
// no retry or state change could ever make legal — §6.2's 400 criterion, the same
// one the mount-binding self-contradiction meets.
//
// ONE sentinel for the status code, EIGHT classes for the caller, and the split is
// deliberate. §6.2's Volume verdicts establish the rule the other way round: what
// must stay distinguishable is what the caller's REMEDY depends on. Here every
// remedy is "fix the declaration", so one status and one sentinel is honest —
// while the class is still recoverable via ClassOf, because the eight remedies are
// different EDITS and a test that could not tell them apart would let two classes
// collapse into one.
var ErrDeclarationInvalid = errors.New("network: declaration is invalid")

// Class names one of §6.2's eight 400 classes.
type Class string

const (
	// ClassDefaultMissing — `egress.default` absent while a `network` object was
	// given. Note what is NOT this class: omitting the whole `network` object is
	// LEGAL and normalizes to Zero() (§5.D / §6.1). The two share a code path in
	// most implementations, which is exactly why they need separate assertions —
	// whichever is written first eats the other.
	ClassDefaultMissing Class = "egress.default-missing"
	// ClassCIDRNotLiteral — a name where an IP literal belongs. Refused on COST
	// grounds, not feasibility (network.md D2): use `via[]` to hand the flow to a
	// process outside the engine that can judge by application-layer information.
	ClassCIDRNotLiteral Class = "cidr-not-an-ip-literal"
	// ClassProtoMissing — a tuple without `proto`. `{cidr, port}` is not
	// well-formed: port matching depends on protocol matching in the mechanism
	// that carries it (network.md D3.1).
	ClassProtoMissing Class = "tuple-missing-proto"
	// ClassViaMatchUDP — a `via[].match[]` naming UDP. There is no usable
	// interface for recovering the ORIGINAL destination of a redirected UDP
	// datagram: SO_ORIGINAL_DST fails outright on a UDP socket, and the cmsg
	// interface that DOES answer returns the post-NAT local endpoint —
	// indistinguishable from "no NAT happened" (measured, network.md D3.2 / D5 ⑥).
	// So a UDP interceptor written against it cannot tell the two apart. Inbound
	// `publish` with `proto: udp` is UNAFFECTED and legal: a DNAT to a fixed guest
	// port needs no original destination, so the objection does not reach it.
	ClassViaMatchUDP Class = "via-match-udp"
	// ClassViaEndpointNotHostLocal — the kernel's reach stops at this host, so a
	// remote endpoint could not be pinned by it; accepting one would rest the L0
	// floor on that process's own discipline, which is the confused deputy §2.4's
	// second invariant rejects (network.md D4.2).
	ClassViaEndpointNotHostLocal Class = "via-endpoint-not-host-local"
	// ClassViaEndpointInFloor — an endpoint inside the floor's daddr list items
	// (loopback / link-local / the sibling CIDR). "The declaration cannot lift the
	// floor" makes every flow steered there PERMANENTLY fail-closed, so it is a
	// declaration provably unable to work.
	//
	// It reads the floor's ADDRESS LIST ONLY, never the "any address of the host
	// itself" item — that item is a ct-state rule rather than an address
	// enumeration, and folding it in would make classes 5 and 6 squeeze `via[]`
	// out of existence between them (§6.2). The tension that leaves is resolved by
	// ORDER, with a narrow accept above the floor drop in the SAME chain, not by
	// narrowing the floor (§2.4).
	ClassViaEndpointInFloor Class = "via-endpoint-inside-the-floor"
	// ClassAllowInsideFloor — an `egress.allow[]` cidr WHOLLY inside the floor's
	// never-liftable items (loopback / link-local, both families). Measured: such a
	// rule's own counter stays 0 forever while the floor's counter rises — it is a
	// rule that is never evaluated (network.md D5 ㉑).
	//
	// Accepting it does not cost "one harmless extra rule": it manufactures a
	// SECOND population of legitimate zeros in the one health signal the floor has,
	// and every deny acceptance in M8 reads that signal. Same criterion as
	// "an uncapped sandbox cannot be given a disk cap" — a promise provably
	// unmaterializable is refused at the write point.
	ClassAllowInsideFloor Class = "allow-inside-the-floor"
	// ClassV4Mapped — a v4-mapped or v4-compatible IPv6 literal. This class is the
	// NECESSARY PREREQUISITE of the previous one, not separate fastidiousness:
	// measured, netip.Prefix.Contains does NOT unmap, so a family-branching
	// containment test judges `::ffff:169.254.169.254/128` to be v6, compares it
	// only against v6 members, and LETS IT THROUGH — after which the packet leaves
	// as IPv4 and dies on the v4 floor. Accepted by the API, silently refused on
	// the wire, which is D4.3's diagnostic-dislocation exactly (network.md D5 ⑲).
	//
	// Refused BEFORE any containment test rather than normalized-then-compared:
	// §5.D takes IP literals, and `::ffff:192.168.1.1` is a SECOND SPELLING of an
	// address that already has one — accepting it means the recorded Binding
	// differs from the caller's text, and the two spellings compare unequal in
	// every subsequent read-modify-write.
	ClassV4Mapped Class = "cidr-v4-mapped-literal"
)

// invalidError carries the class alongside the message.
type invalidError struct {
	class Class
	msg   string
}

func (e *invalidError) Error() string { return e.msg }

// Unwrap makes every class satisfy errors.Is(err, ErrDeclarationInvalid), which
// is what §6.2's status mapping keys on — one arm, eight classes.
func (e *invalidError) Unwrap() error { return ErrDeclarationInvalid }

func invalid(c Class, format string, args ...any) error {
	return &invalidError{class: c, msg: fmt.Sprintf("network: %s: %s", c, fmt.Sprintf(format, args...))}
}

// ClassViaWithoutSecondHopUID is a NINTH refusal, and it is NOT one of §6.2's
// eight classes — §6.2 enumerates eight, all of them properties of the
// declaration TEXT, and this one is a property of the DEPLOYMENT.
//
// The refusal itself is mandated by the M8 plan (§5, the orchestrator row): the
// second hop is pinned by an `output`-hook table narrowed by a uid the DEPLOYMENT
// supplies, not the declaration (§2.4 / §6.2). With no uid there is no table and
// therefore NO ENFORCEMENT of §2.4's second invariant — so accepting a `via[]`
// declaration would record a declaration whose own invariant the engine cannot
// deliver, leaving a confused deputy live on the host with every first-hop
// assertion still passing (network.md D5 ㉔: hop 2 counts zero in `forward`, so the
// first-hop floor never evaluates it).
//
// 400 by §6.2's own criterion, and by analogy to the closest existing verdict:
// it is decidable without reading any other row, no caller retry makes it legal,
// and nothing is full so 507 would be a lie — exactly ErrCapRequired's shape
// ("the node meters this axis, so a cap is required"), which is likewise a 400
// that depends on deployment configuration. Its own sentinel so the caller can
// tell it from the eight; the remedy is an operator's, not theirs.
//
// ⚠ Logged as a spec gap in open-decisions.md ("§6.2 第九类拒绝"): §6.2's list is
// written as eight, and it states that the resource-cap rule is the ONE field
// semantic depending on deployment configuration. Both need a decision at the M8
// review gate; the code takes the leaning rather than the spec taking it silently.
const ClassViaWithoutSecondHopUID Class = "via-without-second-hop-uid"

// ClassOf reports which of §6.2's eight classes an error is, if any.
func ClassOf(err error) (Class, bool) {
	var ie *invalidError
	if errors.As(err, &ie) {
		return ie.class, true
	}
	return "", false
}

// ParseCIDR turns a wire `cidr` string into a prefix, applying the two classes
// that are decidable at PARSE time: a name where a literal belongs (class 2) and
// a v4-mapped / v4-compatible spelling (class 8).
//
// Class 8 is applied HERE, at the parse, and that position is the whole point:
// §6.2 requires it to be refused BEFORE any containment judgment, because a
// family-branching containment test would otherwise let it through and the packet
// would then die on the v4 floor (network.md D5 ⑲).
//
// A bare address is accepted as a host route (`/32`, `/128`), which is the
// spelling a caller naturally uses for a single resolver or endpoint.
func ParseCIDR(s string) (netip.Prefix, error) {
	if s == "" {
		return netip.Prefix{}, invalid(ClassCIDRNotLiteral,
			"cidr is empty — the engine takes IP literals only and resolves no names (§5.D)")
	}
	var p netip.Prefix
	if addr, err := netip.ParseAddr(s); err == nil {
		if err := rejectV4Mapped(addr, s); err != nil {
			return netip.Prefix{}, err
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	} else {
		pp, perr := netip.ParsePrefix(s)
		if perr != nil {
			return netip.Prefix{}, invalid(ClassCIDRNotLiteral,
				"%q is not an IP literal or CIDR — the engine resolves no names, and domain-level "+
					"narrowing is done OUTSIDE it by handing the flow to `egress.via[]` (§5.D / network.md D2)", s)
		}
		if err := rejectV4Mapped(pp.Addr(), s); err != nil {
			return netip.Prefix{}, err
		}
		p = pp.Masked()
	}
	return p, nil
}

// v4Compatible is `::/96` — the deprecated v4-compatible form, refused for the
// same reason as v4-mapped: a second spelling of an address that already has one.
var v4Compatible = netip.MustParsePrefix("::/96")

func rejectV4Mapped(a netip.Addr, raw string) error {
	if a.Is4() || a.Is4In6() || (a.Is6() && v4Compatible.Contains(a)) {
		if a.Is4() {
			return nil // an ordinary dotted-quad, the canonical spelling
		}
		return invalid(ClassV4Mapped,
			"%q is a v4-mapped/v4-compatible IPv6 spelling of %s — refused BEFORE any containment "+
				"test, because `Contains` does not unmap and a family-branching check would judge it v6, "+
				"compare it only against v6 members, and let it past a floor it then dies on as IPv4 "+
				"(measured, network.md D5 ⑲). Write the address in its IPv4 form", raw, a.Unmap())
	}
	return nil
}

// HostLocal answers "is this address one of THIS host's own addresses" — the
// predicate §6.2's fifth class needs.
//
// It is a PARAMETER rather than a package-level read of the interface table, so
// the eight classes are pinned in a root-free unit test against a fixture instead
// of against whatever addresses the test machine happens to carry. SystemHostLocal
// is the production implementation.
//
// Note the asymmetry with the floor, which is not a contradiction: the floor's
// "any address of the host itself" item is deliberately NEVER enumerated (a pure
// ct-state rule is exhaustive over every host address, so the engine need not know
// its own bind addresses — §2.4 / network.md D5 ⑧). This predicate answers the
// opposite, POSITIVE question about one specific declared address, and enumeration
// is the only way to answer it at all. §6.2 states exactly why the negative
// direction must not be enumerated in the seventh class.
type HostLocal func(netip.Addr) (bool, error)

// SystemHostLocal reads this host's interface addresses.
func SystemHostLocal(a netip.Addr) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, fmt.Errorf("network: enumerate host addresses: %w", err)
	}
	for _, ia := range addrs {
		n, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		got, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		if got.Unmap() == a.Unmap() {
			return true, nil
		}
	}
	// Loopback is host-local by definition even on a host whose loopback address
	// is not in the interface list for some reason. Saying otherwise would report
	// class 5 for an endpoint whose real problem is class 6, sending the caller to
	// the wrong fix.
	return a.IsLoopback(), nil
}

// Deployment is what the DEPLOYING PARTY hands the engine, as distinct from what
// a declaration carries (§2.4 / §6.2: the uid/gid pinning the second hop is a
// deployment input, never a declaration field).
type Deployment struct {
	// SecondHopUID / SecondHopGID identify the host-local process behind
	// `via[].endpoint`. Set = the engine installs the second-hop table at startup
	// and `via[]` is permitted; unset = `via[]` is refused (see
	// ClassViaWithoutSecondHopUID).
	//
	// A POINTER because uid 0 is a real uid: a value type could not tell "the
	// operator said root" from "the operator said nothing", and the two must lead to
	// opposite behavior.
	SecondHopUID *int
	SecondHopGID *int
}

// SecondHopPinned reports whether the deployment supplied what §2.4's second
// invariant needs to be ENFORCED rather than merely prefaced.
func (dep Deployment) SecondHopPinned() bool { return dep.SecondHopUID != nil }

// ValidateDeployment is the refusal that depends on the DEPLOYMENT rather than on
// the declaration text — see ClassViaWithoutSecondHopUID for why it is separate
// from §6.2's eight and why it is a 400.
func ValidateDeployment(d Declaration, dep Deployment) error {
	if len(d.Egress.Via) == 0 || dep.SecondHopPinned() {
		return nil
	}
	return invalid(ClassViaWithoutSecondHopUID,
		"this declaration uses `egress.via[]`, but the deployment supplied no second-hop uid. "+
			"§2.4's second invariant requires the endpoint process's OWN egress to be pinned by the "+
			"kernel, and the only predicate available on that hook is its uid (measured: hop 2 counts "+
			"zero in `forward` and is visible only in `output` — network.md D5 ㉔). Without it the "+
			"engine would record a declaration whose invariant it cannot deliver, leaving a confused "+
			"deputy live while every first-hop assertion still passes. Start the orchestrator with "+
			"the second-hop uid, or declare `egress.allow[]` instead")
}

// Validate applies §6.2's eight 400 classes.
//
// IT RUNS BEFORE THE ADMISSION GATE (§6.2): an invalid declaration is refused
// before any byte is touched or any budget is recorded, so 400 and 507 are never
// both true — a request is either refused for an invalid declaration or admitted
// as valid and then possibly refused for node capacity.
//
// `given` distinguishes "a `network` object was supplied" from "the whole object
// was omitted" — only the former is subject to class 1 (§5.D / §6.1).
func Validate(d Declaration, given bool, hostLocal HostLocal) error {
	if given && d.Egress.Default == "" {
		return invalid(ClassDefaultMissing,
			"`egress.default` is required when `network` is given (deny | allow). If you want the "+
				"minimal declaration, omit `network` entirely — that is legal and records "+
				"{deny, [], [], []} (§5.D)")
	}
	if given && d.Egress.Default != DenyAll && d.Egress.Default != AllowPublic {
		return invalid(ClassDefaultMissing,
			"`egress.default` must be %q or %q, got %q", DenyAll, AllowPublic, d.Egress.Default)
	}

	for i, t := range d.Egress.Allow {
		if err := validateTuple(t, fmt.Sprintf("egress.allow[%d]", i)); err != nil {
			return err
		}
		// CLASS 7. WHOLE containment, never intersection, and that is measured
		// rather than cautious: the floor sits ABOVE the declaration's allows, so
		// ordering resolves a supernet correctly — an `allow 10.0.0.0/8` under a
		// floor holding 10.66.0.0/16 reaches the internal 10.77.x host while the
		// sibling stays blocked, each with its own counter moving (network.md D5 ㉑).
		// An intersection test would make that legitimate supernet undeclarable.
		if in := containingFloorItem(t.CIDR); in.IsValid() {
			return invalid(ClassAllowInsideFloor,
				"%s (%s) lies wholly inside the L0 floor item %s, which no declaration can lift — "+
					"the rule would never be evaluated (measured: its own counter stays 0 while the "+
					"floor's rises), and accepting it would put a second population of legitimate "+
					"zeros into the one signal every deny assertion reads (network.md D5 ㉑ / §6.2). "+
					"`port`/`proto` do not change this: the floor's daddr items carry no port "+
					"qualifier, so anything contained by address is dead on every port",
				fmt.Sprintf("egress.allow[%d]", i), t.CIDR, in)
		}
	}

	for i, v := range d.Egress.Via {
		where := fmt.Sprintf("egress.via[%d]", i)
		if len(v.Match) == 0 {
			return invalid(ClassProtoMissing, "%s has no `match[]` — an entry that matches nothing "+
				"steers nothing, and there is no default match", where)
		}
		for j, t := range v.Match {
			mwhere := fmt.Sprintf("%s.match[%d]", where, j)
			if err := validateTuple(t, mwhere); err != nil {
				return err
			}
			// CLASS 4.
			if t.Proto == ruleset.UDP {
				return invalid(ClassViaMatchUDP,
					"%s names proto udp: there is no usable way to recover a redirected datagram's "+
						"ORIGINAL destination — SO_ORIGINAL_DST fails on a UDP socket, and the cmsg "+
						"interface that does answer returns the POST-NAT local endpoint, which is "+
						"indistinguishable from `no NAT happened` (measured, network.md D3.2 / D5 ⑥). "+
						"So a UDP interceptor could not tell the two apart. `ingress.publish[]` accepts "+
						"proto udp — that objection does not reach a DNAT to a fixed guest port", mwhere)
			}
			// CLASS 7 IS DELIBERATELY NOT APPLIED TO `via[].match[]`, and the asymmetry
			// between the two fields is a MECHANISM FACT rather than an oversight: a
			// steered flow is DNAT'd in `prerouting`, which runs BEFORE the filter floor,
			// so it lands in `input` and is taken by the narrow accept there — measured
			// 1–2 ms connect with the real banner read back, while the same address via
			// `allow[]` is a dead rule (network.md D5 ㉑). `via[]` does not lift the
			// floor: the guest never reaches that in-floor address, a HOST-LOCAL process
			// does. Extending this class here would kill "hand an in-floor destination to
			// a process outside the engine", which is one of `via[]`'s main uses (a
			// metadata proxy is exactly that).
		}
		addr, _, err := ParseEndpoint(v.Endpoint)
		if err != nil {
			return invalid(ClassViaEndpointNotHostLocal, "%s.endpoint: %v", where, err)
		}
		// CLASS 6 BEFORE CLASS 5, because loopback satisfies both and class 6 names
		// the caller's actual problem. Reporting "not host-local" for 127.0.0.1 would
		// send them to fix the wrong thing.
		if in := containingFloorDaddrItem(addr); in.IsValid() {
			return invalid(ClassViaEndpointInFloor,
				"%s.endpoint %s lies inside the L0 floor item %s. No declaration lifts the floor, so "+
					"every flow steered there would be permanently fail-closed — a declaration that "+
					"provably cannot work (§2.4 / §6.2)", where, addr, in)
		}
		// CLASS 5.
		local, herr := hostLocal(addr)
		if herr != nil {
			return herr
		}
		if !local {
			return invalid(ClassViaEndpointNotHostLocal,
				"%s.endpoint %s is not an address of this host. The SECOND hop — that process's own "+
					"egress on the sandbox's behalf — has to be pinned by the kernel, and the kernel's "+
					"reach stops here; accepting a remote endpoint would rest the L0 floor on that "+
					"process's own discipline, which is the confused deputy §2.4's second invariant "+
					"rejects (network.md D4.2)", where, addr)
		}
	}

	for i, p := range d.Ingress.Publish {
		where := fmt.Sprintf("ingress.publish[%d]", i)
		if p.Proto == "" {
			return invalid(ClassProtoMissing, "%s: `proto` is required (tcp | udp)", where)
		}
		if p.Proto != ruleset.TCP && p.Proto != ruleset.UDP {
			return invalid(ClassProtoMissing, "%s: `proto` must be tcp or udp, got %q", where, p.Proto)
		}
		if p.GuestPort == 0 {
			return invalid(ClassProtoMissing, "%s: `guest_port` is required and must be non-zero", where)
		}
	}
	return nil
}

// validateTuple applies class 3 (and the well-formedness class 2/8 already
// handled at parse time cannot re-enter here, since a Tuple holds a parsed
// prefix).
func validateTuple(t Tuple, where string) error {
	if t.Proto == "" {
		return invalid(ClassProtoMissing,
			"%s: `proto` is required — `{cidr, port}` is not a well-formed tuple, because port "+
				"matching depends on protocol matching in the mechanism that carries it "+
				"(network.md D3.1)", where)
	}
	if t.Proto != ruleset.TCP && t.Proto != ruleset.UDP {
		return invalid(ClassProtoMissing, "%s: `proto` must be tcp or udp, got %q", where, t.Proto)
	}
	return nil
}

// containingFloorItem returns the never-liftable floor item wholly containing p,
// or an invalid prefix. Loopback and link-local ONLY — see
// ruleset.FloorLiftableNever for why the sibling CIDR is excluded (an allow
// inside it is a LEGAL declaration path, and a 400 naming it would put the
// sandbox address space on the wire).
func containingFloorItem(p netip.Prefix) netip.Prefix {
	if !p.IsValid() {
		return netip.Prefix{}
	}
	for _, item := range ruleset.FloorLiftableNever {
		if wholly(item, p) {
			return item
		}
	}
	return netip.Prefix{}
}

// containingFloorDaddrItem is the same test over the floor's FULL daddr list —
// loopback, link-local AND the sibling CIDR — used by class 6, which §6.2 defines
// over the whole address list rather than the never-liftable subset.
func containingFloorDaddrItem(a netip.Addr) netip.Prefix {
	host := netip.PrefixFrom(a, a.BitLen())
	for _, set := range [][]netip.Prefix{ruleset.FloorV4, ruleset.FloorV6} {
		for _, item := range set {
			if wholly(item, host) {
				return item
			}
		}
	}
	return netip.Prefix{}
}

// wholly reports whether outer contains ALL of inner. `.Unmap()` is applied even
// though ParseCIDR already refused v4-mapped literals, and deliberately so: if
// someone later drops that refusal, this test must not silently become
// family-blind (network.md D5 ⑲ names exactly that regression).
func wholly(outer, inner netip.Prefix) bool {
	o, i := unmapPrefix(outer), unmapPrefix(inner)
	if o.Addr().Is4() != i.Addr().Is4() {
		return false
	}
	return o.Bits() <= i.Bits() && o.Contains(i.Addr())
}

func unmapPrefix(p netip.Prefix) netip.Prefix {
	a := p.Addr().Unmap()
	bits := p.Bits()
	if p.Addr().Is4In6() && bits >= 96 {
		bits -= 96
	}
	return netip.PrefixFrom(a, bits)
}
