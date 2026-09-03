package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
)

// This file carries the control-plane API's Network block: the `network?` create input, the whole-for-
// whole `PATCH` field, `GET …/network`, and `POST …/publish`.
//
// # Why the wire types are separate from the record types
//
// `cidr` is a STRING here while the record holds a parsed netip.Prefix, because
// netip.Prefix's own text unmarshaling ACCEPTS a v4-mapped literal (verified) and would route
// straight past ClassV4Mapped. That class refuses those BEFORE any containment test:
// `Contains` does not unmap, so a family-branching check judges `::ffff:169.254.169.254` to be
// v6, compares it only against v6 members, lets it through, and the packet then dies on the v4
// floor as IPv4 (network substrate measurements). Decoding untrusted input straight into the record type would
// make the class unenforceable by construction.

// networkWire is the network declaration's four-field declaration as it appears on the wire (the control-plane API).
type networkWire struct {
	Egress  egressWire  `json:"egress"`
	Ingress ingressWire `json:"ingress"`

	// State is ACCEPTED AND IGNORED on the write path — THE reference case for every
	// other such field in this file. It is not a declaration field; it is the read-only
	// member `GET …/network` carries (the control-plane API).
	//
	// Accepted because the control-plane API makes the read-modify-write cycle a CONTRACT REQUIREMENT — a
	// PATCH built from a stale read silently drops a publish added in between, on a
	// security-relevant field — and with unknown fields refused, a caller pasting the GET
	// response back would be rejected for echoing a field the engine itself emitted.
	// `DisallowUnknownFields` applies RECURSIVELY, so the rule is "every read-only member
	// the read face emits" and a new one obliges a matching ignored member here.
	//
	// NOT A GENERAL EXEMPTION from DisallowUnknownFields: only a read-only member the read
	// face itself produced belongs here. A misspelled `netwrok` is still a 400, which is
	// the property that matters (the network declaration).
	State string `json:"state,omitempty"`
}

type egressWire struct {
	// Default is REQUIRED whenever a `network` object is given — the API error contract's first 400
	// class. A pointer so "absent" is distinguishable from "empty string": both are
	// refused, but only the absent case is the ClassDefaultMissing message, and a caller who typed
	// `"default": ""` deserves to be told that rather than told the field is missing.
	Default *string `json:"default"`
	// DefaultHitPacket is ACCEPTED AND IGNORED (see networkWire.State) — the read face
	// emits this key whenever counters are readable. Verified by
	// TestAFullReadResponseIsALegalWriteBody rather than by inspection: the counters were
	// caught breaking that round trip when they first landed.
	//
	// `hit_packet` inside allow[] / via[].match[] needs no equivalent here: tupleWire is
	// SHARED by both faces, so it is accepted automatically.
	DefaultHitPacket *uint64     `json:"default_hit_packet"`
	Allow            []tupleWire `json:"allow"`
	Via              []viaWire   `json:"via"`
}

type ingressWire struct {
	Publish []publishWire `json:"publish"`
	// ClosedHitPacket is ACCEPTED AND IGNORED on the write path (see networkWire.State).
	ClosedHitPacket *uint64 `json:"closed_hit_packet"`
}

