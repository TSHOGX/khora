package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/network"
)

// This file carries §6.1's Network block: the `network?` create input, the whole-for-
// whole `PATCH` field, `GET …/network`, and `POST …/publish`.
//
// # Why the wire types are separate from the record types
//
// `cidr` is a STRING here while the record holds a parsed netip.Prefix, and that is
// not incidental duplication. netip.Prefix's own text unmarshaling ACCEPTS a
// v4-mapped literal (verified), which would route straight past §6.2's eighth 400
// class — whose entire point is to refuse those BEFORE any containment test, because
// `Contains` does not unmap and a family-branching check would judge
// `::ffff:169.254.169.254` to be v6, compare it only against v6 members, let it
// through, and then watch the packet die on the v4 floor as IPv4 (network.md D5 ⑲).
// Decoding untrusted input directly into the record type would make that class
// unenforceable by construction.

// networkWire is §5.D's four-field declaration as it appears on the wire (§6.1).
type networkWire struct {
	Egress  egressWire  `json:"egress"`
	Ingress ingressWire `json:"ingress"`

	// State is ACCEPTED AND IGNORED on the write path. It is not a declaration field;
	// it is the read-only member `GET …/network` carries (§6.1).
	//
	// It exists here because §6.1 makes the read-modify-write cycle a CONTRACT
	// REQUIREMENT rather than advice — a PATCH built from a stale read silently drops
	// a publish added in between, on a security-relevant field — and with unknown
	// fields refused, a caller who pastes the GET response back would be rejected for
	// echoing a field the engine itself emitted. Any friction on that path pushes the
	// caller toward the mistake §6.1 exists to prevent.
	//
	// THIS IS NOT A GENERAL EXEMPTION from DisallowUnknownFields: only a read-only
	// member the read face itself produced belongs here. A misspelled `netwrok` is
	// still a 400, which is the property that matters (§5.D / M8 plan item 3b).
	//
	// SETTLED in §6.1 (`GET …/network`): the read face's read-only members — `state`
	// here and `host_endpoint` on publishWire — are accepted and ignored on the write
	// path, which is what makes the GET response body a legal PATCH request body. The
	// rule is "every read-only member the read face emits", so a new one obliges a
	// matching ignored member here.
	State string `json:"state,omitempty"`
}

type egressWire struct {
	// Default is REQUIRED whenever a `network` object is given — §6.2's first 400
	// class. A pointer so "absent" is distinguishable from "empty string": both are
	// refused, but only the absent case is the class-1 message, and a caller who typed
	// `"default": ""` deserves to be told that rather than told the field is missing.
	Default *string     `json:"default"`
	Allow   []tupleWire `json:"allow"`
	Via     []viaWire   `json:"via"`
}

type ingressWire struct {
	Publish []publishWire `json:"publish"`
}

// tupleWire is `{cidr, port, proto}`. TWO of the three members are WILDCARDABLE BY
// OMISSION, and they say so the same way: `cidr` omitted means "any destination"
// (the transparent-interception value inside `via[].match[]`, which makes the
// generator emit exactly one clause fewer — §5.D / network.md D3.3), and `port`
// omitted means "any port" (§5.D). `proto` is the one member with no wildcard, on
// mechanism grounds: port matching depends on protocol matching in the kernel, so
// `{cidr, port}` cannot be rendered at all (D3.1).
type tupleWire struct {
	// Omitted rather than rendered as "" when the tuple pins no destination — the
	// read face's output has to be a legal write body (§6.1's read-modify-write), and
	// `"cidr":""` round-trips only because decode() treats it as "any". Emitting the
	// omission directly says what it means.
	CIDR string `json:"cidr,omitempty"`
	// Port carries `omitempty` for the same reason `cidr` above does, and the reason is
	// NOT tidiness — it is §5.D's own criterion for requiring `egress.default`: if
	// "unspecified" is read as some default value, the record's empty value can no
	// longer be told from the caller having chosen it. Without `omitempty` the read face
	// emits `"port": 0` for a wildcard, which reads as a port number that happens to be
	// zero; a caller cannot tell that response from a record built before the field
	// existed, and the wildcard survives only as a zero value's side effect. Omitting it
	// says what it means, and keeps the GET response a legal PATCH body (§6.1).
	//
	// A VALUE TYPE, not a pointer, and deliberately: `egress.default` is a pointer
	// because absent and `""` are BOTH REFUSED THERE with different messages, so the
	// two must stay distinguishable. Here they are both LEGAL and mean the same thing —
	// `"port": 0` and an omitted `port` decode to ONE record — so there is nothing to
	// distinguish and a pointer would only invite a distinction the contract does not
	// make. Two spellings collapsing onto one record is exactly how `cidr` already
	// behaves (`"cidr": ""` decodes to any-destination), and it is the safe direction of
	// §6.2's v4-mapped objection: what that class refuses is two spellings that
	// SURVIVE INTO the record and then compare unequal on read-modify-write. These do
	// not survive — the read face renders the wildcard exactly one way, as the omission.
	Port  uint16 `json:"port,omitempty"`
	Proto string `json:"proto"`
}

