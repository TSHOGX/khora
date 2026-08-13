package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TSHOGX/warren/internal/lifecycle"
	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/network"
	"github.com/TSHOGX/warren/internal/rootfs"
	"github.com/TSHOGX/warren/internal/store"
	"github.com/TSHOGX/warren/internal/transport"
)

// apiFixture assembles an Engine over a Memory store with no real providers —
// the Template GET/DELETE endpoints are pure store operations (§4.3: Template is
// not a state-machine object, so DeleteTemplate takes no placement / lock), so
// the runtime/rootfs seams are never touched by these routes.
//
// CAUTION for anyone extending this fixture: Runtime/Rootfs/Dial are nil, which
// the PATCH tests survive only because NodeCapacity is zero — that short-circuits
// the admission gate before it would call Rootfs.UncappedBytes. Set a capacity
// here and these tests nil-panic; build a mediator with fakes instead (see
// dataplane_barrier_test.go's barrierFixture).
func apiFixture(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	mem := store.NewMemory()
	med := lifecycle.New(lifecycle.Deps{Store: mem})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, mem
}

func seedTemplate(t *testing.T, mem *store.Memory, id, parent string, builtin bool) {
	t.Helper()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: model.TemplateID(id), Parent: model.TemplateID(parent),
		Builtin: builtin, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template %q: %v", id, err)
	}
}

func do(t *testing.T, method, url string) *http.Response {
	t.Helper()
	return doBody(t, method, url, "")
}

func doBody(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// GET /v1/templates lists the rows with id/parent/builtin.
func TestAPIListTemplates(t *testing.T) {
	srv, mem := apiFixture(t)
	seedTemplate(t, mem, "bare", "", true)
	seedTemplate(t, mem, "tmpl_child", "bare", false)

	resp := do(t, http.MethodGet, srv.URL+"/v1/templates")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET templates = %d; want 200", resp.StatusCode)
	}
	var got []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d templates; want 2", len(got))
	}
}

// DELETE /v1/templates/{id}: unreferenced → 204; referenced by a sandbox → 409
// (§4.3 referenced-means-undeletable); after the sandbox is gone → 204; missing
// → 404.
func TestAPIDeleteTemplateReferenceProtection(t *testing.T) {
	srv, mem := apiFixture(t)
	ctx := context.Background()
	seedTemplate(t, mem, "bare", "", true)
	seedTemplate(t, mem, "unused", "", true)

	// unreferenced → 204.
	resp := do(t, http.MethodDelete, srv.URL+"/v1/templates/unused")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete unreferenced = %d; want 204", resp.StatusCode)
	}

	// referenced by a sandbox → 409 template in use.
	if err := mem.CreateSandbox(ctx, model.Sandbox{
		ID: "sbx_a", Template: "bare", Overlay: "ov_a", State: model.Running,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/bare")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete referenced = %d; want 409", resp.StatusCode)
	}

	// remove the referrer → now deletable (204).
	if err := mem.DeleteSandbox(ctx, "sbx_a", nil); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/bare")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after referrer gone = %d; want 204", resp.StatusCode)
	}

	// missing → 404.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/templates/ghost")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing = %d; want 404", resp.StatusCode)
	}
}

