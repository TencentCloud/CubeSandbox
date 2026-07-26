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
	"net/url"
	"time"
)

// CMError carries a CubeMaster business error (non-zero ret_code) out of
// readResponse so that handlers can map it to the correct HTTP status code
// instead of collapsing every business error into 502. See R11.
type CMError struct {
	RetCode int
	RetMsg  string
}

func (e *CMError) Error() string {
	return fmt.Sprintf("cubemaster error %d: %s", e.RetCode, e.RetMsg)
}

// IsNotFound returns true for CubeMaster "not found" ret codes.
func (e *CMError) IsNotFound() bool {
	return e.RetCode == 130404 || e.RetCode == 404
}

// IsConflict returns true for CubeMaster "conflict" ret codes.
func (e *CMError) IsConflict() bool {
	return e.RetCode == 130409 || e.RetCode == 409
}

// IsPausing returns true for the "sandbox is pausing; retry later" ret code.
// Used by DELETE /sandboxes/:id to return 503 + Retry-After.
func (e *CMError) IsPausing() bool {
	return e.RetCode == 130490
}

// IsResumeFailed returns true for "failed to resume paused sandbox before delete".
// Used by DELETE /sandboxes/:id to return 503 + Retry-After.
func (e *CMError) IsResumeFailed() bool {
	return e.RetCode == 130589
}

// IsCapacity returns true for "resume rejected by capacity policy".
// Used by DELETE /sandboxes/:id to return 409 Conflict.
func (e *CMError) IsCapacity() bool {
	return e.RetCode == 130409
}

// RetryAfter returns the Retry-After value (seconds) for retryable errors,
// or 0 if the error is not retryable.
func (e *CMError) RetryAfter() int {
	switch {
	case e.IsPausing():
		return 2
	case e.IsResumeFailed():
		return 5
	default:
		return 0
	}
}

// Client is a thin HTTP client wrapping CubeMaster REST API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a CubeMaster client pointing at baseURL.
//
// The HTTP client does NOT set a global Timeout — that would break long
// operations like snapshot create/rollback (R12). Instead, per-request
// deadlines are controlled by the context passed by each caller. The
// transport keeps idle connection pooling for efficiency.
func New(baseURL string) *Client {
	return &Client{
		baseURL: trimTrailingSlash(baseURL),
		http: &http.Client{
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
//
// sandboxID and instanceType are passed via query parameters using net/url
// to ensure they are properly percent-encoded. Hand-rolled fmt.Sprintf
// concatenation is unsafe — values containing '&', '=', or '#' could let a
// caller inject extra query parameters or break out of the query string
// entirely. See SECURITY.md for the original advisory.
func (c *Client) GetSandbox(ctx context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
	return c.getWithQuery(ctx, "/cube/sandbox/info", map[string]string{
		"sandbox_id":    sandboxID,
		"instance_type": instanceType,
	})
}

// GetNode fetches a single node's detail from CubeMaster.
//
// nodeID is appended to the path; values.QueryEscape handles any character
// that would otherwise be reserved in the URL path.
func (c *Client) GetNode(ctx context.Context, nodeID string) (json.RawMessage, error) {
	escaped := url.PathEscape(nodeID)
	return c.get(ctx, fmt.Sprintf("/internal/meta/nodes/%s", escaped))
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
	body := map[string]interface{}{
		"request_id": fmt.Sprintf("cubeops-del-snap-%d", time.Now().UnixNano()),
	}
	return c.deleteWithBody(ctx, fmt.Sprintf("/cube/snapshot/%s", snapshotID), body)
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

// --- SDK-facing methods (direct CubeMaster REST calls) ---

// ListSandboxesWithBody lists sandboxes with a custom request body.
func (c *Client) ListSandboxesWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/list", body)
}

// SetSandboxTimeout sets an absolute TTL for a sandbox.
func (c *Client) SetSandboxTimeout(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/timeout", body)
}

// RefreshSandbox extends a sandbox's TTL by a delta.
func (c *Client) RefreshSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/refresh", body)
}

// GetSandboxLogs fetches sandbox stdout/stderr logs.
func (c *Client) GetSandboxLogs(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/logs", body)
}

// ConnectSandboxWithBody resumes a paused sandbox with a custom request body.
func (c *Client) ConnectSandboxWithBody(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/sandbox/connect", body)
}

// ListSnapshots lists snapshots with query parameters.
func (c *Client) ListSnapshots(ctx context.Context, params map[string]string) (json.RawMessage, error) {
	return c.getWithQuery(ctx, "/cube/snapshot", params)
}

// ListTemplates lists templates, or fetches a single one when templateID is non-empty.
func (c *Client) ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error) {
	params := map[string]string{}
	if templateID != "" {
		params["template_id"] = templateID
	}
	if includeRequest {
		params["include_request"] = "true"
	}
	return c.getWithQuery(ctx, "/cube/template", params)
}

// CreateTemplateFromImage creates a template from an OCI image.
func (c *Client) CreateTemplateFromImage(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/template/from-image", body)
}

// RedoTemplate rebuilds an existing template.
func (c *Client) RedoTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/template/redo", body)
}

// DeleteTemplate deletes a template by id.
func (c *Client) DeleteTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.deleteWithBody(ctx, "/cube/template", body)
}

// GetTemplateBuildStatus fetches the build status for a template build job.
func (c *Client) GetTemplateBuildStatus(ctx context.Context, buildID string) (json.RawMessage, error) {
	return c.get(ctx, fmt.Sprintf("/cube/template/build/%s/status", buildID))
}

// StartTemplateBuild starts (or retries) a template build job.
func (c *Client) StartTemplateBuild(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, fmt.Sprintf("/cube/template/build/%s", buildID), body)
}

// GetTemplateCompat fetches the template compatibility matrix.
func (c *Client) GetTemplateCompat(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/cube/template/compat")
}

// AdoptTemplateCompatBaseline adopts the compatibility baseline for a template.
func (c *Client) AdoptTemplateCompatBaseline(ctx context.Context, body interface{}) (json.RawMessage, error) {
	return c.post(ctx, "/cube/template/compat", body)
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

func (c *Client) getWithQuery(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	req.URL.RawQuery = q.Encode()
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
	// Check CubeMaster business error code. CubeMaster uses ret_code=200 for
	// success (and sometimes 0). Any other value is a failure, even when HTTP
	// status is 200. Return a typed CMError so handlers can map ret_code to
	// the correct HTTP status (404/409/503) instead of collapsing to 502.
	var envelope struct {
		Ret struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Ret.RetCode != 0 && envelope.Ret.RetCode != 200 {
		return nil, &CMError{RetCode: envelope.Ret.RetCode, RetMsg: envelope.Ret.RetMsg}
	}
	return json.RawMessage(data), nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
