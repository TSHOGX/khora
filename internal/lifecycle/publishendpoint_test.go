package lifecycle

import (
	"context"
	"testing"

	"warren/internal/model"
)

// A host endpoint is stable WITHIN a placement and only within one. These tests pin the
// §3.3 boundary from the mediator's side — the side a caller of `POST …/publish`
// actually observes.
//
// §3.3 says an endpoint is a placement product: it changes from one placement to the
// next, and must not be cached as a stable identifier. That licenses exactly one kind of
// change and forbids two others:
//
//   - LICENSED: a NEW placement binds new endpoints. Resume gets a new netns, a new IP
//     and new ports, which is why §6.1 reports `host_endpoint` only while a placement
//     exists — a caller must re-read it, and TestPublishEndpointsMayChangeAcrossResume
//     asserts the engine really does behave this way rather than accidentally being
//     stable (an assertion that would otherwise silently pass on a frozen value).
//   - FORBIDDEN: an existing publish's endpoint moving because ANOTHER publish was
//     hot-added to the same live placement. §5.D's endorsement of hot-adding rests on
//     publish being PURELY ADDITIVE.
//   - FORBIDDEN: a repeated identical publish answering with a DIFFERENT endpoint.
//     §6.1 makes it idempotent — "return the existing one".
//
// The second and third are one mechanism in the provider (the binding is reused rather
// than re-drawn) and two separate promises to a caller, so each gets its own assertion.

// pubRunning creates a `running` sandbox to publish against.
func pubRunning(t *testing.T, m *Mediator) model.SandboxID {
	t.Helper()
	id, err := m.Create(context.Background(), CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// pubEndpoint reads back the endpoint §6.1 reports for one publish item.
func pubEndpoint(t *testing.T, m *Mediator, id model.SandboxID, port uint16, proto model.NetProto) string {
	t.Helper()
	info, err := m.Network(context.Background(), id)
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	decl, eps := info.Decl, info.PublishEndpoints
	for i, item := range decl.Ingress.Publish {
		if item.GuestPort == port && item.Proto == proto && i < len(eps) {
			return eps[i]
		}
	}
	return ""
}

// Hot-adding a publish does not move the endpoints already bound at that placement.
//
// The failure this pins is not a crash: the caller that published 8080 gets a 200 with
// an endpoint, someone publishes 9090, and 8080's endpoint silently becomes a different
// port with no record change asking for it. §5.D endorses hot-adding on a `running`
// sandbox precisely because a publish is a PURELY ADDITIVE host-side rule set, and
// re-binding an untouched item is not additive.
func TestHotAddingAPublishKeepsTheEarlierEndpoints(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	id := pubRunning(t, m)

	_, first, now, err := m.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish 8080: %v", err)
	}
	if !now || first == "" {
		t.Fatalf("publishing on a running sandbox gave applies-now=%v endpoint=%q; want both set", now, first)
	}

	if _, second, _, err := m.AddPublish(ctx, id, model.NetPublish{GuestPort: 9090, Proto: model.NetTCP}); err != nil {
		t.Fatalf("publish 9090: %v", err)
	} else if second == first {
		t.Fatalf("the second publish got the SAME endpoint %q as the first", second)
	}

	if got := pubEndpoint(t, m, id, 8080, model.NetTCP); got != first {
		t.Errorf("hot-adding 9090 moved 8080's endpoint: %s → %s. A publish is purely additive (§5.D) — "+
			"that is the property the hot-add exception rests on, so a caller holding the endpoint it "+
			"was just handed must not lose it because somebody else published a different port", first, got)
	}
}

// A repeated identical publish returns the SAME endpoint.
//
// §6.1: "Publishing the same `{guest_port, proto}` twice is idempotent: return the
// existing item". The declaration layer
// already handles this (network.AddPublish reports added=false and nothing is written),
// but the mediator still runs the install path in that case — the record and the rules
// are one un-halvable write — so the endpoint must survive a re-bind, not merely a
// re-append.
func TestARepeatedPublishReturnsTheSameEndpoint(t *testing.T) {
	ctx := context.Background()
	m, st := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	id := pubRunning(t, m)

	item := model.NetPublish{GuestPort: 8080, Proto: model.NetTCP}
	_, first, _, err := m.AddPublish(ctx, id, item)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	_, again, nowAgain, err := m.AddPublish(ctx, id, item)
	if err != nil {
		t.Fatalf("repeat publish: %v", err)
	}
	if again != first {
		t.Errorf("the idempotent repeat answered with %q, the first call with %q. §6.1 makes a repeated "+
			"`{guest_port, proto}` return THE EXISTING item — a caller retrying a 200 it never saw (or "+
			"simply re-asserting its desired state) would otherwise be told its port moved", again, first)
	}
	if !nowAgain {
		t.Errorf("the repeat reported applies-now=false on a running sandbox")
	}
	// And the declaration still holds exactly one item — the property the endpoint
	// assertion above rests on (two items would make "the existing one" ambiguous).
	rec, err := st.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Network.Ingress.Publish); n != 1 {
		t.Errorf("the declaration holds %d items after two identical publishes, want 1", n)
	}
}

