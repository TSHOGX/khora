package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
)

// The premise a `via[]` declaration rests on is re-evaluated AT EVERY PLACEMENT, not
// only where the declaration is written.
//
// The defect these tests pin: a declaration is durable (the record is its SoT and every
// placement regenerates the rule set from it, resource admission) while the second-hop uid is a
// per-process deployment input. Deployment A supplies the uid and admits `via[]` into
// the record; the operator restarts WITHOUT the flag. Every write point is now
// correctly closed and the row is still there — so the sandbox could resume or be
// adopted, install its `prerouting` DNAT and its narrow `input` accept, and steer guest
// flows to an endpoint process whose own egress is pinned by nothing.
//
// It is silent by construction, which is why it needs its own assertions rather than
// riding on an existing one: hop 2 leaves on `output` and counts ZERO in `forward`
// (network substrate measurements), so no first-hop assertion, counter or deny test observes it. And
// the rule set is not the tell either — network.Generate deliberately emits no
// second-hop rule (the pin is one engine-level table sized by the deployment), so the
// rule set for a `via[]` declaration is byte-identical whether or not the pin exists.
//
// THE WRITE-POINT HALF IS NOT WHAT IS UNDER TEST HERE — it already had coverage
// (network.TestViaIsRefusedWhenTheDeploymentPinsNoSecondHop) and it is not the defect.
// What is under test is placement: resume and adopt, the two paths that materialize a
// row nobody is writing.

// viaEndpoint is viaDecl's steering endpoint, and viaHostLocal is the HostLocal
// predicate that makes it host-local.
//
// EVERY mediator in this file must be built with viaHostLocal, or the fixture is not
// the fixture it claims to be. ClassViaEndpointNotHostLocal (endpoint-not-host-local) is checked
// AHEAD of the deployment class, so under the real interface table a test aiming at the
// deployment class refuses one step earlier and an assertion phrased as
// IsDeclarationInvalid passes on the wrong refusal — including when the deployment DOES
// pin the uid, i.e. it would keep passing with the check under test deleted. Injecting
// the predicate is the same discipline network's own unit tests use (noHostLocal /
// allHostLocal): pin the answer to a fixture rather than to whatever addresses the test
// machine happens to carry.
const viaEndpoint = "10.0.0.5:15443"

func viaHostLocal(a netip.Addr) (bool, error) { return a == netip.MustParseAddr("10.0.0.5"), nil }

// viaDecl is a recorded declaration that steers one flow to a host-local endpoint —
// legal under a deployment that pins the second hop, unenforceable under one that does
// not.
func viaDecl(t *testing.T) model.NetworkDecl {
	t.Helper()
	p, err := netip.ParsePrefix("1.1.1.1/32")
	if err != nil {
		t.Fatal(err)
	}
	return model.NetworkDecl{Egress: model.NetEgress{
		Default: model.NetDeny,
		Via: []model.NetVia{{
			Match:    []model.NetTuple{{CIDR: p, Port: 443, Proto: model.NetTCP}},
			Endpoint: viaEndpoint,
		}},
	}}.Normalize()
}

// seedPausedWithVia seeds a `paused` record already carrying `via[]` — the state a
// previous deployment left behind, written directly because the CURRENT deployment's
// write points would (correctly) refuse it.
func seedPausedWithVia(t *testing.T, st *store.Memory, id string) {
	t.Helper()
	seedPaused(t, st, id, "ov_"+id, "bare")
	decl := viaDecl(t)
	if err := st.UpdateBinding(context.Background(), model.SandboxID(id), model.Paused,
		store.Binding{Network: &decl}, nil); err != nil {
		t.Fatal(err)
	}
}