// POST /v1/templates {from_sandbox_id} is the Promote endpoint (§6.1). An empty
// body → 400 (from_sandbox_id required); a promote of an unknown sandbox → 404
// (ErrNotFound). The full freeze path is exercised on the substrate (needs a
// real Overlay upper).
func TestAPIPromoteValidation(t *testing.T) {
	srv, _ := apiFixture(t)

	// missing from_sandbox_id → 400.
	resp := do(t, http.MethodPost, srv.URL+"/v1/templates")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST templates (empty) = %d; want 400", resp.StatusCode)
	}

	// unknown sandbox → 404.
	resp = doBody(t, http.MethodPost, srv.URL+"/v1/templates", `{"from_sandbox_id":"ghost"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST templates (unknown sandbox) = %d; want 404", resp.StatusCode)
	}
}

// §6.1 PATCH /v1/sandboxes/{id} — the Binding write point. These cover the WIRE
// contract (routing, body shape, status codes, applies_at); the semantics live in
// the mediator's own tests.
func TestPatchSandboxBinding(t *testing.T) {
	srv, mem := apiFixture(t)
	seedTemplate(t, mem, "bare", "", true)
	now := time.Now()
	if err := mem.CreateSandbox(context.Background(), model.Sandbox{
		ID: "sbx_p", Template: "bare", Overlay: "ov_p", State: model.Paused,
		Limits:    model.Limits{MemoryByte: 128 << 20, DiskByte: 64 << 20},
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}

	patch := func(t *testing.T, id, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/sandboxes/"+id, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	t.Run("a cap edit reports next-resume", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"memory_byte":536870912}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		var out patchResp
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		// Caps materialize at placement, so the API states the deferral rather than
		// leaving the caller to infer it (§3.4).
		if out.AppliesAt != "next-resume" {
			t.Fatalf("applies_at = %q; want next-resume", out.AppliesAt)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Limits.MemoryByte != 536870912 {
			t.Fatalf("memory not persisted: %d", rec.Limits.MemoryByte)
		}
		// omitted field untouched
		if rec.Limits.DiskByte != 64<<20 {
			t.Fatalf("disk changed to %d; want untouched", rec.Limits.DiskByte)
		}
	})

	t.Run("a TTL edit reports now and accepts duration strings", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"retention":"48h"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		var out patchResp
		_ = json.Unmarshal([]byte(body), &out)
		if out.AppliesAt != "now" {
			t.Fatalf("applies_at = %q; want now (the reaper re-reads the record)", out.AppliesAt)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Retention != 48*time.Hour {
			t.Fatalf("retention = %s; want 48h", rec.Retention)
		}
	})

	t.Run("a zero cap is accepted as unlimited, not treated as omitted", func(t *testing.T) {
		resp, body := patch(t, "sbx_p", `{"disk_byte":0}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
		}
		rec, _ := mem.GetSandbox(context.Background(), "sbx_p")
		if rec.Limits.DiskByte != 0 {
			t.Fatalf("disk = %d; want 0 — an explicit zero must be applied, not read as omitted", rec.Limits.DiskByte)
		}
	})

	t.Run("negative and malformed inputs are 400", func(t *testing.T) {
		for _, body := range []string{`{"memory_byte":-1}`, `{"retention":"not-a-duration"}`, `{"retention":"-5m"}`} {
			resp, got := patch(t, "sbx_p", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("body %s → status %d (%s); want 400", body, resp.StatusCode, got)
			}
		}
	})

	t.Run("unknown sandbox is 404", func(t *testing.T) {
		resp, body := patch(t, "sbx_nope", `{"memory_byte":1}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d (%s); want 404", resp.StatusCode, body)
		}
	})
}

// The two admission verdicts must NOT collapse into one status: a missing cap on a
// metered axis is the caller's request being invalid for this deployment (400),
// while a full node is a capacity condition (507). Reporting "full" for a missing
// cap would send the caller off to wait for capacity that is not the problem.
func TestAdmissionVerdictsMapToDistinctStatuses(t *testing.T) {
	t.Run("missing cap on a metered axis → 400", func(t *testing.T) {
		mem := store.NewMemory()
		med := lifecycle.New(lifecycle.Deps{
			Store:        mem,
			NodeCapacity: lifecycle.NodeCapacity{MemoryByte: 4 << 30},
		})
		eng := &Engine{Mediator: med, Store: mem}
		srv := httptest.NewServer(eng.HTTPHandler())
		t.Cleanup(srv.Close)
		seedTemplate(t, mem, "bare", "", true)

		resp, err := http.Post(srv.URL+"/v1/sandboxes", "application/json",
			strings.NewReader(`{"template_id":"bare"}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d (%s); want 400 — nothing is full, the request is invalid here (507 would misdirect the caller)", resp.StatusCode, b)
		}
	})

	// The OTHER verdict: a genuinely full node → 507. Its own subtest because the mapping at
	// the API edge is one line and a wrong code there passes the entire suite.
	//
	// Asserted on statusCodeFor DIRECTLY rather than through a live server, which is the
	// honest unit: driving it end-to-end would need a full-node condition, i.e. a
	// runtime+rootfs provider stack or a quota-capable backend — neither being the property
	// under test, and both covered in internal/lifecycle's admission tests.
	t.Run("a full node → 507", func(t *testing.T) {
		if got := statusCodeFor(lifecycle.ErrInsufficientCapacity); got != http.StatusInsufficientStorage {
			t.Fatalf("ErrInsufficientCapacity → %d; want 507 — the request is VALID and the node is out of room (§6.2: 507 means capacity; 409 would imply a retryable state conflict, and 400 would blame a request that is fine)", got)
		}
		// The pair must stay DISTINCT. Both verdicts arise in the same admission gate, so
		// a refactor that funnels them through one arm is easy to write and silent.
		if statusCodeFor(lifecycle.ErrCapRequired) == statusCodeFor(lifecycle.ErrInsufficientCapacity) {
			t.Fatalf("ErrCapRequired and ErrInsufficientCapacity map to the SAME status (%d) — a missing cap is the caller's request being invalid for this deployment (400), a full node is a capacity condition (507)", statusCodeFor(lifecycle.ErrCapRequired))
		}
	})

	// The third verdict on the same axis, and the one whose default would be worst: a `0 → N`
	// disk cap PATCH is unmaterializable (§3.4), so unmapped it falls to the default arm as
	// 500 — telling the caller to retry something that can never succeed.
	t.Run("an unmaterializable 0→N disk cap → 400", func(t *testing.T) {
		if got := statusCodeFor(lifecycle.ErrUncappedDiskNotCappable); got != http.StatusBadRequest {
			t.Fatalf("ErrUncappedDiskNotCappable → %d; want 400 — the request is well-formed but the promise cannot be materialized (§3.4). NOT 507 (nothing is full) and NOT 409 (no retry or state change makes it legal; the remedy is a new sandbox)", got)
		}
	})

	// The fourth verdict on this axis, and its SPLIT is the property under test. A disk
	// cap at or below the Overlay unit's usage wedges the storage unit irreversibly
	// (§3.4 / rootfs.ErrCapBelowUsage), and the engine refuses it in TWO places that must
	// not answer the same code: the write point CARRIES `disk_byte` (400 — fix the value
	// you sent), while resume/adopt do NOT (409 — the remedy is state elsewhere). This is
	// the same asymmetry the via[] pair above rests on, and asserting both together is
	// what keeps a later refactor from funnelling them into one arm.
	t.Run("a disk cap below usage → 400 at the write point, 409 at placement", func(t *testing.T) {
		if got := statusCodeFor(lifecycle.ErrDiskCapBelowUsage); got != http.StatusBadRequest {
			t.Fatalf("ErrDiskCapBelowUsage → %d; want 400 — the request is well-formed but cannot be honored (§3.4 strict inequality). NOT 507 (nothing is full) and NOT 409 (no retry makes it legal; the caller must pick a larger size, or 0)", got)
		}
		wrapped := fmt.Errorf("lifecycle: resume: open overlay: %w", rootfs.ErrCapBelowUsage)
		if got := statusCodeFor(wrapped); got != http.StatusConflict {
			t.Fatalf("rootfs.ErrCapBelowUsage → %d; want 409 — the stored cap and the unit's current usage conflict, and the remedy (PATCH disk_byte above usage, or to 0, then resume) changes state elsewhere. NOT 500: nothing failed, and §5.B refused rather than wedging the unit. NOT 400: resume carries no cap to fix", got)
		}
		if statusCodeFor(lifecycle.ErrDiskCapBelowUsage) == statusCodeFor(wrapped) {
			t.Fatal("the write-point and placement-side refusals map to the SAME status — they must not: one names a field the caller sent, the other does not")
		}
	})

	// The PLACEMENT-side network refusal, which fell to the default arm as 500. The asymmetry
	// makes it unmistakable: the same operator condition (restart with no `--via-uid`) answers
	// 400 on create and answered 500 here.
	t.Run("a recorded via[] this deployment cannot pin → 409", func(t *testing.T) {
		wrapped := fmt.Errorf("lifecycle: cold start: %w", network.ErrViaUnenforceableAtPlacement)
		if got := statusCodeFor(wrapped); got != http.StatusConflict {
			t.Fatalf("ErrViaUnenforceableAtPlacement → %d; want 409 — the record and the running "+
				"deployment conflict, and both documented remedies change state elsewhere (restart "+
				"with the uid, or PATCH the via[] away). NOT 500: nothing failed, and retrying "+
				"cannot help. NOT 400: the request carries no declaration to fix", got)
		}
		// It must stay distinguishable from the WRITE-point refusal of the same
		// condition, which is a 400 over a request that did carry the declaration.
		if statusCodeFor(wrapped) == statusCodeFor(network.ErrDeclarationInvalid) {
			t.Error("the placement-side and write-point via[] refusals map to the same status — " +
				"their remedies differ (an operator's vs the caller's), which is why the sentinel " +
				"is separate (§6.2)")
		}
	})
}

