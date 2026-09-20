package khora

import (
	"context"
	"net/http"
	"net/url"
)

// SandboxBinding is a configuration snapshot serialized with lifecycle changes.
// The validator covers this declaration and its placement; use it unchanged for
// conditional mutation or compare the declaration after an ambiguous response.
type SandboxBinding struct {
	SandboxID        string          `json:"sandbox_id"`
	MutationRevision string          `json:"mutation_revision"`
	State            string          `json:"state"`
	PlacementEpoch   uint64          `json:"placement_epoch"`
	PlacementState   string          `json:"placement_state"`
	TemplateID       string          `json:"template_id"`
	CPUMillis        int64           `json:"cpu_millis"`
	MemoryByte       int64           `json:"memory_byte"`
	DiskByte         int64           `json:"disk_byte"`
	IdleTimeout      string          `json:"idle_timeout"`
	Retention        string          `json:"retention"`
	Network          Network         `json:"network"`
	Volumes          []VolumeBinding `json:"volumes"`
}

func (c *Client) Binding(ctx context.Context, id string) (SandboxBinding, error) {
	var result SandboxBinding
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/binding", nil, &result)
	return result, err
}
