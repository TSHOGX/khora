package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/ruleset"
)

// ErrDeclarationInvalid is the one verdict §6.2's nine declaration 400 classes
// share: a declaration that is decidable-invalid WITHOUT reading any other row, and
// that no retry or state change could ever make legal — §6.2's 400 criterion, the
// same one the mount-binding self-contradiction meets.
//
// ONE sentinel for the status code, NINE classes for the caller, and the split is
// deliberate. §6.2's Volume verdicts establish the rule the other way round: what
// must stay distinguishable is what the caller's REMEDY depends on. Here every
// remedy is "fix the declaration", so one status and one sentinel is honest —
// while the class is still recoverable via ClassOf, because the nine remedies are
// different EDITS and a test that could not tell them apart would let two classes
// collapse into one.
var ErrDeclarationInvalid = errors.New("network: declaration is invalid")

// Class names one of §6.2's nine declaration 400 classes (the tenth refusal,
// ClassViaWithoutSecondHopUID, is a property of the DEPLOYMENT — see there).
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
	// ClassProtoMissing — `proto` absent, or not one of the two values, at any of
	// the three sites that carry one (`allow[]`, `via[].match[]`, `publish[]`).
	// `{cidr, port}` is not well-formed: port matching depends on protocol matching
	// in the mechanism that carries it (network.md D3.1).
	//
	// ABSENT and MISSPELLED are ONE class, not two, and class 1 is the precedent
	// inside this same file: `egress.default` absent and `egress.default: "permit"`
	// are both ClassDefaultMissing, because the caller's remedy is the same EDIT —
	// write a legal value in that one field. §6.2's cardinality constrains the
	// semantic kinds a caller must tell apart, not the number of error values in the
	// implementation (the Volume verdicts settle this: six sentinels, four classes).
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
	// Refused rather than accepted as one harmless extra rule, on the same criterion
	// as "an uncapped sandbox cannot be given a disk cap": a promise provably
	// unmaterializable is refused at the write point. What accepting it costs the
	// counter signal is in the error body.
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
	// ClassMalformedShape — a declaration OBJECT that does not hold together as a
	// structure, independently of any value in it: a `via[]` entry with an empty
	// `match[]`, or a `publish[]` entry with `guest_port: 0`.
	//
	// ITS OWN CLASS, by the same remedy criterion class 3 states: class 3's remedy is
	// "write `proto` in this tuple" — one named field — while these need the caller to
	// ADD A MEMBER to an array or supply a port the request never mentioned. Folded into
	// `tuple-missing-proto`, the caller is sent to inspect a `proto` field that may be
	// perfectly written, or (empty `match[]`) does not exist yet for want of a tuple.
	// The two share ONE class because they are one semantic kind and one kind of edit.
	//
	// NOT this class: a tuple with no `port`, which is LEGAL and means "any port"
	// (§5.D). That is the line the class draws — an empty `match[]` steers NOTHING and
	// has no reading under which it does something, while `port` omitted has one.
	ClassMalformedShape Class = "malformed-shape"
)

// invalidError carries the class alongside the message.
type invalidError struct {
	class Class
	msg   string
}

func (e *invalidError) Error() string { return e.msg }

// Unwrap makes every class satisfy errors.Is(err, ErrDeclarationInvalid), which
// is what §6.2's status mapping keys on — one arm, nine classes.
func (e *invalidError) Unwrap() error { return ErrDeclarationInvalid }

func invalid(c Class, format string, args ...any) error {
	return &invalidError{class: c, msg: fmt.Sprintf("network: %s: %s", c, fmt.Sprintf(format, args...))}
}

// ClassViaWithoutSecondHopUID is the refusal PAST the declaration classes, and it is
// NOT one of them — those are all properties of the declaration TEXT, and this one is
// a property of the DEPLOYMENT.
//
// The refusal itself follows from how the second hop is enforced: it is pinned by an
// `output`-hook table narrowed by a uid the DEPLOYMENT supplies, not the declaration
// (§2.4 / §6.2). With no uid there is no table and therefore NO ENFORCEMENT of §2.4's
// second invariant — so accepting a `via[]` declaration would record a declaration
// whose own invariant the engine cannot deliver, leaving a confused deputy live on the
// host with every first-hop assertion still passing (network.md D5 ㉔: hop 2 counts
// zero in `forward`, so the first-hop floor never evaluates it).
//
// 400 by §6.2's own criterion: decidable without reading any other row, no caller retry
// makes it legal, and nothing is full so 507 would be a lie — exactly ErrCapRequired's
// shape, likewise a 400 that depends on deployment configuration. Its own sentinel
// because §6.2 keeps distinguishable what the caller's REMEDY depends on, and this
// remedy is an operator's (supply the uid at startup); no edit to the declaration
// reaches it.
const ClassViaWithoutSecondHopUID Class = "via-without-second-hop-uid"

