// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestCurrentSandboxMetricUsesCubeboxResourceSnapshot(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox-1",
			ResourceWithOverHead: &cubeboxstore.ResourceWithOverHead{
				VmCpuQ:            resource.MustParse("2500m"),
				VmMemQ:            resource.MustParse("512Mi"),
				HostDataDiskMB:    100,
				HostStorageDiskMB: 25,
			},
		},
	}
	now := time.Unix(1700000000, 123)

	metric := currentSandboxMetric(cb, now)

	assert.Equal(t, now.UnixNano(), metric.GetTimestampUnixNano())
	assert.Equal(t, int32(3), metric.GetCpuCount())
	assert.Equal(t, int64(512*1024*1024), metric.GetMemTotal())
	assert.Equal(t, int64(125*1024*1024), metric.GetDiskTotal())
	assert.Zero(t, metric.GetCpuUsedPct())
	assert.Zero(t, metric.GetMemUsed())
	assert.Zero(t, metric.GetMemCache())
	assert.Zero(t, metric.GetDiskUsed())
}

func TestMergeEnvdSandboxMetricUsesEnvdUsageSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, envdMetricsPath, r.URL.Path)
		assert.True(t, strings.HasPrefix(r.Header.Get("User-Agent"), "cubelet-sandbox-metrics/"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ts": 1700000001,
			"cpu_count": 4,
			"cpu_used_pct": 12.5,
			"mem_total": 8192,
			"mem_used": 4096,
			"mem_cache": 512,
			"disk_used": 2048,
			"disk_total": 16384
		}`))
	}))
	defer server.Close()
	host, port := serverHostPort(t, server)
	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox-1",
			ResourceWithOverHead: &cubeboxstore.ResourceWithOverHead{
				VmCpuQ:            resource.MustParse("2000m"),
				VmMemQ:            resource.MustParse("512Mi"),
				HostDataDiskMB:    100,
				HostStorageDiskMB: 25,
			},
		},
		IP: host,
	}
	metric := currentSandboxMetric(cb, time.Unix(1700000000, 0))

	err := l.mergeEnvdSandboxMetric(context.Background(), cb, metric)

	require.NoError(t, err)
	assert.Equal(t, time.Unix(1700000001, 0).UnixNano(), metric.GetTimestampUnixNano())
	assert.Equal(t, int32(4), metric.GetCpuCount())
	assert.Equal(t, 12.5, metric.GetCpuUsedPct())
	assert.Equal(t, int64(4096), metric.GetMemUsed())
	assert.Equal(t, int64(8192), metric.GetMemTotal())
	assert.Equal(t, int64(512), metric.GetMemCache())
	assert.Equal(t, int64(2048), metric.GetDiskUsed())
	assert.Equal(t, int64(16384), metric.GetDiskTotal())
}

func TestMergeEnvdSandboxMetricFallsBackToDeprecatedMemoryMiB(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"cpu_count": 1,
			"cpu_used_pct": 2.5,
			"mem_total_mib": 256,
			"mem_used_mib": 64
		}`))
	}))
	defer server.Close()
	host, port := serverHostPort(t, server)
	l := &local{envdHTTPClient: server.Client(), envdInitPort: port}
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox-1"}, IP: host}
	metric := currentSandboxMetric(cb, time.Unix(1700000000, 0))

	err := l.mergeEnvdSandboxMetric(context.Background(), cb, metric)

	require.NoError(t, err)
	assert.Equal(t, int64(256*1024*1024), metric.GetMemTotal())
	assert.Equal(t, int64(64*1024*1024), metric.GetMemUsed())
	assert.Equal(t, 2.5, metric.GetCpuUsedPct())
}

func TestGetSandboxMetricsFallsBackToResourceSnapshotWhenEnvdUnavailable(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID: "sandbox-1",
			ResourceWithOverHead: &cubeboxstore.ResourceWithOverHead{
				VmCpuQ: resource.MustParse("1000m"),
				VmMemQ: resource.MustParse("128Mi"),
			},
		},
		IP: "127.0.0.1",
	}
	s := &service{
		cubeboxMgr: &local{
			cubeboxManger:  &fakeCubeboxAPI{cb: cb},
			envdHTTPClient: serverThatAlwaysFails(t).Client(),
			envdInitPort:   1,
		},
	}

	rsp, err := s.GetSandboxMetrics(context.Background(), &cubeboxv1.GetSandboxMetricsRequest{SandboxID: "sandbox-1"})

	require.NoError(t, err)
	require.Len(t, rsp.GetMetrics(), 1)
	metric := rsp.GetMetrics()[0]
	assert.Equal(t, int32(1), metric.GetCpuCount())
	assert.Equal(t, int64(128*1024*1024), metric.GetMemTotal())
	assert.Zero(t, metric.GetCpuUsedPct())
	assert.Zero(t, metric.GetMemUsed())
}

func TestCurrentSandboxMetricFallsBackToContainerResources(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sandbox-1"},
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{
				"sandbox-1": {
					Metadata: cubeboxstore.Metadata{
						ID: "sandbox-1",
						Config: &cubeboxv1.ContainerConfig{
							Resources: &cubeboxv1.Resource{
								CpuLimit: "1500m",
								MemLimit: "256Mi",
							},
						},
					},
				},
			},
		},
	}

	metric := currentSandboxMetric(cb, time.Unix(1700000000, 0))

	assert.Equal(t, int32(2), metric.GetCpuCount())
	assert.Equal(t, int64(256*1024*1024), metric.GetMemTotal())
	assert.Zero(t, metric.GetDiskTotal())
}

func TestFirstContainerResourcesMissingContainerIsQuietFallback(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata:      cubeboxstore.Metadata{ID: "sandbox-1"},
		ContainersMap: &cubeboxstore.ContainersMap{},
	}

	assert.Nil(t, firstContainerResources(cb))
	assert.Zero(t, sandboxCPUCount(cb))
	assert.Zero(t, sandboxMemoryTotalBytes(cb))
}

func serverHostPort(t *testing.T, server *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

func serverThatAlwaysFails(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	return server
}