// PATH 2 of 3 — resume's coldStart. A `paused` sandbox whose record carries `via[]`
// must NOT resume under a deployment that pins no second hop.
//
// The two assertions are the atomic transition rollback shape for a resume failure: the placement is
// refused, and the record returns atomically to its source state (`paused`). It also
// checks the refusal happens BEFORE the isolation unit is launched — the point of
// putting it on the derivation funnel rather than after the fact is that no confused
// deputy is ever briefly reachable.
func TestResumeIsRefusedWhenTheDeploymentCannotEnforceTheRecordedVia(t *testing.T) {
	ctx := context.Background()
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m, st := newTestMediator(t, rp, rt, true)
	seedPausedWithVia(t, st, "sbx_via")

	err := m.Resume(ctx, "sbx_via")
	if err == nil {
		t.Fatal("resume placed a sandbox whose recorded egress.via[] this deployment cannot pin — " +
			"that installs the DNAT and the narrow input accept with NO second-hop enforcement " +
			"behind them, i.e. a confused deputy live on the host, and every first-hop assertion " +
			"keeps passing over it (network substrate measurements)")
	}
	if !errors.Is(err, network.ErrViaUnenforceableAtPlacement) {
		t.Errorf("resume failed for some other reason than the unenforceable-via refusal: %v", err)
	}
	// atomic transition rollback: a failed transition returns atomically to the source state.
	rec, gerr := st.GetSandbox(ctx, "sbx_via")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if rec.State != model.Paused {
		t.Errorf("state = %s; want paused (a refused resume returns to its source state, atomic transition rollback)", rec.State)
	}
	// Nothing was launched: the refusal is on the derivation funnel, ahead of the
	// provider, so the deputy is never even briefly reachable.
	if calls := strings.Join(rt.Calls(), ","); strings.Contains(calls, "Launch:") {
		t.Errorf("the isolation unit was launched before the premise was checked; calls=%v", rt.Calls())
	}
	// And the record is untouched — the remedy is the operator's choice of the two, so
	// the engine must not silently rewrite the declaration for them.
	if len(rec.Network.Egress.Via) != 1 {
		t.Errorf("the refusal mutated the declaration (via has %d entries); the engine refuses to "+
			"PLACE it, it does not decide the remedy", len(rec.Network.Egress.Via))
	}
}

// The control arm for the resume path: the SAME record resumes normally once the
// deployment supplies the uid. Without this, a refusal that fired on every resume would
// pass the test above.
func TestResumeSucceedsForTheSameViaRecordWhenTheDeploymentPinsTheSecondHop(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	uid := 4711
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
		Deployment:    network.Deployment{SecondHopUID: &uid},
	})
	seedPausedWithVia(t, st, "sbx_ok")

	if err := m.Resume(ctx, "sbx_ok"); err != nil {
		t.Fatalf("resume refused a via[] record under a deployment that DOES pin the second hop: %v", err)
	}
	rec, err := st.GetSandbox(ctx, "sbx_ok")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Running {
		t.Errorf("state = %s; want running", rec.State)
	}
}

