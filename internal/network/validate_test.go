package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/TSHOGX/warren/internal/model"
)

// noHostLocal / allHostLocal are hostLocal fixtures. The predicate is a PARAMETER
// precisely so these classes are pinned against a fixture rather than against
// whatever addresses the test machine happens to carry — otherwise the class-5
// assertions would pass or fail with the CI host's interface list.
func noHostLocal(netip.Addr) (bool, error)  { return false, nil }
func allHostLocal(netip.Addr) (bool, error) { return true, nil }

func mustCIDR(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return p
}

// §6.2's nine declaration 400 classes, one by one.
func TestTheNineHundredClasses(t *testing.T) {
	cases := []struct {
		name  string
		class Class
		run   func() error
	}{{
		// CLASS 1. Note what this is NOT: omitting the whole `network` object is
		// legal — asserted separately in TestOmittedNetworkNormalizesToDenyAndEmpty.
		name:  "egress.default missing when network is given",
		class: ClassDefaultMissing,
		run: func() error {
			return Validate(Declaration{}, true, noHostLocal)
		},
	}, {
		name:  "egress.default is not one of the two values",
		class: ClassDefaultMissing,
		run: func() error {
			return Validate(Declaration{Egress: Egress{Default: "permit"}}, true, noHostLocal)
		},
	}, {
		// CLASS 2. Refused on COST grounds, not feasibility (network.md D2).
		name:  "cidr is a domain name",
		class: ClassCIDRNotLiteral,
		run: func() error {
			_, err := ParseCIDR("api.example.com")
			return err
		},
	}, {
		// CLASS 3. `{cidr, port}` is not a well-formed tuple (D3.1).
		name:  "tuple has no proto",
		class: ClassProtoMissing,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443}},
			}}, true, noHostLocal)
		},
	}, {
		// CLASS 4. Inbound publish with udp is legal — asserted separately below.
		name:  "via[].match names udp",
		class: ClassViaMatchUDP,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Via: []Via{{
					Match:    []Tuple{{CIDR: mustCIDR(t, "8.8.8.8/32"), Port: 53, Proto: model.NetUDP}},
					Endpoint: "10.0.0.5:1080",
				}},
			}}, true, allHostLocal)
		},
	}, {
		// CLASS 5. The kernel's reach stops at this host, so a remote endpoint
		// cannot be pinned; accepting one rests the floor on that process's own
		// discipline (D4.2).
		name:  "via[].endpoint is not host-local",
		class: ClassViaEndpointNotHostLocal,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Via: []Via{{
					Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
					Endpoint: "203.0.113.9:8080",
				}},
			}}, true, noHostLocal)
		},
	}, {
		// CLASS 6. Every steered flow would be permanently fail-closed.
		name:  "via[].endpoint lies inside the floor",
		class: ClassViaEndpointInFloor,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Via: []Via{{
					Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
					Endpoint: "127.0.0.1:8080",
				}},
			}}, true, allHostLocal)
		},
	}, {
		// CLASS 7. A dead rule, and the cost is a second population of legitimate
		// zeros in the one signal every deny assertion reads (D5 ㉑).
		name:  "egress.allow lies wholly inside a never-liftable floor item",
		class: ClassAllowInsideFloor,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{CIDR: mustCIDR(t, "169.254.169.254/32"), Port: 80, Proto: model.NetTCP}},
			}}, true, noHostLocal)
		},
	}, {
		name:  "egress.allow inside loopback",
		class: ClassAllowInsideFloor,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{CIDR: mustCIDR(t, "127.0.0.53/32"), Port: 53, Proto: model.NetUDP}},
			}}, true, noHostLocal)
		},
	}, {
		name:  "egress.allow inside v6 link-local",
		class: ClassAllowInsideFloor,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{CIDR: mustCIDR(t, "fe80::1/128"), Port: 80, Proto: model.NetTCP}},
			}}, true, noHostLocal)
		},
	}, {
		// V6 LOOPBACK, and it is here because class 8 made this cell UNREACHABLE: a
		// `::/96` containment test also matches `::1`, so the parse refused a floor
		// member with the v4-mapped class and the advice "write it in IPv4 form" —
		// which `::1` has no way to follow. Class 7 could never be reported for it.
		name:  "egress.allow inside v6 loopback",
		class: ClassAllowInsideFloor,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{CIDR: mustCIDR(t, "::1/128"), Port: 80, Proto: model.NetTCP}},
			}}, true, noHostLocal)
		},
	}, {
		// CLASS 8, and it is class 7's NECESSARY PREREQUISITE (D5 ⑲).
		name:  "cidr is a v4-mapped v6 literal",
		class: ClassV4Mapped,
		run: func() error {
			_, err := ParseCIDR("::ffff:169.254.169.254")
			return err
		},
	}, {
		name:  "cidr is a v4-mapped v6 prefix",
		class: ClassV4Mapped,
		run: func() error {
			_, err := ParseCIDR("::ffff:192.168.1.1/128")
			return err
		},
	}, {
		name:  "cidr is a v4-compatible v6 literal",
		class: ClassV4Mapped,
		run: func() error {
			_, err := ParseCIDR("::192.168.1.1")
			return err
		},
	}, {
		// THE ENDPOINT SIDE OF CLASS 8, whose reason is its own rather than an analogy
		// with `cidr`: `dnat` is family-typed and `Is4()` is false for the mapped form,
		// so this renders `dnat ip6 to …` onto an IPv4 flow — the rule installs, matches
		// nothing, and `via[]` is silently dead with NO counter moving. The containment
		// checks survive the spelling (they unmap), which is what hid it.
		//
		// The CLASS is the assertion, not merely the refusal: reporting this as
		// "endpoint not host-local" would send the caller to check whether the address
		// is on this host when their problem is how they spelled it.
		name:  "via endpoint is a v4-mapped v6 literal",
		class: ClassV4Mapped,
		run: func() error {
			return Validate(Declaration{Egress: Egress{Default: DenyAll, Via: []Via{{
				Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
				Endpoint: "[::ffff:10.0.0.5]:1080",
			}}}}, true, allHostLocal)
		},
	}, {
		// CLASS 9, first shape. The remedy is to ADD A MEMBER to an empty array, which
		// is why it is not class 3: there is no tuple yet for a `proto` field to be
		// missing from.
		name:  "via[] entry has an empty match[]",
		class: ClassMalformedShape,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Via:     []Via{{Match: []Tuple{}, Endpoint: "127.0.0.9:15443"}},
			}}, true, allHostLocal)
		},
	}, {
		// CLASS 9, second shape. `guest_port` is a DNAT TARGET, so zero has nothing to
		// rewrite to — the opposite of a tuple's `port`, where zero is the wildcard.
		name:  "publish[] entry has guest_port 0",
		class: ClassMalformedShape,
		run: func() error {
			return Validate(Declaration{
				Egress:  Egress{Default: DenyAll},
				Ingress: Ingress{Publish: []Publish{{GuestPort: 0, Proto: model.NetTCP}}},
			}, true, noHostLocal)
		},
	}, {
		// CLASS 9, third shape — and the one whose reason is the FLOOR rather than the
		// request's shape. The allows are emitted on `input` too (semantics ③), where the
		// floor carries no address list, so a destination-less accept above it reaches
		// EVERY address of the host itself: §2.4's one unliftable item, lifted.
		//
		// It is invisible from the sibling side — the sibling drop sits above the allows
		// and keeps holding — so every sibling-focused assertion passes while the
		// host-itself item is gone. That is why it needs an assertion of its own.
		name:  "egress.allow names no cidr",
		class: ClassMalformedShape,
		run: func() error {
			return Validate(Declaration{Egress: Egress{
				Default: DenyAll,
				Allow:   []Tuple{{Proto: model.NetTCP}},
			}}, true, noHostLocal)
		},
	}, {
		// CLASS 9, fourth shape. §5.D puts deduplication at the DECLARATION layer because
		// the substrate will not do it: nft accepts two byte-identical DNATs and never
		// matches the second. The provider refuses the pair at PLACEMENT, but by then the
		// row is committed — so that refusal repeats at every resume and the sandbox never
		// starts again.
		name:  "publish[] repeats one {guest_port, proto}",
		class: ClassMalformedShape,
		run: func() error {
			return Validate(Declaration{
				Egress: Egress{Default: DenyAll},
				Ingress: Ingress{Publish: []Publish{
					{GuestPort: 8080, Proto: model.NetTCP},
					{GuestPort: 8080, Proto: model.NetTCP},
				}},
			}, true, noHostLocal)
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			// ONE sentinel drives the status code — §6.2 maps all of these to 400.
			if !errors.Is(err, ErrDeclarationInvalid) {
				t.Errorf("error does not satisfy errors.Is(_, ErrDeclarationInvalid): %v", err)
			}
			// …but the CLASS must stay recoverable, or two classes could collapse
			// into one and no test would notice.
			got, ok := ClassOf(err)
			if !ok {
				t.Fatalf("error carries no class: %v", err)
			}
			if got != tc.class {
				t.Errorf("class = %q, want %q (err: %v)", got, tc.class, err)
			}
		})
	}
}

