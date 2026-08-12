package main

import (
	"net/url"
	"os"
	"testing"
)

func TestTransferURLRoundTripsRemotePath(t *testing.T) {
	remotePath := "/tmp/a & b#c?d=雪"
	got, err := transferURL("https://gateway.example/v1/sandboxes/sb/files", remotePath)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath := u.Query().Get("path"); gotPath != remotePath {
		t.Fatalf("path = %q, want %q (URL %q)", gotPath, remotePath, got)
	}
	if u.Fragment != "" {
		t.Fatalf("fragment = %q, want empty (URL %q)", u.Fragment, got)
	}
}

func TestNewHTTPClientRequiresCertificateAndKeyTogether(t *testing.T) {
	for _, tc := range []struct {
		cert string
		key  string
	}{{cert: "cert.pem"}, {key: "key.pem"}} {
		if _, err := newHTTPClient(tc.cert, tc.key, ""); err == nil {
			t.Fatalf("newHTTPClient(%q, %q, empty CA) succeeded", tc.cert, tc.key)
		}
	}
}

func TestNewHTTPClientRejectsEmptyCABundle(t *testing.T) {
	caFile := t.TempDir() + "/ca.pem"
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newHTTPClient("", "", caFile); err == nil {
		t.Fatal("newHTTPClient accepted a CA bundle with no certificates")
	}
}
