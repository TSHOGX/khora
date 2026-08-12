package main

// Test plumbing shared by the CLI's command tests.
//
// The CLI keeps its API target in package globals, set once by the root command's
// PersistentPreRunE — which a test that builds one subcommand directly never runs. So a
// test has to install them, and there are now THREE that must agree (apiURL, httpClient,
// and the thin client built from both). useServer sets them together for exactly that
// reason: three assignments spelled out per test is how one comes to be forgotten, and
// the symptom is a nil-pointer panic several frames inside pkg/warren rather than
// anything that names the omission.

import (
	"net/http/httptest"
	"testing"

	"github.com/TSHOGX/warren/pkg/warren"
)

// useServer points the CLI's globals at srv for the duration of the test, restoring them
// afterwards.
func useServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldURL, oldHTTP, oldAPI := apiURL, httpClient, api
	t.Cleanup(func() { apiURL, httpClient, api = oldURL, oldHTTP, oldAPI })

	apiURL, httpClient = srv.URL, srv.Client()
	cl, err := warren.New(srv.URL, warren.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	api = cl
}