type viaWire struct {
	Match    []tupleWire `json:"match"`
	Endpoint string      `json:"endpoint"`
}

type publishWire struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`

	// HostEndpoint is ACCEPTED AND IGNORED on the write path, for exactly the reason
	// `networkWire.State` is (see there — same contract requirement, same non-exemption
	// from DisallowUnknownFields). It is the SECOND read-only member the read face
	// produces, and the scope of that decision is "every read-only member the read face
	// emits", not `state` alone: `DisallowUnknownFields` applies RECURSIVELY, so a
	// `running` sandbox carrying one publish would have its GET response refused with
	// `json: unknown field "host_endpoint"` — the read-modify-write cycle §6.1 makes a
	// CONTRACT REQUIREMENT, broken one level down from where `state` lives.
	//
	// ⚠ COUNTERINTUITIVE, and it is why no test caught this: the friction appears only
	// on the WRONG operation order. §5.D's safe sequence is `pause → GET → PATCH →
	// resume`, and on that path the GET happens after the pause, when the engine holds
	// no placement and therefore emits no `host_endpoint` at all — so the write-back
	// works. Only `GET` BEFORE `pause` — the order §5.D names as dangerous, because it
	// pulls the whole pause window into the lost-update window — hits the 400. So the
	// gap ACCIDENTALLY acted as a guardrail against the unsafe order. It was never
	// designed as one and must not be relied on as one: the guardrail is unprincipled
	// (a `paused` sandbox with a stale read is equally lost-update-prone and sails
	// through), it reports a JSON schema error for a concurrency mistake, and §6.1 owns
	// that ordering requirement in prose where a caller can read it.
	HostEndpoint string `json:"host_endpoint,omitempty"`
}

// decode turns the wire form into the record form, applying the two 400 classes that
// are decidable at PARSE time (class 2: a name where a literal belongs; class 8: a
// v4-mapped/v4-compatible spelling). The remaining six are Validate's.
func (w networkWire) decode() (model.NetworkDecl, error) {
	var d model.NetworkDecl
	if w.Egress.Default != nil {
		d.Egress.Default = model.NetEgressDefault(*w.Egress.Default)
	}
	d.Egress.Allow = make([]model.NetTuple, 0, len(w.Egress.Allow))
	for _, t := range w.Egress.Allow {
		got, err := t.decode()
		if err != nil {
			return d, err
		}
		d.Egress.Allow = append(d.Egress.Allow, got)
	}
	d.Egress.Via = make([]model.NetVia, 0, len(w.Egress.Via))
	for _, v := range w.Egress.Via {
		out := model.NetVia{Endpoint: v.Endpoint, Match: make([]model.NetTuple, 0, len(v.Match))}
		for _, t := range v.Match {
			got, err := t.decode()
			if err != nil {
				return d, err
			}
			out.Match = append(out.Match, got)
		}
		d.Egress.Via = append(d.Egress.Via, out)
	}
	d.Ingress.Publish = make([]model.NetPublish, 0, len(w.Ingress.Publish))
	for _, p := range w.Ingress.Publish {
		d.Ingress.Publish = append(d.Ingress.Publish,
			model.NetPublish{GuestPort: p.GuestPort, Proto: model.NetProto(p.Proto)})
	}
	return d, nil
}

func (t tupleWire) decode() (model.NetTuple, error) {
	// `port` needs no branch and that is the point of the shapes chosen above: an
	// omitted `port` and an explicit `"port": 0` both arrive here as 0 and both mean
	// ANY PORT (§5.D), so there is exactly one recorded value for the wildcard and
	// nothing for a later read-modify-write to disagree about. encodeTuple renders it
	// back as the omission, which is the single spelling the read face produces.
	out := model.NetTuple{Port: t.Port, Proto: model.NetProto(t.Proto)}
	if t.CIDR == "" {
		// Omitted = any destination. Legal, and only meaningful in `via[].match[]`.
		return out, nil
	}
	p, err := network.ParseCIDR(t.CIDR)
	if err != nil {
		return out, err
	}
	out.CIDR = p
	return out, nil
}

