// Package khora is the thin Go client for the engine control plane and data plane.
//
// THIN IS A CONSTRAINT, NOT A DESCRIPTION. It is the whole reason this package is
// allowed to live in the engine repo. Four rules keep it thin:
//
//   - Every field maps 1:1 onto a control-plane API field. No composite convenience type fusing
//     two responses.
//   - No invented verb. There is no WaitUntilRunning here because there is none in
//     the API — polling, retry, and backoff are the CALLER's, since each is a promise
//     about time the engine never made.
//   - No state that outlives a call. The moment this client cached, leased, or
//     tracked a subscriber it would be a product layer wearing a client's name.
//   - The engine vocabulary boundary applies verbatim. This is engine-facing code in the
//     engine repo, so the banned entity names are banned here too.
//
// What that buys the caller: every method is a single request with a single answer, so the
// failure model is HTTP's and nothing is hidden. What it costs: a caller wanting an ergonomic SDK
// — waiters, retries, streaming helpers, per-language idiom — writes it OUTSIDE, in the consuming
// repo, on top of this.
//
// AUTHENTICATION IS NOT HERE: auth and TLS live outside the engine, and the
// orchestrator's listener refuses anything but
// loopback. This client therefore speaks plain HTTP to a local endpoint by default.
// The TLS knobs exist for the case where something else terminates in front of it —
// see WithTLS — and they configure the client, they do not make the engine
// authenticate anything.
package khora

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultEndpoint is the orchestrator's loopback control plane.
const DefaultEndpoint = "http://127.0.0.1:8080"

// Client talks to one engine.
//
// It holds no per-sandbox state — no cache, no lease, no subscriber registry — which
// is the reason a Client is safe to share across
// goroutines: there is nothing in it for two callers to race over but the *http.Client,
// which is itself concurrent-safe.
type Client struct {
	endpoint string
	http     *http.Client
}

// Option configures a Client at construction. There is no setter: a Client whose
// endpoint or transport could change under a caller mid-flight is the kind of hidden
// state this package excludes.
type Option func(*Client) error

// New builds a client for endpoint (e.g. "http://127.0.0.1:8080"). An empty endpoint
// means DefaultEndpoint.
func New(endpoint string, opts ...Option) (*Client, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	c := &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		http:     &http.Client{},
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// WithHTTPClient supplies the *http.Client to use. The caller owns its timeout: this
// package sets none, because a deadline is a promise about time and those are the
// caller's. Long-lived calls — an event long-poll, a file transfer,
// a process stream — are the reason a blanket client timeout would be wrong here.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) error {
		if h == nil {
			return fmt.Errorf("WithHTTPClient: nil client")
		}
		c.http = h
		return nil
	}
}

// WithTLS configures client certificates and/or a CA for the case where something
// external terminates TLS in front of the engine. It does NOT make the engine
// authenticate: that remains outside, and the engine's own listener is
// loopback-only. Pass empty strings for the halves you do not need; cert and key must
// be given together.
func WithTLS(certFile, keyFile, caFile string) Option {
	return func(c *Client) error {
		if (certFile == "") != (keyFile == "") {
			return fmt.Errorf("client certificate and key must be provided together")
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if certFile != "" {
			pair, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return fmt.Errorf("load client certificate and key: %w", err)
			}
			cfg.Certificates = []tls.Certificate{pair}
		}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return fmt.Errorf("read CA certificate: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil || roots == nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return fmt.Errorf("CA certificate %q contains no PEM certificates", caFile)
			}
			cfg.RootCAs = roots
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = cfg
		if c.http.Transport == nil {
			c.http.Transport = tr
		} else {
			c.http = &http.Client{Transport: tr, Timeout: c.http.Timeout}
		}
		return nil
	}
}

// Endpoint reports the base URL this client was built for. Useful for the data plane,
// which a caller reaches with pkg/transport/gen directly. The generated tree is
// the protocol, and this client is one consumer of it, not its owner.
func (c *Client) Endpoint() string { return c.endpoint }

// Error is a non-2xx answer from the engine, carrying the API error contract's status code and body.
//
// The code is the useful half and the API error contract gives each one exactly one meaning surface-wide
// (404 absent, 409 wrong state, 400 invalid or unenforceable request, 503 a subsystem or the
// guest is unreachable, 507 admission refused). The MESSAGE is deliberately not
// parsed: the API error contract sanitizes it (host paths and placement are stripped), so it is text for
// a human, never a value to branch on.
type Error struct {
	Code    int
	Message string
	Method  string
	Path    string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: engine returned %d", e.Method, e.Path, e.Code)
	}
	return fmt.Sprintf("%s %s: engine returned %d: %s", e.Method, e.Path, e.Code, e.Message)
}

// IsNotFound reports whether err is a 404 — the object does not exist. Provided
// because "did this ID exist" is the one status question nearly every caller asks, and
// the alternative is every caller writing the same errors.As.
func IsNotFound(err error) bool { return codeIs(err, http.StatusNotFound) }

// IsConflict reports whether err is a 409 — the object exists but is in the wrong
// state for this verb (the API error contract).
func IsConflict(err error) bool { return codeIs(err, http.StatusConflict) }

// IsUnavailable reports whether err is a 503 — a subsystem or the guest could not be
// reached. NOT retryable by this package: a retry policy is a promise about time
// so it is the caller's to write.
func IsUnavailable(err error) bool { return codeIs(err, http.StatusServiceUnavailable) }

func codeIs(err error, code int) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == code
}

// do issues one request and decodes a JSON response into out (which may be nil for a
// 204). Every method in this package funnels through here, so the error shape is
// uniform by construction rather than by each method remembering.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body, out any, headers map[string]string) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errorFrom(resp, method, path)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errorFrom(resp, method, path)
	}
	if out != nil {
		if _, err := io.Copy(out, resp.Body); err != nil {
			return fmt.Errorf("%s %s: read response: %w", method, path, err)
		}
	}
	return nil
}

// errorFrom builds an *Error from a non-2xx response. The body is read whole and
// capped: the API error contract bodies are one sanitized line, so anything larger is not a message this
// client should hold in memory on a caller's behalf.
func errorFrom(resp *http.Response, method, path string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return &Error{
		Code:    resp.StatusCode,
		Message: strings.TrimSpace(string(msg)),
		Method:  method,
		Path:    path,
	}
}

// durationString renders a duration the way the control-plane API accepts one: Go's own syntax, which
// is what the engine's flags and JSON fields already parse.
func durationString(d time.Duration) string { return d.String() }
