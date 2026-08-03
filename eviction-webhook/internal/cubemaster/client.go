// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cubemaster provides a thin HTTP client for the CubeMaster APIs used
// by eviction-webhook:
//
//   - PUT  /internal/meta/nodes/:node_id/isolation  — cordon a node
//   - DELETE /internal/meta/nodes/:node_id/isolation — uncordon a node
//   - POST /cube/sandbox/update { action:"pause" }   — freeze a MicroVM
//   - POST /cube/sandbox/update { action:"resume" }  — unfreeze a MicroVM
//
// Authentication uses the same HMAC-SHA1 scheme as reporter.authHeaders so the
// client can be used regardless of whether CubeMaster has auth enabled.
package cubemaster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/auth"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/metrics"
)

const (
	httpTimeout                 = 5 * time.Second
	sandboxUpdatePath           = "/cube/sandbox/update"
	sandboxListPath             = "/cube/sandbox/list"
	metaNodesPath               = "/internal/meta/nodes"
	retCodeSuccess              = 200
	retCodeTaskStateInvalid     = 130490
	retCodeTaskStateInvalidName = "TaskStateInvalid"
)

// isolationPath returns the URL path for node isolation operations.
func isolationPath(nodeID string) string {
	return "/internal/meta/nodes/" + nodeID + "/isolation"
}

// metaNodePath returns the URL path for a single node metadata lookup.
func metaNodePath(nodeID string) string {
	return "/internal/meta/nodes/" + nodeID
}

// Client calls CubeMaster REST APIs.
type Client struct {
	baseURL     string
	userID      string
	secretKey   string
	authEnabled bool
	http        *http.Client
}

// New creates a Client. When authEnabled is false, HMAC headers are omitted.
func New(baseURL, userID, secretKey string, authEnabled bool) *Client {
	return &Client{
		baseURL:     baseURL,
		userID:      userID,
		secretKey:   secretKey,
		authEnabled: authEnabled,
		http:        &http.Client{Timeout: httpTimeout},
	}
}

// sandboxUpdateReq is the wire shape for POST /cube/sandbox/update.
type sandboxUpdateReq struct {
	RequestID    string `json:"requestID"`
	SandboxID    string `json:"sandbox_id"`
	InstanceType string `json:"instance_type"`
	Action       string `json:"action"`
}

// cubeMasterRes is the minimal common response envelope from CubeMaster.
type cubeMasterRes struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
}

// listSandboxesReq is the wire shape for POST /cube/sandbox/list.
type listSandboxesReq struct {
	RequestID string `json:"requestID,omitempty"`
	HostID    string `json:"host_id,omitempty"`
	Size      int    `json:"size,omitempty"`
}

// metaNodeResponse is the response envelope from GET /internal/meta/nodes/:node_id.
type metaNodeResponse struct {
	NodeID string `json:"node_id,omitempty"`
	HostIP string `json:"host_ip,omitempty"`
}

// metaNodeEnvelope is the response envelope from GET /internal/meta/nodes/:node_id.
type metaNodeEnvelope struct {
	Ret  *cubeMasterRet   `json:"ret,omitempty"`
	Data metaNodeResponse `json:"data,omitempty"`
}

