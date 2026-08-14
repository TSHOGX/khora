package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/ruleset"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
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
//
// # Why the deployment premise is RE-CHECKED here and not only at the write points
//
// Because a declaration outlives the deployment that admitted it. `via[]` is only
// legal when the deployment supplies a second-hop uid (network.ValidateDeployment, at
// create/PATCH/publish), but the uid is a per-process input while the declaration is a
// durable record — so "restart without `--via-uid`" pulls the pair apart with no write
// anywhere: the write points are now correctly closed, and the row admitted by the
// previous run is still in the store, still regenerated at every placement.
//
// Here rather than in Generate, on the division §5.D already draws: the second-hop pin is NOT
// part of a sandbox's rule set (one engine-level `output` table narrowed by uid covers the whole
// deployment, installed once at startup), so its absence cannot show up as a missing rule — only
// as a PRECONDITION on placing a sandbox whose declaration needs it. network.ValidatePlacement
// holds the mechanism and the measurement; this is the funnel that makes it structural, all
// three placement paths (create's coldStart, resume's coldStart, adopt) deriving their spec here
// exactly as all three write points go through ValidateDeclaration — an invariant a write point
// guards must be RE-EVALUATED wherever the state it guarded is re-materialized.
func (m *Mediator) steeringFor(rec model.Sandbox) (runtime.SteeringSpec, error) {
	if err := network.ValidatePlacement(rec.Network, m.deps.Deployment); err != nil {
		return runtime.SteeringSpec{}, fmt.Errorf("network: place %s: %w", rec.ID, err)
	}
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
// With NO uid supplied it installs nothing (and the provider drops a stale table left
// by a previous run — see InstallSecondHop). §2.4's second invariant is then
// unenforced, so `via[]` is refused at BOTH ends, which is one obligation in two
// places rather than a belt-and-braces pair: the write points keep such a declaration
// out of the record (network.ValidateDeployment — recording it would leave a confused
// deputy live while every FIRST-hop assertion still passed), and every placement
// re-checks the premise (steeringFor, whose doc states why a write-point check is not
// enough).
func (m *Mediator) InstallSecondHopTable(ctx context.Context) error {
	if !m.deps.Deployment.SecondHopPinned() {
		// The nil uid is handed to the provider rather than short-circuited here: tearing
		// down the stale table is a MECHANISM (an nft table on this host), so it belongs to
		// §5.A. The mediator's part is knowing that "no uid" is a state the provider must
		// be TOLD about, not one it can be left to infer from never being called.
		if err := m.deps.Runtime.InstallSecondHop(ctx, ruleset.RuleSet{}, nil); err != nil {
			return err
		}
		return nil
	}
	rs, err := network.SecondHopRules()
	if err != nil {
		return err
	}
	return m.deps.Runtime.InstallSecondHop(ctx, rs, m.deps.Deployment.SecondHopUID)
}

// NetworkInfo is the §6.1 `GET …/network` read shape.
//
// A STRUCT rather than more return values: this read already carried four, and the
// per-entry hit counters would make six — at which point a caller reading
// `_, _, x, _, _, err :=` is one reordering away from a silent mix-up between the
// declaration's endpoints and its counters.
type NetworkInfo struct {
	Decl  model.NetworkDecl
	State model.Lifecycle
	// PublishEndpoints are the current placement's bound host endpoints, nil when the
	// engine holds no placement (§3.3: an endpoint is a placement product).
	PublishEndpoints []string
	// Hits maps abstract §5.D counter names to packet counts for the CURRENT placement,
	// and is NIL when there is no placement or the counters could not be read.
	//
	// Nil rather than an empty map, because §6.1 omits the key in exactly those cases and
	// an empty map would let a caller distinguish neither. Note that pause DESTROYS the
	// rule-set table rather than zeroing it, so "paused" genuinely has no counters to
	// report — this is not an omission convention papering over a zero.
	Hits map[string]uint64
	// Attribution maps declaration entries to the counter names above (per-entry
	// `hit_packet`). Regenerated from the record on read, which is sound because of the
	// biconditional on Network below.
	Attribution network.CounterAttribution
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
func (m *Mediator) Network(ctx context.Context, id model.SandboxID) (NetworkInfo, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return NetworkInfo{}, err
	}
	info := NetworkInfo{
		Decl:             rec.Network.Normalize(),
		State:            rec.State,
		PublishEndpoints: m.publishEndpoints(id),
	}
	// Attribution is a pure function of the declaration, so it is available regardless of
	// whether a placement exists — which is what lets §6.1 report the entries with their
	// keys omitted rather than reporting nothing at all.
	if spec, gerr := network.Generate(info.Decl); gerr == nil {
		info.Attribution = spec.Counters
	}
	info.Hits = m.ruleHits(ctx, id)
	return info, nil
}

// ruleHits reads the current placement's §5.D counters, or nil when there is no
// placement or the read fails.
//
// DEGRADES rather than failing the read, the same rule inspect-shaped readers follow
// everywhere else: a counter is observation, and §6.1 omits what it cannot read. It must
// never fail `GET …/network`, because that endpoint is a CONTRACT REQUIREMENT of the
// read-modify-write cycle for editing a declaration — a counter read breaking it would
// make an unreadable counter block a security-relevant edit.
func (m *Mediator) ruleHits(ctx context.Context, id model.SandboxID) map[string]uint64 {
	m.mu.Lock()
	ls := m.live[id]
	var pl *runtime.Placement
	if ls != nil {
		pl = ls.placement
	}
	m.mu.Unlock()
	if pl == nil || m.deps.Runtime == nil {
		return nil
	}
	hits, err := m.deps.Runtime.RuleHits(ctx, pl)
	if err != nil {
		m.deps.Logf("read rule counters for %s: %v", id, err)
		return nil
	}
	return hits
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

// AddPublish is `POST /v1/sandboxes/{id}/publish` (§5.D / §6.1): the one Binding
// field with a second write point, and the one that may be written on a `running`
// sandbox.
//
// ALLOWED ON `running` because a publish is a pure host-side rule set, purely additive, touching
// no isolation unit — so unlike the egress fields there is no way for the record to say one thing
// while a live isolation unit says another (§5.D).
//
// `paused` IS EQUALLY LEGAL: the exception is "MAY be written on `running`", not "only on". §3.4's
// state gate tightens `running`, while the `paused` side is open for every Binding field, so
// refusing it would make this the one field harder to write paused than running — and would push
// the caller onto the whole-binding PATCH, the write point §6.1 documents as dangerous.
//
// RECORD FIRST, THEN RULES, because the invariant is one-directional ("there is no inbound port
// the record does not describe"): of the two half-done outcomes, "recorded, not installed" is a
// fail-CLOSED gap that self-heals at the next placement, while "installed, not recorded" is an
// open port the engine does not know about. Hence the rollback on install failure, and hence the
// transition lock covers the whole thing.
//
// THE READ-APPEND HAPPENS HERE so two concurrent publishes cannot lose one: the store replaces
// the whole value rather than appending. §6.1's caller-side lost-update edge for PATCH is a
// different thing and does not excuse an engine-side one.
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
	//
	// Through ValidateDeclaration, which is what makes "all three write points go
	// through one funnel" true rather than nearly true: calling network.Validate
	// directly here skipped ValidateDeployment, so a record already carrying `via[]`
	// (written under a deployment that DID supply a uid) accepted a publish under one
	// that does not — committing a declaration whose §2.4 invariant this engine cannot
	// deliver. Publish cannot introduce `via[]` itself, so nothing was live.
	if err := m.ValidateDeclaration(next, true); err != nil {
		return p, "", false, err
	}
	if added {
		if err := m.deps.Store.UpdateBinding(ctx, id, rec.State, store.Binding{Network: &next}, nil); err != nil {
			return p, "", false, fmt.Errorf("publish: record the declaration: %w", err)
		}
	}

	live := m.liveState(id)
	if rec.State != model.Running || live == nil || live.placement == nil {
		// `paused`: recorded only. Not a gap in the invariant — a paused sandbox has NO
		// placement, so its whole declaration has not one rule installed, and a record
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
// Logged rather than silent on failure: the residue is the fail-closed direction (see
// AddPublish), but an operator needs to know, because the next placement WILL install it.
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

// ValidateDeclaration applies §6.2's nine 400 classes plus the deployment refusal,
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