// PATH 3 of 3 — adopt. The subtle one: the sandbox is ALIVE AND SERVING, and the engine
// still refuses to take it over.
//
// The disposition asserted here is DEMOTE, not protect, and the distinction is the
// point of the test. atomic transition rollback's protect-on-uncertainty covers facts the engine could not
// DETERMINE — a probe error, an un-rebuildable handle — and protecting means leaving
// the sandbox `running` in the keep-set. Here the fact is fully determined and it is
// determined to be unsafe, so protecting would mean deliberately keeping an unenforced
// deputy alive and re-deciding identically forever. `paused` is the fail-closed resting
// state (no placement ⇒ not one rule installed) and it is fully recoverable: the upper
// is untouched and a resume works the moment either remedy is applied.
//
// Four assertions, each a way the fix could be wrong: the record lands `paused`; the
// event is `crash-paused` (the FS-first pause and resume involuntary pause, so a subscriber can tell it from
// a user pause); NO death cause is stamped (nothing died — the unit was L1-alive and
// serving, and the provider security contract forbids guessing); and the placement is NOT in the keep-set, since
// reclaiming it is what actually removes the deputy's reachability.
func TestAdoptDemotesALiveSandboxThisDeploymentCannotPin(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	rt := &fakeRuntime{adoptAlive: map[string]bool{"sbx_adopt": true}}
	var logs []string
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
		Logf:          func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})
	// A `running` record carrying via[] — a survivor of the deployment that admitted it.
	seedRec(t, st, "sbx_adopt", "ov_adopt", model.Running)
	decl := viaDecl(t)
	if err := st.UpdateBinding(ctx, "sbx_adopt", model.Running, store.Binding{Network: &decl}, nil); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rec, err := st.GetSandbox(ctx, "sbx_adopt")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Paused {
		t.Fatalf("state = %s; want paused. An alive-and-serving survivor whose recorded via[] this "+
			"deployment cannot pin must be demoted, not adopted: adopting it re-installs the rule "+
			"set and leaves the endpoint process egressing on the guest's behalf under no floor, "+
			"and PROTECTING it (the uncertainty disposition) would keep that deputy alive on every "+
			"pass forever", rec.State)
	}
	// Nothing died: the unit was L1-alive and answering Health. A stamped cause would be
	// a false statement about it, the same judgment the not-serving arm makes.
	if rec.DeathCause != model.CauseNone {
		t.Errorf("death_cause = %q; want none — nothing died here, the unit was alive and serving "+
			"(the provider security contract forbids guessing a cause)", rec.DeathCause)
	}
	// The FS-first pause and resume involuntary pause emits `crash-paused`, never `paused`.
	evs, _ := st.UnpublishedEvents(ctx, allUnpublished)
	found := false
	for _, e := range evs {
		if e.Sandbox == "sbx_adopt" && e.Kind == "crash-paused" && e.State == model.Paused {
			found = true
		}
	}
	if !found {
		t.Errorf("no crash-paused event for the involuntary demote; got %+v", evs)
	}
	// The placement must NOT be protected: reclaiming it is what removes the deputy's
	// reachability (the netns and the rule set go with it).
	last := lastReclaimCall(rt.Calls())
	if strings.Contains(last, "sbx_adopt") {
		t.Errorf("the demoted sandbox's placement was kept from reclamation (%q) — the demotion only "+
			"closes the record; tearing the placement down is what actually removes the unenforced "+
			"deputy", last)
	}
	// The operator has to be able to act on this, so the log names the remedies.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "sbx_adopt") || !strings.Contains(joined, "--via-uid") {
		t.Errorf("the demote was not logged with an actionable cause; logs=%v", logs)
	}
}

