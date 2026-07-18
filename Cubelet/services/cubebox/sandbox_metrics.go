// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Sandbox metrics are served as a Cubelet gRPC method and collected from envd
// on demand. The gRPC request/response shape is defined in cubebox.proto; this
// file implements the server side and the envd HTTP fetch.
//
// Flow with fallback branch:
//
//	CubeMaster
//	   |
//	   | gRPC GetSandboxMetrics(sandboxID)
//	   v
//	Cubelet services/cubebox
//	   |
//	   | 1. load CubeBox metadata from local store
//	   | 2. build a fallback snapshot from resource quotas
//	   | 3. GET http://<sandbox-ip>:49983/metrics from envd
//	   |
//	   +-- envd returns JSON
//	   |      |
//	   |      v
//	   |   merge live usage into fallback snapshot
//	   |
//	   +-- envd request fails or old envd has no endpoint
//	          |
//	          v
//	       keep fallback snapshot
//	   |
//	   v
//	return one current metrics point to CubeMaster
//
// If envd is not reachable, Cubelet still returns the fallback snapshot so old
// templates or short startup races do not make the metrics API fail outright.
package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const bytesPerMiB = int64(1024 * 1024)

const (
	envdMetricsPath    = "/metrics"
	envdMetricsTimeout = 500 * time.Millisecond
)

type envdSandboxMetrics struct {
	Timestamp      int64   `json:"ts"`
	CPUCount       int64   `json:"cpu_count"`
	CPUUsedPercent float64 `json:"cpu_used_pct"`
	MemTotal       int64   `json:"mem_total"`
	MemUsed        int64   `json:"mem_used"`
	MemCache       int64   `json:"mem_cache"`
	DiskUsed       int64   `json:"disk_used"`
	DiskTotal      int64   `json:"disk_total"`

	// Older envd builds reported memory in MiB. Keep these as a compatibility
	// bridge so mixed template fleets still produce useful totals.
	MemTotalMiB int64 `json:"mem_total_mib"`
	MemUsedMiB  int64 `json:"mem_used_mib"`
}

// GetSandboxMetrics implements the Cubelet gRPC endpoint used by CubeMaster to
// fetch one sandbox's current metrics snapshot.
func (s *service) GetSandboxMetrics(ctx context.Context, req *cubebox.GetSandboxMetricsRequest) (*cubebox.GetSandboxMetricsResponse, error) {
	sandboxID := strings.TrimSpace(req.GetSandboxID())
	rsp := &cubebox.GetSandboxMetricsResponse{
		RequestID: req.GetRequestID(),
		SandboxID: sandboxID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if sandboxID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "sandboxID is required"
		return rsp, nil
	}
	if err := pathutil.ValidateSafeID(sandboxID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid sandboxID: %v", err)
		return rsp, nil
	}
	if s.cubeboxMgr == nil || s.cubeboxMgr.cubeboxManger == nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "cubebox manager is not ready"
		return rsp, nil
	}
	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		if errors.Is(err, utils.ErrorKeyNotFound) || utils.IsNotFound(err) {
			rsp.Ret.RetMsg = fmt.Sprintf("sandbox %s not found", sandboxID)
		} else {
			rsp.Ret.RetMsg = fmt.Sprintf("failed to get sandbox %s: %v", sandboxID, err)
		}
		return rsp, nil
	}
	metric := currentSandboxMetric(cb, time.Now())
	if err := s.cubeboxMgr.mergeEnvdSandboxMetric(ctx, cb, metric); err != nil {
		log.G(ctx).Warnf("GetSandboxMetrics envd metrics fallback sandbox=%s: %v", sandboxID, err)
	}
	rsp.Metrics = []*cubebox.SandboxMetric{metric}
	return rsp, nil
}

// mergeEnvdSandboxMetric overlays envd's live usage metrics onto the fallback
// snapshot built from CubeBox metadata.
func (l *local) mergeEnvdSandboxMetric(ctx context.Context, cb *cubeboxstore.CubeBox, metric *cubebox.SandboxMetric) error {
	if cb == nil || metric == nil {
		return nil
	}
	sandboxIP := strings.TrimSpace(cb.IP)
	if sandboxIP == "" || sandboxIP == "<nil>" {
		return fmt.Errorf("sandbox IP is empty")
	}

	envdMetric, err := l.getEnvdSandboxMetric(ctx, sandboxIP)
	if err != nil {
		return err
	}
	if envdMetric.Timestamp > 0 {
		metric.TimestampUnixNano = time.Unix(envdMetric.Timestamp, 0).UnixNano()
	}
	if envdMetric.CPUCount > 0 {
		metric.CpuCount = int32(envdMetric.CPUCount)
	}
	metric.CpuUsedPct = envdMetric.CPUUsedPercent
	if envdMetric.MemTotal > 0 {
		metric.MemTotal = envdMetric.MemTotal
	} else if envdMetric.MemTotalMiB > 0 {
		metric.MemTotal = envdMetric.MemTotalMiB * bytesPerMiB
	}
	if envdMetric.MemUsed > 0 {
		metric.MemUsed = envdMetric.MemUsed
	} else if envdMetric.MemUsedMiB > 0 {
		metric.MemUsed = envdMetric.MemUsedMiB * bytesPerMiB
	}
	metric.MemCache = envdMetric.MemCache
	metric.DiskUsed = envdMetric.DiskUsed
	if envdMetric.DiskTotal > 0 {
		metric.DiskTotal = envdMetric.DiskTotal
	}
	return nil
}

