package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// newHTTPClient configures one transport shared by the REST, event-streaming,
// and Connect-RPC commands. With no TLS flags it retains Go's default behavior.
func newHTTPClient(certFile, keyFile, caFile string) (*http.Client, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("--client-cert and --client-key must be provided together")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read --ca-cert: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca-cert %q contains no PEM certificates", caFile)
		}
		tlsConfig.RootCAs = roots
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}

func transferURL(endpoint, remotePath string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse transfer URL: %w", err)
	}
	query := u.Query()
	query.Set("path", remotePath)
	u.RawQuery = query.Encode()
	return u.String(), nil
}