// §6.2's general rule: a wrong method on ANY collection/object route is 405, not 404 —
// the route exists, the method does not apply to it. Asserted across ALL the sandbox
// sub-routes because the two network ones carried explicit arms while the three others
// fell into `default` as 404, so the rule held on half the router only.
func TestAWrongMethodOnEverySandboxSubRouteIs405(t *testing.T) {
	mem := store.NewMemory()
	med := lifecycle.New(lifecycle.Deps{Store: mem})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)

	// A method that is wrong for each route, chosen so none is the route's real verb.
	for _, tc := range []struct{ method, sub string }{
		{http.MethodDelete, "network"},
		{http.MethodGet, "publish"},
		{http.MethodDelete, "files"},
		{http.MethodGet, "pause"},
		{http.MethodGet, "resume"},
	} {
		t.Run(tc.method+" "+tc.sub, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, srv.URL+"/v1/sandboxes/sbx_x/"+tc.sub, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s /v1/sandboxes/{id}/%s → %d; want 405 (§6.2: a wrong method on any "+
					"collection/object route is 405, never 404 — 404 says the route does not "+
					"exist, which sends the caller to check their URL)", tc.method, tc.sub,
					resp.StatusCode)
			}
		})
	}
}

// --- GET /v1/events (§6.1 / §4.4-C event-stream replay, pull only) -----------