// tupleWire is `{cidr, port, proto}`. TWO of the three members are WILDCARDABLE BY
// OMISSION, and they say so the same way: `cidr` omitted means "any destination"
// (the transparent-interception value inside `via[].match[]`, which makes the
// generator emit exactly one clause fewer — the network declaration / via intent forms), and `port`
// omitted means "any port" (the network declaration). `proto` is the one member with no wildcard, on
// mechanism grounds: port matching depends on protocol matching in the kernel, so
// `{cidr, port}` cannot be rendered at all (network enforcement).
type tupleWire struct {
	// Omitted rather than rendered as "" when the tuple pins no destination — the
	// read face's output has to be a legal write body (the control-plane API's read-modify-write), and
	// `"cidr":""` round-trips only because decode() treats it as "any". Emitting the
	// omission directly says what it means.
	CIDR string `json:"cidr,omitempty"`
	// Port carries `omitempty` for the reason `cidr` above does, sharpened by the network declaration's own
	// criterion for requiring `egress.default`: without it the read face emits
	// `"port": 0` for a wildcard, which reads as a port number that happens to be zero,
	// and the wildcard survives only as a zero value's side effect.
	//
	// A VALUE TYPE, not a pointer, unlike `egress.default` — there absent and `""` are
	// BOTH REFUSED with different messages, so they must stay distinguishable; here they
	// are both LEGAL and decode to ONE record. That collapse is the safe direction of
	// the API error contract's v4-mapped objection: what that class refuses is two spellings that SURVIVE
	// INTO the record and then compare unequal on read-modify-write. These do not survive
	// — the read face renders the wildcard exactly one way, as the omission.
	Port  uint16 `json:"port,omitempty"`
	Proto string `json:"proto"`
	// HitPacket is how many packets this ENTRY's rules have acted on since the current
	// rule set was installed (the control-plane API). Absent when the engine holds no live placement, when
	// the counters could not be read, or when this entry emitted no rule — pause DESTROYS
	// the table rather than zeroing it, so a `paused` sandbox genuinely has nothing to
	// report.
	//
	// A POINTER so a real 0 SURVIVES: "installed and matched nothing" is a different fact
	// from "not measured", and the provider security contract's acceptance discipline rests on telling them apart —
	// a bypassed rule's only symptom is a counter reading 0. With a value type plus
	// omitempty the two would serialize identically. THE reference case for the other
	// counters in this file.
	//
	// ACCEPTED AND IGNORED on the write path (see networkWire.State).
	HitPacket *uint64 `json:"hit_packet,omitempty"`
}

type viaWire struct {
	Match    []tupleWire `json:"match"`
	Endpoint string      `json:"endpoint"`
}

type publishWire struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`

	// HostEndpoint is ACCEPTED AND IGNORED on the write path (see networkWire.State): a
	// `running` sandbox carrying one publish would otherwise have its GET response refused
	// with `json: unknown field "host_endpoint"`, one level down from where `state` lives.
	//
	// ⚠ COUNTERINTUITIVE, and it is why no test caught this: the friction appears only
	// on the WRONG operation order. the network declaration's safe sequence is `pause → GET → PATCH →
	// resume`, and on that path the GET happens after the pause, when the engine holds
	// no placement and therefore emits no `host_endpoint` at all — so the write-back
	// works. Only `GET` BEFORE `pause` — the order the network declaration names as dangerous, because it
	// pulls the whole pause window into the lost-update window — hits the 400. So the
	// gap ACCIDENTALLY acted as a guardrail against the unsafe order. It was never
	// designed as one and must not be relied on as one: the guardrail is unprincipled
	// (a `paused` sandbox with a stale read is equally lost-update-prone and sails
	// through), it reports a JSON schema error for a concurrency mistake, and the control-plane API owns
	// that ordering requirement in prose where a caller can read it.
	HostEndpoint string `json:"host_endpoint,omitempty"`
}

// decode turns the wire form into the record form, applying the two 400 classes that
// are decidable at PARSE time (ClassCIDRNotLiteral: a name where a literal belongs; ClassV4Mapped: a
// v4-mapped/v4-compatible spelling). The remaining seven are Validate's.
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
	// `port` needs no branch, which is the point of the shapes chosen above: an omitted
	// `port` and an explicit `"port": 0` both arrive as 0 and both mean ANY PORT (the network declaration), so
	// there is one recorded value for the wildcard and nothing for a later
	// read-modify-write to disagree about.
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