// getEnvdSandboxMetric fetches envd's E2B-compatible JSON metrics endpoint from
// inside the sandbox network namespace.
func (l *local) getEnvdSandboxMetric(ctx context.Context, sandboxIP string) (*envdSandboxMetrics, error) {
	reqCtx, cancel := context.WithTimeout(ctx, envdMetricsTimeout)
	defer cancel()

	reqURL := formatURL("http", sandboxIP, l.getEnvdInitPort(), envdMetricsPath)
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build envd metrics request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent("sandbox-metrics"))

	resp, err := l.getEnvdHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("envd metrics request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read envd metrics response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("envd metrics returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var metric envdSandboxMetrics
	if err := json.Unmarshal(respBody, &metric); err != nil {
		return nil, fmt.Errorf("decode envd metrics response: %w", err)
	}
	return &metric, nil
}

func currentSandboxMetric(cb *cubeboxstore.CubeBox, now time.Time) *cubebox.SandboxMetric {
	if cb == nil {
		return &cubebox.SandboxMetric{TimestampUnixNano: now.UnixNano()}
	}
	return &cubebox.SandboxMetric{
		TimestampUnixNano: now.UnixNano(),
		CpuCount:          sandboxCPUCount(cb),
		CpuUsedPct:        0,
		MemUsed:           0,
		MemTotal:          sandboxMemoryTotalBytes(cb),
		MemCache:          0,
		DiskUsed:          0,
		DiskTotal:         sandboxDiskTotalBytes(cb),
	}
}

func sandboxCPUCount(cb *cubeboxstore.CubeBox) int32 {
	if cb == nil {
		return 0
	}
	if cb.ResourceWithOverHead != nil {
		if milli := cb.ResourceWithOverHead.VmCpuQ.MilliValue(); milli > 0 {
			return int32((milli + 999) / 1000)
		}
		if milli := cb.ResourceWithOverHead.HostCpuQ.MilliValue(); milli > 0 {
			return int32((milli + 999) / 1000)
		}
	}
	if res := firstContainerResources(cb); res != nil {
		if v := quantityMilli(res.GetCpuLimit()); v > 0 {
			return int32((v + 999) / 1000)
		}
		if v := quantityMilli(res.GetCpu()); v > 0 {
			return int32((v + 999) / 1000)
		}
	}
	return 0
}

func sandboxMemoryTotalBytes(cb *cubeboxstore.CubeBox) int64 {
	if cb == nil {
		return 0
	}
	if cb.ResourceWithOverHead != nil {
		if v := cb.ResourceWithOverHead.VmMemQ.Value(); v > 0 {
			return v
		}
		if v := cb.ResourceWithOverHead.HostMemQ.Value(); v > 0 {
			return v
		}
		if v := cb.ResourceWithOverHead.MemReq.Value(); v > 0 {
			return v
		}
	}
	if res := firstContainerResources(cb); res != nil {
		if v := quantityValue(res.GetMemLimit()); v > 0 {
			return v
		}
		if v := quantityValue(res.GetMem()); v > 0 {
			return v
		}
	}
	return 0
}

func sandboxDiskTotalBytes(cb *cubeboxstore.CubeBox) int64 {
	if cb == nil || cb.ResourceWithOverHead == nil {
		return 0
	}
	totalMB := cb.ResourceWithOverHead.HostDataDiskMB + cb.ResourceWithOverHead.HostStorageDiskMB
	if totalMB <= 0 {
		return 0
	}
	return totalMB * bytesPerMiB
}

func firstContainerResources(cb *cubeboxstore.CubeBox) *cubebox.Resource {
	if cb == nil {
		return nil
	}
	id := cb.FirstContainerName
	if id == "" {
		id = cb.ID
	}
	var c *cubeboxstore.Container
	if cb.ContainersMap != nil {
		if got, err := cb.ContainersMap.Get(id); err == nil {
			c = got
		}
	}
	if c == nil && cb.Containers != nil {
		c = cb.Containers[id]
	}
	if c == nil || c.Config == nil {
		return nil
	}
	return c.Config.GetResources()
}

func quantityMilli(raw string) int64 {
	q, err := resource.ParseQuantity(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func quantityValue(raw string) int64 {
	q, err := resource.ParseQuantity(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return q.Value()
}
