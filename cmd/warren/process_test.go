package main

import "testing"

func TestProcessTransportURLPreservesAPISchemeAndPrefix(t *testing.T) {
	old := apiURL
	t.Cleanup(func() { apiURL = old })

	for _, tc := range []struct {
		api  string
		want string
	}{
		{"https://gateway.example", "https://gateway.example/v1/sandboxes/sb-1/connect"},
		{"https://gateway.example/engine/v1/", "https://gateway.example/engine/v1/sandboxes/sb-1/connect"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1/sandboxes/sb-1/connect"},
	} {
		apiURL = tc.api
		got, err := processTransportURL("sb-1")
		if err != nil {
			t.Fatalf("processTransportURL(%q): %v", tc.api, err)
		}
		if got != tc.want {
			t.Errorf("processTransportURL(%q) = %q, want %q", tc.api, got, tc.want)
		}
	}
}
