package orchestrator

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

// Malformed query/body values are request errors, not backend failures.  The
// OpenAPI surface advertises these routes as 400-capable; an untyped validation
// error passed to statusFor otherwise falls through to 500.
func TestMalformedVolumeInputsAreBadRequest(t *testing.T) {
	srv, _, _ := volumeFixture(t)
	resp := doBody(t, http.MethodPost, srv.URL+"/v1/volumes", `{}`)
	v := decodeVolume(t, resp)
	base := srv.URL + "/v1/volumes/" + v.VolumeID

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "stat missing path", method: http.MethodGet, path: base + "/stat"},
		{name: "lstat missing path", method: http.MethodGet, path: base + "/lstat"},
		{name: "read malformed offset", method: http.MethodGet, path: base + "/read?path=x&offset=bad"},
		{name: "read negative offset", method: http.MethodGet, path: base + "/read?path=x&offset=-1"},
		{name: "read malformed length", method: http.MethodGet, path: base + "/read?path=x&length=bad"},
		{name: "write malformed offset", method: http.MethodPut, path: base + "/write?path=x&offset=bad", body: "x"},
		{name: "write negative offset", method: http.MethodPut, path: base + "/write?path=x&offset=-1", body: "x"},
		{name: "mkdir malformed mode", method: http.MethodPost, path: base + "/mkdir?path=x&mode=bad"},
		{name: "mkdir negative mode", method: http.MethodPost, path: base + "/mkdir?path=x&mode=-1"},
		{name: "remove missing path", method: http.MethodDelete, path: base + "/remove"},
		{name: "move malformed JSON", method: http.MethodPost, path: base + "/move", body: "{"},
		{name: "move missing fields", method: http.MethodPost, path: base + "/move", body: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doBody(t, tc.method, tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s = %d; want 400", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

func TestMalformedTelemetryQueryIsBadRequest(t *testing.T) {
	srv, mem := apiFixture(t)
	seedTemplate(t, mem, "bare", "", true)
	if err := mem.CreateSandbox(context.Background(), model.Sandbox{
		ID: "sbx_telemetry_inputs", Template: "bare", Overlay: "ov_telemetry_inputs",
		State: model.Paused, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?after=bad", "?after=-1", "?limit=bad", "?limit=0", "?limit=-1"} {
		t.Run(query, func(t *testing.T) {
			resp := do(t, http.MethodGet, srv.URL+"/v1/sandboxes/sbx_telemetry_inputs/telemetry"+query)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("GET telemetry%s = %d; want 400", query, resp.StatusCode)
			}
		})
	}
}