// Class 8's ORDERING half: a v4-mapped literal must be refused
// BEFORE any containment judgment.
//
// The position is the whole point (D5 ⑲): `netip.Prefix.Contains` does NOT unmap,
// so a family-branching containment test judges `::ffff:169.254.169.254` to be v6,
// compares it only against v6 members, and LETS IT THROUGH — after which the packet
// leaves as IPv4 and dies on the v4 floor. Accepted by the API, silently refused on
// the wire.
//
// Asserted by the CLASS, not merely by "it was rejected": if the containment test
// ran first this would come back as ClassAllowInsideFloor (or as an accept), and
// either answer means the ordering broke.
func TestV4MappedIsRefusedBeforeAnyContainmentTest(t *testing.T) {
	// The bare netip fact this class exists because of. If a Go release ever makes
	// Contains unmap, this test says so rather than the class quietly losing its
	// reason.
	linkLocal := netip.MustParsePrefix("169.254.0.0/16")
	mapped := netip.MustParseAddr("::ffff:169.254.169.254")
	if linkLocal.Contains(mapped) {
		t.Error("netip.Prefix.Contains now unmaps v4-mapped addresses — D5 ⑲'s premise changed; " +
			"re-check whether class 8 still needs to precede the containment test")
	}
	if !linkLocal.Contains(mapped.Unmap()) {
		t.Fatal("fixture is wrong: the unmapped address is not in link-local")
	}

	_, err := ParseCIDR("::ffff:169.254.169.254")
	if err == nil {
		t.Fatal("a v4-mapped spelling of a link-local address was accepted")
	}
	class, _ := ClassOf(err)
	if class != ClassV4Mapped {
		t.Errorf("class = %q, want %q — anything else means the containment test ran FIRST, "+
			"which is the family-blind path D5 ⑲ measured: judged v6, compared only against v6 "+
			"members, let through, then killed on the v4 floor", class, ClassV4Mapped)
	}
}

