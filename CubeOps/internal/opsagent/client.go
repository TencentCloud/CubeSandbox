// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package opsagent implements the CubeOps → ops-agent push client: a
// synchronous HTTP call that carries a quota spec to the node-local agent.
package opsagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// Client pushes quota specs to ops-agent instances.
type Client struct {
	port    int
	timeout time.Duration
	token   string
	http    *http.Client
}

// TokenHeader carries the shared secret to ops-agent pushes.
const TokenHeader = "X-Ops-Agent-Token"

// New returns a push client targeting agentPort.
func New(agentPort int, timeout time.Duration, token string) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		port:    agentPort,
		timeout: timeout,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

// PushQuota posts the spec to the agent on hostIP. It returns the agent's
// applied flag; a transport error or non-200 response is an error.
func (c *Client) PushQuota(ctx context.Context, hostIP string, req model.OpsAgentPushRequest) (bool, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	url := fmt.Sprintf("http://%s:%d/api/v1/config/quota", hostIP, c.port)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set(TokenHeader, c.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("agent unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("agent returned %s", resp.Status)
	}
	var out model.OpsAgentPushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode agent response: %w", err)
	}
	if out.Error != "" {
		return false, fmt.Errorf("agent rejected: %s", out.Error)
	}
	return out.Applied, nil
}
