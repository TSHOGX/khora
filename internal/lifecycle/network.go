package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"engine/internal/model"
	"engine/internal/network"
	"engine/internal/ruleset"
	"engine/internal/runtime"
	"engine/internal/store"
)

// This file is the §4 mediator's network face: it threads §5.D's generator output
// to §5.A's installer, and owns the two write points for the declaration
// (§3.4/§5.D). It is the ONLY place that calls both subsystems, which is exactly
// what §4 exists for — `network` and `runtime` never call each other.

// steeringFor derives the §5.A steering spec from a sandbox's CURRENT record.
//
// Called at every placement rather than cached, and that is the definition of a
// Binding (§3.4): the declaration's SoT is the record, so a resume regenerates the
// rule set from whatever the record says NOW — not from anything frozen with the
// previous placement.
//
// The deployment's second-hop uid rides along because the provider installs the
// engine-level table once at startup and needs to know whether one was supplied at
// all; the DECLARATION never carries it (§2.4 / §6.2).
func (m *Mediator) steeringFor(rec model.Sandbox) (runtime.SteeringSpec, error) {
	spec, err := network.Generate(rec.Network)
	if err != nil {
		return runtime.SteeringSpec{}, fmt.Errorf("network: generate rule set for %s: %w", rec.ID, err)
	}
	ports := make([]runtime.PublishPort, 0, len(spec.Publish))
	for _, p := range spec.Publish {
		ports = append(ports, runtime.PublishPort{GuestPort: p.GuestPort, Proto: protoFor(p.Proto)})
	}
	return runtime.SteeringSpec{
		Rules:        spec.Rules,
		PublishPorts: ports,
		RoutedEgress: spec.RoutedEgress,
		SecondHopUID: m.deps.Deployment.SecondHopUID,
		SecondHopGID: m.deps.Deployment.SecondHopGID,
	}, nil
}

// protoFor translates the record's protocol enum into the enforcement layer's, in
// the mediator — the one place that legitimately knows both (the DeathCause shape).
func protoFor(p model.NetProto) ruleset.Proto {
	if p == model.NetUDP {
		return ruleset.UDP
	}
	return ruleset.TCP
}

// InstallSecondHopTable installs the ENGINE-LEVEL second-hop table (§2.4 invariant
// 2's second half). Called ONCE at orchestrator boot, before serving.
//
// Not per placement because the predicate is not per-sandbox: one
// deployment-supplied uid covers every `via[]` in the deployment, and hop 2's
// traffic arrives on no interface at all (measured: it counts ZERO in `forward` in
// all three shapes and is visible only in `output` — network.md D5 ㉔).
//
// With NO uid supplied it installs nothing, and the corresponding refusal lives at
// the write points: a `via[]` declaration is rejected rather than recorded, because
// recording it would leave a confused deputy live on the host while every FIRST-hop
// assertion still passed (network.ValidateDeployment).
func (m *Mediator) InstallSecondHopTable(ctx context.Context) error {
	if !m.deps.Deployment.SecondHopPinned() {
		// Nothing to install — but the STALE table from a previous run, started with a
		// uid this one does not have, must go. Leaving it would keep a host-level
		// `output` table narrowed by an identity this deployment no longer knows about:
		// a rule set nobody can explain, refusing traffic for a reason not in the
		// current configuration.
		if err := m.deps.Runtime.InstallSecondHop(ctx, ruleset.RuleSet{}, nil, nil); err != nil {
			return err
		}
		return nil
	}
	rs, err := network.SecondHopRules()
	if err != nil {
		return err
	}
	return m.deps.Runtime.InstallSecondHop(ctx, rs, m.deps.Deployment.SecondHopUID, m.deps.Deployment.SecondHopGID)
}

// Network reads a sandbox's declaration together with the two facts §6.1's read face
// needs alongside it: its lifecycle state, and the host endpoint each publish is
// bound to at the CURRENT placement.
//
// `state` is a member of that response rather than a second request because of a
// mechanical biconditional §6.1 spells out: `running` ⟺ what is installed in the
// kernel IS this declaration; not `running` ⟹ nothing is installed. Neither write
// point can make a `running` sandbox's record diverge from its live rule set —
// editing the three egress fields on `running` is a 409, and publish makes
// "record" and "rules" one un-halvable write. So there is no persistable difference
// between "declared" and "in effect", and the read face has exactly one bit to
// expose: whether a placement exists.
//
// The endpoints come from the PLACEMENT, not the record, because that is what they
// are (§3.3): rebound every resume, invalidated with the old placement. A `paused`
// sandbox has none, which is why §6.1 omits the key rather than reporting an empty
// string — a falsy-but-present field invites reading it as "is this publish in
// effect", and that judgment is doubly wrong (the rule set is installed whole and
// atomically, so there is no per-publish install state to report).
func (m *Mediator) Network(ctx context.Context, id model.SandboxID) (model.NetworkDecl, model.Lifecycle, []string, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return model.NetworkDecl{}, "", nil, err
	}
	return rec.Network.Normalize(), rec.State, m.publishEndpoints(id), nil
}

