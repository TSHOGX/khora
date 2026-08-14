package khora

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The §6.1 sandbox verbs.
//
// There is no WaitUntilRunning, no RetryOnUnavailable, and no CreateAndStart here.
// Each would be an INVENTED VERB (ADR-0008 rule 2) — a promise about time that the
// engine never made — and inventing one inside the client is worse than leaving it to
// the caller, because the caller cannot see the polling interval it got.
//
// A caller who wants to wait has two honest options and should pick deliberately:
// poll Get until the state satisfies them, or subscribe to Events and react to
// `started` / `auto-paused` / `died`. The second scales; the first is simpler. That
// choice is the caller's to make, which is why this package does not make it.

// Duration renders d as the duration string §6.1 accepts ("30m", "24h"). Provided so
// a caller does not hand-format one and get the unit wrong.
func Duration(d time.Duration) string { return durationString(d) }

// Create creates a sandbox and returns its ID.
//
// Create returns when the RECORD exists, which is not the same as the sandbox being
// usable — §4.1 has it pass through `creating`. The engine reports its own progress
// through state and events; this method reports what the request returned.
func (c *Client) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes", req, &out)
	return out, err
}

// Get reads one sandbox.
func (c *Client) Get(ctx context.Context, sandboxID string) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+sandboxID, nil, &out)
	return out, err
}

// List lists sandboxes. The entries carry the identical shape Get returns, so a caller
// filtering on state or death cause needs no follow-up request per ID.
//
// Note the one deliberate omission: UsedByte is absent on list entries. Reading it
// costs one storage-backend round trip per sandbox, so the engine does not pay it N
// times for a list — call Get for the sandbox whose usage you need.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &out)
	return out, err
}

// Patch rewrites parts of a sandbox's binding. Only the non-nil fields are sent — see
// PatchRequest for why every field is a pointer.
func (c *Client) Patch(ctx context.Context, sandboxID string, req PatchRequest) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, http.MethodPatch, "/v1/sandboxes/"+sandboxID, req, &out)
	return out, err
}

// Pause freezes a sandbox. The engine drains in-flight writes first, then freezes; the
// returned State is read back after the verb.
func (c *Client) Pause(ctx context.Context, sandboxID string) (VerbResult, error) {
	var out VerbResult
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/pause", nil, &out)
	return out, err
}

// Resume thaws a paused sandbox onto a fresh placement. Its host endpoint and any
// published port endpoints are NEW after a resume — a placement is not preserved
// across a pause, so a caller holding an old endpoint must re-read it.
func (c *Client) Resume(ctx context.Context, sandboxID string) (VerbResult, error) {
	var out VerbResult
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/resume", nil, &out)
	return out, err
}

// Destroy destroys a sandbox and its record.
//
// IRREVERSIBLE, and the row goes with it: after this returns, Get is a 404 and the
// death cause is gone. A caller that needs to know WHY a sandbox ended reads it from
// the `destroyed` event's Cause (or from Get, BEFORE destroying) — there is no
// post-hoc query, because there is no row left to query.
func (c *Client) Destroy(ctx context.Context, sandboxID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+sandboxID, nil, nil)
}

// RecordActivity stamps the guest's idle clock and returns the reading taken AFTER the
// stamp.
//
// For work the engine cannot see. The idle clock advances on guest-side activity the
// engine observes — a process write, a file transfer — so a caller whose sandbox is
// busy in some way the engine has no window into (a long computation with no I/O
// through the engine's own paths) uses this to say so.
//
// THREE PROPERTIES that follow from the stamp happening in the guest:
//   - Running only. A paused sandbox is a 409 (IsConflict) — this is not a
//     resume-by-side-effect.
//   - A 503 (IsUnavailable) when the guest cannot be reached, exactly when a health
//     probe would also fail. The clock is guest-side; there is no host-side fallback,
//     by design, because a host timestamp would keep a wedged sandbox alive forever.
//   - NO PROTECTED PERIOD. It records one activity NOW. It is not a lease, not a
//     keepalive, and not a pause-inhibit — none of those exist, because each would
//     have the engine promise a duration. A caller that wants a sandbox held open
//     stamps on an interval shorter than its idle timeout, and owns that interval.
func (c *Client) RecordActivity(ctx context.Context, sandboxID string) (ActivityResult, error) {
	var out ActivityResult
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/activity", nil, &out)
	return out, err
}