// seedSandboxEvents commits n lifecycle transitions on one sandbox so the outbox
// has ordered rows to replay.
func seedSandboxEvents(t *testing.T, mem *store.Memory, n int) {
	t.Helper()
	ctx := context.Background()
	seedTemplate(t, mem, "bare", "", true)
	if err := mem.CreateSandbox(ctx, model.Sandbox{
		ID: "sb", Template: "bare", State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	state := model.Creating
	for i := 0; i < n; i++ {
		next, kind := model.Running, model.EventResumed
		if state == model.Running {
			next, kind = model.Paused, model.EventPaused
		}
		if err := mem.CompareAndSetState(ctx, "sb", state, next, &store.Event{
			ID: fmt.Sprintf("evt_%d", i), Sandbox: "sb", Kind: kind, State: next, OccurredAt: time.Now(),
		}, nil); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
		state = next
	}
}

func getEvents(t *testing.T, srv *httptest.Server, query string) eventsResp {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+"/v1/events"+query)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/events%s = %d: %s", query, resp.StatusCode, b)
	}
	var out eventsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestAPIEventsCarryCauseOnlyWhereAKindHasTwoRoutes pins the wire shape of §4.4-C's cause:
// present on `destroyed` (two routes, and the record is GONE so the payload is the only
// carrier), absent on a kind whose identity already says why. The omission half is half the
// contract — `omitempty` is what lets a subscriber read a present cause as a signal.
func TestAPIEventsCarryCauseOnlyWhereAKindHasTwoRoutes(t *testing.T) {
	srv, mem := apiFixture(t)
	ctx := context.Background()
	seedTemplate(t, mem, "bare", "", true)
	if err := mem.CreateSandbox(ctx, model.Sandbox{
		ID: "sb", Template: "bare", State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, &store.Event{ID: "evt_c", Sandbox: "sb", Kind: model.EventCreated, State: model.Creating, OccurredAt: time.Now()}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := mem.DeleteSandbox(ctx, "sb", &store.Event{
		ID: "evt_d", Sandbox: "sb", Kind: model.EventDestroyed, State: model.Destroyed,
		Cause: model.CauseRetentionExpired, OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	got := getEvents(t, srv, "")
	if len(got.Events) != 2 {
		t.Fatalf("got %d events; want 2: %+v", len(got.Events), got.Events)
	}
	if got.Events[0].Cause != "" {
		t.Errorf("`created` carried cause %q; want none — the kind is already the whole why, and a "+
			"cause on every event makes its presence meaningless", got.Events[0].Cause)
	}
	if got.Events[1].Cause != string(model.CauseRetentionExpired) {
		t.Errorf("`destroyed` cause=%q; want %q", got.Events[1].Cause, model.CauseRetentionExpired)
	}

	// The record is gone, which is exactly why the cause had to ride the event: this 404
	// is the contract, so a subscriber cannot ask afterwards.
	resp := do(t, http.MethodGet, srv.URL+"/v1/sandboxes/sb")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a destroyed sandbox = %d; want 404 (the premise of carrying the cause on "+
			"the event)", resp.StatusCode)
	}

	// And absent must be OMITTED from the JSON, not serialized as "cause":"".
	rawResp := do(t, http.MethodGet, srv.URL+"/v1/events")
	raw, _ := io.ReadAll(rawResp.Body)
	rawResp.Body.Close()
	if strings.Contains(string(raw), `"cause":""`) {
		t.Errorf("an absent cause was serialized as an empty string: %s", raw)
	}
}

// TestAPIEventsWaitReturnsWhenAnEventArrives: `wait` collapses a subscriber's detection
// latency from its poll interval to the event itself. The assertion is that the response
// arrives WELL BEFORE the requested wait — a handler that ignored the parameter and one
// that slept the whole duration would both return an event, so only the timing separates
// the working case from the two broken ones.
func TestAPIEventsWaitReturnsWhenAnEventArrives(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 1)
	head := getEvents(t, srv, "")
	cursor := head.Events[len(head.Events)-1].Seq

	// Commit one transition shortly after the request is in its wait.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = mem.CompareAndSetState(context.Background(), "sb", model.Running, model.Paused,
			&store.Event{ID: "evt_late", Sandbox: "sb", Kind: model.EventPaused, State: model.Paused, OccurredAt: time.Now()}, nil)
	}()

	start := time.Now()
	got := getEvents(t, srv, fmt.Sprintf("?after=%d&wait=10s", cursor))
	elapsed := time.Since(start)

	if len(got.Events) != 1 || got.Events[0].ID != "evt_late" {
		t.Fatalf("wait returned %+v; want the one event committed during the hold", got.Events)
	}
	if elapsed > 5*time.Second {
		t.Errorf("wait took %v to deliver an event committed at ~150ms — it is sleeping out the "+
			"whole duration instead of returning on arrival", elapsed)
	}
	// …and it did hold rather than returning the empty page instantly.
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, before the event existed — `wait` was ignored", elapsed)
	}
}

// An expired wait is the SAME empty 200 an immediate call returns, and the cursor is
// unmoved. This is the property that makes the parameter additive: a caller that sets it
// cannot end up in a state a caller that omits it could not reach.
func TestAPIEventsWaitExpiresAsAnOrdinaryEmptyPage(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 2)
	head := getEvents(t, srv, "")
	cursor := head.Events[len(head.Events)-1].Seq

	start := time.Now()
	waited := getEvents(t, srv, fmt.Sprintf("?after=%d&wait=400ms", cursor))
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("returned after %v; want a hold of about 400ms", elapsed)
	}
	immediate := getEvents(t, srv, fmt.Sprintf("?after=%d", cursor))

	if len(waited.Events) != 0 {
		t.Fatalf("expired wait returned %d events; want none", len(waited.Events))
	}
	if waited.Earliest != immediate.Earliest || waited.Latest != immediate.Latest {
		t.Errorf("expired wait window %d/%d differs from the immediate call's %d/%d — the bounds "+
			"must come from the same read as the page",
			waited.Earliest, waited.Latest, immediate.Earliest, immediate.Latest)
	}
}

// A non-empty first read must NOT wait: waiting after the answer is already in hand adds
// latency to the common case.
func TestAPIEventsWaitDoesNotDelayANonEmptyPage(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 3)

	start := time.Now()
	got := getEvents(t, srv, "?wait=10s")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a page with %d events took %v — the wait ran despite having results",
			len(got.Events), elapsed)
	}
	if len(got.Events) != 3 {
		t.Fatalf("got %d events; want 3", len(got.Events))
	}
}