// sumHits totals the named counters, returning nil when NONE of them is present — the
// the control-plane API omission rule, preserving the 0-vs-absent distinction tupleWire.HitPacket states.
// A zero-valued return for "not measured" would manufacture exactly the false zero the provider security contract's
// acceptance discipline refuses to treat as a pass.
func sumHits(hits map[string]uint64, names ...string) *uint64 {
	if hits == nil || len(names) == 0 {
		return nil
	}
	var total uint64
	var found bool
	for _, n := range names {
		if n == "" {
			continue
		}
		if v, ok := hits[n]; ok {
			total += v
			found = true
		}
	}
	if !found {
		return nil
	}
	return &total
}

// encodeNetwork renders a declaration for the control-plane API's read face.
//
// THE THREE ARRAYS ALWAYS APPEAR, empty as `[]` — never omitted, never null (the control-plane API). They
// are DEFINITE values in the record; the omit-when-unreadable rule (`death_cause`,
// `used_byte`) governs facts that cannot be READ, and a declaration has none of those.
func encodeNetwork(d model.NetworkDecl, endpoints []string, hits map[string]uint64, attr network.CounterAttribution) networkReadResp {
	d = d.Normalize()
	out := networkReadResp{}
	out.Egress.Default = string(d.Egress.Default)
	// The DECLARATION's own catch-all, and nothing else — see egressReadWire.DefaultHitPacket.
	out.Egress.DefaultHitPacket = sumHits(hits, attr.Default)
	out.Egress.Allow = make([]tupleWire, 0, len(d.Egress.Allow))
	for i, t := range d.Egress.Allow {
		tw := encodeTuple(t)
		// SUMMED over this entry's rules, which are mutually exclusive per packet: a
		// packet takes one hook, and within `input` an accept ends the chain. The
		// self-gateway accept MUST be in the sum — it is the only delivery path for a
		// declared host-local resolver, so omitting it makes that entry read 0 while
		// plainly working (network substrate measurements).
		if i < len(attr.Allow) {
			tw.HitPacket = sumHits(hits, attr.Allow[i]...)
		}
		out.Egress.Allow = append(out.Egress.Allow, tw)
	}
	out.Egress.Via = make([]viaReadWire, 0, len(d.Egress.Via))
	for i, v := range d.Egress.Via {
		item := viaReadWire{Endpoint: v.Endpoint, Match: make([]tupleWire, 0, len(v.Match))}
		for j, t := range v.Match {
			tw := encodeTuple(t)
			// ONE counter, the prerouting DNAT, because that rule IS the match. Adding the
			// companion accept would count one flow twice (they act on the same flow at
			// different hooks), and their divergence is an engine-defect signature rather
			// than a caller-actionable number (network enforcement).
			if i < len(attr.Via) && j < len(attr.Via[i]) {
				tw.HitPacket = sumHits(hits, attr.Via[i][j])
			}
			item.Match = append(item.Match, tw)
		}
		out.Egress.Via = append(out.Egress.Via, item)
	}
	// Emitted regardless of whether anything is published: a sandbox publishing nothing is
	// the case where "closed and nothing tried" vs "closed and something was refused"
	// matters most. See ingressReadWire.ClosedHitPacket.
	out.Ingress.ClosedHitPacket = sumHits(hits, attr.IngressClosed)
	out.Ingress.Publish = make([]publishReadWire, 0, len(d.Ingress.Publish))
	for i, p := range d.Ingress.Publish {
		item := publishReadWire{GuestPort: p.GuestPort, Proto: string(p.Proto)}
		// `host_endpoint` is OMITTED when the engine holds no live placement — not the empty
		// string, not null (the control-plane API). The endpoint is a placement product (the ID-reference model), so a `paused`
		// sandbox has none to report, and a falsy-but-present field would invite reading it
		// as "is this publish in effect" — which it cannot mean: the rule set is installed
		// WHOLE and atomically, so there is no per-publish install state.
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
// the response is a legal write body (the control-plane API's read-modify-write). See tupleWire.
func encodeTuple(t model.NetTuple) tupleWire {
	w := tupleWire{Port: t.Port, Proto: string(t.Proto)}
	if t.CIDR.IsValid() {
		w.CIDR = t.CIDR.String()
	}
	return w
}

// networkReadResp is `GET /v1/sandboxes/{id}/network` (the control-plane API).
type networkReadResp struct {
	// State is a MEMBER of this response, saving the caller a second round trip (the same
	// N+1 argument that puts `death_cause` on list). It is meaningful here because of a
	// mechanical biconditional the control-plane API states: `running` ⟺ the rules installed in the kernel
	// ARE this declaration; not `running` ⟹ nothing is installed.
	//
	// That holds because neither write point can make a `running` sandbox's record diverge
	// from its live rule set: editing the three egress fields on `running` is a 409, and
	// publish makes "record" and "rules" one un-halvable write.
	//
	// Hence NO `in_effect` / `pending` / `applies_at` here: `in_effect` would be a second
	// spelling of this field AND an overreach (the engine can honestly say "I hold a
	// placement", not "the kernel has these rules right now" — a `running` record whose
	// unit died but has not been demoted yet is the counter-example); `pending` would need
	// a second declaration in the record, which the API error contract forbids; `applies_at` is a WRITE
	// response's vocabulary.
	State   string          `json:"state"`
	Egress  egressReadWire  `json:"egress"`
	Ingress ingressReadWire `json:"ingress"`
}

type egressReadWire struct {
	Default string `json:"default"`
	// DefaultHitPacket counts the rule `egress.default` itself generates — the catch-all
	// refusal under `deny`, the catch-all accept under `allow` — and NOTHING else. The L0
	// floor and the internal deny-safe tier are excluded because neither is liftable by any
	// declaration (the provider security contract): reporting their hits here would send a caller to widen an
	// allow-list that cannot help.
	//
	// Same omission rule and same pointer reasoning as tupleWire.HitPacket.
	DefaultHitPacket *uint64       `json:"default_hit_packet,omitempty"`
	Allow            []tupleWire   `json:"allow"`
	Via              []viaReadWire `json:"via"`
}

type viaReadWire struct {
	Match    []tupleWire `json:"match"`
	Endpoint string      `json:"endpoint"`
}

type ingressReadWire struct {
	Publish []publishReadWire `json:"publish"`
	// ClosedHitPacket counts the ingress default-deny — inbound traffic to this guest that
	// no `publish[]` entry opened (the isolation model). Outside the engine, "refused" and "nothing
	// arrived" are indistinguishable: there is no vantage point from which a dropped
	// inbound packet is visible at all.
	//
	// DIRECTION-LEVEL, and deliberately not per-publish: a publish entry's rules span three
	// hooks and the `forward` one matches other sandboxes' egress, so no per-entry number is
	// cleanly attributable (the control-plane API). It likewise does NOT identify the sender — this is the one
	// rule in a per-sandbox table that can match another sandbox's egress, so nonzero means
	// "something was refused on the way to this guest", never who sent it.
	//
	// Same omission rule and pointer reasoning as tupleWire.HitPacket. 0 never means "never
	// refused": counters are placement-local and reset on pause/resume and on a
	// control-plane restart.
	ClosedHitPacket *uint64 `json:"closed_hit_packet,omitempty"`
}

type publishReadWire struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint is omitted when no placement holds one — see encodeNetwork.
	HostEndpoint string `json:"host_endpoint,omitempty"`
}

// handleNetworkGet serves `GET /v1/sandboxes/{id}/network` (the control-plane API).
func (e *Engine) handleNetworkGet(w http.ResponseWriter, r *http.Request, id model.SandboxID) {
	info, err := e.Mediator.Network(r.Context(), id)
	if err != nil {
		statusFor(w, err)
		return
	}
	resp := encodeNetwork(info.Decl, info.PublishEndpoints, info.Hits, info.Attribution)
	resp.State = string(info.State)
	writeJSON(w, http.StatusOK, resp)
}

// publishReq / publishResp are `POST /v1/sandboxes/{id}/publish` (the control-plane API).
type publishReq struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint is ACCEPTED AND IGNORED (see networkWire.State): `GET …/network` emits
	// this key on a `running` sandbox's publish items, so without it here, re-posting one
	// item back is a 400 on a field the engine itself produced. The engine never reads the
	// value — the endpoint is a placement product (the ID-reference model), so a caller cannot choose it.
	HostEndpoint string `json:"host_endpoint"`
}