// publishEndpoints returns the current placement's bound publish endpoints, or nil
// when the engine holds no placement.
func (m *Mediator) publishEndpoints(id model.SandboxID) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.live[id]
	if ls == nil || ls.placement == nil {
		return nil
	}
	return append([]string(nil), ls.placement.PublishEndpoints...)
}

// SetNetwork is the `PATCH /v1/sandboxes/{id}` write for the declaration: whole for
// whole (§6.2 — no field-level merge, no pending value).
//
// Editable ONLY from `paused`, which is the same rule the resource caps follow and
// for the same reason: the declaration is consumed by the provider, which
// materializes it at placement. Accepting the edit on a `running` sandbox would
// leave the record and the live rule set disagreeing for an unbounded period — a
// fake invariant, on a security-relevant field. `applies_at: next-resume`.
//
// Note what does NOT come through here: `ingress.publish[]` has its own hot-add
// endpoint. It can, however, be REMOVED through here, because there is no
// un-publish endpoint — and that is a mechanism decision rather than an omission:
// measured, deleting a publish's DNATs refuses new connections while ESTABLISHED
// flows keep transferring data (conntrack holds the NAT binding and rule deletion
// is not evaluated against it), so a `running` un-publish could only promise "no new
// connections", never "the port is closed" — and the difference is exactly what a
// caller revoking an inbound port wants. Going through this whole-binding PATCH has
// no such ambiguity: the placement is rebuilt and the old flows go with it.
func (m *Mediator) SetNetwork(ctx context.Context, id model.SandboxID, d model.NetworkDecl) error {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return fmt.Errorf("set network: %w", err)
	}
	switch rec.State {
	case model.Paused:
		// editable
	case model.Running:
		return fmt.Errorf("set network: %w: the network declaration is editable only from `paused` "+
			"(the rule set is regenerated at the next placement, §3.4/§5.D) — pause first, then "+
			"PATCH, then resume. `POST /v1/sandboxes/%s/publish` is the one field that hot-adds on a "+
			"running sandbox", store.ErrConflict, id)
	default:
		return fmt.Errorf("set network: %w (state=%s: a Binding is editable only from running or paused)",
			store.ErrConflict, rec.State)
	}
	next := d.Normalize()
	if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, store.Binding{Network: &next}, nil); err != nil {
		return fmt.Errorf("set network: %w", err)
	}
	return nil
}

// AddPublish is `POST /v1/sandboxes/{id}/publish` (§5.D / §6.1): the one Binding
// field with a second write point, and the one that may be written on a `running`
// sandbox.
//
// # Why it is allowed on `running` at all
//
// A publish is a pure HOST-SIDE rule set, purely additive, touching no isolation
// unit — so unlike the egress fields there is no way for the record to say one thing
// while a live isolation unit does another (§5.D).
//
// # Why `paused` is equally legal
//
// The exception is "MAY be written on `running`", not "only on `running`". §3.4's
// state gate is a TIGHTENING of `running`; the `paused` side is open for every
// Binding field, and a publish endpoint that refused `paused` would be the only
// field in the engine harder to write while paused than while running. It would also
// push the caller onto the whole-binding PATCH — the very write point §6.1 documents
// as dangerous.
//
// # The atomic order, which is not a style choice
//
// Record FIRST, then rules. The invariant is one-directional — "there is no inbound
// port the record does not describe" — so of the two half-done outcomes only one is
// acceptable: "recorded, not installed" is a functional gap that is fail-CLOSED and
// self-heals at the next placement, while "installed, not recorded" is an open port
// the engine itself does not know about, surviving until the next placement. Hence
// the rollback on an install failure, and hence the transition lock is held for the
// whole thing.
//
// # Why the read-append happens HERE
//
// Two concurrent publishes must not lose one. The store replaces the whole value —
// it cannot append — so the append has to happen under this lock. §6.1 documents a
// caller-side lost-update sharp edge for PATCH; that is a DIFFERENT thing, and it
// does not excuse an engine-side one.
//
// Returns the resulting item, its host endpoint (empty when no placement holds one),
// and whether it took effect now.
func (m *Mediator) AddPublish(ctx context.Context, id model.SandboxID, p model.NetPublish) (model.NetPublish, string, bool, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()

	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return p, "", false, fmt.Errorf("publish: %w", err)
	}
	switch rec.State {
	case model.Running, model.Paused:
		// both legal, differing only in when the rules appear (§6.1)
	default:
		return p, "", false, fmt.Errorf("publish: %w (state=%s: publish is valid on running or paused)",
			store.ErrConflict, rec.State)
	}

	next, added := network.AddPublish(rec.Network, p)
	// Validate the RESULT, not the item: a declaration is validated as a whole, and
	// this one must still be legal after the append (§6.2 — the checks run before
	// anything is materialized).
	if err := network.Validate(next, true, m.hostLocal()); err != nil {
		return p, "", false, err
	}
	if added {
		if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, store.Binding{Network: &next}, nil); err != nil {
			return p, "", false, fmt.Errorf("publish: record the declaration: %w", err)
		}
	}

	live := m.liveState(id)
	if rec.State != model.Running || live == nil || live.placement == nil {
		// `paused`: recorded only. The rules appear at the next placement, which is
		// not a gap in the invariant — a paused sandbox has NO placement, so its whole
		// declaration (the egress fields included) has not one rule installed. A record
		// describing MORE than what exists is the definition of `paused`; the reverse is
		// what §2.4 refuses.
		return p, "", false, nil
	}

	// `running`: install now. The whole table is re-rendered and re-installed as ONE
	// atomic file, which is both required and sufficient — measured, that leaves live
	// flows intact 5/5, while a delete-then-install kills them 5/5 (network.md D5 ㉒).
	// It also makes the four rules of a publish appear as one unit with no
	// "only the DNAT is installed" state observable.
	steering, gerr := m.steeringFor(model.Sandbox{ID: id, Network: next})
	if gerr != nil {
		m.rollbackPublish(ctx, id, rec, added)
		return p, "", false, gerr
	}
	if err := m.deps.Runtime.ReinstallRuleSet(ctx, live.placement, steering); err != nil {
		// Install failed → remove the row again, so the record does not describe an
		// inbound port that does not exist... which is the SAFE direction of the two:
		// see the doc comment. The rollback is a whole-table re-render too, so unlike a
		// per-rule teardown it cannot leave a HALF-torn rule set — the shape whose
		// failure signature is "connects, then reads hang", the hardest to diagnose
		// (network.md D5 ⑰).
		m.rollbackPublish(ctx, id, rec, added)
		return p, "", false, fmt.Errorf("publish: install rules: %w", err)
	}
	return p, m.endpointFor(id, p), true, nil
}

