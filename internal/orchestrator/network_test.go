package orchestrator

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"engine/internal/model"
	"engine/internal/network"
)

// --- the wire decode / encode contract ----------------------------------------

// The `cidr` field is a STRING on the wire and is parsed through ParseCIDR, which is
// what makes §6.2's eighth 400 class enforceable at all.
//
// Decoding into the RECORD type instead would route past it: netip.Prefix's own text
// unmarshaling ACCEPTS `::ffff:169.254.169.254/128` (verified), and `Contains` does
// not unmap — so a family-branching containment check would judge it v6, compare it
// only against v6 members, let it through, and the packet would then die on the v4
// floor as IPv4. Accepted by the API, silently refused on the wire (network.md D5 ⑲).
func TestTheWireCIDRIsParsedRatherThanUnmarshaled(t *testing.T) {
	// The bare fact this test exists because of: the record type WOULD accept it.
	var probe model.NetTuple
	if err := json.Unmarshal([]byte(`{"cidr":"::ffff:169.254.169.254/128","proto":"tcp"}`), &probe); err != nil {
		t.Fatalf("premise changed: the record type now refuses a v4-mapped literal (%v) — if netip "+
			"started refusing these, this test's reason is gone and the wire indirection could be "+
			"revisited", err)
	}
	if !probe.CIDR.IsValid() {
		t.Fatal("premise changed: the record type no longer decodes a v4-mapped literal")
	}

	// …and the WIRE type refuses it, as class 8.
	w := networkWire{Egress: egressWire{
		Allow: []tupleWire{{CIDR: "::ffff:169.254.169.254/128", Proto: "tcp", Port: 80}},
	}}
	_, err := w.decode()
	if err == nil {
		t.Fatal("the wire decode accepted a v4-mapped literal — that is §6.2's eighth 400 class, and " +
			"it must be refused BEFORE any containment test (D5 ⑲)")
	}
	if class, _ := network.ClassOf(err); class != network.ClassV4Mapped {
		t.Errorf("class = %q, want %q", class, network.ClassV4Mapped)
	}
}

// An omitted `cidr` means ANY destination — the transparent-interception value, which
// makes the generator emit exactly one clause fewer (§5.D / network.md D3.3).
func TestAnOmittedCidrDecodesToAnyDestination(t *testing.T) {
	w := networkWire{Egress: egressWire{
		Via: []viaWire{{Match: []tupleWire{{Port: 443, Proto: "tcp"}}, Endpoint: "127.0.0.9:15443"}},
	}}
	d, err := w.decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !d.Egress.Via[0].Match[0].AnyDest() {
		t.Error("an omitted cidr did not decode to any-destination")
	}
	// …and a present one does not.
	w2 := networkWire{Egress: egressWire{
		Allow: []tupleWire{{CIDR: "1.1.1.1/32", Port: 443, Proto: "tcp"}},
	}}
	d2, err := w2.decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d2.Egress.Allow[0].AnyDest() {
		t.Error("a present cidr decoded to any-destination")
	}
}