// The MUST-BE-200 half. Class 7 is the one class with a real risk of
// over-reach, and each of these is a case a naive implementation refuses.
func TestClassSevenDoesNotOverReach(t *testing.T) {
	cases := []struct {
		name string
		d    Declaration
		why  string
	}{{
		// A PARTIALLY overlapping supernet. Ordering resolves it correctly —
		// measured: under a floor holding 10.66.0.0/16, an `allow 10.0.0.0/8` reaches
		// the internal 10.77.x host while the sibling stays blocked, each with its own
		// counter moving (D5 ㉑). An intersection-based test would make this
		// legitimate declaration impossible.
		name: "supernet partially overlapping the floor",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "10.0.0.0/8"), Port: 53, Proto: model.NetUDP}},
		}},
		why: "whole containment, never intersection (D5 ㉑)",
	}, {
		// THE SAME CIDR inside `via[].match[]` is legal where it is fatal in
		// `allow[]`, and the asymmetry is a MECHANISM fact: a steered flow is DNAT'd
		// in prerouting, BEFORE the filter floor, so it lands in `input` and is taken
		// by the narrow accept there (measured: 1–2 ms connect, real banner read
		// back) — while the same address via `allow[]` is a dead rule. Extending
		// class 7 here would kill `via[]`'s main use, handing an in-floor
		// destination to a process outside the engine (a metadata proxy is exactly
		// that).
		name: "an in-floor CIDR inside via[].match[]",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Via: []Via{{
				Match:    []Tuple{{CIDR: mustCIDR(t, "169.254.169.254/32"), Port: 80, Proto: model.NetTCP}},
				Endpoint: "10.0.0.5:15080",
			}},
		}},
		why: "via[] does not lift the floor: the guest never reaches that address, a host-local process does (D5 ㉑)",
	}, {
		// Inside the SIBLING CIDR, including this sandbox's own gateway /32. It is
		// the DECLARED path §5.D semantics ③ requires — the only stably-available
		// host-local resolver address — not a dead rule (D4.2 / D5 ⑦).
		name: "an allow inside the sibling CIDR",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "10.66.0.0/16"), Port: 53, Proto: model.NetUDP}},
		}},
		why: "the sibling CIDR is excluded from class 7 (§6.2): this sandbox's own gateway lives there",
	}, {
		name: "an allow naming a single /32 gateway inside the sibling CIDR",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "10.66.250.1/32"), Port: 53, Proto: model.NetUDP}},
		}},
		why: "§5.D semantics ③'s declared resolver path",
	}, {
		// A supernet of a floor item is not contained BY it.
		name: "a supernet of link-local",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "169.0.0.0/8"), Port: 80, Proto: model.NetTCP}},
		}},
		why: "containment is one-directional: the floor item must contain the declaration",
	}, {
		// `::1/128` MUST PARSE, or class 7 is unreachable for v6 loopback: a `::/96`
		// containment test in class 8 also matches `::1`, and the parse then refuses a
		// floor member with the wrong class and an unfollowable remedy. Here it must
		// reach class 7 — which the class table asserts; this arm asserts the OTHER
		// direction, that a legal v6 declaration nearby is not swept up.
		name: "a v6 supernet of loopback",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "2606:4700::/32"), Port: 443, Proto: model.NetTCP}},
		}},
		why: "an ordinary global v6 prefix is not a v4 spelling of anything",
	}, {
		// THE WILDCARD DESTINATION IS LEGAL HERE and fatal on `allow[]` — the same
		// mechanism asymmetry class 7 already has between the two fields. This is D3.3's
		// declared transparent-interception form, and the steering rule still pins the
		// flow by its post-NAT port and `iifname`; an allow has no other clauses at all.
		name: "a cidr-less tuple inside via[].match[]",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Via: []Via{{
				Match:    []Tuple{{Port: 443, Proto: model.NetTCP}},
				Endpoint: "10.0.0.5:15443",
			}},
		}},
		why: "an omitted cidr in via[].match[] is the transparent-interception form (D3.3)",
	}, {
		// Two publishes on ONE guest port are DISTINCT items: the key is
		// {guest_port, proto}, so tcp and udp there are two rules, not a duplicate.
		name: "tcp and udp published on the same guest port",
		d: Declaration{
			Egress: Egress{Default: DenyAll},
			Ingress: Ingress{Publish: []Publish{
				{GuestPort: 8080, Proto: model.NetTCP},
				{GuestPort: 8080, Proto: model.NetUDP},
			}},
		},
		why: "a publish is keyed on {guest_port, proto}, so these are two items",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.d, true, allHostLocal); err != nil {
				t.Errorf("refused a legal declaration (%s): %v", tc.why, err)
			}
		})
	}
}

