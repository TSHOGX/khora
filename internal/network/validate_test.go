package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"engine/internal/model"
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

// Gate A item 2: §6.2's eight 400 classes, one by one.
func TestTheEightHundredClasses(t *testing.T) {
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

// Gate A item 2, class 8's ORDERING half: a v4-mapped literal must be refused
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

// Gate A item 2, the MUST-BE-200 half. Class 7 is the one class with a real risk of
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
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.d, true, allHostLocal); err != nil {
				t.Errorf("refused a legal declaration (%s): %v", tc.why, err)
			}
		})
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

// Gate A item 2b: omitting `network` normalizes to an EXPLICIT {deny, [], [], []},
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

// Gate A item 14d's declaration-layer half: repeated publish of the same
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

// Gate A item 4b's third assertion: a `via[]` declaration is REFUSED when the
// deployment supplied no second-hop uid, rather than installing a rule set with
// no second-hop enforcement.
//
// This is a ninth refusal and it is NOT one of §6.2's eight — it is a property of
// the DEPLOYMENT, not of the declaration text. Logged as a spec gap
// (open-decisions "§6.2 第九类拒绝"); see ClassViaWithoutSecondHopUID.
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
