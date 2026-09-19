package khora

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
)

func mutationHeaders(revision string) (map[string]string, error) {
	if len(revision) != 64 {
		return nil, fmt.Errorf("mutation revision from Get is required")
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return nil, fmt.Errorf("invalid mutation revision")
	}
	return map[string]string{"If-Match": strconv.Quote(revision)}, nil
}

// Conditional writes preserve the original revision on every retry. A 412 says
// the original observation no longer authorizes mutation, including after a
// lost successful response. Inspect and reconcile; never silently refresh and
// retry the old intent against a newer placement.
func (c *Client) PauseIfMatch(ctx context.Context, id, revision string) (VerbResult, error) {
	headers, err := mutationHeaders(revision)
	if err != nil {
		return VerbResult{}, err
	}
	var out VerbResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/pause", nil, &out, headers)
	return out, err
}
func (c *Client) ResumeIfMatch(ctx context.Context, id, revision string) (VerbResult, error) {
	headers, err := mutationHeaders(revision)
	if err != nil {
		return VerbResult{}, err
	}
	var out VerbResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/resume", nil, &out, headers)
	return out, err
}
func (c *Client) DestroyIfMatch(ctx context.Context, id, revision string) error {
	headers, err := mutationHeaders(revision)
	if err != nil {
		return err
	}
	return c.doWithHeaders(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, headers)
}
func (c *Client) PatchIfMatch(ctx context.Context, id, revision string, request PatchRequest) (Sandbox, error) {
	headers, err := mutationHeaders(revision)
	if err != nil {
		return Sandbox{}, err
	}
	var out Sandbox
	err = c.doWithHeaders(ctx, http.MethodPatch, "/v1/sandboxes/"+id, request, &out, headers)
	return out, err
}
func IsPreconditionFailed(err error) bool { return codeIs(err, http.StatusPreconditionFailed) }