// A tuple with NO `port` is LEGAL and means ANY PORT (§5.D), at all three sites that
// carry a tuple.
//
// This is the direction that was silently wrong before the class list grew a
// well-formedness bucket: `{cidr, proto}` was accepted and rendered as "every port on
// that CIDR" — which is the RIGHT semantic, but it existed only as the zero value's
// side effect, with nothing in §5.D saying so and nothing here asserting it. Now it is
// a declared semantic, so it gets the assertion a declared semantic gets, in both
// directions (the refusal direction is `proto`, asserted in the class table above).
func TestAnOmittedPortMeansAnyPortAtEverySite(t *testing.T) {
	cases := []struct {
		name string
		d    Declaration
	}{{
		name: "egress.allow[] without a port",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Allow:   []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Proto: model.NetTCP}},
		}},
	}, {
		name: "via[].match[] without a port",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Via: []Via{{
				Match: []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Proto: model.NetTCP}},
				// NOT a loopback endpoint: that is class 6 regardless of the port, and it
				// would make this case pass or fail for the wrong reason.
				Endpoint: "10.0.0.5:15443",
			}},
		}},
	}, {
		// Both wildcards at once: no cidr AND no port. "Every TCP flow this sandbox
		// opens goes to the endpoint" is a legal and useful declaration — it is what a
		// transparent egress proxy is declared as.
		name: "via[].match[] with neither cidr nor port",
		d: Declaration{Egress: Egress{
			Default: DenyAll,
			Via:     []Via{{Match: []Tuple{{Proto: model.NetTCP}}, Endpoint: "10.0.0.5:15443"}},
		}},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.d, true, allHostLocal); err != nil {
				t.Errorf("refused a tuple with no port: %v — an omitted `port` means ANY PORT "+
					"(§5.D); only `proto` is mandatory, because port matching depends on protocol "+
					"matching in the kernel (D3.1)", err)
			}
		})
	}

	// The wildcard must also be READABLE as one rather than inferred from a bare zero,
	// which is what AnyPort exists for (model.NetTuple's twin of AnyDest).
	wild := Tuple{CIDR: mustCIDR(t, "1.1.1.1/32"), Proto: model.NetTCP}
	if !wild.AnyPort() {
		t.Error("a tuple with no port does not report AnyPort")
	}
	if (Tuple{Port: 443, Proto: model.NetTCP}).AnyPort() {
		t.Error("a tuple that named a port reports AnyPort")
	}
}

// The well-formedness class must be DISTINGUISHABLE from the proto class, and that is
// the entire reason it exists as a class rather than as two more `invalid(...)` calls
// under class 3.
//
// §6.2's cardinality criterion is the caller's REMEDY, not the implementation's error
// count (six Volume sentinels, four classes). These remedies are different EDITS:
// class 3 says "write `proto` in this tuple" — one field, and the error names it —
// while class 9 says "this array has no entries" or "supply the target port". Reported
// as `tuple-missing-proto`, an empty `match[]` sends the caller to inspect a `proto`
// field that does not exist yet, because there is no tuple to hold one.
func TestMalformedShapeIsDistinguishableFromAMisspelledProto(t *testing.T) {
	emptyMatch := Validate(Declaration{Egress: Egress{
		Default: DenyAll,
		Via:     []Via{{Match: []Tuple{}, Endpoint: "10.0.0.5:15443"}},
	}}, true, allHostLocal)
	badProto := Validate(Declaration{Egress: Egress{
		Default: DenyAll,
		Via: []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: "tpc"}},
			Endpoint: "10.0.0.5:15443",
		}},
	}}, true, allHostLocal)
	if emptyMatch == nil || badProto == nil {
		t.Fatalf("one of the two malformed declarations was accepted: emptyMatch=%v badProto=%v",
			emptyMatch, badProto)
	}
	shapeClass, _ := ClassOf(emptyMatch)
	protoClass, _ := ClassOf(badProto)
	if shapeClass == protoClass {
		t.Errorf("both report class %q — an empty `match[]` and a misspelled `proto` need "+
			"DIFFERENT edits, so collapsing them sends the caller to inspect a `proto` field "+
			"that does not exist yet (§6.2's remedy criterion)", shapeClass)
	}
	if shapeClass != ClassMalformedShape {
		t.Errorf("empty match[] reports class %q, want %q", shapeClass, ClassMalformedShape)
	}
	if protoClass != ClassProtoMissing {
		t.Errorf("misspelled proto reports class %q, want %q", protoClass, ClassProtoMissing)
	}

	// `guest_port: 0` shares class 9 with the empty `match[]` — ONE class, because both
	// remedies are "supply the structurally required member". Splitting them would count
	// implementation error values rather than caller-visible kinds.
	zeroGuestPort := Validate(Declaration{
		Egress:  Egress{Default: DenyAll},
		Ingress: Ingress{Publish: []Publish{{Proto: model.NetTCP}}},
	}, true, noHostLocal)
	if zeroGuestPort == nil {
		t.Fatal("a publish with guest_port 0 was accepted — it is a DNAT target, so zero has " +
			"nothing to rewrite to")
	}
	if c, _ := ClassOf(zeroGuestPort); c != ClassMalformedShape {
		t.Errorf("guest_port 0 reports class %q, want %q", c, ClassMalformedShape)
	}
	// …and every one of them is still a 400 through the single sentinel.
	for _, err := range []error{emptyMatch, badProto, zeroGuestPort} {
		if !errors.Is(err, ErrDeclarationInvalid) {
			t.Errorf("%v does not satisfy errors.Is(_, ErrDeclarationInvalid)", err)
		}
	}
}