// rollbackPublish restores the pre-publish declaration after a failed install.
//
// Logged rather than silent on failure: if this fails the record describes an
// inbound port whose rules are not installed, which is the fail-closed direction —
// but an operator needs to know, because the next placement WILL install it.
func (m *Mediator) rollbackPublish(ctx context.Context, id model.SandboxID, rec model.Sandbox, added bool) {
	if !added {
		return // idempotent no-op: nothing was written, nothing to undo
	}
	prev := rec.Network.Normalize()
	if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, store.Binding{Network: &prev}, nil); err != nil {
		m.deps.Logf("publish %s: rule install failed AND the record rollback failed (%v) — the record "+
			"now describes an inbound port whose rules are not installed. That is the fail-closed "+
			"direction, but the NEXT placement will install it: reconcile the declaration by hand", id, err)
	}
}

// endpointFor returns the host endpoint bound to a publish item at the current
// placement, or "" if there is none.
func (m *Mediator) endpointFor(id model.SandboxID, p model.NetPublish) string {
	rec, err := m.deps.Store.GetSandbox(context.Background(), id)
	if err != nil {
		return ""
	}
	eps := m.publishEndpoints(id)
	for i, item := range rec.Network.Ingress.Publish {
		if item.GuestPort == p.GuestPort && item.Proto == p.Proto && i < len(eps) {
			return eps[i]
		}
	}
	return ""
}

// liveState returns the sandbox's live placement handle, or nil.
func (m *Mediator) liveState(id model.SandboxID) *liveState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[id]
}

// ValidateDeclaration applies §6.2's eight 400 classes plus the deployment refusal,
// BEFORE the admission gate (§6.2: an invalid declaration is refused before any byte
// is touched or any budget recorded, so 400 and 507 are never both true).
//
// It lives on the mediator so every write point passes through it structurally —
// create, PATCH and publish — rather than by each call site remembering to.
func (m *Mediator) ValidateDeclaration(d model.NetworkDecl, given bool) error {
	if err := network.Validate(d, given, m.hostLocal()); err != nil {
		return err
	}
	return network.ValidateDeployment(d, m.deps.Deployment)
}

// hostLocal returns the host-locality predicate §6.2's fifth class needs, from the
// deployment (tests inject a fixture; production reads the interface table).
func (m *Mediator) hostLocal() network.HostLocal {
	if m.deps.Deployment.HostLocal != nil {
		return m.deps.Deployment.HostLocal
	}
	return network.SystemHostLocal
}

// IsDeclarationInvalid reports whether err is one of §6.2's declaration refusals, so
// the API layer maps it to 400 through one arm rather than nine.
func IsDeclarationInvalid(err error) bool { return errors.Is(err, network.ErrDeclarationInvalid) }
