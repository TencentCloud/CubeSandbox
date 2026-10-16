// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package upstream is the single egress point towards CubeOps (pull
// reconcile). Task domains never talk to the control plane directly.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SpecResponse mirrors CubeOps' ops-agent pull payload.
type SpecResponse struct {
	NodeID   string   `json:"node_id"`
	Managed  bool     `json:"managed"`
	Revision int64    `json:"revision"`
	Spec     *Spec    `json:"spec"`
	Physical Physical `json:"physical"`
	// OnlyPausedRatio restricts the reconcile to the ratio field.
	OnlyPausedRatio bool `json:"only_paused_ratio,omitempty"`
}

// Spec mirrors the desired quota shape (see tasks/quota.Spec).
type Spec struct {
	MCpuLimit                  int64   `json:"mcpu_limit"`
	MemLimit                   string  `json:"mem_limit"`
	MvmLimit                   int64   `json:"mvm_limit"`
	CreationConcurrentNum      int64   `json:"creation_concurrent_num"`
	PausedResourceReleaseRatio float64 `json:"paused_resource_release_ratio"`
}

// Physical mirrors the capacity snapshot for node-side guards.
type Physical struct {
	CpuTotal   int64 `json:"cpu_total"`
	MemMBTotal int64 `json:"mem_mb_total"`
}

// Client pulls specs from CubeOps.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a pull client for baseURL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// PullSpec fetches the desired spec for nodeID.
func (c *Client) PullSpec(ctx context.Context, nodeID string) (*SpecResponse, error) {
	url := fmt.Sprintf("%s/internal/v1/ops-agent/%s/config/quota", c.baseURL, nodeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cubeops unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("node %s not registered", nodeID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cubeops returned %s", resp.Status)
	}
	var out SpecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	return &out, nil
}