// §6.1: the three arrays ALWAYS appear, empty as `[]` — never omitted, never null.
// They are definite values in the record; the omit-when-unreadable rule governs facts
// that cannot be READ, of which a declaration has none.
func TestTheReadFaceAlwaysRendersTheThreeArrays(t *testing.T) {
	body, err := json.Marshal(encodeNetwork(network.Zero(), nil))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{`"allow":[]`, `"via":[]`, `"publish":[]`, `"default":"deny"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the read face is missing %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("the read face rendered a null — §6.1 requires `[]`, and two spellings of absence "+
			"is one more than needed:\n%s", got)
	}
}

// `host_endpoint` is OMITTED when the engine holds no placement — not "", not null.
//
// The endpoint is a placement product (§3.3), so a `paused` sandbox has none. A
// falsy-but-present field would invite reading it as "is this publish in effect",
// and that judgment is doubly wrong: the rule set is installed WHOLE and atomically,
// so there is no per-publish install state for the field to report.
func TestHostEndpointIsOmittedWithoutAPlacement(t *testing.T) {
	d := network.Zero()
	d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}

	noPlacement, err := json.Marshal(encodeNetwork(d, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(noPlacement), "host_endpoint") {
		t.Errorf("`host_endpoint` is present with no placement — it must be OMITTED, because a "+
			"falsy-but-present field invites reading it as `is this publish in effect`, which is not "+
			"a question this field can answer (§6.1):\n%s", noPlacement)
	}
	withPlacement, err := json.Marshal(encodeNetwork(d, []string{"127.0.0.1:21000"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withPlacement), `"host_endpoint":"127.0.0.1:21000"`) {
		t.Errorf("`host_endpoint` is missing with a live placement:\n%s", withPlacement)
	}
}

// The write face and the read face must not diverge on the same field: `POST
// …/publish` follows `GET …/network`'s omission rule exactly (§6.1 — a divergence
// there is a contract defect).
func TestThePublishResponseFollowsTheReadFacesOmissionRule(t *testing.T) {
	paused, err := json.Marshal(publishResp{GuestPort: 8080, Proto: "tcp", AppliesAt: "next-resume"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(paused), "host_endpoint") {
		t.Errorf("the publish response carries `host_endpoint` with no endpoint bound — the write "+
			"face and the read face diverging on one field is a contract defect (§6.1):\n%s", paused)
	}
	running, err := json.Marshal(publishResp{GuestPort: 8080, Proto: "tcp", HostEndpoint: "127.0.0.1:21000", AppliesAt: "now"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(running), `"host_endpoint":"127.0.0.1:21000"`) {
		t.Errorf("the publish response omitted a bound endpoint:\n%s", running)
	}
}

// UNKNOWN FIELDS ARE REFUSED, and this is the one that matters most in this file.
//
// With unknown keys ignored, a caller who typed `"netwrok"` gets a 201 and believes
// their declaration took effect, while the engine recorded the omitted-network
// normalization — a `deny` sandbox (§5.D). Both are LEGAL outcomes, which is exactly
// why the misspelling must be an error rather than silently one of them.
func TestStrictDecodeRefusesAMisspelledDeclarationKey(t *testing.T) {
	var req createReq
	err := strictDecode(strings.NewReader(`{"template_id":"bare","netwrok":{"egress":{"default":"allow"}}}`), &req)
	if err == nil {
		t.Fatal("a misspelled `netwrok` key was accepted — the caller would get a 201 and believe " +
			"their declaration took effect, while the engine recorded the omitted-network " +
			"normalization: a DENY sandbox (§5.D). Both outcomes are legal, so the typo has to be " +
			"an error")
	}
	if !strings.Contains(err.Error(), "netwrok") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
	// The correctly-spelled key is accepted, or the guard would be refusing everything.
	var ok createReq
	if err := strictDecode(strings.NewReader(`{"template_id":"bare","network":{"egress":{"default":"allow"}}}`), &ok); err != nil {
		t.Fatalf("the correctly-spelled key was refused: %v", err)
	}
	if ok.Network == nil || ok.Network.Egress.Default == nil || *ok.Network.Egress.Default != "allow" {
		t.Errorf("the declaration did not decode: %+v", ok.Network)
	}
	// An EMPTY body is still fine — every field of create is optional except the
	// template, and an empty body must not become "unknown field" noise.
	var empty createReq
	if err := strictDecode(strings.NewReader(``), &empty); err != nil {
		t.Errorf("an empty body was refused: %v", err)
	}
	// A misspelling inside the NESTED declaration is caught too, which is where a
	// typo is likeliest (the nesting is three deep).
	var nested createReq
	if err := strictDecode(strings.NewReader(`{"network":{"egress":{"defualt":"allow"}}}`), &nested); err == nil {
		t.Error("a misspelling INSIDE the declaration was accepted — `egress.defualt` would leave " +
			"`egress.default` absent, which is §6.2's first 400 class, but the caller would be told " +
			"nothing about the typo")
	}
}

// `network` omitted from create is LEGAL and normalizes to an explicit
// `{deny, [], [], []}`; `network` GIVEN but missing `egress.default` is §6.2's first
// 400 class. BOTH DIRECTIONS, because they share one code path and whichever is
// implemented first eats the other.
func TestOmittedNetworkIsLegalWhileAMissingDefaultIs400(t *testing.T) {
	// The omission: no `network` object at all → the create body decodes with a nil
	// declaration, which the mediator normalizes.
	var omitted createReq
	if err := strictDecode(strings.NewReader(`{"template_id":"bare"}`), &omitted); err != nil {
		t.Fatalf("a create with no `network` was refused at decode: %v", err)
	}
	if omitted.Network != nil {
		t.Error("an omitted `network` did not decode to nil — the pointer is what keeps `omitted` " +
			"distinguishable from `given but incomplete`, and collapsing them makes one of the two " +
			"cases unreachable")
	}

	// Given but incomplete: decodes fine, and Validate refuses it as class 1.
	var given createReq
	if err := strictDecode(strings.NewReader(`{"template_id":"bare","network":{"egress":{"allow":[]}}}`), &given); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if given.Network == nil {
		t.Fatal("a given `network` decoded to nil")
	}
	decl, err := given.Network.decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	verr := network.Validate(decl, true, func(netip.Addr) (bool, error) { return false, nil })
	if class, _ := network.ClassOf(verr); class != network.ClassDefaultMissing {
		t.Errorf("a given-but-incomplete declaration produced class %q, want %q", class, network.ClassDefaultMissing)
	}
	// …and the omission, validated with given=false, is accepted.
	if err := network.Validate(network.Zero(), false, func(netip.Addr) (bool, error) { return false, nil }); err != nil {
		t.Errorf("the normalized omission was refused: %v", err)
	}
}

// An explicit `"default": ""` must be told it is invalid rather than told the field
// is missing: the pointer distinguishes them, and a caller who typed an empty value
// deserves the accurate message.
func TestAnExplicitEmptyDefaultIsDistinguishedFromAnAbsentOne(t *testing.T) {
	var explicit createReq
	if err := strictDecode(strings.NewReader(`{"network":{"egress":{"default":""}}}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Network.Egress.Default == nil {
		t.Error("an explicit empty `default` decoded as ABSENT — the two are different caller " +
			"mistakes and the pointer exists to tell them apart")
	}
	if got := *explicit.Network.Egress.Default; got != "" {
		t.Errorf("explicit default = %q, want the empty string", got)
	}
}

// Round-trip: what the caller writes must come back out of the read face in a form
// they can PATCH straight back (§6.1 makes the read-modify-write a contract
// requirement, so the read face's output has to be a legal write body).
func TestTheReadFaceOutputIsALegalWriteBody(t *testing.T) {
	const in = `{"egress":{"default":"deny","allow":[{"cidr":"10.0.0.0/8","port":53,"proto":"udp"}],` +
		`"via":[{"match":[{"port":443,"proto":"tcp"}],"endpoint":"127.0.0.9:15443"}]},` +
		`"ingress":{"publish":[{"guest_port":8080,"proto":"tcp"}]}}`
	var w networkWire
	if err := json.NewDecoder(strings.NewReader(in)).Decode(&w); err != nil {
		t.Fatal(err)
	}
	decl, err := w.decode()
	if err != nil {
		t.Fatal(err)
	}
	// Encode through the read face, then decode THAT as a write body.
	out, err := json.Marshal(encodeNetwork(decl, nil))
	if err != nil {
		t.Fatal(err)
	}
	var back networkWire
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("the read face's own output is not a legal write body (%v) — §6.1 requires a "+
			"read-modify-write cycle, so the GET response has to be PATCH-able verbatim:\n%s", err, out)
	}
	// …and it means the same thing.
	again, err := back.decode()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Egress.Allow) != 1 || again.Egress.Allow[0].CIDR.String() != "10.0.0.0/8" ||
		again.Egress.Allow[0].Port != 53 || again.Egress.Allow[0].Proto != model.NetUDP {
		t.Errorf("the allow entry did not survive the round trip: %+v", again.Egress.Allow)
	}
	if len(again.Egress.Via) != 1 || again.Egress.Via[0].Endpoint != "127.0.0.9:15443" ||
		!again.Egress.Via[0].Match[0].AnyDest() {
		t.Errorf("the via entry did not survive the round trip: %+v", again.Egress.Via)
	}
	if len(again.Ingress.Publish) != 1 || again.Ingress.Publish[0].GuestPort != 8080 {
		t.Errorf("the publish entry did not survive the round trip: %+v", again.Ingress.Publish)
	}
	// The `state` member the read face adds is NOT a write field, so a caller who
	// PATCHes the GET response verbatim would be rejected by DisallowUnknownFields.
	// That is worth knowing about explicitly rather than discovering: the round-trip
	// above passes because `networkWire` has no `state`, and this asserts the read
	// response's EXTRA member is the only difference.
	var full networkReadResp
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"state"`) && full.State != "" {
		t.Error("unexpected non-empty state in a directly-encoded declaration")
	}
}