// Templates lists the templates a sandbox can be created from.
func (c *Client) Templates(ctx context.Context) ([]Template, error) {
	var out []Template
	err := c.do(ctx, http.MethodGet, "/v1/templates", nil, &out)
	return out, err
}

// Promote turns a sandbox's current filesystem into a reusable Template and returns
// the Template's ID.
//
// The ID is MINTED BY THE ENGINE, not chosen by the caller — which is why this returns
// one rather than taking one. A caller-chosen name would be a second namespace for the
// engine to police, and the engine's IDs are the only names it guarantees.
func (c *Client) Promote(ctx context.Context, fromSandboxID string) (string, error) {
	req := struct {
		FromSandboxID string `json:"from_sandbox_id"`
	}{FromSandboxID: fromSandboxID}
	var out struct {
		TemplateID string `json:"template_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/templates", req, &out); err != nil {
		return "", err
	}
	return out.TemplateID, nil
}

// DeleteTemplate deletes a Template. A Template some sandbox was built from is a 409
// (IsConflict): its layers are still in use below those sandboxes.
func (c *Client) DeleteTemplate(ctx context.Context, templateID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/templates/"+templateID, nil, nil)
}

// EventsOptions selects a page of the event stream.
type EventsOptions struct {
	// After is the cursor: the last Seq already processed. Zero starts from the
	// beginning of the RETAINED window, which is not necessarily the beginning of time.
	After int64
	// Limit caps the page. Zero means the engine's default.
	Limit int
	// Wait turns the call into a long poll: if no event is newer than After, the engine
	// holds the request open for up to this long rather than answering with an empty
	// page. Zero returns immediately.
	//
	// This is the ONE place this client can save a caller a busy loop without inventing
	// a verb, because the waiting happens in the ENGINE — Wait maps onto §6.1's own
	// `wait=` parameter. A timeout is a normal, successful, EMPTY page, not an error:
	// "nothing happened" is an answer.
	Wait time.Duration
}

// Events reads a page of the §4.4-C outbox.
//
// PULL ONLY, and that is architectural: the engine tracks no subscribers, so nothing
// here registers anything. Two consequences a caller must handle rather than assume
// away:
//
//   - AT-LEAST-ONCE delivery. Dedup by Event.ID; the same event can appear twice.
//   - The window is FINITE. A cursor that falls below Earliest is a 410 (Gone), and the
//     answer is to restart from the EventPage.Earliest the error page reports — which is
//     why that field exists.
//
// There is no Subscribe(chan Event) here. A channel implies an ownership and a
// lifetime this package must not hold (ADR-0008 rule 3: no state that outlives a
// call), and the loop it would hide — cursor advance, dedup, backoff on 503, restart on
// 410 — is exactly the policy a caller has to be able to see and choose.
func (c *Client) Events(ctx context.Context, opt EventsOptions) (EventPage, error) {
	q := url.Values{}
	if opt.After > 0 {
		q.Set("after", strconv.FormatInt(opt.After, 10))
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	if opt.Wait > 0 {
		q.Set("wait", durationString(opt.Wait))
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out EventPage
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Health reports the engine's own readiness, as /readyz answers it.
//
// ABOUT THE ENGINE, NOT A SANDBOX. There is no per-sandbox health call in §6.1 on
// purpose: a sandbox's condition is its State plus its DeathCause, and a separate
// "is it healthy" question would invite a caller to build a verdict the engine already
// published — and to disagree with it.
func (c *Client) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET /readyz: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errorFrom(resp, http.MethodGet, "/readyz")
	}
	return nil
}
