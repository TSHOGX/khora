package khora

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

func (c *Client) Telemetry(ctx context.Context, sandboxID string, after uint64, limit int) (TelemetryPage, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatUint(after, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/telemetry"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out TelemetryPage
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
