package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkPublishDisplaysHostEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"guest_port":8080,"proto":"tcp","host_endpoint":"127.0.0.1:21000","applies_at":"now"}`)
	}))
	defer server.Close()

	oldAPIURL, oldClient := apiURL, httpClient
	apiURL, httpClient = server.URL, server.Client()
	t.Cleanup(func() { apiURL, httpClient = oldAPIURL, oldClient })

	var out bytes.Buffer
	cmd := newNetworkPublishCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"sandbox-1", "--guest-port", "8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "host endpoint 127.0.0.1:21000") {
		t.Fatalf("output = %q, want published host endpoint", got)
	}
}
