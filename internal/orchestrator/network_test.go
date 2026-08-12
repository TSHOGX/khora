package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/network"
)

// The assembly REFUSES a second-hop uid equal to its own euid, and it refuses BEFORE
// touching anything — no provider constructed, no state directory materialized, no
// table installed.
//
// Both halves are the assertion. `network.ValidateSecondHopUID` owns the mechanism
// (see its test); this owns the POSITION, which is the part a flag-level check in
// cmd/orchestrator would not deliver: the substrate harness builds an Engine directly,
// so a check that lived in main.go would be bypassed by every test and by any embedder.
// And placing it after the providers are up would mean discovering the problem from an
// engine that has already installed the table and begun dialing through it.
//
// Root-free precisely because the check runs first: rootfs/runtime/btrfs are never
// reached, so this test does not need the substrate the rest of the assembly does.
func TestTheAssemblyRefusesItsOwnUIDAsTheSecondHopUID(t *testing.T) {
	base := t.TempDir()
	euid := os.Geteuid()
	_, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
		SecondHopUID: &euid,
	})
	if err == nil {
		t.Fatal("the assembly accepted its own euid as the second-hop uid — that installs an " +
			"engine-level output drop covering the engine's OWN dials to guest addresses, breaking " +
			"transport for every sandbox at once and intermittently (network.ValidateSecondHopUID)")
	}
	if !errors.Is(err, network.ErrSecondHopUIDIsTheEngineItself) {
		t.Errorf("New failed for some other reason than the self-uid refusal: %v", err)
	}
	// The position: nothing was materialized. If the check ever drifts below the
	// provider construction, this is what fires.
	if _, statErr := os.Stat(filepath.Join(base, "state")); statErr == nil {
		t.Error("the refusal happened AFTER the providers materialized state — it must be the last " +
			"point at which nothing exists yet, because an engine that has already installed the " +
			"second-hop table is past the point where learning this helps")
	}

	// THE CONTROL ARM, asserting success rather than the absence of one error. The same
	// config with a uid the engine does not run as assembles cleanly (measured: err nil,
	// state dir materialized — the assembly reaches no substrate, which is why this whole
	// test is root-free).
	//
	// `err == nil` and not `!errors.Is(err, ErrSecondHopUIDIsTheEngineItself)`: the weaker
	// form is satisfied by New failing for ANY unrelated reason, so it would keep passing
	// if the config never reached the uid check at all — and then it would not be a control
	// on anything. What it has to establish is that the refusal above is specific to the
	// self-uid rather than to the presence of a second-hop uid.
	other := euid + 4711
	eng, cerr := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
		SecondHopUID: &other,
	})
	if cerr != nil {
		t.Fatalf("a uid the engine does not run as was refused: %v — the check must reject the ENGINE'S "+
			"OWN uid, not the feature", cerr)
	}
	if eng == nil {
		t.Fatal("New returned no engine and no error")
	}
	if _, statErr := os.Stat(filepath.Join(base, "state")); statErr != nil {
		t.Errorf("the control assembly did not materialize its state dir (%v) — then it never reached "+
			"the uid check, and it is not a control on the refusal above", statErr)
	}
}

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
	body, err := json.Marshal(encodeNetwork(network.Zero(), nil, nil, network.CounterAttribution{}))
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

	noPlacement, err := json.Marshal(encodeNetwork(d, nil, nil, network.CounterAttribution{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(noPlacement), "host_endpoint") {
		t.Errorf("`host_endpoint` is present with no placement — it must be OMITTED, because a "+
			"falsy-but-present field invites reading it as `is this publish in effect`, which is not "+
			"a question this field can answer (§6.1):\n%s", noPlacement)
	}
	withPlacement, err := json.Marshal(encodeNetwork(d, []string{"127.0.0.1:21000"}, nil, network.CounterAttribution{}))
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

// §5.D's PORT WILDCARD on the wire: a declaration carrying a wildcard tuple must come
// back out of the read face as a legal write body that still MEANS the same thing.
//
// This is the property `cidr` already had and `port` did not. With `port` rendered as
// `"port": 0`, the wildcard survived only as a zero value's side effect — the read face
// emitted a port number that happens to be zero, which is §5.D's own objection to a
// defaultable `egress.default`: the record's empty value stops being distinguishable
// from the caller having chosen it. So the assertion is not "it round-trips" but "the
// wildcard is SPELLED as the omission, on both faces".
func TestThePortWildcardIsSpelledAsAnOmissionOnBothFaces(t *testing.T) {
	// A wildcard tuple in each of the two egress sites, written the way a caller writes
	// it: the field simply absent.
	const in = `{"egress":{"default":"deny","allow":[{"cidr":"1.1.1.1/32","proto":"tcp"}],` +
		`"via":[{"match":[{"proto":"tcp"}],"endpoint":"10.0.0.5:15443"}]},"ingress":{"publish":[]}}`
	var w networkWire
	dec := json.NewDecoder(strings.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("a tuple with no `port` was refused by the decoder: %v — omitting it is legal "+
			"and means any port (§5.D)", err)
	}
	decl, err := w.decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decl.Egress.Allow[0].AnyPort() || !decl.Egress.Via[0].Match[0].AnyPort() {
		t.Fatalf("an omitted `port` did not decode to the any-port wildcard: %+v", decl)
	}

	out, err := json.Marshal(encodeNetwork(decl, nil, nil, network.CounterAttribution{}))
	if err != nil {
		t.Fatal(err)
	}
	// THE READ FACE MUST NOT EMIT `"port": 0`. That spelling reads as a port number, and
	// a caller cannot tell it from a record built before the field existed.
	if strings.Contains(string(out), `"port":0`) {
		t.Errorf("the read face rendered the wildcard as `\"port\":0` — it must render it as the "+
			"OMISSION, or the recorded wildcard is indistinguishable from a caller who chose port "+
			"zero (§5.D's criterion for requiring `egress.default`):\n%s", out)
	}
	if strings.Contains(string(out), `"port"`) {
		t.Errorf("the read face emitted a `port` member for a wildcard tuple:\n%s", out)
	}

	// …and the output is a legal write body that still means any-port.
	var back networkWire
	rt := json.NewDecoder(bytes.NewReader(out))
	rt.DisallowUnknownFields()
	if err := rt.Decode(&back); err != nil {
		t.Fatalf("the read face's own output is not a legal write body (%v):\n%s", err, out)
	}
	again, err := back.decode()
	if err != nil {
		t.Fatal(err)
	}
	if !again.Egress.Allow[0].AnyPort() || !again.Egress.Via[0].Match[0].AnyPort() {
		t.Errorf("the wildcard did not survive the round trip: %+v", again)
	}

	// THE OTHER DIRECTION, so the omission is not simply "port is never emitted": a tuple
	// that NAMED a port must still carry it out, or every declaration would silently widen
	// to all ports on the first read-modify-write.
	pinned, err := networkWire{Egress: egressWire{
		Allow: []tupleWire{{CIDR: "1.1.1.1/32", Port: 443, Proto: "tcp"}},
	}}.decode()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(encodeNetwork(pinned, nil, nil, network.CounterAttribution{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"port":443`) {
		t.Errorf("a tuple that named a port lost it on the read face — every read-modify-write "+
			"would then widen the declaration to ALL ports:\n%s", body)
	}
}

// An EXPLICIT `"port": 0` and an omitted `port` must record the SAME value, and the read
// face must render that value exactly one way.
//
// Two spellings of one meaning is only a defect when both SURVIVE INTO the record and
// then compare unequal on a later read-modify-write — that is what §6.2's v4-mapped class
// refuses. Here they collapse at the decoder, which is how `cidr` already behaves
// (`"cidr": ""` decodes to any-destination). This asserts the collapse rather than
// leaving it to be re-derived, because the alternative — admitting both and recording
// them differently — is the failure that class exists to name.
func TestAnExplicitZeroPortRecordsAsTheWildcardItself(t *testing.T) {
	decodeOne := func(body string) model.NetTuple {
		t.Helper()
		var w networkWire
		d := json.NewDecoder(strings.NewReader(body))
		d.DisallowUnknownFields()
		if err := d.Decode(&w); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		got, err := w.decode()
		if err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return got.Egress.Allow[0]
	}
	omitted := decodeOne(`{"egress":{"default":"deny","allow":[{"cidr":"1.1.1.1/32","proto":"tcp"}]}}`)
	explicit := decodeOne(`{"egress":{"default":"deny","allow":[{"cidr":"1.1.1.1/32","port":0,"proto":"tcp"}]}}`)
	if omitted != explicit {
		t.Errorf("`\"port\": 0` and an omitted `port` recorded DIFFERENT values (%+v vs %+v) — "+
			"two spellings that survive into the record compare unequal on every subsequent "+
			"read-modify-write, which is §6.2's v4-mapped objection", omitted, explicit)
	}
	if !explicit.AnyPort() {
		t.Error("an explicit `\"port\": 0` did not record as the any-port wildcard — the decided " +
			"semantic is that it is LEGAL and means any port, not a 400")
	}
	// One emitted spelling for the one recorded value.
	out, err := json.Marshal(encodeTuple(explicit))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"port"`) {
		t.Errorf("the read face emitted a `port` member for the wildcard: %s", out)
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
	out, err := json.Marshal(encodeNetwork(decl, nil, nil, network.CounterAttribution{}))
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
	// `encodeNetwork` alone does not set `state` — the handler does, which is why the
	// body above round-trips without exercising the read-only members at all. That is
	// precisely the hole TestAFullReadResponseIsALegalWriteBody closes.
	var full networkReadResp
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"state"`) && full.State != "" {
		t.Error("unexpected non-empty state in a directly-encoded declaration")
	}
}

// A FULL read response — one carrying BOTH read-only members the read face can emit,
// `state` and a per-publish `host_endpoint` — must be a legal write body verbatim.
//
// This is the assertion the test above cannot make. It builds its body from
// `encodeNetwork` alone, which sets neither read-only member (`state` is the handler's,
// `host_endpoint` needs a live placement), so it round-trips while leaving §6.1's
// contract requirement untested at both points where the response actually differs
// from a declaration.
//
// The gap it hid was real: `DisallowUnknownFields` applies RECURSIVELY, so a `running`
// sandbox with one publish produced a GET response that PATCH refused with
// `json: unknown field "host_endpoint"` — one level down from where `state` lives. It
// survived because §5.D's SAFE sequence (`pause → GET → PATCH → resume`) reads after
// the pause, when there is no placement and hence no endpoint to emit; only the order
// §5.D names as dangerous hit the 400.
func TestAFullReadResponseIsALegalWriteBody(t *testing.T) {
	decl := model.NetworkDecl{}
	decl.Egress.Default = model.NetDeny
	decl.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}

	// An `allow[]` entry too, so the body carries a per-entry `hit_packet` alongside the
	// aggregate — both are read-only members and both must round-trip.
	decl.Egress.Allow = []model.NetTuple{{CIDR: netip.MustParsePrefix("1.1.1.1/32"), Port: 443, Proto: model.NetTCP}}

	// Counters as a live placement would report them. THIS IS THE HALF THAT ACTUALLY BIT:
	// when the counter keys first landed, `default_hit_packet` was missing from the write
	// face and this round trip failed on a field the engine itself emits. Passing nil
	// hits here would make the assertion below vacuous for those keys.
	spec, err := network.Generate(decl.Normalize())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	hits := map[string]uint64{}
	for _, c := range spec.Rules.Counters {
		hits[c] = 7
	}

	// A live placement: encodeNetwork renders `host_endpoint` for publish[0], and the
	// handler stamps `state`. Together this is byte-for-byte what a caller GETs from a
	// `running` sandbox that has published a port.
	resp := encodeNetwork(decl, []string{"10.66.0.1:33445"}, hits, spec.Counters)
	resp.State = string(model.Running)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state"`, `"host_endpoint"`, `"hit_packet"`, `"default_hit_packet"`, `"closed_hit_packet"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("this test is only meaningful if the body carries %s — it does not, so the "+
				"read face changed and the assertion below is vacuous:\n%s", want, out)
		}
	}

	var back networkWire
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("a full read response is not a legal write body (%v) — §6.1 makes the "+
			"read-modify-write a CONTRACT REQUIREMENT, so every read-only member the read face "+
			"emits must be accepted and ignored on the write path, `host_endpoint` included:\n%s",
			err, out)
	}

	// Accepted AND IGNORED: the ignored members must not reach the record. An endpoint
	// that survived decode would be a placement fact written back into a declaration.
	again, err := back.decode()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Ingress.Publish) != 1 || again.Ingress.Publish[0].GuestPort != 8080 ||
		again.Ingress.Publish[0].Proto != model.NetTCP {
		t.Errorf("the publish entry did not survive the round trip: %+v", again.Ingress.Publish)
	}
	if again.Ingress.Publish[0] != (model.NetPublish{GuestPort: 8080, Proto: model.NetTCP}) {
		t.Errorf("a read-only member leaked into the record through decode: %+v",
			again.Ingress.Publish[0])
	}

	// The non-exemption, at the SAME nesting depth as the new ignored member: a
	// misspelling inside a publish item is still refused. Ignoring `host_endpoint` must
	// not have been implemented as "publish items tolerate unknown keys".
	bad := `{"egress":{"default":"deny","allow":[],"via":[]},` +
		`"ingress":{"publish":[{"guest_port":8080,"proto":"tcp","host_endpiont":"x"}]}}`
	strict := json.NewDecoder(strings.NewReader(bad))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&networkWire{}); err == nil {
		t.Error("a misspelled key inside a publish item was accepted — accepting the read face's " +
			"own read-only members is not a general exemption from DisallowUnknownFields (§6.1)")
	}
}