// The control arm for the adopt path: the same live survivor is ADOPTED normally when
// the deployment pins the second hop — the refusal is specific to the missing uid, not
// to `via[]`.
func TestAdoptTakesOverAViaSurvivorWhenTheDeploymentPinsTheSecondHop(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	uid := 4711
	rt := &fakeRuntime{adoptAlive: map[string]bool{"sbx_keep": true}}
	m := New(Deps{
		Store: st, Runtime: rt, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
		Deployment:    network.Deployment{SecondHopUID: &uid},
	})
	seedRec(t, st, "sbx_keep", "ov_keep", model.Running)
	decl := viaDecl(t)
	if err := st.UpdateBinding(ctx, "sbx_keep", model.Running, store.Binding{Network: &decl}, nil); err != nil {
		t.Fatal(err)
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	rec, err := st.GetSandbox(ctx, "sbx_keep")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Running {
		t.Fatalf("state = %s; want running — a via[] survivor under a deployment that pins the "+
			"second hop is adoptable", rec.State)
	}
	if last := lastReclaimCall(rt.Calls()); !strings.Contains(last, "sbx_keep") {
		t.Errorf("the adopted survivor was not protected from reclamation: %q", last)
	}
}

// The boot half of the same restart: with no uid, the mediator must still CALL the
// provider with a nil one rather than skipping the call.
//
// That is the only way the stale engine-level table from the previous run gets torn down
// (the destroy is a mechanism and lives in the runtime provider contract's InstallSecondHop). Short-circuiting on
// `!SecondHopPinned()` would leave a host-level `output` table narrowed by a uid this
// deployment does not know, dropping traffic for a reason absent from the running
// configuration — and nothing else in the engine enumerates that table.
func TestBootTellsTheProviderThereIsNoSecondHopUIDRatherThanStayingSilent(t *testing.T) {
	rt := &fakeRuntime{}
	m, _ := newTestMediator(t, &fakeRootfs{}, rt, true)

	if err := m.InstallSecondHopTable(context.Background()); err != nil {
		t.Fatalf("InstallSecondHopTable: %v", err)
	}
	calls := strings.Join(rt.Calls(), ",")
	if !strings.Contains(calls, "InstallSecondHop:uid=nil") {
		t.Errorf("boot with no second-hop uid never reached the provider; calls=%v. The nil uid is "+
			"what triggers teardown of a STALE table left by a run that HAD one — skipping the call "+
			"leaves an engine-level output table narrowed by an identity this deployment cannot "+
			"explain, and nothing else enumerates it", rt.Calls())
	}
}

// PATH 1 of 3 — create's coldStart, for completeness of the funnel. Create ALSO passes
// through the write-point check (ValidateDeclaration), so this asserts the funnel is
// wired rather than a distinct hole: the refusal arrives from the write point, and the
// create leaves no record behind (atomic transition rollback: create has no fallback state).
func TestCreateWithViaIsRefusedWhenTheDeploymentPinsNoSecondHop(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{}
	m, st := newTestMediator(t, &fakeRootfs{}, rt, true)
	decl := viaDecl(t)

	_, err := m.Create(ctx, CreateSpec{Template: "bare", Network: &decl})
	if err == nil {
		t.Fatal("create accepted a via[] declaration with no second-hop uid")
	}
	if !IsDeclarationInvalid(err) {
		t.Errorf("create's refusal is not the 400-shaped declaration refusal: %v", err)
	}
	// THE CLASS, not merely the category: the fixture endpoint is one ClassViaEndpointNotHostLocal slip away
	// from refusing for a reason that has nothing to do with the deployment (see
	// viaHostLocal), and IsDeclarationInvalid cannot tell the two apart.
	if c, _ := network.ClassOf(err); c != network.ClassViaWithoutSecondHopUID {
		t.Errorf("create refused with class %q, want %q — the refusal under test is the DEPLOYMENT's "+
			"missing second-hop uid, and any other class means this test would keep passing with "+
			"that check deleted", c, network.ClassViaWithoutSecondHopUID)
	}
	recs, lerr := st.ListSandboxes(ctx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(recs) != 0 {
		t.Errorf("a refused create left %d record(s) behind; want none", len(recs))
	}
}

// THE PUBLISH WRITE POINT, which was outside the funnel: AddPublish called
// network.Validate directly and so skipped ValidateDeployment, while the funnel's own
// doc comment claimed all three write points passed through ValidateDeclaration.
//
// Reachable without any via[] in the request: the record already carries one (written
// under a deployment that DID supply a uid), and a publish under a deployment that does
// not would commit a declaration this engine cannot protect with a second-hop floor. That
// is exactly the state the deployment class refuses at the other two write points.
func TestPublishIsRefusedWhenTheRecordedViaOutlivesTheDeploymentsUID(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seedBare(t, st)
	seedPausedWithVia(t, st, "sbx_via")

	// A deployment with NO second-hop uid, over a store whose record already carries a
	// via[] — the ordinary operational shape (the record is persistent, the uid is a
	// per-process deployment input, so a restart without the flag separates them with no
	// write in between).
	m := New(Deps{
		Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{},
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
		HealthTimeout: time.Second,
		Deployment:    network.Deployment{HostLocal: viaHostLocal},
	})

	_, _, _, err := m.AddPublish(ctx, "sbx_via", model.NetPublish{GuestPort: 8080, Proto: model.NetTCP})
	if err == nil {
		t.Fatal("publish was accepted while the recorded via[] cannot be pinned by this deployment " +
			"— committing it records a declaration the engine cannot protect with a second-hop floor, " +
			"which is exactly what the deployment class refuses at create and PATCH. AddPublish " +
			"called network.Validate directly and so skipped ValidateDeployment")
	}
	if !IsDeclarationInvalid(err) {
		t.Errorf("publish's refusal is not the 400-shaped declaration refusal: %v", err)
	}
	if c, _ := network.ClassOf(err); c != network.ClassViaWithoutSecondHopUID {
		t.Errorf("publish refused with class %q, want %q — see the same note on create: without the "+
			"class pinned, a ClassViaEndpointNotHostLocal refusal on the fixture endpoint satisfies IsDeclarationInvalid "+
			"and the deployment check under test is never exercised", c, network.ClassViaWithoutSecondHopUID)
	}
}