type cubeMasterRet struct {
	RetCode int    `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
}

// SandboxBrief is the minimal sandbox info returned by ListSandboxesByNode.
// Mirrors a subset of CubeMaster's SandboxBriefData.
// Note: CubeMaster's SandboxBriefData does NOT contain InstanceType —
// it is read from the pod's label by the admission handler and passed
// through via event.InstanceType.
type SandboxBrief struct {
	SandboxID string `json:"sandbox_id,omitempty"`
	Status    int32  `json:"status,omitempty"`
}

// listSandboxesRes is the response envelope from POST /cube/sandbox/list.
type listSandboxesRes struct {
	Ret  *cubeMasterRet `json:"ret"`
	Data []SandboxBrief `json:"data,omitempty"`
}

// IsolateNode cordons nodeID so the scheduler stops placing new Sandboxes there.
// Idempotent — safe to call multiple times.
func (c *Client) IsolateNode(ctx context.Context, nodeID string) error {
	respBody, err := c.do(ctx, http.MethodPut, isolationPath(nodeID), nil)
	if err != nil {
		return fmt.Errorf("IsolateNode %s: %w", nodeID, err)
	}
	if err := checkRetCode(respBody, false); err != nil {
		return fmt.Errorf("IsolateNode %s: %w", nodeID, err)
	}
	log.Printf("[cubemaster] node isolated nodeID=%s", nodeID)
	return nil
}

// UnisolateNode removes the cordon on nodeID so the scheduler can use it again.
// Idempotent — safe to call multiple times.
func (c *Client) UnisolateNode(ctx context.Context, nodeID string) error {
	respBody, err := c.do(ctx, http.MethodDelete, isolationPath(nodeID), nil)
	if err != nil {
		return fmt.Errorf("UnisolateNode %s: %w", nodeID, err)
	}
	if err := checkRetCode(respBody, false); err != nil {
		return fmt.Errorf("UnisolateNode %s: %w", nodeID, err)
	}
	log.Printf("[cubemaster] node unisolated nodeID=%s", nodeID)
	return nil
}

// PauseSandbox freezes the MicroVM for sandboxID.
func (c *Client) PauseSandbox(ctx context.Context, sandboxID, instanceType, requestID string) error {
	return c.updateSandbox(ctx, sandboxID, instanceType, "pause", requestID)
}

// ResumeSandbox unfreezes the MicroVM for sandboxID.
func (c *Client) ResumeSandbox(ctx context.Context, sandboxID, instanceType, requestID string) error {
	return c.updateSandbox(ctx, sandboxID, instanceType, "resume", requestID)
}

// ListSandboxesByNode returns all sandboxes running on the given host (node).
// hostID is resolved via ResolveHostID if the initial lookup fails.
func (c *Client) ListSandboxesByNode(ctx context.Context, hostID string) ([]SandboxBrief, error) {
	body, err := json.Marshal(&listSandboxesReq{
		RequestID: fmt.Sprintf("eviction-list-%d", time.Now().UnixNano()),
		HostID:    hostID,
		Size:      1000, // large enough to get all sandboxes on one node
	})
	if err != nil {
		return nil, err
	}

	respBody, err := c.do(ctx, http.MethodPost, sandboxListPath, body)
	if err != nil {
		return nil, fmt.Errorf("ListSandboxesByNode %s: %w", hostID, err)
	}

	var res listSandboxesRes
	if err := json.Unmarshal(respBody, &res); err != nil {
		return nil, fmt.Errorf("ListSandboxesByNode %s: unmarshal: %w", hostID, err)
	}
	if res.Ret == nil {
		return nil, fmt.Errorf("ListSandboxesByNode %s: missing CubeMaster ret envelope", hostID)
	}
	if res.Ret.RetCode != retCodeSuccess {
		return nil, fmt.Errorf("ListSandboxesByNode %s: CubeMaster ret_code=%d msg=%s",
			hostID, res.Ret.RetCode, res.Ret.RetMsg)
	}

	log.Printf("[cubemaster] listed sandboxes hostID=%s count=%d", hostID, len(res.Data))
	return res.Data, nil
}

// ResolveHostID tries to resolve the CubeMaster internal node_id for a given
// identifier (which might be a K8s node name, an IP, or already the node_id).
// It first tries GET /internal/meta/nodes/:id directly; if that fails, it
// lists all nodes and matches by host_ip.
func (c *Client) ResolveHostID(ctx context.Context, identifier string) (string, error) {
	// Fast path: try the identifier directly.
	respBody, err := c.do(ctx, http.MethodGet, metaNodePath(identifier), nil)
	if err == nil {
		if node, ok := parseMetaNode(respBody); ok {
			return node.NodeID, nil
		}
	}

	// Fallback: list all nodes and match by host_ip.
	respBody, err = c.do(ctx, http.MethodGet, metaNodesPath, nil)
	if err != nil {
		return "", fmt.Errorf("ResolveHostID %s: list nodes: %w", identifier, err)
	}

	var listResp struct {
		Data []metaNodeResponse `json:"data,omitempty"`
	}
	if err := json.Unmarshal(respBody, &listResp); err != nil {
		return "", fmt.Errorf("ResolveHostID %s: unmarshal: %w", identifier, err)
	}

	for _, node := range listResp.Data {
		if node.HostIP == identifier || node.NodeID == identifier {
			return node.NodeID, nil
		}
	}

	return "", fmt.Errorf("ResolveHostID %s: node not found in CubeMaster", identifier)
}

func parseMetaNode(body []byte) (metaNodeResponse, bool) {
	var enveloped metaNodeEnvelope
	if err := json.Unmarshal(body, &enveloped); err == nil && enveloped.Data.NodeID != "" {
		if enveloped.Ret != nil && enveloped.Ret.RetCode != retCodeSuccess {
			return metaNodeResponse{}, false
		}
		return enveloped.Data, true
	}

	var bare metaNodeResponse
	if err := json.Unmarshal(body, &bare); err == nil && bare.NodeID != "" {
		return bare, true
	}

	return metaNodeResponse{}, false
}

func (c *Client) updateSandbox(ctx context.Context, sandboxID, instanceType, action, requestID string) error {
	body, err := json.Marshal(&sandboxUpdateReq{
		RequestID:    requestID,
		SandboxID:    sandboxID,
		InstanceType: instanceType,
		Action:       action,
	})
	if err != nil {
		return err
	}

	respBody, err := c.do(ctx, http.MethodPost, sandboxUpdatePath, body)
	if err != nil {
		return fmt.Errorf("%sSandbox %s: %w", action, sandboxID, err)
	}

	if err := checkRetCode(respBody, true); err != nil {
		return fmt.Errorf("%sSandbox %s: %w", action, sandboxID, err)
	}
	log.Printf("[cubemaster] sandbox action=%s sandboxID=%s", action, sandboxID)
	return nil
}

func checkRetCode(body []byte, allowAlreadyInState bool) error {
	var res cubeMasterRes
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	if res.Ret == nil {
		return fmt.Errorf("missing CubeMaster ret envelope")
	}
	if res.Ret.RetCode == retCodeSuccess {
		return nil
	}
	if allowAlreadyInState && res.Ret.RetCode == retCodeTaskStateInvalid {
		log.Printf("[cubemaster] treating ret_code=%d (%s) as success msg=%s",
			retCodeTaskStateInvalid, retCodeTaskStateInvalidName, res.Ret.RetMsg)
		return nil
	}
	return fmt.Errorf("CubeMaster ret_code=%d msg=%s", res.Ret.RetCode, res.Ret.RetMsg)
}

// do executes an HTTP request against CubeMaster, optionally attaching auth headers.
// Returns the raw response body on HTTP 200; returns an error for other status codes.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	start := time.Now()
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.authEnabled {
		if err := c.setAuthHeaders(req); err != nil {
			return nil, fmt.Errorf("auth headers: %w", err)
		}
	}

	resp, err := c.http.Do(req)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		metrics.CubeMasterAPILatencySeconds.WithLabelValues(method, "error").Observe(elapsed)
		metrics.CubeMasterAPIErrorsTotal.WithLabelValues(method, "http_error").Inc()
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		metrics.CubeMasterAPILatencySeconds.WithLabelValues(method, fmt.Sprintf("%d", resp.StatusCode)).Observe(elapsed)
		metrics.CubeMasterAPIErrorsTotal.WithLabelValues(method, fmt.Sprintf("%d", resp.StatusCode)).Inc()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	metrics.CubeMasterAPILatencySeconds.WithLabelValues(method, "200").Observe(elapsed)
	return respBody, nil
}

// setAuthHeaders attaches the six HMAC-SHA1 headers that CubeMaster's auth
// middleware verifies.
func (c *Client) setAuthHeaders(req *http.Request) error {
	authHdrs, err := auth.Headers(c.userID, c.secretKey)
	if err != nil {
		return fmt.Errorf("build auth headers: %w", err)
	}
	for k, vs := range authHdrs {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return nil
}