// `wait` is validated and CLAMPED, the same treatment `limit` gets: a caller-controlled
// parameter given only a default is given nothing, and a clamp keeps a caller who asks
// for too much working instead of breaking them for asking.
func TestAPIEventsWaitValidationAndCeiling(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 1)

	for _, bad := range []string{"?wait=abc", "?wait=-5s", "?wait=10"} {
		resp := do(t, http.MethodGet, srv.URL+"/v1/events"+bad)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /v1/events%s = %d (%s); want 400", bad, resp.StatusCode, body)
		}
	}

	// Above the ceiling → accepted and clamped, never refused. Asserted on a page that
	// has results, so the clamp is observable without waiting out the ceiling.
	got := getEvents(t, srv, "?wait=48h")
	if len(got.Events) == 0 {
		t.Error("a wait above the ceiling was not served")
	}
}

// The cursor contract: page forward with `after`, receive each event exactly
// once, in sequence order, with the window bounds attached.
func TestAPIEventsCursorPaging(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 5)

	all := getEvents(t, srv, "")
	if len(all.Events) != 5 {
		t.Fatalf("got %d events; want 5", len(all.Events))
	}
	if all.Earliest != all.Events[0].Seq || all.Latest != all.Events[4].Seq {
		t.Fatalf("window bounds %d/%d do not match the returned range %d..%d",
			all.Earliest, all.Latest, all.Events[0].Seq, all.Events[4].Seq)
	}
	var prev int64
	for i, e := range all.Events {
		if e.Seq <= prev {
			t.Fatalf("event %d has seq %d, not increasing after %d", i, e.Seq, prev)
		}
		prev = e.Seq
		if e.ID != fmt.Sprintf("evt_%d", i) {
			t.Fatalf("position %d holds %s; want evt_%d", i, e.ID, i)
		}
		if e.SandboxID != "sb" || e.Kind == "" || e.State == "" || e.OccurredAt == "" {
			t.Fatalf("event %d is missing fields: %+v", i, e)
		}
	}

	// page with limit=2 from a cursor: no duplicate at the boundary, no gap.
	var ids []string
	cursor := int64(0)
	for i := 0; i < 10; i++ {
		pg := getEvents(t, srv, fmt.Sprintf("?after=%d&limit=2", cursor))
		if len(pg.Events) == 0 {
			break
		}
		for _, e := range pg.Events {
			if e.Seq <= cursor {
				t.Fatalf("after=%d returned seq %d — an already-served event came back", cursor, e.Seq)
			}
			ids = append(ids, e.ID)
			cursor = e.Seq
		}
	}
	if len(ids) != 5 {
		t.Fatalf("paging yielded %d events (%v); want 5 exactly once", len(ids), ids)
	}

	// a cursor at the head returns an empty page, not an error.
	tail := getEvents(t, srv, fmt.Sprintf("?after=%d", all.Latest))
	if len(tail.Events) != 0 {
		t.Fatalf("cursor at latest returned %d events; want 0", len(tail.Events))
	}
	if tail.Latest != all.Latest {
		t.Fatalf("empty page reported latest=%d; want %d (bounds must be reported regardless)", tail.Latest, all.Latest)
	}
}

// The `limit` HARD CAP. Not a default: the caller controls the parameter, so a
// default alone protects nothing. With no trimming the window is ALL history, so
// after=0 pages through everything — which is why this cap is mandatory.
func TestAPIEventsLimitIsHardCapped(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, eventsMaxLimit+50)

	// asking for far more than the cap yields at most the cap, not everything.
	got := getEvents(t, srv, "?limit=100000")
	if len(got.Events) != eventsMaxLimit {
		t.Fatalf("limit=100000 returned %d events; want the hard cap %d", len(got.Events), eventsMaxLimit)
	}
	// the default applies when limit is omitted.
	def := getEvents(t, srv, "")
	if len(def.Events) != eventsDefaultLimit {
		t.Fatalf("omitted limit returned %d events; want the default %d", len(def.Events), eventsDefaultLimit)
	}
	// a limit under the cap is honored as given.
	small := getEvents(t, srv, "?limit=7")
	if len(small.Events) != 7 {
		t.Fatalf("limit=7 returned %d events", len(small.Events))
	}
}

// Malformed cursor / limit are 400s, not 500s and not silent coercions.
func TestAPIEventsRejectsBadParams(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 2)

	for _, q := range []string{"?after=abc", "?after=-1", "?limit=abc", "?limit=0", "?limit=-5"} {
		resp := do(t, http.MethodGet, srv.URL+"/v1/events"+q)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET /v1/events%s = %d (%s); want 400", q, resp.StatusCode, body)
		}
	}
	// non-GET is 405.
	resp := do(t, http.MethodPost, srv.URL+"/v1/events")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/events = %d; want 405", resp.StatusCode)
	}
}

// An empty outbox is an empty window, not an error — and specifically not a 410,
// which is unreachable while nothing is trimmed.
func TestAPIEventsEmptyOutbox(t *testing.T) {
	srv, _ := apiFixture(t)
	got := getEvents(t, srv, "?after=0")
	if len(got.Events) != 0 || got.Earliest != 0 || got.Latest != 0 {
		t.Fatalf("empty outbox: %+v; want no events and 0/0 bounds", got)
	}
}