// `ingress.publish[]` with `proto: udp` is legal. §6.2 is explicit: `via[]`
// excludes UDP because the original destination cannot be recovered, and a DNAT to
// a FIXED guest port needs no such recovery — so that objection does not reach
// inbound at all.
func TestPublishAcceptsUDPWhileViaRefusesIt(t *testing.T) {
	d := Declaration{
		Egress:  Egress{Default: DenyAll},
		Ingress: Ingress{Publish: []Publish{{GuestPort: 5353, Proto: model.NetUDP}}},
	}
	if err := Validate(d, true, noHostLocal); err != nil {
		t.Errorf("publish with proto udp was refused: %v — a DNAT to a fixed guest port does not "+
			"need the original destination, so via[]'s UDP objection does not apply (§6.2)", err)
	}
	// The paired direction, so this is not just "udp appears somewhere and passes".
	viaUDP := Declaration{Egress: Egress{
		Default: DenyAll,
		Via: []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "8.8.8.8/32"), Port: 53, Proto: model.NetUDP}},
			Endpoint: "10.0.0.5:1080",
		}},
	}}
	if err := Validate(viaUDP, true, allHostLocal); err == nil {
		t.Error("via[].match with proto udp was accepted")
	}
}

// Omitting `network` normalizes to an EXPLICIT {deny, [], [], []},
// and "given but missing egress.default" is still a 400.
//
// BOTH DIRECTIONS, because the two share a code path and whichever is implemented
// first eats the other: a normalizer that fills in `deny` for a missing field
// turns class 1 into a silent accept, and a validator that requires the field
// unconditionally makes the legal omission a 400.
func TestOmittedNetworkNormalizesToDenyAndEmpty(t *testing.T) {
	z := Zero()
	if z.Egress.Default != DenyAll {
		t.Errorf("Zero().Egress.Default = %q, want %q — the fail-closed side, and exactly the "+
			"minimal declaration (§5.D)", z.Egress.Default, DenyAll)
	}
	// Non-nil empties: §6.1 requires the three arrays to always appear as `[]`,
	// never null and never omitted — they are DEFINITE values in the record, and
	// the omit-when-unreadable rule governs facts that cannot be read.
	if z.Egress.Allow == nil || z.Egress.Via == nil || z.Ingress.Publish == nil {
		t.Error("Zero() leaves a nil slice — §6.1: the three arrays always appear, empty is `[]`")
	}
	if len(z.Egress.Allow) != 0 || len(z.Egress.Via) != 0 || len(z.Ingress.Publish) != 0 {
		t.Error("Zero() is not empty")
	}
	// The omission is legal (given=false skips class 1)…
	if err := Validate(z, false, noHostLocal); err != nil {
		t.Errorf("the normalized omission was refused: %v", err)
	}
	// …and it must still generate a working rule set, not an empty one: a sandbox
	// that declared nothing still gets the floor.
	spec, err := Generate(z)
	if err != nil {
		t.Fatalf("Generate(Zero()): %v", err)
	}
	if spec.Rules.Empty() {
		t.Error("the minimal declaration generated NO rules — `deny` + empty is not `no enforcement`, " +
			"it is the L0 floor plus a default-deny")
	}
	// …while "given, missing the field" is class 1.
	err = Validate(Declaration{}, true, noHostLocal)
	if class, _ := ClassOf(err); class != ClassDefaultMissing {
		t.Errorf("a given-but-incomplete network object produced class %q, want %q — the two cases "+
			"share a path and the first one written eats the other", class, ClassDefaultMissing)
	}
}

// Normalize must not mutate a caller's declaration in place, or a
// read-modify-write cycle could alias the record it just read.
func TestNormalizeIsIdempotentAndDoesNotAlias(t *testing.T) {
	d := Declaration{Egress: Egress{Default: AllowPublic}}
	n1 := d.Normalize()
	if d.Egress.Allow != nil {
		t.Error("Normalize mutated its receiver")
	}
	n2 := n1.Normalize()
	if len(n2.Egress.Allow) != 0 || len(n2.Ingress.Publish) != 0 {
		t.Error("Normalize is not idempotent")
	}
}

