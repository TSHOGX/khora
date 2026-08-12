package warren

import (
	"context"
	"fmt"
	"net/http"
)

// The §5.D network declaration, and the one place this client makes a shape decision
// rather than copying one.
//
// The engine has TWO wire structs for this — a write form and a read form — because
// they differ in three fields (the read face adds `state` and packet counters, and its
// `default` is always present). This client has ONE, on purpose: §6.1 makes
// read-modify-write a CONTRACT REQUIREMENT for editing a declaration, since a PATCH
// built from a stale read silently drops a publish added in between, on a
// security-relevant field. So `GET …/network`'s response body must be a legal PATCH
// body, and the engine spends three comments guaranteeing exactly that (its write
// structs ACCEPT AND IGNORE every read-only member the read face emits).
//
// Two client types would hand that guarantee back: a caller would convert between
// them field by field, and the first field they forgot would be the lost update the
// requirement exists to prevent. One type means Network(ctx, id) returns something
// SetNetwork(ctx, id, …) accepts, unchanged.
//
// This is not a composite convenience type (ADR-0008 rule 1 bars those): it is one
// endpoint's one object, and every field below is one §6.1 field.
type Network struct {
	Egress  Egress  `json:"egress"`
	Ingress Ingress `json:"ingress"`
	// State is READ-ONLY: the engine emits it and ignores it on write. Meaningful
	// because of a biconditional §6.1 states — `running` ⟺ the rules installed in the
	// kernel ARE this declaration; not `running` ⟹ nothing is installed.
	State SandboxState `json:"state,omitempty"`
}

// Egress is the outbound half of the declaration.
type Egress struct {
	// Default is REQUIRED on write whenever a Network object is given, and a POINTER so
	// that "absent" stays distinguishable from "the empty string". Both are refused,
	// but they are different 400s, and a caller who wrote "" deserves to be told that
	// rather than told the field is missing. Values: "deny" | "allow".
	Default *string `json:"default"`
	// DefaultHitPacket counts the rule `Default` itself generates and NOTHING else —
	// the L0 floor and the internal deny-safe tier are excluded, because neither is
	// liftable by any declaration, so counting their hits here would send a caller to
	// widen an allow-list that cannot help. READ-ONLY.
	DefaultHitPacket *uint64 `json:"default_hit_packet,omitempty"`
	Allow            []Tuple `json:"allow,omitempty"`
	Via              []Via   `json:"via,omitempty"`
}

// Ingress is the inbound half.
type Ingress struct {
	Publish []Publish `json:"publish,omitempty"`
}

// Tuple is `{cidr, port, proto}`. TWO of the three are wildcardable BY OMISSION —
// omitted CIDR means "any destination", omitted Port means "any port". Proto has no
// wildcard, on mechanism grounds: port matching depends on protocol matching in the
// kernel, so `{cidr, port}` cannot be rendered at all.
type Tuple struct {
	CIDR  string `json:"cidr,omitempty"`
	Port  uint16 `json:"port,omitempty"`
	Proto string `json:"proto"`
	// HitPacket is READ-ONLY, and nil rather than 0 when counters are unreadable: a
	// zero counter is itself the signal that a rule is never matching, so the two must
	// not collapse.
	HitPacket *uint64 `json:"hit_packet,omitempty"`
}

// Via steers matching flows to an endpoint instead of letting them out.
type Via struct {
	Match    []Tuple `json:"match"`
	Endpoint string  `json:"endpoint"`
}

// Publish exposes one guest port.
type Publish struct {
	GuestPort uint16 `json:"guest_port"`
	Proto     string `json:"proto"`
	// HostEndpoint is READ-ONLY — a placement product, so a caller cannot choose it,
	// and it is absent when no placement holds one (i.e. while not `running`).
	HostEndpoint string `json:"host_endpoint,omitempty"`
}

// PublishResult is what PublishPort answers with.
type PublishResult struct {
	GuestPort    uint16 `json:"guest_port"`
	Proto        string `json:"proto"`
	HostEndpoint string `json:"host_endpoint,omitempty"`
	// AppliesAt is "now" on a `running` sandbox and "next-resume" on a `paused` one.
	// BOTH are a 200 — the two states differ only in the effect point, so this field is
	// the answer to "did anything happen yet", and there is no status code for it.
	AppliesAt string `json:"applies_at"`
}

// Network reads a sandbox's network declaration.
//
// The returned value is a legal input to SetNetwork — see the Network type's doc for
// why that property is the reason this client has one type here and the engine has two.
func (c *Client) Network(ctx context.Context, sandboxID string) (Network, error) {
	var out Network
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+sandboxID+"/network", nil, &out)
	return out, err
}

// SetNetwork replaces a sandbox's network declaration wholesale (PATCH with a
// `network` object).
//
// WHOLESALE IS THE CONTRACT, not this client's choice: §6.1 has no partial edit for a
// declaration, because a merge semantics on an allow-list is how a rule gets dropped
// without anyone deciding to drop it. The safe sequence is `pause → Network() →
// SetNetwork() → Resume()`, and reading BEFORE the pause pulls the whole pause window
// into the lost-update window.
//
// Editing the three egress fields on a `running` sandbox is a 409 (IsConflict).
func (c *Client) SetNetwork(ctx context.Context, sandboxID string, n Network) error {
	if n.Egress.Default == nil {
		return fmt.Errorf("SetNetwork: egress.default is required (\"deny\" or \"allow\")")
	}
	body := struct {
		Network Network `json:"network"`
	}{Network: n}
	return c.do(ctx, http.MethodPatch, "/v1/sandboxes/"+sandboxID, body, nil)
}

// PublishPort exposes one guest port. Unlike the egress fields, this is legal on a
// `running` sandbox — the record and the kernel rules are one un-halvable write — and
// on a `paused` one it is recorded and appears at the next placement (see
// PublishResult.AppliesAt).
func (c *Client) PublishPort(ctx context.Context, sandboxID string, guestPort uint16, proto string) (PublishResult, error) {
	req := struct {
		GuestPort uint16 `json:"guest_port"`
		Proto     string `json:"proto"`
	}{GuestPort: guestPort, Proto: proto}
	var out PublishResult
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/publish", req, &out)
	return out, err
}
