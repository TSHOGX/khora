package khora

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// The the documented invariant volume verbs. A volume's lifetime is INDEPENDENT of any sandbox's — that is
// the whole point of the subsystem — so none of these takes a sandbox ID. A volume is
// attached by naming it in a sandbox's `volumes` binding at create or on a patch.

// CreateVolume creates a volume. Size 0 means uncapped.
func (c *Client) CreateVolume(ctx context.Context, size int64) (Volume, error) {
	var out Volume
	err := c.do(ctx, http.MethodPost, "/v1/volumes", VolumeCreateRequest{Size: size}, &out)
	return out, err
}

// Volumes lists volumes. As with the sandbox list, UsedByte is absent on entries —
// reading it is one backend round trip per volume, which a list must not pay N times.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var out []Volume
	err := c.do(ctx, http.MethodGet, "/v1/volumes", nil, &out)
	return out, err
}

// Volume reads one volume, including its usage.
func (c *Client) Volume(ctx context.Context, volumeID string) (Volume, error) {
	var out Volume
	err := c.do(ctx, http.MethodGet, "/v1/volumes/"+volumeID, nil, &out)
	return out, err
}

// ResizeVolume changes a volume's cap.
//
// A cap that is not STRICTLY GREATER than current usage is refused (422) rather than
// applied — shrinking below what is stored would make the quota unenforceable rather
// than free any space. Read Volume().UsedByte first if you need to know the floor.
func (c *Client) ResizeVolume(ctx context.Context, volumeID string, size int64) (Volume, error) {
	var out Volume
	err := c.do(ctx, http.MethodPatch, "/v1/volumes/"+volumeID, VolumePatchRequest{Size: &size}, &out)
	return out, err
}

// DeleteVolume deletes a volume. A volume some sandbox still binds is a 409
// (IsConflict) — detach it first by patching that sandbox's `volumes`.
func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/volumes/"+volumeID, nil, nil)
}

// SnapshotVolume derives a new volume from this one's current content. Size 0 means
// uncapped. Returns the new volume's ID.
func (c *Client) SnapshotVolume(ctx context.Context, volumeID string, size int64) (string, error) {
	return c.derive(ctx, volumeID, "snapshot", size)
}

// CloneVolume derives a new independent volume from this one. Returns the new volume's
// ID.
//
// Snapshot and clone are separate verbs because they answer different questions, and
// this client keeps them separate rather than folding them into one call with a boolean
// — a flag at the call site reads as an implementation detail of a single operation,
// which these are not.
func (c *Client) CloneVolume(ctx context.Context, volumeID string, size int64) (string, error) {
	return c.derive(ctx, volumeID, "clone", size)
}

func (c *Client) derive(ctx context.Context, volumeID, verb string, size int64) (string, error) {
	var out Volume
	err := c.do(ctx, http.MethodPost, "/v1/volumes/"+volumeID+"/"+verb,
		VolumeCreateRequest{Size: size}, &out)
	if err != nil {
		return "", err
	}
	return out.VolumeID, nil
}

// decodeJSON decodes r into out. Used by the streaming routes that answer with JSON,
// which cannot go through do() because their REQUEST body is a stream.
func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}