// The declaration-layer half of publish idempotence: repeated publish of the same
// `{guest_port, proto}` is idempotent.
//
// DEDUPLICATION HAS TO LIVE HERE, because the substrate will not find it: nft
// accepts two byte-identical DNAT rules and the second simply never matches
// (measured, §5.D). And with two identical items recorded, a later PATCH removing
// one leaves the other — "revoked, and the port is still open".
func TestAddPublishIsIdempotentOnPortAndProto(t *testing.T) {
	d := Zero()
	d, added := AddPublish(d, Publish{GuestPort: 8080, Proto: model.NetTCP})
	if !added {
		t.Fatal("first publish reported no change")
	}
	d, added = AddPublish(d, Publish{GuestPort: 8080, Proto: model.NetTCP})
	if added {
		t.Error("a repeated publish of the same {guest_port, proto} reported a change")
	}
	if len(d.Ingress.Publish) != 1 {
		t.Errorf("declaration holds %d publish items after a repeat, want 1 — nft would accept two "+
			"identical DNATs silently, and a later PATCH removing one would leave the port open (§5.D)",
			len(d.Ingress.Publish))
	}
	// Same port, DIFFERENT proto is a different item — the key is the pair.
	d, added = AddPublish(d, Publish{GuestPort: 8080, Proto: model.NetUDP})
	if !added || len(d.Ingress.Publish) != 2 {
		t.Errorf("tcp/8080 and udp/8080 collapsed into one item (%d items, added=%v)", len(d.Ingress.Publish), added)
	}
	// It must not alias: the copy-on-append is what keeps a read-modify-write from
	// writing through to the value it read.
	base := Zero()
	base, _ = AddPublish(base, Publish{GuestPort: 1, Proto: model.NetTCP})
	a, _ := AddPublish(base, Publish{GuestPort: 2, Proto: model.NetTCP})
	b, _ := AddPublish(base, Publish{GuestPort: 3, Proto: model.NetTCP})
	if a.Ingress.Publish[1] == b.Ingress.Publish[1] {
		t.Error("AddPublish aliased the backing array — two independent appends produced the same element")
	}
}

// A `via[]` declaration is REFUSED when the deployment supplied no second-hop uid,
// rather than installing a rule set with no second-hop enforcement.
//
// This refusal is NOT one of §6.2's declaration-text classes — it is a property of
// the DEPLOYMENT, which is why §6.2 states it separately and why it keeps its own
// sentinel (its remedy is an operator's). See ClassViaWithoutSecondHopUID.
func TestViaIsRefusedWhenTheDeploymentPinsNoSecondHop(t *testing.T) {
	via := Declaration{Egress: Egress{
		Default: DenyAll,
		Via: []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "10.0.0.5:15443",
		}},
	}}
	err := ValidateDeployment(via, Deployment{})
	if err == nil {
		t.Fatal("a via[] declaration was accepted with no second-hop uid — that records a " +
			"declaration whose §2.4 invariant 2 the engine cannot enforce, leaving a confused deputy " +
			"live while every FIRST-hop assertion still passes (D5 ㉔)")
	}
	if class, _ := ClassOf(err); class != ClassViaWithoutSecondHopUID {
		t.Errorf("class = %q, want %q", class, ClassViaWithoutSecondHopUID)
	}
	if !errors.Is(err, ErrDeclarationInvalid) {
		t.Error("the deployment refusal does not map to the 400 sentinel")
	}
	// Given the uid, the same declaration passes.
	uid := 4711
	if err := ValidateDeployment(via, Deployment{SecondHopUID: &uid}); err != nil {
		t.Errorf("via[] refused even with a second-hop uid: %v", err)
	}
	// uid 0 is a REAL uid: a value type could not tell "the operator said root"
	// from "the operator said nothing", and the two must behave oppositely.
	root := 0
	if err := ValidateDeployment(via, Deployment{SecondHopUID: &root}); err != nil {
		t.Errorf("uid 0 was treated as unset: %v", err)
	}
	// And a declaration with no via[] is unaffected either way.
	noVia := Declaration{Egress: Egress{Default: AllowPublic}}
	if err := ValidateDeployment(noVia, Deployment{}); err != nil {
		t.Errorf("a declaration without via[] was refused for a missing second-hop uid: %v", err)
	}
}

