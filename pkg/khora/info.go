package khora

import (
	"context"
	"net/http"
)

// Info identifies the durable state and running build, independently of uptime.
type Info struct {
	StoreID string `json:"store_id"`
	Version string `json:"version"`
	// ManualColdRecovery means replacement placements require an explicit Resume.
	ManualColdRecovery bool `json:"manual_cold_recovery"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.do(ctx, http.MethodGet, "/v1/info", nil, &info)
	return info, err
}
