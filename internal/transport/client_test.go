package transport_test

import (
	"context"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"engine/internal/transport"
)

// h2cEcho starts a throwaway h2c server and returns its endpoint plus a counter of
// ACCEPTED CONNECTIONS. The counter is the whole instrument: it distinguishes "the
// pool was reused" from "a fresh connection was opened", which no assertion on the
// RPC's own result can see.
//
// The bodies are deliberately not valid Connect responses — every call below fails at
// decode, and that is fine. What is under test is the connection underneath, and a
// failed RPC has still opened one.
func h2cEcho(t *testing.T) (transport.Endpoint, *int64) {
	t.Helper()
	var accepted int64
	h := h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), &http2.Server{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: h,
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				atomic.AddInt64(&accepted, 1)
			}
		},
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return transport.Endpoint(ln.Addr().String()), &accepted
}

// TestDialSharesOneConnectionPerEndpoint is the regression guard for a transport built
// PER Dial call.
//
// WHY IT IS A LEAK AND NOT AN INEFFICIENCY: an http2.Transport owns a connection pool,
// and each pooled connection runs its own read and write loops. Nothing in the engine
// closes a transport — Client is documented as stateless and simply dropped — so a
// per-call transport strands every connection it opened, with its goroutines, for the
// life of the process. Dial is on the path of every data-plane RPC AND of the reaper's
// health probe for each running sandbox every tick, so the growth is continuous rather
// than load-dependent: an idle node with 20 running sandboxes leaks on a 30s timer.
//
// Measured with the per-call shape: 50 sequential Health calls to ONE endpoint accepted
// 50 connections and left ~150 goroutines resident after two forced GCs. The
// assertions are deliberately loose (a handful, not exactly one) because pooling is
// allowed to open a replacement; what must never happen is growth proportional to the
// call count.
func TestDialSharesOneConnectionPerEndpoint(t *testing.T) {
	ep, accepted := h2cEcho(t)

	runtime.GC()
	before := runtime.NumGoroutine()

	const calls = 50
	for i := 0; i < calls; i++ {
		// A fresh Client per call ON PURPOSE: that is exactly how §4 uses it (built per
		// endpoint, discarded), and it is the usage a per-call transport punishes.
		if _, err := transport.Dial(ep).Health(context.Background()); err == nil {
			t.Fatal("expected the stub echo to fail decoding; the probe would prove nothing if it succeeded")
		}
	}

	time.Sleep(500 * time.Millisecond) // let any teardown that is going to happen happen
	runtime.GC()
	runtime.GC()

	if got := atomic.LoadInt64(accepted); got > calls/10 {
		t.Errorf("%d calls to one endpoint opened %d connections; want a pooled handful — "+
			"one connection per call means each Dial built its own transport, and nothing closes those",
			calls, got)
	}
	if delta := runtime.NumGoroutine() - before; delta > calls/2 {
		t.Errorf("goroutines grew by %d across %d calls — a per-call transport strands each "+
			"connection's read/write loops for the life of the process (fd exhaustion, not a slow leak)",
			delta, calls)
	}
}

// TestDialKeepsEndpointsOnSeparateConnections pins the other side: sharing the pool
// must not let two sandboxes share a connection.
//
// Sharing here would be a placement-isolation bug, not just surprising — each endpoint
// is a distinct guest netns IP (§3.3), and the pool is keyed by host:port precisely so
// that a resumed sandbox at a new IP transparently gets a new connection while the
// retired one ages out. If this ever failed, the pool key would have been widened to
// something that ignores the endpoint.
func TestDialKeepsEndpointsOnSeparateConnections(t *testing.T) {
	epA, a := h2cEcho(t)
	epB, b := h2cEcho(t)

	for i := 0; i < 5; i++ {
		_, _ = transport.Dial(epA).Health(context.Background())
		_, _ = transport.Dial(epB).Health(context.Background())
	}

	if atomic.LoadInt64(a) == 0 || atomic.LoadInt64(b) == 0 {
		t.Fatalf("endpoint A accepted %d and B accepted %d; each endpoint must get its own "+
			"connection (they are different guest netns IPs, §3.3)",
			atomic.LoadInt64(a), atomic.LoadInt64(b))
	}
}
