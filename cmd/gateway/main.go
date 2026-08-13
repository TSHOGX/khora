// Command gateway is the outward-facing boundary in front of the orchestrator
// (ADR-0007): it terminates TLS, authenticates callers with mTLS, and forwards to
// the engine's loopback listener.
//
// IT IS NOT PART OF THE ENGINE. §1.4 excludes the credential family permanently — every byte
// the engine can hand a guest is readable by uid 0 inside it, so the engine cannot be a
// secrecy boundary — and §1.1 bars those names as engine-internal entities. So this lives
// under cmd/ as a separate binary, imports nothing from internal/, and the engine neither
// starts it, health-checks it, nor knows it exists. The precedent is the process behind
// `egress.via[]` (§1.1's qualifier): the deployment topology gains a process while the
// engine's dependency list stays the same.
//
// A REVERSE PROXY IS THE PRIMARY DOCUMENTED PATH, not this binary (ADR-0007). Most operators
// already run nginx or Caddy and should terminate TLS there; deploy/ has a worked config. This
// exists for deployments that would rather not add one, and does one thing: verify a client
// certificate against a CA bundle, then proxy.
//
// Why mTLS and not a bearer token: a token would mean this repo ships issuance, storage and
// revocation, and the whole family is what §1.4 excludes. mTLS puts caller identity in the TLS
// layer, where the operator's existing PKI holds it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/http2"
)

func main() {
	var (
		addr = flag.String("addr", ":8443", "listen address for the TLS front. Unlike the orchestrator's --addr this one is MEANT to be routable: it is the outward-facing surface, and it authenticates")
		up   = flag.String("upstream", "http://127.0.0.1:8080", "the orchestrator's §6.1 listener. Loopback, because ADR-0007 pins the engine there — a routable upstream would mean the engine is reachable without passing through this binary, and then this is a door beside an open wall")
		cert = flag.String("cert", "", "PEM certificate chain this gateway serves (required)")
		key  = flag.String("key", "", "PEM private key for --cert (required)")
		ca   = flag.String("client-ca", "", "PEM bundle of certificate authorities that may issue CLIENT certificates (required). Every caller must present a certificate this bundle verifies; there is no unauthenticated path. Scope it to a CA you issue from — a public bundle here authenticates the entire internet")
	)
	flag.Parse()

	for _, req := range []struct{ flag, val string }{{"--cert", *cert}, {"--key", *key}, {"--client-ca", *ca}} {
		if req.val == "" {
			// REFUSED, never defaulted: a gateway without a client CA would forward every
			// caller to an unauthenticated control plane while looking like a boundary.
			fmt.Fprintf(os.Stderr, "gateway: %s is required — this binary exists to authenticate callers, "+
				"and one that starts without the material to do so would proxy every request to an "+
				"unauthenticated control plane while looking like a boundary (ADR-0007)\n", req.flag)
			os.Exit(2)
		}
	}

	serverCert, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatalf("gateway: load --cert/--key: %v", err)
	}
	pool, err := clientCAPool(*ca)
	if err != nil {
		log.Fatalf("gateway: load --client-ca: %v", err)
	}

	upstream, err := url.Parse(*up)
	if err != nil {
		log.Fatalf("gateway: parse --upstream %q: %v", *up, err)
	}
	if upstream.Scheme != "http" {
		// h2c upstream, so the scheme is http even though the front is TLS. Accepting `https`
		// would produce a handshake failure at the first request rather than here.
		log.Fatalf("gateway: --upstream must be http:// (the engine serves cleartext h2c on loopback, "+
			"transport D4.2); got %q", upstream.Scheme)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// The verified client identity is NOT forwarded as a header (§1.4): passing one
			// would invite the engine to read it, which is how an authorization subject
			// enters a system that has none, and §1.1 bars the names such a header would
			// need. Authentication is complete here — that a request reached the upstream is
			// the entire contract between the two processes.
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")
		},
		// h2c to the upstream. Measured necessary rather than assumed: an ordinary
		// HTTP/1.1 upstream does carry the Connect protocol's streams (both
		// directions, with the arrival spread preserved), but a consumer using the
		// gRPC protocol requires HTTP/2 end to end, and which protocol a consumer
		// picks is not ours to constrain.
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			},
		},
		// FlushInterval -1 = flush immediately after each write, which is what the streaming
		// data plane needs: a PTY whose events arrive in one flush when the process exits is
		// not an interactive PTY.
		//
		// BELT-AND-BRACES rather than the mechanism. Measured: ReverseProxy ignores
		// FlushInterval when the response has no Content-Length — which is every Connect
		// stream — so the spread survives at 400 ms over three 200 ms-spaced chunks even with
		// the interval at 30 s. It bites only on a response that HAS a Content-Length, where
		// the same setting collapses the spread to 0 s. Set so a length-carrying response is
		// not buffered either, not because the streams depend on it.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The upstream's address is host-local and does not belong in a response body
			// (the §6.2 discipline the engine's own edge follows): logged, generic on the wire.
			log.Printf("gateway: proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: proxy,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			// RequireAndVerify, not VerifyIfGiven: there is no unauthenticated path
			// through this binary. Measured — a caller with no certificate and a
			// caller with a certificate from another CA are both refused at the
			// handshake, before any request reaches the engine.
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  pool,
			// TLS 1.3 floor. This is a new deployment surface with no installed base
			// to be compatible with, so there is nothing to weigh against it.
			MinVersion: tls.VersionTLS13,
			// h2 first: the data plane's streams need HTTP/2 for the gRPC-protocol
			// consumer, and http/1.1 stays for the REST control plane and for
			// Connect-protocol clients.
			NextProtos: []string{"h2", "http/1.1"},
		},
	}

	log.Printf("gateway: mTLS front on %s → %s (client CAs from %s)", *addr, upstream, *ca)
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("gateway: serve: %v", err)
	}
}

// clientCAPool reads the CA bundle that may issue client certificates.
//
// A bundle that parses to ZERO certificates is refused rather than accepted as an empty pool:
// an empty ClientCAs with RequireAndVerifyClientCert refuses every caller, so the symptom
// would be "nothing can connect" with a clean startup log and the real fault — a wrong path,
// or a file holding a key instead of a certificate — invisible.
func clientCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificates — with client-certificate verification "+
			"required, an empty authority set refuses every caller, so this would present as "+
			"\"nothing can connect\" rather than as a bad path", path)
	}
	return pool, nil
}
