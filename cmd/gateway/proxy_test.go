package main

// The gateway's two load-bearing claims, asserted end to end against a real TLS
// handshake and a real h2c upstream rather than against the config structs:
//
//  1. an unauthenticated caller NEVER reaches the upstream, and
//  2. an authenticated one gets a working STREAM — in both directions, arriving
//     incrementally.
//
// Both are the whole point of the binary, and each fails in a way the other's test
// would not notice: a gateway that refuses everyone satisfies (1) trivially, and one
// that authenticates correctly but buffers responses satisfies (1) while making the
// interactive data plane useless. So neither test stands alone.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// buildGateway assembles the same proxy + TLS config main() serves, over a test PKI.
// It deliberately reuses the production shapes (h2c transport, FlushInterval -1,
// RequireAndVerifyClientCert, the header scrub) so a drift in main() shows up here.
func buildGateway(t *testing.T, upstreamURL string, pool *x509.CertPool, serverCert tls.Certificate) (addr string, reached *int64) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")
		},
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: proxy,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"h2", "http/1.1"},
		},
		// Silence the expected handshake-failure logs from the refusal arms.
		ErrorLog: quietLogger(),
	}
	t.Cleanup(func() { srv.Close() })
	go srv.ServeTLS(l, "", "")
	return l.Addr().String(), nil
}

// streamingUpstream is a cleartext h2c origin that emits `n` chunks `gap` apart and
// counts requests that actually arrived. Standing in for the engine's Connect data
// plane: what is under test here is the gateway's forwarding behavior, and a
// chunk-emitting handler exercises the same flush path with none of the engine's
// substrate needs (this test is root-free and runs in the routine gate).
func streamingUpstream(t *testing.T, n int, gap time.Duration) (string, *int64) {
	t.Helper()
	var reached int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reached, 1)
		// Echo back what the CLIENT streamed, so the client-streaming direction is
		// observable from the test's side.
		if r.Header.Get("X-Probe") == "echo" {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "chunk-%d\n", i)
			fl.Flush()
			time.Sleep(gap)
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(h, &http2.Server{})}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(l)
	return "http://" + l.Addr().String(), &reached
}

// An unauthenticated caller is refused AT THE HANDSHAKE and never reaches the
// upstream. The upstream request counter is the load-bearing assertion: a gateway
// that returned 401 after forwarding would satisfy a status-code check while having
// already exposed the control plane.
func TestAnUnauthenticatedCallerNeverReachesTheUpstream(t *testing.T) {
	ca := newAuthority(t, "khora-test-ca")
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	serverCert := issueServer(t, ca)

	upstreamURL, reached := streamingUpstream(t, 1, 0)
	gwAddr, _ := buildGateway(t, upstreamURL, pool, serverCert)

	rogue := newAuthority(t, "rogue-ca")

	for _, tc := range []struct {
		name string
		cert *tls.Certificate
	}{
		{"no client certificate at all", nil},
		{"a certificate from an authority the gateway does not trust", ptr(issue(t, rogue, "client-a", x509.ExtKeyUsageClientAuth))},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13, ServerName: "127.0.0.1"}
			if tc.cert != nil {
				tlsCfg.Certificates = []tls.Certificate{*tc.cert}
			}
			cl := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
			resp, err := cl.Get("https://" + gwAddr + "/v1/sandboxes")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("the gateway served an unauthenticated caller (%s) — it answered %s instead of "+
					"refusing at the handshake (the deployment boundary: there is no unauthenticated path through this "+
					"binary)", resp.Status, resp.Status)
			}
			if n := atomic.LoadInt64(reached); n != 0 {
				t.Errorf("%d request(s) reached the upstream from an unauthenticated caller — the "+
					"refusal must happen BEFORE the engine sees anything, since the engine has no "+
					"authentication of its own to fall back on (the engine non-goals)", n)
			}
		})
	}
}

// An authenticated caller gets a working stream, and the arrival SPREAD is preserved
// through the gateway.
//
// The spread is the assertion, not the byte count: an interactive PTY whose events
// arrive in one buffered flush when the process exits is not a working PTY, and a
// buffering hop passes every "did the bytes arrive" check.
//
// WHAT HOLDS THE PROPERTY IS NOT FlushInterval. Measured, over three chunks 200 ms apart: an
// upstream response with no Content-Length — every Connect stream — keeps its 400 ms spread even
// with FlushInterval at 30 s, because ReverseProxy ignores the interval for such responses. The
// interval bites only on a Content-Length-carrying response, covered by the companion test below.
// So this pins the streaming path THROUGH the TLS front, which no library guarantee covers.
func TestAnAuthenticatedCallerGetsAStreamThatIsNotBuffered(t *testing.T) {
	const (
		chunks = 3
		gap    = 200 * time.Millisecond
	)
	ca := newAuthority(t, "khora-test-ca")
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	serverCert := issueServer(t, ca)

	upstreamURL, reached := streamingUpstream(t, chunks, gap)
	gwAddr, _ := buildGateway(t, upstreamURL, pool, serverCert)

	clientCert := issue(t, ca, "client-a", x509.ExtKeyUsageClientAuth)
	cl := &http.Client{Transport: &http2.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS13,
		ServerName:   "127.0.0.1",
	}}}

	resp, err := cl.Get("https://" + gwAddr + "/v1/sandboxes")
	if err != nil {
		t.Fatalf("an authenticated caller was refused: %v — the gateway must reject unauthenticated "+
			"callers, not all of them", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated caller got %s", resp.Status)
	}

	// Read chunk by chunk, timing arrivals.
	var first, last time.Time
	got := 0
	buf := make([]byte, 64)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			last = time.Now()
			got += strings.Count(string(buf[:n]), "chunk-")
		}
		if rerr != nil {
			break
		}
	}
	if got != chunks {
		t.Errorf("got %d chunks through the gateway, want %d", got, chunks)
	}
	if atomic.LoadInt64(reached) != 1 {
		t.Errorf("upstream saw %d requests, want 1", atomic.LoadInt64(reached))
	}

	// The upstream spaces chunks `gap` apart, so a non-buffering path spreads
	// arrivals over about (chunks-1)*gap. A buffering hop collapses that to ~0.
	spread := last.Sub(first)
	if floor := gap; spread < floor {
		t.Errorf("chunk arrivals spread over only %v (want at least %v): the gateway is BUFFERING the "+
			"response, which makes every server-streaming call — process Start/Connect, and any "+
			"interactive PTY — arrive in one flush at exit instead of as it is produced", spread, floor)
	}
}