// tcp/8080 and udp/8080 are TWO publishes, and both get an endpoint.
//
// The declaration layer keys on the pair (network.AddPublish dedupes on it, §5.D defines
// idempotency on it), so both items are legal and Generate emits four DNATs. A provider
// keying its binding on the guest port alone refuses the second as a duplicate whose
// deduplication "belongs to the declaration layer" — which had in fact done its job.
// Asserted end-to-end here because the failure is reachable only through both layers:
// the declaration is in the store by the time the binding refuses it, so the sandbox
// then fails to resume FOREVER, its entry point being a `POST …/publish` that returned
// 200.
func TestTCPAndUDPOnOneGuestPortBothGetEndpoints(t *testing.T) {
	ctx := context.Background()
	m, st := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	id := pubRunning(t, m)

	_, tcpEP, _, err := m.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish tcp/8080: %v", err)
	}
	_, udpEP, _, err := m.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetUDP})
	if err != nil {
		t.Fatalf("publish udp/8080 alongside tcp/8080: %v — both items are legal at the declaration "+
			"layer and the generator emits four DNATs for them, so a refusal here is the provider's "+
			"key width mistaken for a duplicate", err)
	}
	if tcpEP == "" || udpEP == "" {
		t.Fatalf("one of the pair got no endpoint: tcp=%q udp=%q", tcpEP, udpEP)
	}
	if tcpEP == udpEP {
		t.Errorf("tcp/8080 and udp/8080 report the same host endpoint %s — they are two publishes and "+
			"one endpoint cannot carry both DNATs", tcpEP)
	}
	rec, err := st.GetSandbox(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Network.Ingress.Publish); n != 2 {
		t.Fatalf("the declaration holds %d items; want 2 — tcp/8080 and udp/8080 are distinct (§5.D)", n)
	}
	// The whole record must also survive a REPLACEMENT of the placement, which is where
	// the port-only key turned a legal declaration into a permanently unresumable
	// sandbox: pause discards the placement, resume regenerates from the record.
	if err := m.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := m.Resume(ctx, id); err != nil {
		t.Fatalf("resume with `[{8080,tcp},{8080,udp}]` recorded: %v — the declaration is durable, so a "+
			"binding that refuses the pair fails EVERY resume, not just the publish that wrote it", err)
	}
}

// ACROSS placements the endpoint MAY change — the other side of the §3.3 boundary.
//
// This is not a nice-to-have: the two tests above would also pass if endpoints were
// globally frozen per sandbox, and a frozen endpoint IS a stable identifier, which is
// what §3.3 forbids and what §6.1's "no `host_endpoint` key without a placement" exists
// to stop a caller relying on. So this asserts the rebinding really happens.
func TestPublishEndpointsAreReboundAtANewPlacement(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestMediator(t, &fakeRootfs{}, &fakeRuntime{}, true)
	id := pubRunning(t, m)

	_, before, _, err := m.AddPublish(ctx, id, model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if err := m.Pause(ctx, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// While paused there is NO placement, so there is no endpoint to report (§6.1 omits
	// the key rather than reporting a falsy one).
	if got := pubEndpoint(t, m, id, 8080, model.NetTCP); got != "" {
		t.Errorf("a paused sandbox reported the host endpoint %q; want none — the endpoint is a "+
			"placement product and a paused sandbox has no placement (§3.3/§6.1)", got)
	}
	if err := m.Resume(ctx, id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	after := pubEndpoint(t, m, id, 8080, model.NetTCP)
	if after == "" {
		t.Fatal("the publish lost its endpoint across resume — the declaration is durable and every " +
			"placement regenerates the rule set from it (§5.D)")
	}
	if after == before {
		t.Errorf("the endpoint survived a placement REPLACEMENT unchanged (%s). §3.3 makes it a "+
			"placement product precisely so it cannot be cached as a stable identifier; a value that "+
			"never moves invites exactly that", before)
	}
}