// Replay is over the retained window, NOT over delivery state: an event already
// drained by the in-process publisher must still be replayable, because whether
// the local sink has seen it is not a fact an external subscriber depends on.
func TestAPIEventsReplayIgnoresDeliveryState(t *testing.T) {
	srv, mem := apiFixture(t)
	seedSandboxEvents(t, mem, 3)

	p := store.NewPublisher(mem, store.NewDedupSink(func(store.Event) {}))
	if n, err := p.PublishOnce(context.Background()); err != nil || n != 3 {
		t.Fatalf("drain n=%d err=%v; want 3,nil", n, err)
	}
	if un, _ := mem.UnpublishedEvents(context.Background(), 100 /* any bound; this asserts on ZERO events */); len(un) != 0 {
		t.Fatalf("%d rows still unpublished after the drain", len(un))
	}
	got := getEvents(t, srv, "?after=0")
	if len(got.Events) != 3 {
		t.Fatalf("replay after a full drain returned %d events; want all 3", len(got.Events))
	}
}

// --- death_cause on the §6.1 read path (§4.3) --------------------------------

// The fact the §4.3 recovery decision rests on must be readable WITHOUT first
// attempting the resume that would be refused.
func TestAPIGetSandboxExposesDeathCause(t *testing.T) {
	srv, mem := apiFixture(t)
	ctx := context.Background()
	seedTemplate(t, mem, "bare", "", true)
	if err := mem.CreateSandbox(ctx, model.Sandbox{
		ID: "sb", Template: "bare", State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	// healthy sandbox: death_cause is ABSENT, not "" — absence means "not observed
	// dead", so there is no falsy-but-present field to mistake for a liveness test.
	resp := do(t, http.MethodGet, srv.URL+"/v1/sandboxes/sb")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "death_cause") {
		t.Fatalf("healthy sandbox response carries a death_cause key: %s", raw)
	}

	// crash-paused with a resource-exceeded cause (the §4.3 do-NOT-resume case).
	cause := model.CauseResourceExceeded
	if err := mem.CompareAndSetState(ctx, "sb", model.Creating, model.Paused, nil, &cause); err != nil {
		t.Fatalf("demote: %v", err)
	}
	resp = do(t, http.MethodGet, srv.URL+"/v1/sandboxes/sb")
	var got sandboxResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if got.State != string(model.Paused) {
		t.Fatalf("state = %q; want paused", got.State)
	}
	if got.DeathCause != string(model.CauseResourceExceeded) {
		t.Fatalf("death_cause = %q; want %q — the caller cannot make the §4.3 decision without it",
			got.DeathCause, model.CauseResourceExceeded)
	}

	// List carries the same field, so deciding across several sandboxes does not
	// need N+1 requests.
	resp = do(t, http.MethodGet, srv.URL+"/v1/sandboxes")
	var list []sandboxResp
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0].DeathCause != string(model.CauseResourceExceeded) {
		t.Fatalf("list = %+v; want one entry carrying the death cause", list)
	}

	// A successful-resume clear is reflected on the read path (no stale cause).
	none := model.CauseNone
	if err := mem.CompareAndSetState(ctx, "sb", model.Paused, model.Running, nil, &none); err != nil {
		t.Fatalf("clear: %v", err)
	}
	resp = do(t, http.MethodGet, srv.URL+"/v1/sandboxes/sb")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "death_cause") {
		t.Fatalf("cleared cause still present on the read path: %s", raw)
	}
}

// --- §6.2 on the REST bypass (the other half of the funnel) -------------------

// placementLeakingClient is a transport.Client whose bypass calls fail the way the REAL inner
// client fails: transport/client.go returns `hc.Do`'s error verbatim, and that client dials the
// sandbox's NETNS VETH IP, so the message carries `10.x.y.z:port`. The RPC half already
// sanitizes this (proxyErr) — the REST bypass was the arm that did not.
//
// It also leaks a HOST overlay path, since the guest's own diagnostic travels back through
// bypassErr and can name a location inside the merged rootfs.
type placementLeakingClient struct {
	*barrierFakeClient
	endpoint string // the veth IP:port that must not reach the caller
	hostPath string // an overlay path that must not reach the caller either
}

func (c *placementLeakingClient) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("Get \"http://%s/files?path=%%2Fetc%%2Fx\": dial tcp %s: connect: connection refused",
		c.endpoint, c.endpoint)
}

func (c *placementLeakingClient) WriteStream(context.Context, string, io.Reader) (int64, error) {
	return 0, fmt.Errorf("upload /etc/x: open %s/etc/x: permission denied", c.hostPath)
}

// TestFileBypassErrorsHideThePlacement: GET|PUT /v1/sandboxes/{id}/files refuse WITHOUT naming
// the guest endpoint or a host path, and still say what failed. This route is REST because it
// streams bytes, which puts it outside proxyErr's reach — it has its own status mapping
// (bypassCode), so the placement-hiding discipline is re-established here rather than inherited.
func TestFileBypassErrorsHideThePlacement(t *testing.T) {
	const veth = "10.88.0.7:9000"
	const overlay = "/srv/state/overlays/ov_x/merged"
	cl := &placementLeakingClient{
		barrierFakeClient: &barrierFakeClient{},
		endpoint:          veth,
		hostPath:          overlay,
	}
	// Assembled here rather than through barrierFixture because that one hard-wires its
	// own client, and the leaking one IS the input under test.
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:         mem,
		Runtime:       barrierFakeRuntime{},
		Rootfs:        barrierFakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return cl },
		HealthTimeout: 2 * time.Second,
		Logf:          func(string, ...any) {},
	})
	sid, err := med.Create(context.Background(), lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)

	base := srv.URL + "/v1/sandboxes/" + string(sid) + "/files?path=/etc/x"
	for _, tc := range []struct{ name, method string }{
		{"download", http.MethodGet},
		{"upload", http.MethodPut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, tc.method, base, "payload")
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s succeeded against a failing client", tc.name)
			}
			for _, leak := range []string{"10.88.0.7", veth, overlay, "/srv"} {
				if strings.Contains(string(body), leak) {
					t.Errorf("%s leaks %q — placement and host paths stay inside the engine (§6.2 / D5.1): %s",
						tc.name, leak, strings.TrimSpace(string(body)))
				}
			}
			// The caller's OWN guest path survives: it is what says which file failed, and
			// they typed it.
			if !strings.Contains(string(body), "/etc/x") {
				t.Errorf("%s does not name the path the caller asked for: %s", tc.name, strings.TrimSpace(string(body)))
			}
		})
	}
}