// The same premise, at PLACEMENT. The write-point check above is only half of it,
// because the two facts it compares have different lifetimes: the declaration is durable
// (record = SoT, regenerated at every placement, §3.4) while the uid is a per-process
// deployment input. "Restart without the flag" separates them with no write anywhere —
// the write points are then correctly closed and the row admitted by the previous
// deployment is still in the store.
//
// It cannot be delegated to Generate, and that is the mechanism rather than a layering
// preference: the second-hop pin is not part of a sandbox's rule set at all (one
// engine-level `output` table narrowed by uid covers the deployment), so the generated
// rule set for a `via[]` declaration is identical whether or not the pin exists. Absence
// of enforcement is invisible in the artifact — it can only be a precondition on placing.
//
// The verdict is its OWN sentinel rather than the ninth class's 400: a resume or an adopt
// carries no declaration, so "the request is invalid" would be a false statement and
// would send the caller to edit something they never sent. The remedy is an operator's.
func TestPlacementRefusesARecordedViaTheDeploymentCannotPin(t *testing.T) {
	via := Declaration{Egress: Egress{
		Default: DenyAll,
		Via: []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "10.0.0.5:15443",
		}},
	}}
	err := ValidatePlacement(via, Deployment{})
	if err == nil {
		t.Fatal("a recorded via[] was placed under a deployment that pins no second hop — the rule " +
			"set installs and the steered flow reaches the endpoint process, whose own egress is " +
			"then pinned by nothing: the confused deputy §2.4 invariant 2 rejects, and invisible " +
			"because hop 2 counts zero in `forward` (D5 ㉔)")
	}
	if !errors.Is(err, ErrViaUnenforceableAtPlacement) {
		t.Errorf("the placement refusal carries no recoverable sentinel: %v", err)
	}
	// NOT the per-request 400: nothing about a resume/adopt request is malformed, and
	// reporting it as a declaration class would point the caller at their own payload.
	if errors.Is(err, ErrDeclarationInvalid) {
		t.Error("the placement refusal maps to the declaration-invalid 400 sentinel — a resume " +
			"carries no declaration, so 400 would send the caller to fix a payload they did not send")
	}
	if class, ok := ClassOf(err); ok {
		t.Errorf("the placement refusal reports a §6.2 declaration class (%q); it is a property of "+
			"the deployment meeting a RECORD, not of any request text", class)
	}
	// The message must name the mechanism and both remedies — an operator reading it has
	// to know that restoring the flag or PATCHing the declaration are the two ways out.
	for _, want := range []string{"confused deputy", "--via-uid", "PATCH", "forward"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q; it must explain the mechanism (the pin is not "+
				"installed, hop 2 is invisible to every first-hop assertion) and both remedies, or an "+
				"operator cannot act on it: %v", want, err)
		}
	}

	// Given the uid, the same record places.
	uid := 4711
	if err := ValidatePlacement(via, Deployment{SecondHopUID: &uid}); err != nil {
		t.Errorf("a via[] record was refused under a deployment that DOES pin the second hop: %v", err)
	}
	// uid 0 is a real uid here too, for the same reason as at the write point.
	root := 0
	if err := ValidatePlacement(via, Deployment{SecondHopUID: &root}); err != nil {
		t.Errorf("uid 0 was treated as unset at placement: %v", err)
	}
	// A declaration with no via[] places under any deployment — this refusal must not
	// become a general "no uid ⇒ no placement" gate.
	noVia := Declaration{Egress: Egress{Default: AllowPublic}}
	if err := ValidatePlacement(noVia, Deployment{}); err != nil {
		t.Errorf("a declaration without via[] was refused at placement: %v", err)
	}
}

// A second-hop uid EQUAL TO THE ENGINE'S OWN euid is refused, and this is the
// assertion that did not exist while the defect did.
//
// The mechanism, in full, because "reject uid 0" looks like fastidiousness until it is
// spelled out: the second-hop floor arm is a `meta skuid <uid>` drop whose daddr set
// contains ruleset.SandboxCIDR (FloorV4's sibling item), and the engine's own transport
// endpoint is a GUEST veth address inside that /16 dialed from the host. Under a uid
// equal to the engine's, every engine→guest dial matches that arm. The
// `established,related` return arm cannot rescue it BY DESIGN — the dial is `ct state
// new`, exactly what the arm excludes so that a new connection to an in-floor address
// still meets the floor (§2.4 invariant 2).
//
// Two things make it the worst shape of failure rather than merely a broken one, and
// both are why this refuses at ASSEMBLY rather than at a write point:
//   - the table is ENGINE-LEVEL, so every sandbox loses transport at once, including
//     ones with no `via[]` at all;
//   - `skuid` matches only ~4/5 packets of a flow (D5 ㉔), so the symptom is
//     intermittent sandbox failure, not a clean break.
//
// The euid is a PARAMETER, which is what lets this run root-free: the refusal is
// asserted against a fixture rather than against whatever uid the test process is.
func TestASecondHopUIDEqualToTheEnginesOwnIsRefused(t *testing.T) {
	// The production shape: the engine runs as root (ADR-0002) and the operator passes
	// --via-uid 0.
	root := 0
	err := ValidateSecondHopUID(Deployment{SecondHopUID: &root}, 0)
	if err == nil {
		t.Fatal("a second-hop uid equal to the engine's own euid was accepted. That installs an " +
			"engine-level `output` drop matching the engine's OWN dials to guest addresses (the " +
			"floor's daddr set contains the sandbox /16, and the transport endpoint is a guest veth " +
			"address), which kills readiness, liveness and every process/fs RPC for every sandbox — " +
			"intermittently, since skuid misses packets (D5 ㉔)")
	}
	if !errors.Is(err, ErrSecondHopUIDIsTheEngineItself) {
		t.Errorf("the refusal does not carry its sentinel: %v", err)
	}
	// It is NOT one of the declaration 400s. The distinction is load-bearing: mapping it
	// to 400 would make it a per-request answer to a question no request asks, and the
	// engine must refuse to ASSEMBLE rather than serve while dropping its own traffic.
	if errors.Is(err, ErrDeclarationInvalid) {
		t.Error("the refusal maps to the declaration-invalid sentinel — this is a property of the " +
			"DEPLOYMENT alone, decidable with no declaration in hand, and it must fail startup " +
			"rather than one request")
	}
	// The message must name the MECHANISM, not just call the uid invalid: an operator
	// reading it has to learn that transport dies and that the symptom is intermittent,
	// or they will retry the same value assuming a flake.
	for _, want := range []string{"skuid", "10.66.0.0/16", "INTERMITTENT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q — it must explain the mechanism and the "+
				"intermittency, not just report an invalid uid: %v", want, err)
		}
	}

	// Non-root parity: the rule is "the engine's own uid", not "uid 0". An engine
	// running as 1000 must refuse 1000 and accept 0.
	engine := 1000
	if err := ValidateSecondHopUID(Deployment{SecondHopUID: &engine}, engine); err == nil {
		t.Error("a non-root engine accepted its OWN uid — the predicate is euid equality, not a " +
			"hardcoded 0")
	}
	if err := ValidateSecondHopUID(Deployment{SecondHopUID: &root}, engine); err != nil {
		t.Errorf("uid 0 was refused by an engine that does not run as root: %v — uid 0 is only "+
			"illegal because it is usually the ENGINE's uid, and nothing else: %v", err, err)
	}

	// Any other uid passes, and so does "no uid at all" (that case is
	// ClassViaWithoutSecondHopUID's, refused per-declaration at the write point).
	other := 12345
	if err := ValidateSecondHopUID(Deployment{SecondHopUID: &other}, 0); err != nil {
		t.Errorf("a dedicated non-engine uid was refused: %v", err)
	}
	if err := ValidateSecondHopUID(Deployment{}, 0); err != nil {
		t.Errorf("an unset second-hop uid was refused at assembly: %v — that is not a broken "+
			"deployment, it is a deployment with no `via[]`, and its refusal belongs at the write "+
			"point instead", err)
	}
}