type publishResp struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint follows `GET …/network`'s omission rule exactly — the write face
	// and the read face diverging on the same field would be a contract defect (the control-plane API).
	HostEndpoint string `json:"host_endpoint,omitempty"`
	// AppliesAt is "now" on a `running` sandbox (the rules went on during this
	// request) and "next-resume" on a `paused` one (recorded; the rules appear at the
	// next placement). Both are 200 — the two states differ only in the effect point
	// (the control-plane API).
	AppliesAt string `json:"applies_at"`
}

// handlePublish serves `POST /v1/sandboxes/{id}/publish`.
//
// `proto` accepts udp as well as tcp, and that is a deliberate asymmetry with
// `via[]`: UDP is excluded there because a redirected datagram's ORIGINAL
// destination cannot be recovered, and a DNAT to a FIXED guest port needs no such
// recovery — so the objection does not reach inbound at all (the API error contract / UDP steering limits).
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
// Required on every write point that carries a declaration, because the failure is
// SILENT: with unknown keys ignored, a caller who typed `"netwrok"` gets a 201 while the
// engine recorded the OMITTED-network normalization — a `deny` sandbox (the network declaration). The two
// cases are indistinguishable to the caller precisely because both are legal outcomes,
// which is why the misspelling has to be an error rather than a default.
func strictDecode(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty body retains the established optional-request behavior.
			return nil
		}
		return fmt.Errorf("request body: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("request body: top-level null is not a request object")
	}
	// Decode the captured value with the strict field policy. Keeping this
	// separate from the outer decoder lets us reject null explicitly while
	// preserving recursive unknown-field checks.
	value := json.NewDecoder(bytes.NewReader(raw))
	value.DisallowUnknownFields()
	if err := value.Decode(v); err != nil {
		return fmt.Errorf("request body: %w", err)
	}
	// A decoder stops after the first JSON value. Consume one more value so
	// concatenated documents or trailing garbage cannot be accepted as a valid
	// request with an ignored suffix.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body: multiple JSON values")
		}
		return fmt.Errorf("request body: trailing data: %w", err)
	}
	return nil
}

// networkStatus maps the API error contract's declaration refusals to 400 — ONE arm for all ten refusals,
// because every remedy is "fix the declaration". The CLASS stays recoverable through
// network.ClassOf for tests and logs; what the API error contract requires to stay distinguishable is what
// the caller's REMEDY depends on, and here it does not vary by class.
func networkStatus(err error) (int, bool) {
	if lifecycle.IsDeclarationInvalid(err) {
		return http.StatusBadRequest, true
	}
	// The PLACEMENT-side twin, and NOT a 400: a resume or a publish carries no declaration,
	// so 400 would send the caller to edit something they did not send. A recorded `via[]`
	// meets a deployment that supplies no second-hop uid, and BOTH documented remedies
	// change state elsewhere (restart the engine with the uid, or PATCH the `via[]` away).
	//
	// Without this arm it falls through to 500 — "engine fault, retry" when nothing failed
	// and no retry can help. The asymmetry makes that unmistakable: the SAME operator
	// condition would answer 400 on create (where ValidateDeployment sees the declaration)
	// and 500 on resume.
	if errors.Is(err, network.ErrViaUnenforceableAtPlacement) {
		return http.StatusConflict, true
	}
	return 0, false
}
