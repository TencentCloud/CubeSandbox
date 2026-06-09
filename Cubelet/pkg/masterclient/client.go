// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package masterclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	corev1 "k8s.io/api/core/v1"
)

type ResourceSnapshot struct {
	MilliCPU int64 `json:"milli_cpu,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`
}

// ComponentVersion describes the version of a single component installed on
// this node. Reported to CubeMaster on register and heartbeat. Source is one
// of "manifest" | "binary" | "file".
type ComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Source    string `json:"source,omitempty"`
}

type RegisterNodeRequest struct {
	RequestID           string             `json:"requestID,omitempty"`
	NodeID              string             `json:"node_id,omitempty"`
	HostIP              string             `json:"host_ip,omitempty"`
	GRPCPort            int                `json:"grpc_port,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Capacity            ResourceSnapshot   `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot   `json:"allocatable,omitempty"`
	InstanceType        string             `json:"instance_type,omitempty"`
	ClusterLabel        string             `json:"cluster_label,omitempty"`
	QuotaCPU            int64              `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64              `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64              `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64              `json:"max_mvm_num,omitempty"`
	Versions            []ComponentVersion `json:"versions,omitempty"`
}

type UpdateNodeStatusRequest struct {
	RequestID      string                           `json:"requestID,omitempty"`
	Conditions     []corev1.NodeCondition           `json:"conditions,omitempty"`
	Images         []cubeletnodemeta.ContainerImage `json:"images,omitempty"`
	LocalTemplates []cubeletnodemeta.LocalTemplate  `json:"local_templates,omitempty"`
	HeartbeatTime  time.Time                        `json:"heartbeat_time,omitempty"`

	Allocated  *AllocatedResources `json:"allocated,omitempty"`
	DiskUsage  *DiskUsage          `json:"disk_usage,omitempty"`
	MetricTime time.Time           `json:"metric_time,omitempty"`

	Versions []ComponentVersion `json:"versions,omitempty"`
}

// AllocatedResources represents sandbox-quota resources already committed by
// this cubelet, aggregated across all good-state sandboxes.
//
// Field naming aligns with CubeMaster's Redis HSET schema (RedisNodeInfo).
type AllocatedResources struct {
	MilliCPU      int64 `json:"milli_cpu,omitempty"`
	MemoryMB      int64 `json:"memory_mb,omitempty"`
	MvmNum        int64 `json:"mvm_num,omitempty"`
	MvmRunningNum int64 `json:"mvm_running_num,omitempty"`
	NicQueues     int64 `json:"nic_queues,omitempty"`

	DataDiskMB    int64 `json:"data_disk_mb,omitempty"`
	StorageDiskMB int64 `json:"storage_disk_mb,omitempty"`
}

// DiskUsage carries statfs / cubecow snapshots of disk fill percentage for
// the data, storage and system filesystems on the cubelet host. Values are
// 0~100 (percentage). Omit any dimension that cannot be observed.
type DiskUsage struct {
	DataDiskUsagePer    float64 `json:"data_disk_usage_per,omitempty"`
	StorageDiskUsagePer float64 `json:"storage_disk_usage_per,omitempty"`
	SysDiskUsagePer     float64 `json:"sys_disk_usage_per,omitempty"`
}

type Client struct {
	baseURLs   []string
	cursor     uint32
	httpClient *http.Client
}

type responseEnvelope struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret,omitempty"`
}

// New constructs a master client. endpoint may be a single host:port, a full
// URL, or a comma-separated list of either. When multiple endpoints are
// provided the client tries them in round-robin order on each request and
// fails over to the next on connection errors, providing simple HA against
// multiple cubemaster replicas without external service discovery.
func New(endpoint string, timeout time.Duration) *Client {
	return &Client{
		baseURLs: parseEndpoints(endpoint),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func parseEndpoints(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, strings.TrimRight(p, "/"))
	}
	return out
}

func (c *Client) Readyz(ctx context.Context) error {
	return c.get(ctx, "/internal/meta/readyz")
}

func (c *Client) RegisterNode(ctx context.Context, req *RegisterNodeRequest) error {
	return c.post(ctx, "/internal/meta/nodes/register", req)
}

func (c *Client) UpdateNodeStatus(ctx context.Context, nodeID string, req *UpdateNodeStatusRequest) error {
	return c.post(ctx, "/internal/meta/nodes/"+nodeID+"/status", req)
}

func (c *Client) get(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) post(ctx context.Context, path string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, body)
}

// do walks the configured endpoints round-robin and falls over on transport
// errors. HTTP-level errors (>=300) are returned without failover because
// they indicate a reachable peer that explicitly rejected the request, which
// should bubble up to the caller for retry on the next heartbeat.
func (c *Client) do(ctx context.Context, method, path string, body []byte) error {
	if len(c.baseURLs) == 0 {
		return fmt.Errorf("masterclient: no endpoint configured")
	}
	start := int(atomic.AddUint32(&c.cursor, 1)-1) % len(c.baseURLs)
	var lastErr error
	for i := 0; i < len(c.baseURLs); i++ {
		base := c.baseURLs[(start+i)%len(c.baseURLs)]
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, reqBody)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request %s %s failed: %w", method, base+path, err)
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				lastErr = fmt.Errorf("request %s failed: %s", path, resp.Status)
				return
			}
			lastErr = decodeRet(resp.Body)
		}()
		return lastErr
	}
	return lastErr
}

func decodeRet(body io.Reader) error {
	var envelope responseEnvelope
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Ret != nil && envelope.Ret.RetCode != 200 {
		return fmt.Errorf("request failed: %s", envelope.Ret.RetMsg)
	}
	return nil
}