// ParseEndpoint must require a port: the narrow accept keeping `via[]` from
// reopening the floor pins the POST-NAT port, which is this endpoint's port
// (D5 ⑩), so an endpoint without one cannot be enforced at all.
func TestParseEndpointRequiresAPortAndAnIPLiteral(t *testing.T) {
	for _, bad := range []string{"", "10.0.0.5", "proxy.local:8080", "10.0.0.5:0", "10.0.0.5:notaport", "[::1]"} {
		if _, _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("ParseEndpoint(%q) accepted", bad)
		}
	}
	addr, port, err := ParseEndpoint("10.0.0.5:15443")
	if err != nil || addr.String() != "10.0.0.5" || port != 15443 {
		t.Errorf("ParseEndpoint(v4) = %v %d %v", addr, port, err)
	}
	addr, port, err = ParseEndpoint("[fd00::1]:8080")
	if err != nil || addr.String() != "fd00::1" || port != 8080 {
		t.Errorf("ParseEndpoint(v6) = %v %d %v", addr, port, err)
	}
}

// A bare address parses as a host route — the spelling a caller naturally uses for
// a single resolver or endpoint.
func TestParseCIDRAcceptsBareAddressesAsHostRoutes(t *testing.T) {
	for in, want := range map[string]string{
		"1.1.1.1":     "1.1.1.1/32",
		"2606:4700::": "2606:4700::/128",
		"10.0.0.0/8":  "10.0.0.0/8",
		"10.0.0.7/8":  "10.0.0.0/8", // masked, so the record holds a canonical prefix
		"fc00::/7":    "fc00::/7",
	} {
		got, err := ParseCIDR(in)
		if err != nil {
			t.Errorf("ParseCIDR(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("ParseCIDR(%q) = %s, want %s", in, got, want)
		}
	}
}

// Class 6 must be reported ahead of class 5 for an endpoint that satisfies both
// (loopback is host-local AND inside the floor). Reporting "not host-local" for
// 127.0.0.1 would send the caller to fix the wrong thing.
func TestFloorEndpointIsReportedAsClassSixNotClassFive(t *testing.T) {
	d := Declaration{Egress: Egress{
		Default: DenyAll,
		Via: []Via{{
			Match:    []Tuple{{CIDR: mustCIDR(t, "1.1.1.1/32"), Port: 443, Proto: model.NetTCP}},
			Endpoint: "127.0.0.1:8080",
		}},
	}}
	// Even with a hostLocal that says NO, the floor class must win: the message the
	// caller needs is "this address can never work", not "this is not your host".
	err := Validate(d, true, noHostLocal)
	if class, _ := ClassOf(err); class != ClassViaEndpointInFloor {
		t.Errorf("class = %q, want %q — loopback satisfies both classes, and class 6 names the "+
			"caller's actual problem", class, ClassViaEndpointInFloor)
	}
	if !strings.Contains(err.Error(), "127.0.0.0/8") {
		t.Errorf("the message does not name the floor item it hit: %v", err)
	}
}
