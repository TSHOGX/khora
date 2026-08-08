package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/transport"
	v1 "engine/internal/transport/gen/engine/transport/v1"
	"engine/internal/transport/gen/engine/transport/v1/transportv1connect"
)

// The two halves of proxyErr's contract, which pull in opposite directions and so
// need pinning together:
//
//   - the CODE crosses, because sandboxd's classification is what tells a caller
//     whether to fix the request or retry it;
//   - the MESSAGE does not, because the inner client dials the netns veth IP, so
//     the inner text carries placement (§6.2 / D5.1).
//
// Collapsing everything to `unavailable` satisfied the second and broke the first:
// `unavailable` means "transient, retry me", so a nonexistent path or a malformed
// argument came back as an invitation to retry forever. Returning the inner error
// verbatim would do the reverse. A test that checked only for the absence of the IP
// would pass on the old defect, so each case below asserts BOTH.

// codedFailClient is a transport.Client whose calls fail the way the REAL inner
// client fails: with the Connect code sandboxd chose (transport/client.go maps the
// guest's status), and a message carrying the netns veth IP plus a host overlay
// path. `raw` instead fails with a plain error — a dial that never reached sandboxd,
// so nothing classified it.
type codedFailClient struct {
	*barrierFakeClient
	code     connect.Code
	endpoint string // the veth IP:port that must not reach the caller
	hostPath string // an overlay path that must not reach the caller either
	raw      bool
}

// innerDetail is a distinctive substring of the inner message. Asserting on it is
// stronger than asserting on the IP alone: it catches a "sanitizer" that redacts
// host-shaped tokens and passes the rest of the inner prose through.
const innerDetail = "no such file or directory"

func (c *codedFailClient) fail(op string) error {
	msg := fmt.Sprintf("%s: dial tcp %s: open %s/etc/x: %s", op, c.endpoint, c.hostPath, innerDetail)
	if c.raw {
		return errors.New(msg)
	}
	return connect.NewError(c.code, errors.New(msg))
}

func (c *codedFailClient) Read(context.Context, string, int64, int64) ([]byte, error) {
	return nil, c.fail("read")
}

// Start hands back a channel that ends with Kind:"error" — the D4.3 invariant-1
// overflow disconnect's shape. The stream must terminate with that error rather
// than closing cleanly (a clean close says "the process finished").
func (c *codedFailClient) Start(context.Context, transport.StartSpec) (<-chan transport.ProcessEvent, error) {
	ch := make(chan transport.ProcessEvent, 2)
	ch <- transport.ProcessEvent{Kind: "start", PID: 4242}
	ch <- transport.ProcessEvent{Kind: "error", Err: c.fail("stream")}
	close(ch)
	return ch, nil
}