// ClassOf reports which of §6.2's nine declaration classes an error is, if any.
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

// isV4Compatible reports whether a is RFC 4291's deprecated `::a.b.c.d` form: the
// high 96 bits zero AND the low 32 carrying a v4 address.
//
// The low-word test is what keeps this from over-reaching, and it is not a detail.
// A bare `::/96` containment test also matches `::` and `::1` — and `::1/128` is a
// FLOOR MEMBER (ruleset.FloorLiftableNever), so refusing it here makes §6.2's
// SEVENTH class unreachable for v6 loopback: the parse refuses the tuple before the
// containment test that would have reported the right class. It also refuses `::/0`,
// the v6 spelling of "any destination", whose v4 counterpart `0.0.0.0/0` is legal.
// RFC 4291 excludes both: the unspecified and loopback addresses are not v4
// spellings of anything, and `::1` has no IPv4 form to rewrite it into — so the
// refusal's own remedy ("write it in IPv4 form") could not be followed.
func isV4Compatible(a netip.Addr) bool {
	b := a.As16()
	for _, x := range b[:12] {
		if x != 0 {
			return false
		}
	}
	return binary.BigEndian.Uint32(b[12:]) > 1
}

func rejectV4Mapped(a netip.Addr, raw string) error {
	if a.Is4() || a.Is4In6() || (a.Is6() && isV4Compatible(a)) {
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
// the declaration classes are pinned in a root-free unit test against a fixture instead
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

// ErrSecondHopUIDIsTheEngineItself is the STARTUP refusal of a second-hop uid equal
// to the engine's own effective uid. Deliberately NOT an invalidError: the nine
// declaration classes and the deployment one are all per-request 400s over a
// declaration, while this one
// is a property of the deployment ALONE — it is decidable with no declaration in
// hand, it must be decided before the table is installed, and no request ever
// reaches a state where returning it as a 400 would be right.
var ErrSecondHopUIDIsTheEngineItself = errors.New("network: the second-hop uid is the engine's own uid")

// Deployment is what the DEPLOYING PARTY hands the engine, as distinct from what
// a declaration carries (§2.4 / §6.2: the uid pinning the second hop is a
// deployment input, never a declaration field).
type Deployment struct {
	// SecondHopUID identifies the host-local process behind `via[].endpoint`. Set =
	// the engine installs the second-hop table at startup and `via[]` is permitted;
	// unset = `via[]` is refused (see ClassViaWithoutSecondHopUID).
	//
	// A POINTER because uid 0 is a real uid: a value type could not tell "the
	// operator said root" from "the operator said nothing", and the two must lead to
	// opposite behavior.
	//
	// UID ONLY, no gid counterpart — see ruleset.ScopeSecondHopUID.
	SecondHopUID *int

	// HostLocal answers §6.2's fifth class ("is this endpoint an address of THIS
	// host"). nil → SystemHostLocal, which reads the interface table. Injectable for
	// the reason the HostLocal type states.
	HostLocal HostLocal
}

// SecondHopPinned reports whether the deployment supplied what §2.4's second
// invariant needs to be ENFORCED rather than merely prefaced.
func (dep Deployment) SecondHopPinned() bool { return dep.SecondHopUID != nil }

// ValidateSecondHopUID refuses a second-hop uid equal to the ENGINE'S OWN effective
// uid, at assembly time and before any table is installed.
//
// IT KILLS THE TRANSPORT PLANE via the engine's own topology: the floor arm is `meta skuid
// <uid> ip daddr {…, 10.66.0.0/16} drop` on the engine-level `output` chain, whose daddr set
// contains ruleset.SandboxCIDR — the /16 every guest /30 comes from. The transport endpoint IS
// a guest veth IP:port dialed BY THIS PROCESS, so under the engine's own uid every engine→guest
// dial is dropped: the readiness gate, the reaper's liveness probe, every process/fs RPC. The
// `hop2_established` arm does not save it BY DESIGN — that dial is `ct state new`, which the arm
// excludes precisely so a new connection to an in-floor address still meets the floor.
//
// REFUSED RATHER THAN WORKED AROUND: a `skuid` accept above the floor leaks whatever skuid
// misses (measured, network.md D5 ㉔; ruleset.Validate refuses one outright), and there is
// nothing else to narrow the engine's own dial by — it arrives on no interface and its
// destination is the in-floor address itself.
//
// AT STARTUP rather than the write point: the table is engine-level, so the damage lands on
// every sandbox at once, including ones with no `via[]`. The failure is also INTERMITTENT, skuid
// matching ~4/5 packets of a flow (D5 ㉔), so it presents as flaky sandboxes rather than a
// broken deployment. It is therefore not ValidateDeployment's per-request 400 either: this needs
// no declaration to be wrong (network.md D4.2 argues the whole refusal).
//
// euid is a PARAMETER rather than an os.Geteuid() call, like HostLocal: the assertion is then
// pinned in a root-free unit test against a fixture rather than the test process's own uid.
func ValidateSecondHopUID(dep Deployment, engineEUID int) error {
	if dep.SecondHopUID == nil || *dep.SecondHopUID != engineEUID {
		return nil
	}
	return fmt.Errorf("%w: %d is the uid this orchestrator runs as. The second-hop floor arm is "+
		"`meta skuid %d ip daddr {…, %s} drop` on the engine-level `output` chain, and the engine's "+
		"own transport endpoint is a GUEST veth address inside %s dialed from this host — so every "+
		"engine→guest dial would match that arm and be dropped, taking out the readiness gate, the "+
		"reaper's liveness probe, and every process/fs/files RPC, for EVERY sandbox at once (the "+
		"table is engine-level, not per-sandbox). The `established,related` return arm does not "+
		"admit it: that dial is `ct state new`, which the arm excludes precisely so a new connection "+
		"to an in-floor address still meets the floor (§2.4 invariant 2). Nor is it survivable — "+
		"`skuid` matches only about four packets in five (network.md D5 ㉔), so the symptom would be "+
		"INTERMITTENT sandbox failures rather than a clean break. Run the `egress.via[]` endpoint "+
		"process as its own dedicated uid and pass that one",
		ErrSecondHopUIDIsTheEngineItself, engineEUID, engineEUID, ruleset.SandboxCIDR, ruleset.SandboxCIDR)
}

// ErrViaUnenforceableAtPlacement is the PLACEMENT-side refusal of an already-recorded
// `via[]` declaration under a deployment that supplies no second-hop uid.
//
// Its own sentinel rather than ClassViaWithoutSecondHopUID's invalidError, because the
// verdict differs where it matters: the write-point refusal is a 400 over a request
// that carried the declaration, while this one judges a declaration ALREADY IN THE
// RECORD against the deployment now running. Nothing about the request is malformed —
// a resume carries no declaration at all — so 400 would send the caller to edit
// something they did not send. See ValidatePlacement.
var ErrViaUnenforceableAtPlacement = errors.New("network: the recorded egress.via[] cannot be enforced by this deployment")

// ValidatePlacement re-checks, AT EVERY PLACEMENT, the deployment premise that
// ValidateDeployment checked at the write point.
//
// THE WRITE POINT IS NOT ENOUGH because the two checks read the same facts but not the same fact
// PAIR over time: a declaration is durable, its record the SoT that every placement regenerates
// from (§3.4 Binding), while the second-hop uid lives and dies with one orchestrator process. So
// the pair comes apart with no write at all — deployment A supplies the uid and admits a `via[]`
// row, the operator restarts WITHOUT the flag, every write point is now correctly closed, and the
// row that got in under A is still there.
//
// Nothing on the placement path notices, mechanically: Generate emits no second-hop rule (the pin
// is one engine-level `output` table sized by the deployment, not the sandbox count), so its
// output for a `via[]` declaration is identical whether or not the pin exists. The DNAT and the
// narrow `input` accept install and work, the steered flow reaches the endpoint process, and that
// process's own egress is pinned by nothing — and NOTHING ELSE REPORTS IT, since hop 2 counts
// zero in `forward` (measured, network.md D5 ㉔). Hence a refusal rather than a degradation.
//
// The general shape, and the reason this function exists: an invariant enforced only where state
// is WRITTEN holds only while nothing else changes. This one is re-evaluated where the state is
// RE-MATERIALIZED — the point at which the engine actually promises it.
func ValidatePlacement(d Declaration, dep Deployment) error {
	if len(d.Egress.Via) == 0 || dep.SecondHopPinned() {
		return nil
	}
	return fmt.Errorf("%w: this sandbox's record carries %d `egress.via[]` entr%s, admitted by a "+
		"deployment that supplied a second-hop uid — and THIS deployment supplies none, so the "+
		"engine-level `output` table pinning the endpoint process's own egress is not installed. "+
		"Placing the sandbox anyway would install a rule set that steers the guest's flows to that "+
		"process with NO enforcement of §2.4's second invariant behind it: a confused deputy live on "+
		"the host, reachable by the guest, egressing on its behalf under no floor. It would also be "+
		"SILENT — hop 2 leaves on `output` and counts zero in `forward` (measured, network.md D5 ㉔), "+
		"so the first-hop floor never evaluates one of its packets and every first-hop assertion, "+
		"counter and deny test keeps passing. Two remedies, both an operator's: restart the "+
		"orchestrator with the second-hop uid (`--via-uid`) so the pin is installed again, or "+
		"`PATCH /v1/sandboxes/{id}` the declaration to drop `egress.via[]` (editable from `paused`, "+
		"whole for whole) and place it without the steering",
		ErrViaUnenforceableAtPlacement, len(d.Egress.Via), plural(len(d.Egress.Via)))
}

// plural spells the one suffix this file's messages need ("entry" / "entries").
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// ValidateDeployment is the refusal that depends on the DEPLOYMENT rather than on
// the declaration text — see ClassViaWithoutSecondHopUID for why it is separate
// from §6.2's nine declaration classes and why it is a 400.
//
// It is the WRITE-POINT half of the premise. ValidatePlacement is the other half, and
// the two are not redundant: this one keeps the row out of the record, that one keeps
// a row that got in under an earlier deployment from being materialized by this one.
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

// Validate applies §6.2's nine declaration 400 classes.
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
		// A WILDCARD DESTINATION IS ILLEGAL ON `allow[]`, the one class whose reason is
		// the floor itself rather than the shape of the request. An allow with no `cidr`
		// renders `iifname <veth> meta l4proto tcp accept` — no destination clause at all
		// — above the `input` floor, which carries no address list either, and so hands
		// the guest every address of the host itself on every port of that protocol:
		// §2.4's one unliftable item, lifted by a declaration (network.md D5 ⑧).
		//
		// It is invisible from the sibling side, which is why it needs its own class: the
		// sibling drop sits above the allows and keeps holding, so every sibling-focused
		// assertion still passes while the host-itself item is gone.
		//
		// The same wildcard is LEGAL in `via[].match[]` — the mechanism asymmetry class 7
		// already has between the two fields, argued in the error body below.
		if t.AnyDest() {
			return invalid(ClassMalformedShape, "egress.allow[%d] names no `cidr` — a wildcard "+
				"destination has no reading here other than lifting the L0 floor: the allows are "+
				"emitted on the `input` hook too (§5.D semantics ③), where the floor carries no "+
				"address list, so a destination-less accept above it would reach EVERY address of "+
				"the host itself on every %s port (§2.4 / network.md D5 ⑧). Name the destination. "+
				"(`via[].match[]` is the opposite case: there an omitted `cidr` is the declared "+
				"transparent-interception form, because the steering rule still pins the flow by "+
				"its post-NAT port and input interface, D3.3)", i, t.Proto)
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
			return invalid(ClassMalformedShape, "%s has no `match[]` — an entry that matches nothing "+
				"steers nothing, and there is no default match. Note the contrast with an omitted "+
				"`port`, which IS legal and means any port (§5.D): that omission has a reading, an "+
				"empty `match[]` has none", where)
		}
		for j, t := range v.Match {
			mwhere := fmt.Sprintf("%s.match[%d]", where, j)
			if err := validateTuple(t, mwhere); err != nil {
				return err
			}
			// CLASS 4.
			if t.Proto == model.NetUDP {
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
			// PRESERVE a class the parse already decided. ParseEndpoint refuses a
			// v4-mapped literal as ClassV4Mapped, and relabelling it "not host-local"
			// would send the caller to check whether the address is on this host when
			// their actual problem is the SPELLING — the same wrong-fix hazard the
			// class-6-before-class-5 ordering below exists to avoid.
			if _, ok := ClassOf(err); ok {
				return err
			}
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

	seenPublish := make(map[model.NetPublish]int, len(d.Ingress.Publish))
	for i, p := range d.Ingress.Publish {
		where := fmt.Sprintf("ingress.publish[%d]", i)
		if p.Proto == "" {
			return invalid(ClassProtoMissing, "%s: `proto` is required (tcp | udp)", where)
		}
		if p.Proto != model.NetTCP && p.Proto != model.NetUDP {
			return invalid(ClassProtoMissing, "%s: `proto` must be tcp or udp, got %q", where, p.Proto)
		}
		// A publish's `guest_port` is the DNAT target, so zero is not a wildcard here
		// and could not be one: the rule rewrites the destination to ONE port on the
		// guest IP. That is the whole asymmetry with a tuple's `port` — a MATCH omitting
		// a port matches every port, while a TARGET omitting one has nothing to rewrite
		// to (§5.D).
		if p.GuestPort == 0 {
			return invalid(ClassMalformedShape, "%s: `guest_port` is required and must be non-zero — "+
				"it is the DNAT TARGET port, not a match, so zero has nothing to rewrite to. (A "+
				"tuple's `port` is the opposite case: omitted there means any port, §5.D)", where)
		}
		// SELF-CONTRADICTION WITHIN ONE DECLARATION — the same shape as a mount binding
		// repeating a dest, and refused at the same place for the same reason (see
		// AddPublish for why the substrate will not find this for us). The provider does
		// refuse the pair at PLACEMENT, but by then the row is committed, so that refusal
		// repeats at every resume and the sandbox never starts again.
		//
		// `POST …/publish` dedups on its own (AddPublish), so this guards the paths that
		// carry the whole array — create and PATCH.
		if prev, dup := seenPublish[p]; dup {
			return invalid(ClassMalformedShape, "%s duplicates ingress.publish[%d] (%s/%d) — a publish "+
				"is keyed on `{guest_port, proto}` and two identical items cannot both take effect: "+
				"the substrate accepts both rules and never matches the second, and a later PATCH "+
				"removing one would leave the port open (§5.D)", where, prev, p.Proto, p.GuestPort)
		}
		seenPublish[p] = i
	}
	return nil
}

// validateTuple applies class 3 (and the well-formedness class 2/8 already
// handled at parse time cannot re-enter here, since a Tuple holds a parsed
// prefix).
//
// # `port` IS NOT VALIDATED HERE, AND THAT IS THE DECIDED SEMANTIC
//
// An omitted `port` means ANY PORT — §5.D says so explicitly, and this function's
// silence about the field is therefore a statement rather than a gap. The decision
// went the other way from `proto` on purpose: `proto` is a FORMAT requirement (the
// kernel mechanism makes port matching depend on protocol matching, D3.1), so
// `{cidr, port}` genuinely cannot be rendered, while `{cidr, proto}` renders
// perfectly into a rule with one clause fewer — the same shape an omitted `cidr`
// already has.
//
// The wildcard is also WRITTEN rather than left as a zero value's side effect — `port`
// is omitted on both faces when it is any-port, so the record's spelling and the
// caller's text agree. Argued at the spelling: tupleWire in the orchestrator.
func validateTuple(t Tuple, where string) error {
	if t.Proto == "" {
		return invalid(ClassProtoMissing,
			"%s: `proto` is required — `{cidr, port}` is not a well-formed tuple, because port "+
				"matching depends on protocol matching in the mechanism that carries it "+
				"(network.md D3.1). `port` itself is OPTIONAL and omitting it means any port "+
				"(§5.D); `proto` is the one member of the tuple with no wildcard", where)
	}
	if t.Proto != model.NetTCP && t.Proto != model.NetUDP {
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
