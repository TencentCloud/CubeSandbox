// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubemaster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a thin HTTP client wrapping CubeMaster REST API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a CubeMaster client pointing at baseURL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: trimTrailingSlash(baseURL),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
			},
		},
	}
}

// GetNodes fetches cluster node information from CubeMaster.
func (c *Client) GetNodes(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/internal/meta/nodes")
}

// ClusterOverview fetches cluster overview from CubeMaster.
func (c *Client) ClusterOverview(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/internal/meta/cluster/overview")
}

// ClusterVersions fetches version information from CubeMaster.
func (c *Client) ClusterVersions(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/internal/meta/version-matrix")
}

// GetSandbox fetches sandbox detail from CubeMaster.
func (c *Client) GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
	return c.get(ctx, fmt.Sprintf("/cube/sandbox/info?sandbox_id=%s&instance_type=%s", sandboxID, instanceType))
}

// GetNode fetches a single node's detail from CubeMaster.
func (c *Client) GetNode(ctx context.Context, nodeID string) (json.RawMessage, error) {
	return c.get(ctx, fmt.Sprintf("/internal/meta/nodes/%s", nodeID))
}

// ListSandboxes fetches the sandbox list from CubeMaster.
func (c *Client) ListSandboxes(ctx context.Context) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/list", map[string]interface{}{
		"start_idx": 1,
		"size":      500,
	})
}

// CreateSandbox creates a sandbox via CubeMaster.
func (c *Client) CreateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox", body)
}

// DeleteSandbox deletes a sandbox via CubeMaster.
func (c *Client) DeleteSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.deleteWithBody(ctx, "/cube/sandbox", body)
}

// CreateSnapshot creates a snapshot via CubeMaster.
func (c *Client) CreateSnapshot(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/snapshot", body)
}

// GetTemplate fetches template info from CubeMaster.
func (c *Client) GetTemplate(ctx context.Context, templateID string) (json.RawMessage, error) {
	return c.get(ctx, fmt.Sprintf("/cube/template/%s", templateID))
}

// DeleteSnapshot deletes a snapshot via CubeMaster.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshotID string) (json.RawMessage, error) {
	return c.delete(ctx, fmt.Sprintf("/cube/snapshot/%s", snapshotID))
}

// RollbackSandbox rolls back a sandbox to a snapshot via CubeMaster.
func (c *Client) RollbackSandbox(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, fmt.Sprintf("/cube/sandbox/%s/rollback", sandboxID), body)
}

// UpdateSandbox sends a pause/resume action to CubeMaster.
func (c *Client) UpdateSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/update", body)
}

// ConnectSandbox resumes a paused sandbox via CubeMaster (POST /cube/sandbox/connect).
func (c *Client) ConnectSandbox(ctx context.Context, sandboxID string, timeout int) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/connect", map[string]interface{}{
		"request_id":    fmt.Sprintf("req-%d", time.Now().UnixNano()),
		"sandbox_id":    sandboxID,
		"instance_type": "cubebox",
		"timeout":       timeout,
	})
}

// --- internal helpers ---

func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResponse(resp)
}

func (c *Client) post(ctx context.Context, path string, body interface{}) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResponse(resp)
}

func (c *Client) delete(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResponse(resp)
}

func (c *Client) deleteWithBody(ctx context.Context, path string, body interface{}) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResponse(resp)
}

func readResponse(resp *http.Response) (json.RawMessage, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cubemaster returned %d: %s", resp.StatusCode, string(data))
	}
	return json.RawMessage(data), nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