// codedFixture assembles an Engine over the failing client. Built here rather than
// through barrierFixture because that one hard-wires *barrierFakeClient, and the
// failing client IS the input under test (api_test.go's placement-leak test does the
// same for the same reason).
func codedFixture(t *testing.T, cl transport.Client) (*httptest.Server, model.SandboxID) {
	t.Helper()
	mem := store.NewMemory()
	if err := mem.CreateTemplate(context.Background(), model.Template{
		ID: "bare", Builtin: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	med := lifecycle.New(lifecycle.Deps{
		Store:         mem,
		Runtime:       barrierFakeRuntime{},
		Rootfs:        barrierFakeRootfs{},
		Dial:          func(transport.Endpoint) transport.Client { return cl },
		HealthTimeout: 2 * time.Second,
		Logf:          func(string, ...any) {},
	})
	sid, err := med.Create(context.Background(), lifecycle.CreateSpec{Template: "bare"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	srv := httptest.NewServer((&Engine{Mediator: med, Store: mem}).HTTPHandler())
	t.Cleanup(srv.Close)
	return srv, sid
}

const (
	leakVeth    = "10.88.0.7:9000"
	leakOverlay = "/srv/state/overlays/ov_x/merged"
)

// assertNoLeak: nothing from the inner message crossed.
func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	for _, leak := range []string{leakVeth, "10.88.0.7", leakOverlay, "/srv", innerDetail, "dial tcp"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("proxied error leaks %q — only the CODE crosses, never the inner text "+
				"(§6.2 / D5.1): %v", leak, err)
		}
	}
}

func failingClient(code connect.Code, raw bool) *codedFailClient {
	return &codedFailClient{
		barrierFakeClient: &barrierFakeClient{},
		code:              code,
		endpoint:          leakVeth,
		hostPath:          leakOverlay,
		raw:               raw,
	}
}

// A code sandboxd chose survives the proxy hop; the message it chose does not.
func TestProxiedUnaryPreservesTheInnerCode(t *testing.T) {
	for _, code := range []connect.Code{
		connect.CodeNotFound,          // fs.Read of a path that is not there
		connect.CodeInvalidArgument,   // a malformed argument: retrying cannot help
		connect.CodePermissionDenied,  // the guest refused
		connect.CodeResourceExhausted, // §3.4 disk cap: valid request, no room
	} {
		t.Run(code.String(), func(t *testing.T) {
			srv, sid := codedFixture(t, failingClient(code, false))
			fs := transportv1connect.NewFsServiceClient(h2cClient(), srv.URL, sidHeader(string(sid)))

			_, err := fs.Read(context.Background(), connect.NewRequest(&v1.ReadRequest{Path: "/etc/x"}))
			if err == nil {
				t.Fatal("fs.Read succeeded against a failing client")
			}
			if got := connect.CodeOf(err); got != code {
				t.Errorf("outer code = %v, want %v — sandboxd classified this failure and the "+
					"proxy must not overwrite the verdict; collapsing it to `unavailable` tells "+
					"the caller to retry something retrying cannot fix (err %v)", got, code, err)
			}
			assertNoLeak(t, err)
		})
	}
}

// A failure that never reached sandboxd has no classification to preserve — this is
// the case that genuinely IS a transient transport outage, and the only one for which
// `unavailable` is the honest answer.
func TestProxiedRawDialFailureStaysUnavailable(t *testing.T) {
	srv, sid := codedFixture(t, failingClient(connect.CodeNotFound /* ignored: raw */, true))
	fs := transportv1connect.NewFsServiceClient(h2cClient(), srv.URL, sidHeader(string(sid)))

	_, err := fs.Read(context.Background(), connect.NewRequest(&v1.ReadRequest{Path: "/etc/x"}))
	if err == nil {
		t.Fatal("fs.Read succeeded against a failing client")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("outer code = %v, want unavailable — an unclassified transport failure is the "+
			"one case where 'retry me' is true, and the engine must not invent a verdict it "+
			"does not have (err %v)", got, err)
	}
	assertNoLeak(t, err)
}

// The mid-stream half. An overflow disconnect (D4.3 invariant 1) arrives as
// ProcessEvent{Kind:"error"} carrying resource_exhausted — the guest reports it that
// way precisely so a consumer can tell it from a clean end. The outer stream must
// still TERMINATE with an error (never close as if the process finished), and now
// with the code that carries the distinction.
func TestProxiedStreamTerminationPreservesTheInnerCode(t *testing.T) {
	srv, sid := codedFixture(t, failingClient(connect.CodeResourceExhausted, false))
	proc := transportv1connect.NewProcessServiceClient(h2cClient(), srv.URL)

	req := connect.NewRequest(&v1.StartRequest{Cmd: "/bin/true"})
	req.Header().Set(sandboxIDHeader, string(sid))
	stream, err := proc.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stream.Close()

	var frames int
	for stream.Receive() {
		frames++
	}
	if frames == 0 {
		t.Error("no frames before the termination — the start event should have been forwarded first")
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("the outer stream closed CLEANLY after a mid-stream error — that tells the client " +
			"the process finished, which is exactly wrong (D4.3 invariant 1)")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Errorf("stream termination code = %v, want resource_exhausted — reconnecting is the "+
			"right response to an overflow disconnect and the wrong response to a real outage, "+
			"so the code has to survive the hop (err %v)", got, err)
	}
	assertNoLeak(t, err)
}