// The companion: FlushInterval: -1 is pinned where it is REACHABLE, i.e. on a
// response that carries a Content-Length.
//
// This exists because the streaming test above cannot pin it — ReverseProxy ignores
// the interval for a response with no Content-Length, so that test passes whatever
// the field says, and a production `FlushInterval: 0` would go unnoticed by it. The
// two arms here are measured: with -1 the arrival spread survives, and with a long
// positive interval the same upstream's spread collapses to ~0. So this is the test
// that bites if the field is ever dropped from main().
func TestFlushIntervalIsPinnedWhereItActuallyBites(t *testing.T) {
	const (
		chunks = 3
		gap    = 200 * time.Millisecond
	)
	body := ""
	for i := 0; i < chunks; i++ {
		body += fmt.Sprintf("chunk-%d\n", i)
	}

	// An upstream that DOES declare a length while writing incrementally.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "chunk-%d\n", i)
			fl.Flush()
			time.Sleep(gap)
		}
	})
	ul, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	usrv := &http.Server{Handler: h2c.NewHandler(h, &http2.Server{})}
	t.Cleanup(func() { usrv.Close() })
	go usrv.Serve(ul)
	u, _ := url.Parse("http://" + ul.Addr().String())

	spreadWith := func(flush time.Duration) time.Duration {
		p := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u) },
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			},
			FlushInterval: flush,
		}
		l, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			t.Fatal(lerr)
		}
		srv := &http.Server{Handler: h2c.NewHandler(p, &http2.Server{})}
		t.Cleanup(func() { srv.Close() })
		go srv.Serve(l)

		cl := &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}
		resp, rerr := cl.Get("http://" + l.Addr().String() + "/")
		if rerr != nil {
			t.Fatal(rerr)
		}
		defer resp.Body.Close()
		var first, last time.Time
		buf := make([]byte, 64)
		for {
			n, e := resp.Body.Read(buf)
			if n > 0 {
				if first.IsZero() {
					first = time.Now()
				}
				last = time.Now()
			}
			if e != nil {
				break
			}
		}
		return last.Sub(first)
	}

	immediate := spreadWith(-1)
	buffered := spreadWith(30 * time.Second)

	if immediate < gap {
		t.Errorf("with FlushInterval -1 the arrivals spread over only %v (want at least %v) — the "+
			"immediate-flush setting is not taking effect", immediate, gap)
	}
	// The control on the assertion above: the same upstream MUST collapse under a
	// long interval. If it does not, this test is measuring nothing and would keep
	// passing with the field removed.
	if buffered >= gap {
		t.Errorf("with FlushInterval 30s the arrivals still spread over %v — then this test cannot "+
			"detect buffering at all, and the -1 arm above is not evidence of anything", buffered)
	}
}

// The verified client identity is NOT forwarded to the engine. The engine has no
// authorization subject and the engine vocabulary boundary bars the names one would need; a header carrying
// caller identity invites it to grow one, so the scrub is asserted rather than left
// to the comment in main().
func TestTheGatewayForwardsNoCallerIdentityToTheEngine(t *testing.T) {
	ca := newAuthority(t, "khora-test-ca")
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	serverCert := issueServer(t, ca)

	seen := make(chan http.Header, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(h, &http2.Server{})}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(l)

	gwAddr, _ := buildGateway(t, "http://"+l.Addr().String(), pool, serverCert)

	clientCert := issue(t, ca, "client-a", x509.ExtKeyUsageClientAuth)
	cl := &http.Client{Transport: &http2.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS13,
		ServerName:   "127.0.0.1",
	}}}
	resp, err := cl.Get("https://" + gwAddr + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	hdr := <-seen
	for _, banned := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Client-Cert", "X-Client-Subject", "X-Ssl-Client-Cn",
	} {
		if v := hdr.Get(banned); v != "" {
			t.Errorf("the gateway forwarded %s=%q to the engine — caller identity must stop at the "+
				"gateway, since the engine has no authorization subject and the names such a header "+
				"would need are barred as engine-internal entities (the engine vocabulary boundary / the engine non-goals)", banned, v)
		}
	}
}

// issueServer signs the gateway's own leaf, with 127.0.0.1 as an IP SAN so a test
// client verifying by address accepts it.
func issueServer(t *testing.T, a *authority) tls.Certificate {
	t.Helper()
	return issue(t, a, "gateway", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
}

// quietLogger discards the gateway's expected TLS handshake-failure lines: the
// refusal arms provoke one per attempt by design, and they would otherwise drown the
// output of a real failure.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func ptr[T any](v T) *T { return &v }