// usageRootfs is barrierFakeRootfs with a controllable Usage: a value, or a failure.
type usageRootfs struct {
	barrierFakeRootfs
	used int64
	err  error
}

func (u usageRootfs) Usage(context.Context, string) (int64, int64, error) {
	if u.err != nil {
		return 0, 0, u.err
	}
	return u.used, 0, nil
}

// TestSandboxReadSurfaceReportsUsedByte pins §6.1's `used_byte` on the SANDBOX single read,
// with the three properties that make it usable rather than merely present. It is on the read
// surface because PATCH disk_byte refuses a cap not STRICTLY greater than usage (§3.4), so a
// caller needs the number — its only other route being that 400's message text.
func TestSandboxReadSurfaceReportsUsedByte(t *testing.T) {
	seedOne := func(t *testing.T, rp rootfs.Provider, state model.Lifecycle) (*httptest.Server, string) {
		t.Helper()
		mem := store.NewMemory()
		seedTemplate(t, mem, "bare", "", true)
		med := lifecycle.New(lifecycle.Deps{Store: mem, Rootfs: rp, Logf: func(string, ...any) {}})
		eng := &Engine{Mediator: med, Store: mem}
		srv := httptest.NewServer(eng.HTTPHandler())
		t.Cleanup(srv.Close)
		if err := mem.CreateSandbox(context.Background(), model.Sandbox{
			ID: "sbx_u", Template: "bare", Overlay: "ov_u", State: state,
		}, nil); err != nil {
			t.Fatalf("seed sandbox: %v", err)
		}
		return srv, "sbx_u"
	}

	t.Run("a readable usage is reported, and a genuine 0 is PRESENT", func(t *testing.T) {
		// 0 is the COMMON case here, not an edge one: a fresh uncapped sandbox's
		// upper+work walk is 0. With a value type plus omitempty it would serialize as
		// absent, which §6.1 defines to mean "unreadable" — so a caller would read "no
		// bytes written yet" as "the backend did not answer", or the reverse.
		srv, id := seedOne(t, usageRootfs{used: 0}, model.Paused)
		body := readRaw(t, srv, "/v1/sandboxes/"+id)
		if !strings.Contains(body, `"used_byte":0`) {
			t.Errorf("a readable usage of 0 is not reported as `\"used_byte\":0` — an EMPTY sandbox is then indistinguishable from one whose usage could not be READ. Body: %s", body)
		}
	})

	t.Run("present while paused, the only state a disk cap may be edited from", func(t *testing.T) {
		// Unlike host_endpoint, this key does NOT depend on a live placement: the unit is
		// readable at rest. Omitting it while paused would omit it from exactly the state
		// its consumer works in (§3.4: changing a cap on a running sandbox is a 409).
		srv, id := seedOne(t, usageRootfs{used: 4096}, model.Paused)
		body := readRaw(t, srv, "/v1/sandboxes/"+id)
		if !strings.Contains(body, `"used_byte":4096`) {
			t.Errorf("used_byte missing or wrong for a PAUSED sandbox — that is the one state a disk cap may be edited from, so a caller that cannot read usage there cannot choose a legal cap. Body: %s", body)
		}
	})

	t.Run("an unreadable usage OMITS the key and degrades rather than failing", func(t *testing.T) {
		srv, id := seedOne(t, usageRootfs{err: errors.New("quota row unreadable")}, model.Running)
		body := readRaw(t, srv, "/v1/sandboxes/"+id)
		if strings.Contains(body, `"used_byte"`) {
			t.Errorf("an unreadable usage still emitted used_byte — it must be omitted, or the caller reads a failure as \"this sandbox is empty\". Body: %s", body)
		}
		// The read still succeeds: the cap WRITE point fails closed, an inspect degrades.
		if !strings.Contains(body, `"state":"running"`) {
			t.Errorf("the response lost its other fields when usage was unreadable: %s", body)
		}
		// And the backend's error text must not ride out (§6.2 / edge_test).
		if strings.Contains(body, "quota row") {
			t.Errorf("the degraded response leaks backend error text: %s", body)
		}
	})

	t.Run("the LIST omits it, because usage costs one round trip per sandbox", func(t *testing.T) {
		srv, _ := seedOne(t, usageRootfs{used: 4096}, model.Paused)
		body := readRaw(t, srv, "/v1/sandboxes")
		if strings.Contains(body, `"used_byte"`) {
			t.Errorf("the list carries used_byte — one backend round trip multiplied by the sandbox count, which is the same reason the volume list omits it (§6.1). Body: %s", body)
		}
	})
}

