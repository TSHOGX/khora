package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- test PKI -----------------------------------------------------------------

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newAuthority(t *testing.T, cn string) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return &authority{cert: c, key: key, der: der}
}

func (a *authority) writePEM(t *testing.T, path string) string {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}
	return path
}

// --- the client-CA bundle contract --------------------------------------------

// A bundle holding no certificates is REFUSED, not accepted as an empty pool.
//
// This is the failure mode worth a test rather than a comment: an empty ClientCAs
// with RequireAndVerifyClientCert refuses every caller, so an operator who pointed
// --client-ca at the wrong file would see "nothing can connect" alongside a clean
// startup log, and the real fault — a bad path, or a file holding a key where a
// certificate was meant — would be nowhere in evidence.
func TestAnEmptyClientCABundleIsRefusedRatherThanAcceptedAsAnEmptyPool(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct{ name, content string }{
		{"empty file", ""},
		{"no PEM at all", "this is not a certificate\n"},
		{"a PRIVATE KEY where a certificate was meant", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQ==\n-----END EC PRIVATE KEY-----\n"},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".pem")
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		pool, err := clientCAPool(path)
		if err == nil {
			t.Errorf("%s: clientCAPool accepted it (pool=%v) — with client certificates required, an "+
				"empty authority set refuses every caller, so this must fail at startup and name the "+
				"file rather than present later as \"nothing can connect\"", tc.name, pool)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: the refusal does not name the file, which is the one fact the operator needs: %v",
				tc.name, err)
		}
	}
}

// A real CA bundle loads, and the pool actually contains it. The control arm: without
// this, the test above would keep passing if clientCAPool rejected everything.
func TestARealClientCABundleLoads(t *testing.T) {
	dir := t.TempDir()
	ca := newAuthority(t, "warren-test-ca")
	path := ca.writePEM(t, filepath.Join(dir, "ca.pem"))

	pool, err := clientCAPool(path)
	if err != nil {
		t.Fatalf("clientCAPool refused a valid single-certificate bundle: %v", err)
	}
	if pool == nil {
		t.Fatal("clientCAPool returned no pool and no error")
	}
	// The pool must actually verify a certificate this CA issued — an "it parsed"
	// assertion would not distinguish a pool holding the wrong certificate.
	leaf := issue(t, ca, "client-a", x509.ExtKeyUsageClientAuth)
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("the loaded pool does not verify a certificate its own CA issued: %v", err)
	}
}

// issue signs a leaf certificate. ips are IP SANs, needed for a SERVER certificate
// a client verifies by address (the test gateway listens on 127.0.0.1); a client
// certificate needs none.
func issue(t *testing.T, a *authority, cn string, usage x509.ExtKeyUsage, ips ...net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