// encodeNetwork renders a declaration for §6.1's read face.
//
// THE THREE ARRAYS ALWAYS APPEAR, empty as `[]` — never omitted, never null (§6.1).
// They are DEFINITE values in the record; the omit-when-unreadable rule
// (`death_cause`, `used_byte`) governs facts that cannot be READ, and a declaration
// has none of those. Two spellings of absence would be one more than needed.
func encodeNetwork(d model.NetworkDecl, endpoints []string) networkReadResp {
	d = d.Normalize()
	out := networkReadResp{}
	out.Egress.Default = string(d.Egress.Default)
	out.Egress.Allow = make([]tupleWire, 0, len(d.Egress.Allow))
	for _, t := range d.Egress.Allow {
		out.Egress.Allow = append(out.Egress.Allow, encodeTuple(t))
	}
	out.Egress.Via = make([]viaReadWire, 0, len(d.Egress.Via))
	for _, v := range d.Egress.Via {
		item := viaReadWire{Endpoint: v.Endpoint, Match: make([]tupleWire, 0, len(v.Match))}
		for _, t := range v.Match {
			item.Match = append(item.Match, encodeTuple(t))
		}
		out.Egress.Via = append(out.Egress.Via, item)
	}
	out.Ingress.Publish = make([]publishReadWire, 0, len(d.Ingress.Publish))
	for i, p := range d.Ingress.Publish {
		item := publishReadWire{GuestPort: p.GuestPort, Proto: string(p.Proto)}
		// `host_endpoint` is OMITTED when the engine holds no live placement — not the
		// empty string, not null (§6.1). The endpoint is a placement product (§3.3), so
		// a `paused` sandbox has none to report, and a falsy-but-present field would
		// invite reading it as "is this publish in effect" — a judgment that is doubly
		// wrong, because the rule set is installed WHOLE and atomically, so there is no
		// per-publish install state for it to mean.
		if i < len(endpoints) && endpoints[i] != "" {
			item.HostEndpoint = endpoints[i]
		}
		out.Ingress.Publish = append(out.Ingress.Publish, item)
	}
	return out
}

// encodeTuple renders one tuple for the read face. BOTH wildcards come out as
// OMISSIONS — `cidr` because the field is left empty, `port` because 0 plus
// `omitempty` drops it — so the wildcard has exactly one spelling on the way out and
// the response is a legal write body (§6.1's read-modify-write). See tupleWire.
func encodeTuple(t model.NetTuple) tupleWire {
	w := tupleWire{Port: t.Port, Proto: string(t.Proto)}
	if t.CIDR.IsValid() {
		w.CIDR = t.CIDR.String()
	}
	return w
}

// networkReadResp is `GET /v1/sandboxes/{id}/network` (§6.1).
type networkReadResp struct {
	// State is a MEMBER of this response, saving the caller a second round trip to
	// interpret what they just read (the same N+1 argument that puts `death_cause` on
	// list). It is meaningful here because of a mechanical biconditional §6.1 states:
	// `running` ⟺ the rules installed in the kernel ARE this declaration; not
	// `running` ⟹ nothing is installed.
	//
	// That holds because neither write point can make a `running` sandbox's record
	// diverge from its live rule set: editing the three egress fields on `running` is a
	// 409, and publish makes "record" and "rules" one un-halvable write. So there is no
	// persistable difference between "declared" and "in effect", and this response
	// needs exactly one bit to expose it.
	//
	// Hence NO `in_effect` / `pending` / `applies_at` here: `in_effect` would be a
	// second spelling of this field AND an overreach (the engine can honestly say
	// "I hold a placement", not "the kernel has these rules right now" — a `running`
	// record whose unit died but has not been demoted yet is the counter-example);
	// `pending` would need a second declaration in the record, which §6.2 forbids;
	// `applies_at` is a WRITE response's vocabulary.
	State   string          `json:"state"`
	Egress  egressReadWire  `json:"egress"`
	Ingress ingressReadWire `json:"ingress"`
}

type egressReadWire struct {
	Default string        `json:"default"`
	Allow   []tupleWire   `json:"allow"`
	Via     []viaReadWire `json:"via"`
}

type viaReadWire struct {
	Match    []tupleWire `json:"match"`
	Endpoint string      `json:"endpoint"`
}

type ingressReadWire struct {
	Publish []publishReadWire `json:"publish"`
}

type publishReadWire struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint is omitted when no placement holds one — see encodeNetwork.
	HostEndpoint string `json:"host_endpoint,omitempty"`
}

// handleNetworkGet serves `GET /v1/sandboxes/{id}/network` (§6.1).
func (e *Engine) handleNetworkGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	decl, state, endpoints, err := e.Mediator.Network(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	resp := encodeNetwork(decl, endpoints)
	resp.State = string(state)
	writeJSON(w, http.StatusOK, resp)
}