// readRaw returns a GET's raw body: raw because the properties above are serialization
// ones, which a decoded struct would hide.
func readRaw(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+path)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestEventsGoneWhenCursorFallsBelowTheWindow exercises §6.1's 410, UNREACHABLE until trimming
// existed: with nothing ever deleted, `earliest` never advances, so "the cursor is below the
// window" could not be constructed.
//
// It protects the promise §4.4-C rests on — "losing one loses an invoice". The tempting
// implementation returns a short page, which lets a subscriber conclude it is caught up while
// having silently skipped everything trimmed.
func TestEventsGoneWhenCursorFallsBelowTheWindow(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	seedTemplate(t, mem, "bare", "", true)
	med := lifecycle.New(lifecycle.Deps{
		Store: mem,
		// A window that has already elapsed, so every delivered row qualifies at once.
		EventRetention: time.Nanosecond,
		Logf:           func(string, ...any) {},
	})
	eng := &Engine{Mediator: med, Store: mem}
	srv := httptest.NewServer(eng.HTTPHandler())
	t.Cleanup(srv.Close)

	if err := mem.CreateSandbox(ctx, model.Sandbox{ID: "sbx_e", Template: "bare", Overlay: "ov_e", State: model.Creating}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i, k := range []struct {
		from, to model.Lifecycle
		kind     model.EventKind
	}{
		{model.Creating, model.Running, "created"},
		{model.Running, model.Paused, "paused"},
		{model.Paused, model.Running, "resumed"},
		{model.Running, model.Paused, "paused"},
	} {
		e := &store.Event{ID: fmt.Sprintf("evt_%d", i), Sandbox: "sbx_e", Kind: k.kind, State: k.to, OccurredAt: time.Now()}
		if err := mem.CompareAndSetState(ctx, "sbx_e", k.from, k.to, e, nil); err != nil {
			t.Fatalf("transition %d: %v", i, err)
		}
	}
	// Deliver them all, since only DELIVERED rows are trimmable.
	un, _ := mem.UnpublishedEvents(ctx, 100)
	for _, e := range un {
		if err := mem.MarkPublished(ctx, e.ID); err != nil {
			t.Fatalf("mark: %v", err)
		}
	}

	if err := med.TrimOutbox(ctx); err != nil {
		t.Fatalf("trim: %v", err)
	}

	// The window has advanced. A cursor below it must be REFUSED, not served short.
	resp := do(t, http.MethodGet, srv.URL+"/v1/events?after=1")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("after=1 against a trimmed window = %d, want 410 — a short page here lets a subscriber conclude it is caught up while silently skipping every trimmed event, which is the one outcome §4.4-C's ordering guarantee exists to prevent. Body: %s", resp.StatusCode, body)
	}
	// And it must carry the window so the caller can restart deliberately.
	var out eventsResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode 410 body: %v (%s)", err, body)
	}
	if out.Earliest == 0 {
		t.Errorf("the 410 does not report `earliest`, so the caller has nothing to restart from: %s", body)
	}

	// after=0 must NEVER 410: it is contractually "from the start of the retained
	// window", i.e. an explicit request for whatever is retained. A bare
	// `after < earliest` test would break every cold-start cursor on a trimmed outbox.
	resp = do(t, http.MethodGet, srv.URL+"/v1/events?after=0")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after=0 against a trimmed window = %d, want 200 — 0 means 'from the start of the retained window', so refusing it breaks the documented way to begin reading", resp.StatusCode)
	}

	// A cursor INSIDE the window is served normally.
	resp = do(t, http.MethodGet, srv.URL+fmt.Sprintf("/v1/events?after=%d", out.Earliest))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a cursor at `earliest` = %d, want 200", resp.StatusCode)
	}

	// THE BOUNDARY, asserted separately from the two cases above: `after` is EXCLUSIVE (the
	// page is `seq > after`), so a cursor at `earliest-1` has consumed exactly the trimmed
	// prefix and nothing retained.
	//
	// 410-ing it is worse than a false alarm: the documented remedy is "restart from
	// `earliest`", and a caller that follows it skips the event AT `earliest` — MANUFACTURING
	// the gap this endpoint exists to prevent. `after=1` against `earliest=4` cannot catch it,
	// being two below the boundary where the correct and off-by-one predicates agree.
	resp = do(t, http.MethodGet, srv.URL+fmt.Sprintf("/v1/events?after=%d", out.Earliest-1))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a cursor at `earliest`-1 (=%d, with earliest=%d) = %d, want 200: `after` is exclusive, so this cursor has consumed precisely the trimmed rows and lost nothing. Refusing it tells a caught-up subscriber it lost data, and the remedy it documents then skips seq %d. Body: %s",
			out.Earliest-1, out.Earliest, resp.StatusCode, out.Earliest, body)
	}
	// …and it must actually serve the event at `earliest`, not merely 200 with an
	// empty page — that would be the silent short page under a different status.
	var page eventsResp
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode boundary page: %v (%s)", err, body)
	}
	if len(page.Events) == 0 || page.Events[0].Seq != out.Earliest {
		t.Errorf("a cursor at `earliest`-1 must serve seq %d first, got %+v — an empty 200 here is the silent gap wearing a success code", out.Earliest, page.Events)
	}
}
