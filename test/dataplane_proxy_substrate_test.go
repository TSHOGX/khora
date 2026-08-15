//go:build substrate

// C6: the outer-edge Connect data plane through the orchestrator.
// A Connect client speaks the SAME transport.proto to the orchestrator, using
// ONLY sandbox_id (a header) — the orchestrator re-proxies to the sandbox's
// sandboxd (the transport contract). Asserts: Start/Connect/fs work end-to-end; placement (netns IP
// / container id / host path) never appears at the outer edge; there is no
// REST/NDJSON data-plane facade (those routes 404). Requires root (real runsc).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -timeout 300s ./test/ -run C6
package test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/TSHOGX/khora/internal/lifecycle"
	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
	"github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1/transportv1connect"
)

// h2cClient builds an http.Client that speaks cleartext HTTP/2 (h2c), matching
// the orchestrator's h2c handler — so Connect server-streams work.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// sidHeader sets the sandbox-id header on an outer-edge request (the only
// addressing the client ever uses — placement stays inside the engine).
func sidHeader(sid string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Engine-Sandbox-Id", sid)
			return next(ctx, req)
		}
	}))
}

func TestC6OuterEdgeConnectProxy(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupEngine(t, 30*time.Second)
	m := eng.Mediator

	sid, err := m.Create(ctx, lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = m.Destroy(ctx, sid) }()

	// the internal endpoint — must NEVER appear in any outer-edge response.
	endpoint, err := m.Endpoint(sid)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	ip := strings.Split(endpoint, ":")[0]
	t.Logf("internal placement endpoint = %s (must not leak to outer edge)", endpoint)

	srv := httptest.NewServer(eng.HTTPHandler())
	defer srv.Close()

	hc := h2cClient()
	// The outer edge sets the sandbox-id header via an interceptor; streams need
	// it too, so we also pass it explicitly on the stream request below.
	proc := transportv1connect.NewProcessServiceClient(hc, srv.URL, sidHeader(string(sid)))
	fs := transportv1connect.NewFsServiceClient(hc, srv.URL, sidHeader(string(sid)))

	// --- fs round-trip through the proxy (unary) ---
	marker := "OUTER_MARKER_" + time.Now().Format("150405.000")
	if _, err := fs.Write(ctx, connect.NewRequest(&v1.WriteRequest{Path: "/home/outer.txt", Data: []byte(marker)})); err != nil {
		t.Fatalf("fs.Write via proxy: %v", err)
	}
	rd, err := fs.Read(ctx, connect.NewRequest(&v1.ReadRequest{Path: "/home/outer.txt"}))
	if err != nil {
		t.Fatalf("fs.Read via proxy: %v", err)
	}
	if string(rd.Msg.Data) != marker {
		t.Fatalf("fs read-back = %q; want %q", rd.Msg.Data, marker)
	}

	// --- process.Start through the proxy (server-stream) ---
	req := connect.NewRequest(&v1.StartRequest{Cmd: "/bin/sh", Args: []string{"-c", "echo PROXY_OK"}})
	req.Header().Set("Engine-Sandbox-Id", string(sid)) // stream: header set directly
	stream, err := proc.Start(ctx, req)
	if err != nil {
		t.Fatalf("process.Start via proxy: %v", err)
	}
	var out strings.Builder
	gotStart, exit := false, -1
	for stream.Receive() {
		ev := stream.Msg()
		switch k := ev.Kind.(type) {
		case *v1.ProcessEvent_Start:
			gotStart = k.Start.Pid > 0
		case *v1.ProcessEvent_Data:
			out.Write(k.Data.Data)
		case *v1.ProcessEvent_End:
			exit = int(k.End.ExitCode)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	_ = stream.Close()
	if !gotStart {
		t.Error("no start event with pid via proxy")
	}
	if !strings.Contains(out.String(), "PROXY_OK") {
		t.Errorf("proxied stdout = %q; want contains PROXY_OK", out.String())
	}
	if exit != 0 {
		t.Errorf("proxied exit = %d; want 0", exit)
	}

	// --- placement never leaks: the internal IP must not appear in output ---
	if ip != "" && strings.Contains(out.String(), ip) {
		t.Errorf("placement IP %s leaked into outer-edge output", ip)
	}

	// --- no REST data-plane facade: these routes 404 ---
	assertGone := func(method, path string, body string) {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("probe %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("REST data-plane route %s %s returned %d; want 404 (no facade)", method, path, resp.StatusCode)
		}
	}
	assertGone(http.MethodPost, "/v1/sandboxes/"+string(sid)+"/processes", `{"cmd":"/bin/sh"}`)
	assertGone(http.MethodGet, "/v1/sandboxes/"+string(sid)+"/fs?path=/home/outer.txt", "")

	// --- error path never leaks placement: pause the sandbox so its placement is
	// gone, then a unary proxy op must error WITHOUT the netns IP in the message
	// (proxyErr sanitizes inner dial errors — the API error contract / the transport contract).
	if err := m.Pause(ctx, sid); err != nil {
		t.Fatalf("pause for error-path check: %v", err)
	}
	_, derr := fs.Read(ctx, connect.NewRequest(&v1.ReadRequest{Path: "/home/outer.txt"}))
	if derr == nil {
		t.Fatal("fs.Read on paused sandbox should error (no live placement)")
	}
	if ip != "" && strings.Contains(derr.Error(), ip) {
		t.Errorf("placement IP %s leaked into proxy error: %v", ip, derr)
	}
}
