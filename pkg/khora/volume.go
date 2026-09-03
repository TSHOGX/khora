package khora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// The control-plane API volume verbs. A volume's lifetime is INDEPENDENT of any sandbox's — that is
// the whole point of the subsystem — so none of these takes a sandbox ID. A volume is
// attached by naming it in a sandbox's `volumes` binding at create or on a patch.

// CreateVolume creates a volume. Size 0 means uncapped.
func (c *Client) CreateVolume(ctx context.Context, size int64) (Volume, error) {
	return c.CreateVolumeWithKey(ctx, size, "")
}

// CreateVolumeWithKey creates a volume with a durable idempotency key.
func (c *Client) CreateVolumeWithKey(ctx context.Context, size int64, key IdempotencyKey) (Volume, error) {
	var out Volume
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/volumes", VolumeCreateRequest{Size: size}, &out,
		map[string]string{"Idempotency-Key": string(key)})
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
	return c.deriveWithKey(ctx, volumeID, "snapshot", size, "")
}

func (c *Client) SnapshotVolumeWithKey(ctx context.Context, volumeID string, size int64, key IdempotencyKey) (string, error) {
	return c.deriveWithKey(ctx, volumeID, "snapshot", size, key)
}

// CloneVolume derives a new independent volume from this one. Returns the new volume's
// ID.
//
// Snapshot and clone are separate verbs because they answer different questions, and
// this client keeps them separate rather than folding them into one call with a boolean
// — a flag at the call site reads as an implementation detail of a single operation,
// which these are not.
func (c *Client) CloneVolume(ctx context.Context, volumeID string, size int64) (string, error) {
	return c.deriveWithKey(ctx, volumeID, "clone", size, "")
}

func (c *Client) CloneVolumeWithKey(ctx context.Context, volumeID string, size int64, key IdempotencyKey) (string, error) {
	return c.deriveWithKey(ctx, volumeID, "clone", size, key)
}

func (c *Client) derive(ctx context.Context, volumeID, verb string, size int64) (string, error) {
	return c.deriveWithKey(ctx, volumeID, verb, size, "")
}

func (c *Client) deriveWithKey(ctx context.Context, volumeID, verb string, size int64, key IdempotencyKey) (string, error) {
	var out Volume
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/volumes/"+volumeID+"/"+verb,
		VolumeCreateRequest{Size: size}, &out, map[string]string{"Idempotency-Key": string(key)})
	if err != nil {
		return "", err
	}
	return out.VolumeID, nil
}

func volumePathURL(volumeID, verb, name string, offset, length *int64) string {
	q := url.Values{}
	q.Set("path", name)
	if offset != nil {
		q.Set("offset", strconv.FormatInt(*offset, 10))
	}
	if length != nil {
		q.Set("length", strconv.FormatInt(*length, 10))
	}
	return "/v1/volumes/" + url.PathEscape(volumeID) + "/" + verb + "?" + q.Encode()
}

// StatVolume reads root-confined LSTAT metadata.
func (c *Client) StatVolume(ctx context.Context, volumeID, name string) (VolumeFileInfo, error) {
	var out VolumeFileInfo
	err := c.do(ctx, http.MethodGet, volumePathURL(volumeID, "stat", name, nil, nil), nil, &out)
	return out, err
}

// LstatVolume reads metadata for the final entry itself, preserving symlink
// type and target.
func (c *Client) LstatVolume(ctx context.Context, volumeID, name string) (VolumeFileInfo, error) {
	var out VolumeFileInfo
	err := c.do(ctx, http.MethodGet, volumePathURL(volumeID, "lstat", name, nil, nil), nil, &out)
	return out, err
}

// ListVolume lists one root-confined directory. An empty name addresses the root.
func (c *Client) ListVolume(ctx context.Context, volumeID, name string) ([]VolumeFileInfo, error) {
	if name == "" {
		name = "."
	}
	var out []VolumeFileInfo
	err := c.do(ctx, http.MethodGet, volumePathURL(volumeID, "list", name, nil, nil), nil, &out)
	return out, err
}

// ReadVolume reads a bounded range (length <= 0 means to EOF).
func (c *Client) ReadVolume(ctx context.Context, volumeID, name string, offset, length int64) ([]byte, error) {
	var out bytes.Buffer
	var o, l *int64
	o = &offset
	l = &length
	err := c.doRaw(ctx, http.MethodGet, volumePathURL(volumeID, "read", name, o, l), nil, &out)
	return out.Bytes(), err
}

// WriteVolume writes raw bytes at offset and returns the server's byte count.
func (c *Client) WriteVolume(ctx context.Context, volumeID, name string, offset int64, data []byte) (VolumeWriteResult, error) {
	var raw bytes.Buffer
	if err := c.doRaw(ctx, http.MethodPut, volumePathURL(volumeID, "write", name, &offset, nil), bytes.NewReader(data), &raw); err != nil {
		return VolumeWriteResult{}, err
	}
	var out VolumeWriteResult
	if err := json.Unmarshal(raw.Bytes(), &out); err != nil {
		return VolumeWriteResult{}, err
	}
	return out, nil
}

// MkdirVolume creates one root-confined directory.
func (c *Client) MkdirVolume(ctx context.Context, volumeID, name string, mode uint32) (VolumeFileInfo, error) {
	q := url.Values{"path": {name}, "mode": {strconv.FormatUint(uint64(mode), 10)}}
	var out VolumeFileInfo
	err := c.do(ctx, http.MethodPost, "/v1/volumes/"+url.PathEscape(volumeID)+"/mkdir?"+q.Encode(), nil, &out)
	return out, err
}

// MoveVolume renames one root-confined path.
func (c *Client) MoveVolume(ctx context.Context, volumeID, source, destination string) (VolumeFileInfo, error) {
	var out VolumeFileInfo
	err := c.do(ctx, http.MethodPost, "/v1/volumes/"+url.PathEscape(volumeID)+"/move", struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}{source, destination}, &out)
	return out, err
}

// RemoveVolumePath removes one root-confined path. Removing an absent path is
// idempotent per the Root contract.
func (c *Client) RemoveVolumePath(ctx context.Context, volumeID, name string) error {
	q := url.Values{"path": {name}}
	return c.do(ctx, http.MethodDelete, "/v1/volumes/"+url.PathEscape(volumeID)+"/remove?"+q.Encode(), nil, nil)
}

// decodeJSON decodes r into out. Used by the streaming routes that answer with JSON,
// which cannot go through do() because their REQUEST body is a stream.
func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}
