package khora

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The streaming routes: sandbox files and volume import/export.
//
// EVERY ONE OF THESE TAKES OR RETURNS AN io.Reader, and never a []byte. That is not a
// style preference — it is the same reason these four routes are REST rather than
// Connect-RPC in the first place: a file must not become one message, nor sit whole in
// memory at either end. A convenience `ReadFileBytes` would hand that property back on
// the caller's behalf, so it is not here; a caller who wants the bytes calls
// io.ReadAll and has then chosen the memory cost visibly.
//
// The download and export methods return an io.ReadCloser THE CALLER MUST CLOSE. It is
// the live response body — closing it is what releases the connection, and on a stream
// the caller abandons early, what tells the engine to stop reading from the guest.

// ReadFile opens a file in a running sandbox for reading. Close the returned reader.
//
// Running only: a paused sandbox has no guest to serve the read, so it is a 409
// (IsConflict), and an unreachable guest is a 503 (IsUnavailable).
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string) (io.ReadCloser, error) {
	u, err := c.pathURL("/v1/sandboxes/"+sandboxID+"/files", path)
	if err != nil {
		return nil, err
	}
	return c.stream(ctx, http.MethodGet, u, nil)
}

// WriteFile writes a file in a running sandbox, streaming from body.
//
// NOT ATOMIC AT THE ENGINE LEVEL, and the client cannot make it so: the guest writes
// through a temp file and renames, so a torn write is not visible under the final path,
// but a caller whose request is cut off mid-stream has produced a partial temp file the
// guest will clean up rather than a half-written target. The distinction matters for a
// caller deciding whether a retry is safe — it is, because the target is only ever
// replaced whole.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, body io.Reader) error {
	u, err := c.pathURL("/v1/sandboxes/"+sandboxID+"/files", path)
	if err != nil {
		return err
	}
	rc, err := c.stream(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// ImportVolume writes into a volume directly, without any sandbox.
//
// The point of this route is that it needs NO RUNNING SANDBOX: the host reaches the
// volume through a root-confined accessor, so a caller can seed a volume before
// anything mounts it. `path` is relative to the VOLUME ROOT, and a traversal CLAMPS
// inside the volume rather than erroring — the accessor is confined by construction, so
// there is nothing here to validate and nothing a caller can escape with.
func (c *Client) ImportVolume(ctx context.Context, volumeID, path string, body io.Reader) (VolumeWriteResult, error) {
	u, err := c.pathURL("/v1/volumes/"+volumeID+"/import", path)
	if err != nil {
		return VolumeWriteResult{}, err
	}
	rc, err := c.stream(ctx, http.MethodPost, u, body)
	if err != nil {
		return VolumeWriteResult{}, err
	}
	defer rc.Close()
	var out VolumeWriteResult
	if err := decodeJSON(rc, &out); err != nil {
		return VolumeWriteResult{}, fmt.Errorf("import volume: %w", err)
	}
	return out, nil
}

// ExportVolume reads from a volume directly, without any sandbox. Close the returned
// reader. Same root-confinement as ImportVolume.
func (c *Client) ExportVolume(ctx context.Context, volumeID, path string) (io.ReadCloser, error) {
	u, err := c.pathURL("/v1/volumes/"+volumeID+"/export", path)
	if err != nil {
		return nil, err
	}
	return c.stream(ctx, http.MethodGet, u, nil)
}

// pathURL builds an absolute URL with the `?path=` parameter set. Query-escaped rather
// than concatenated, so a path containing `&` or `#` addresses the file the caller
// meant instead of silently truncating.
func (c *Client) pathURL(route, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	u, err := url.Parse(c.endpoint + route)
	if err != nil {
		return "", fmt.Errorf("build URL: %w", err)
	}
	q := u.Query()
	q.Set("path", path)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// stream issues a request whose response body is handed to the caller OPEN. The error
// path closes it; the success path must not, which is why this cannot reuse do().
func (c *Client) stream(ctx context.Context, method, url string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, errorFrom(resp, method, req.URL.Path)
	}
	return resp.Body, nil
}