// …and the SAME rule at the publish endpoint, which is the other write face for this
// field and was the gap: §6.1's rule is "every read-only member the read face emits is
// accepted and ignored", not "every one on the PATCH path".
//
// `GET …/network` emits `host_endpoint` on a running sandbox's publish items, so a
// caller re-posting one item they just read back hit a 400 on a key the engine itself
// produced — precisely the friction §6.1 argues must not sit on the read-modify-write
// path, one endpoint over from where it was fixed.
func TestAReadBackPublishItemIsALegalPublishBody(t *testing.T) {
	// Exactly what the read face emits for one publish item on a live placement.
	resp := encodeNetwork(func() model.NetworkDecl {
		d := model.NetworkDecl{}
		d.Egress.Default = model.NetDeny
		d.Ingress.Publish = []model.NetPublish{{GuestPort: 8080, Proto: model.NetTCP}}
		return d
	}(), []string{"10.66.0.1:33445"}, nil, network.CounterAttribution{})
	item, err := json.Marshal(resp.Ingress.Publish[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(item), `"host_endpoint"`) {
		t.Fatalf("this test is only meaningful if the read face emits host_endpoint — it does "+
			"not, so the assertion below is vacuous: %s", item)
	}

	var req publishReq
	if err := strictDecode(bytes.NewReader(item), &req); err != nil {
		t.Fatalf("a read-back publish item is not a legal publish body (%v) — §6.1 requires every "+
			"read-only member the read face emits to be accepted and ignored on the write path, and "+
			"this endpoint is a write path for the same field: %s", err, item)
	}
	if req.GuestPort != 8080 || req.Proto != string(model.NetTCP) {
		t.Errorf("the item did not survive the round trip: %+v", req)
	}

	// The non-exemption, at this endpoint too.
	if err := strictDecode(strings.NewReader(
		`{"guest_port":8080,"proto":"tcp","host_endpiont":"x"}`), &publishReq{}); err == nil {
		t.Error("a misspelled `host_endpiont` was accepted at the publish endpoint — accepting the " +
			"read face's own read-only member is not a general exemption (§6.1)")
	}
}

// TestHitPacketFollowsTheOmissionRule pins the three cases §6.1 distinguishes, and the
// distinction is not stylistic: §2.4's acceptance discipline rests on a counter reading 0
// meaning "installed, matched nothing". If "not measured" also serialized as 0, the one
// signal §5.D has for "is this rule live" would be destroyed at the read surface.
func TestHitPacketFollowsTheOmissionRule(t *testing.T) {
	decl := model.NetworkDecl{}
	decl.Egress.Default = model.NetDeny
	decl.Egress.Allow = []model.NetTuple{{CIDR: netip.MustParsePrefix("1.1.1.1/32"), Port: 443, Proto: model.NetTCP}}
	spec, err := network.Generate(decl.Normalize())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	t.Run("no placement omits every counter key", func(t *testing.T) {
		// nil hits is what the mediator returns with no live placement — and pause
		// DESTROYS the rule-set table rather than zeroing it, so this is the truth rather
		// than a convention papering over a zero.
		out, _ := json.Marshal(encodeNetwork(decl, nil, nil, spec.Counters))
		if strings.Contains(string(out), "hit_packet") {
			t.Errorf("a response with no placement carries a counter key: %s", out)
		}
	})

	t.Run("a real zero is PRESENT", func(t *testing.T) {
		hits := map[string]uint64{}
		for _, c := range spec.Rules.Counters {
			hits[c] = 0
		}
		out, _ := json.Marshal(encodeNetwork(decl, nil, hits, spec.Counters))
		if !strings.Contains(string(out), `"hit_packet":0`) {
			t.Errorf("a counter READ AS 0 is not reported — 'installed and matched nothing' is a different fact from 'not measured', and collapsing them destroys §2.4's only liveness signal: %s", out)
		}
		if !strings.Contains(string(out), `"default_hit_packet":0`) {
			t.Errorf("the default aggregate read as 0 is not reported: %s", out)
		}
	})

	t.Run("a counter absent from the table is omitted, not zeroed", func(t *testing.T) {
		// A non-empty map that simply lacks this entry's counters: the rule was never
		// rendered. Reporting 0 would claim it is installed and idle.
		out, _ := json.Marshal(encodeNetwork(decl, nil, map[string]uint64{"unrelated": 5}, spec.Counters))
		if strings.Contains(string(out), "hit_packet") {
			t.Errorf("a counter MISSING from the installed set was reported rather than omitted, which claims the rule is installed and idle when it was never rendered: %s", out)
		}
	})

	t.Run("an allow entry SUMS its rules", func(t *testing.T) {
		hits := map[string]uint64{}
		for _, n := range spec.Counters.Allow[0] {
			hits[n] = 3
		}
		out, _ := json.Marshal(encodeNetwork(decl, nil, hits, spec.Counters))
		want := fmt.Sprintf(`"hit_packet":%d`, 3*len(spec.Counters.Allow[0]))
		if !strings.Contains(string(out), want) {
			t.Errorf("allow[0] did not sum its %d counters (wanted %s) — the rules are mutually exclusive per packet, so summing double-counts nothing and reporting only one hook undercounts: %s", len(spec.Counters.Allow[0]), want, out)
		}
	})
}

// The ingress default-deny's counter answers the question the egress side answers per
// entry and the inbound side structurally cannot: was anything REFUSED on the way to this
// guest, as against nothing having arrived. Outside the engine those two are
// indistinguishable — there is no vantage point from which a dropped inbound packet is
// visible — so without this the whole inbound half of the diagnostic surface is dark.
func TestIngressClosedCounterIsReportedDirectionLevel(t *testing.T) {
	decl := model.NetworkDecl{}
	decl.Egress.Default = model.NetDeny
	spec, err := network.Generate(decl.Normalize())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The counter must be ATTRIBUTED even with nothing published: a sandbox that publishes
	// nothing is exactly where "closed and something tried" versus "closed and nothing
	// tried" carries the most information.
	if spec.Counters.IngressClosed == "" {
		t.Fatal("no ingress-closed counter was attributed for a declaration with no publish entries — that is the case where the refused/never-arrived distinction matters most")
	}
	if !slices.Contains(spec.Rules.Counters, spec.Counters.IngressClosed) {
		t.Fatalf("attributed ingress counter %q is not among the generated rule set's counters %v — the read surface would omit it forever",
			spec.Counters.IngressClosed, spec.Rules.Counters)
	}

	t.Run("a real zero is present", func(t *testing.T) {
		hits := map[string]uint64{spec.Counters.IngressClosed: 0}
		out, _ := json.Marshal(encodeNetwork(decl, nil, hits, spec.Counters))
		if !strings.Contains(string(out), `"closed_hit_packet":0`) {
			t.Errorf("an ingress counter read as 0 is not reported; 'installed and nothing refused' is a different fact from 'not measured': %s", out)
		}
	})

	t.Run("a nonzero value is reported", func(t *testing.T) {
		hits := map[string]uint64{spec.Counters.IngressClosed: 7}
		out, _ := json.Marshal(encodeNetwork(decl, nil, hits, spec.Counters))
		if !strings.Contains(string(out), `"closed_hit_packet":7`) {
			t.Errorf("ingress refusals were not reported: %s", out)
		}
	})

	t.Run("no placement omits it", func(t *testing.T) {
		// Pause DESTROYS the table rather than zeroing it, so absent is the truth here —
		// and absent must stay distinguishable from 0, or a paused sandbox reads as one
		// that refused nothing.
		out, _ := json.Marshal(encodeNetwork(decl, nil, nil, spec.Counters))
		if strings.Contains(string(out), "closed_hit_packet") {
			t.Errorf("a response with no placement carries the ingress counter key: %s", out)
		}
	})
}