// publishReq / publishResp are `POST /v1/sandboxes/{id}/publish` (§6.1).
type publishReq struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint is ACCEPTED AND IGNORED, exactly as `state` is on networkWire and
	// for the same reason: §6.1's rule is that EVERY read-only member the read face
	// emits is taken and ignored on the write path, so that a caller can round-trip
	// what they read. `GET …/network` emits this key on a `running` sandbox's publish
	// items, so without it here, re-posting one item back is a 400 on a field the
	// engine itself produced — the friction §6.1 spends a paragraph refusing to
	// impose, one endpoint over.
	//
	// It is not a general exemption from DisallowUnknownFields: a misspelled
	// `host_endpiont` is still a 400. The engine never reads this value — the endpoint
	// is a placement product (§3.3), so a caller cannot choose it.
	HostEndpoint string `json:"host_endpoint"`
}

type publishResp struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint follows `GET …/network`'s omission rule exactly — the write face
	// and the read face diverging on the same field would be a contract defect (§6.1).
	HostEndpoint string `json:"host_endpoint,omitempty"`
	// AppliesAt is "now" on a `running` sandbox (the rules went on during this
	// request) and "next-resume" on a `paused` one (recorded; the rules appear at the
	// next placement). Both are 200 — the two states differ only in the effect point
	// (§6.1).
	AppliesAt string `json:"applies_at"`
}

// handlePublish serves `POST /v1/sandboxes/{id}/publish`.
//
// `proto` accepts udp as well as tcp, and that is a deliberate asymmetry with
// `via[]`: UDP is excluded there because a redirected datagram's ORIGINAL
// destination cannot be recovered, and a DNAT to a FIXED guest port needs no such
// recovery — so the objection does not reach inbound at all (§6.2 / network.md D3.2).
func (e *Engine) handlePublish(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	var req publishReq
	if err := strictDecode(r.Body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	item := model.NetPublish{GuestPort: req.GuestPort, Proto: model.NetProto(req.Proto)}
	got, endpoint, now, err := e.Mediator.AddPublish(r.Context(), id, item)
	if err != nil {
		statusFor(w, err)
		return
	}
	applies := "next-resume"
	if now {
		applies = "now"
	}
	writeJSON(w, http.StatusOK, publishResp{
		GuestPort:    got.GuestPort,
		Proto:        string(got.Proto),
		HostEndpoint: endpoint,
		AppliesAt:    applies,
	})
}

// strictDecode decodes a request body with UNKNOWN FIELDS REFUSED.
//
// This is required on every write point that carries a declaration, and the reason
// is a silent failure rather than tidiness: with unknown keys ignored, a caller who
// typed `"netwrok"` gets a 201 and believes their declaration took effect, while the
// engine recorded the OMITTED-network normalization — a `deny` sandbox (§5.D). The
// two cases are indistinguishable to the caller precisely because both are legal
// outcomes, which is why the misspelling has to be an error rather than a default.
//
// It also makes the create path's `network?` a real field rather than one more
// ignored key: before this, the key was dropped by the decoder with no signal at all.
func strictDecode(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body: %w", err)
	}
	return nil
}

// networkStatus maps §6.2's declaration refusals to 400 — ONE arm for all ten
// refusals, because every remedy is "fix the declaration" (or, for the
// deployment-natured one, "start the engine with a second-hop uid"). The CLASS stays
// recoverable through
// network.ClassOf for tests and logs; what §6.2 requires to stay distinguishable is
// what the caller's REMEDY depends on, and here it does not vary by class.
func networkStatus(err error) (int, bool) {
	if lifecycle.IsDeclarationInvalid(err) {
		return http.StatusBadRequest, true
	}
	// The PLACEMENT-side twin, and it is NOT a 400: a resume or a publish carries no
	// declaration, so 400 would send the caller to edit something they did not send.
	// A recorded `via[]` meets a deployment that supplies no second-hop uid — the
	// record and the running deployment conflict, and BOTH documented remedies change
	// state elsewhere (restart the engine with the uid, or PATCH the `via[]` away).
	//
	// Without this arm it fell through to 500, which tells the caller "engine fault,
	// retry" when nothing failed and no retry can help. The asymmetry made it
	// unmistakable: the SAME operator condition answered 400 on create (where
	// ValidateDeployment sees the declaration) and 500 on resume.
	if errors.Is(err, network.ErrViaUnenforceableAtPlacement) {
		return http.StatusConflict, true
	}
	return 0, false
}
