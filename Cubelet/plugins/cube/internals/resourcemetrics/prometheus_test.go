package resourcemetrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/api/types"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type countingGuestMetricsReader struct{ calls int }

func (r *countingGuestMetricsReader) Metrics(context.Context, string) (*types.Metric, error) {
	r.calls++
	return nil, nil
}

type countingHostUsageReader struct{ calls int }

func (r *countingHostUsageReader) UsageSnapshot(context.Context, string) (handle.UsageSnapshot, error) {
	r.calls++
	return handle.UsageSnapshot{}, nil
}

func TestPrometheusHandlerExportsAvailableGuestAndHostSnapshotsWithoutCollecting(t *testing.T) {
	guestReader := &countingGuestMetricsReader{}
	hostReader := &countingHostUsageReader{}
	guestLimit := int64(750)
	readyAt := time.Unix(105, 0).UTC()
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		reader: guestReader,
		latest: map[string]GuestWorkloadLatest{
			"sandbox/sandbox": {
				Workload:            WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"},
				EpochGeneration:     3,
				EpochStartedAt:      time.Unix(100, 0).UTC(),
				EpochReadyAt:        &readyAt,
				CollectedAt:         time.Unix(110, 0).UTC(),
				Availability:        GuestWorkloadAvailable,
				CumulativeAvailable: true,
				CPULimitMillicores:  &guestLimit,
				Snapshot: &GuestWorkloadSnapshot{
					Timestamp:  time.Unix(110, 0).UTC(),
					Workload:   WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"},
					BaselineID: "sandbox/sandbox/3",
					Counters: GuestWorkloadCounters{
						CPUUsageTotalNS:          1_500_000_000,
						CPUUserTotalNS:           1_000_000_000,
						CPUSystemTotalNS:         500_000_000,
						CPUThrottledTotalNS:      250_000_000,
						CPUPeriodsTotal:          20,
						CPUThrottledPeriodsTotal: 4,
						MemoryFailuresTotal:      2,
					},
					MemoryCurrentBytes: 4096,
					MemoryLimitBytes:   16384,
				},
				MemoryCurrentBytes: 4096,
				MemoryLimitBytes:   16384,
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		reader: hostReader,
		latest: map[string]HostSandboxLatest{
			"sandbox": {
				SandboxID:    "sandbox",
				CollectedAt:  time.Unix(111, 0).UTC(),
				Availability: HostSandboxAvailable,
				Snapshot: &HostSandboxSnapshot{
					Timestamp:                time.Unix(111, 0).UTC(),
					SandboxID:                "sandbox",
					CPUUsageTotalNS:          2_000_000_000,
					CPUUserTotalNS:           1_250_000_000,
					CPUSystemTotalNS:         750_000_000,
					CPUThrottledTotalNS:      100_000_000,
					CPUPeriodsTotal:          30,
					CPUThrottledPeriodsTotal: 5,
					CPULimitQuotaUS:          150000,
					CPULimitPeriodUS:         100000,
					MemoryCurrentBytes:       8192,
					MemoryLimitBytes:         32768,
					MemoryFailuresTotal:      3,
				},
			},
		},
	}
	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)
	handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil)
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()

	for _, expected := range []string{
		"# TYPE cubesandbox_guest_workload_cpu_usage_seconds_total counter",
		"cubesandbox_guest_workload_cpu_usage_seconds_total",
		"cubesandbox_guest_workload_cpu_limit_cores",
		"cubesandbox_guest_workload_memory_current_bytes",
		"cubesandbox_guest_workload_memory_failures_total",
		"cubesandbox_guest_workload_metrics_epoch",
		"cubesandbox_guest_workload_metrics_epoch_start_time_seconds",
		"# TYPE cubesandbox_host_sandbox_cpu_usage_seconds_total counter",
		"cubesandbox_host_sandbox_cpu_usage_seconds_total",
		"cubesandbox_host_sandbox_cpu_limit_cores",
		"cubesandbox_host_sandbox_memory_current_bytes",
		"cubesandbox_host_sandbox_memory_failures_total",
		`sandbox_id="sandbox"`,
		`container_id="sandbox"`,
	} {
		require.Contains(t, body, expected)
	}
	require.NotContains(t, body, "baseline_id")
	require.NotContains(t, body, "last_error")
	require.NotContains(t, body, "container_cpu")
	require.Equal(t, 22, countPrometheusSamples(body))
	require.Equal(t, 0, guestReader.calls)
	require.Equal(t, 0, hostReader.calls)
	require.True(t, strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain"))
}

func TestPrometheusHandlerExportsPendingPointGaugesButOmitsCumulativeAndUnavailableViews(t *testing.T) {
	limit := int64(500)
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"pending/pending": {
				Workload:            WorkloadRef{SandboxID: "pending", ContainerID: "pending"},
				EpochGeneration:     2,
				EpochStartedAt:      time.Unix(100, 0).UTC(),
				CollectedAt:         time.Unix(110, 0).UTC(),
				Availability:        GuestWorkloadAvailable,
				CumulativeAvailable: false,
				CPULimitMillicores:  &limit,
				MemoryCurrentBytes:  2048,
				MemoryLimitBytes:    8192,
			},
			"paused/paused": {
				Workload:           WorkloadRef{SandboxID: "paused", ContainerID: "paused"},
				Availability:       GuestWorkloadUnavailable,
				CollectedAt:        time.Unix(110, 0).UTC(),
				MemoryCurrentBytes: 4096,
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]HostSandboxLatest{
			"stale": {
				SandboxID:    "stale",
				Availability: HostSandboxStale,
				CollectedAt:  time.Unix(10, 0).UTC(),
				Snapshot:     &HostSandboxSnapshot{SandboxID: "stale", CPUUsageTotalNS: 100},
			},
		},
	}
	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)
	handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
	body := recorder.Body.String()

	require.Contains(t, body, `cubesandbox_guest_workload_memory_current_bytes{container_id="pending",sandbox_id="pending"}`)
	require.Contains(t, body, `cubesandbox_guest_workload_cpu_limit_cores{container_id="pending",sandbox_id="pending"}`)
	require.NotContains(t, body, `cubesandbox_guest_workload_cpu_usage_seconds_total{container_id="pending"`)
	require.NotContains(t, body, `sandbox_id="paused"`)
	require.NotContains(t, body, `sandbox_id="stale"`)
}

func TestPrometheusHandlerKeepsHostScopeWhenGuestCapabilityIsUnavailable(t *testing.T) {
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"sandbox/sandbox": {
				Workload:     WorkloadRef{SandboxID: "sandbox", ContainerID: "sandbox"},
				Availability: GuestWorkloadUnavailable,
				LastError:    "guest does not support resource metrics version 1 (reported version 0)",
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]HostSandboxLatest{
			"sandbox": {
				SandboxID:    "sandbox",
				CollectedAt:  time.Unix(110, 0).UTC(),
				Availability: HostSandboxAvailable,
				Snapshot:     &HostSandboxSnapshot{SandboxID: "sandbox", MemoryCurrentBytes: 4096},
			},
		},
	}
	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)
	handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
	body := recorder.Body.String()

	require.Contains(t, body, `cubesandbox_host_sandbox_memory_current_bytes{sandbox_id="sandbox"} 4096`)
	require.NotContains(t, body, `cubesandbox_guest_workload_`)
}

func TestPrometheusHandlerUsesTypedGuestMemoryLimitSemantics(t *testing.T) {
	guest := &GuestWorkloadSampler{
		config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute},
		latest: map[string]GuestWorkloadLatest{
			"zero/zero": {
				Workload:         WorkloadRef{SandboxID: "zero", ContainerID: "zero"},
				CollectedAt:      time.Unix(110, 0).UTC(),
				Availability:     GuestWorkloadAvailable,
				MemoryLimitBytes: 0,
			},
			"finite/finite": {
				Workload:         WorkloadRef{SandboxID: "finite", ContainerID: "finite"},
				CollectedAt:      time.Unix(110, 0).UTC(),
				Availability:     GuestWorkloadAvailable,
				MemoryLimitBytes: uint64(1) << 60,
			},
			"unlimited/unlimited": {
				Workload:             WorkloadRef{SandboxID: "unlimited", ContainerID: "unlimited"},
				CollectedAt:          time.Unix(110, 0).UTC(),
				Availability:         GuestWorkloadAvailable,
				MemoryLimitBytes:     ^uint64(0),
				MemoryLimitUnlimited: true,
			},
		},
	}
	host := &HostSandboxSampler{
		config: HostSandboxSamplerConfig{StaleAfter: time.Minute},
		latest: make(map[string]HostSandboxLatest),
	}
	cache, err := NewSandboxResourceCache(guest, host)
	require.NoError(t, err)
	handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
	body := recorder.Body.String()

	require.Contains(t, body, `cubesandbox_guest_workload_memory_limit_bytes{container_id="finite",sandbox_id="finite"} 1.152921504606847e+18`)
	require.Contains(t, body, `cubesandbox_guest_workload_memory_limit_bytes{container_id="zero",sandbox_id="zero"} 0`)
	require.NotContains(t, body, `cubesandbox_guest_workload_memory_limit_bytes{container_id="unlimited"`)
}

func TestPrometheusHandlerAppliesConfiguredExportScopes(t *testing.T) {
	cache := makeSyntheticResourceCache(t, 1, ResourceScopeGuestWorkload)
	handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))

	require.Contains(t, recorder.Body.String(), "cubesandbox_guest_workload_cpu_usage_seconds_total")
	require.NotContains(t, recorder.Body.String(), "cubesandbox_host_sandbox_")
}

func TestPrometheusHandlerLimitsConcurrentScrapes(t *testing.T) {
	cache, err := NewSandboxResourceCache(nil, nil, ResourceScopeHostSandbox)
	require.NoError(t, err)
	entered := make(chan struct{}, maxConcurrentPrometheusScrapes+1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	handler := newPrometheusHandler(cache, func() time.Time {
		entered <- struct{}{}
		<-release
		return time.Unix(120, 0).UTC()
	})

	var wg sync.WaitGroup
	statuses := make(chan int, maxConcurrentPrometheusScrapes)
	for range maxConcurrentPrometheusScrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
			statuses <- recorder.Code
		}()
	}
	for range maxConcurrentPrometheusScrapes {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for scrape to enter collector")
		}
	}

	overflowDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
		overflowDone <- recorder.Code
	}()
	select {
	case status := <-overflowDone:
		require.Equal(t, http.StatusServiceUnavailable, status)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("overflow scrape was not rejected")
	}

	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	close(statuses)
	for status := range statuses {
		require.Equal(t, http.StatusOK, status)
	}
}

func BenchmarkPrometheusHandler(b *testing.B) {
	for _, sandboxes := range []int{1000, 3000} {
		b.Run(fmt.Sprintf("sandboxes_%d", sandboxes), func(b *testing.B) {
			cache := makeSyntheticResourceCache(b, sandboxes)
			handler := newPrometheusHandler(cache, func() time.Time { return time.Unix(120, 0).UTC() })
			probe := httptest.NewRecorder()
			handler.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
			responseBytes := probe.Body.Len()
			series := countPrometheusSamples(probe.Body.String())
			gzipProbe := httptest.NewRecorder()
			gzipRequest := httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil)
			gzipRequest.Header.Set("Accept-Encoding", "gzip")
			handler.ServeHTTP(gzipProbe, gzipRequest)
			gzipBytes := gzipProbe.Body.Len()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/metrics/resource", nil))
			}
			b.StopTimer()
			b.ReportMetric(float64(responseBytes), "bytes/scrape")
			b.ReportMetric(float64(gzipBytes), "gzip_bytes/scrape")
			b.ReportMetric(float64(series), "series/scrape")
		})
	}
}

type testingTB interface {
	Helper()
	Fatalf(string, ...interface{})
}

func makeSyntheticResourceCache(tb testingTB, count int, scopes ...ResourceScope) *SandboxResourceCache {
	tb.Helper()
	guestLatest := make(map[string]GuestWorkloadLatest, count)
	hostLatest := make(map[string]HostSandboxLatest, count)
	for i := 0; i < count; i++ {
		sandboxID := fmt.Sprintf("sandbox-%056d", i)
		limit := int64(1000)
		workload := WorkloadRef{SandboxID: sandboxID, ContainerID: sandboxID}
		guestLatest[guestWorkloadKey(workload)] = GuestWorkloadLatest{
			Workload:            workload,
			EpochGeneration:     1,
			EpochStartedAt:      time.Unix(100, 0).UTC(),
			CollectedAt:         time.Unix(110, 0).UTC(),
			Availability:        GuestWorkloadAvailable,
			CumulativeAvailable: true,
			CPULimitMillicores:  &limit,
			MemoryCurrentBytes:  4096,
			MemoryLimitBytes:    16384,
			Snapshot: &GuestWorkloadSnapshot{
				Workload: workload,
				Counters: GuestWorkloadCounters{
					CPUUsageTotalNS:          1_000_000_000,
					CPUUserTotalNS:           700_000_000,
					CPUSystemTotalNS:         300_000_000,
					CPUThrottledTotalNS:      10_000_000,
					CPUPeriodsTotal:          10,
					CPUThrottledPeriodsTotal: 1,
					MemoryFailuresTotal:      2,
				},
			},
		}
		hostLatest[sandboxID] = HostSandboxLatest{
			SandboxID:    sandboxID,
			CollectedAt:  time.Unix(110, 0).UTC(),
			Availability: HostSandboxAvailable,
			Snapshot: &HostSandboxSnapshot{
				SandboxID:                sandboxID,
				CPUUsageTotalNS:          2_000_000_000,
				CPUUserTotalNS:           1_300_000_000,
				CPUSystemTotalNS:         700_000_000,
				CPUThrottledTotalNS:      20_000_000,
				CPUPeriodsTotal:          20,
				CPUThrottledPeriodsTotal: 2,
				CPULimitQuotaUS:          100000,
				CPULimitPeriodUS:         100000,
				MemoryCurrentBytes:       8192,
				MemoryLimitBytes:         32768,
				MemoryFailuresTotal:      3,
			},
		}
	}
	cache, err := NewSandboxResourceCache(
		&GuestWorkloadSampler{config: GuestWorkloadSamplerConfig{StaleAfter: time.Minute}, latest: guestLatest},
		&HostSandboxSampler{config: HostSandboxSamplerConfig{StaleAfter: time.Minute}, latest: hostLatest},
		scopes...,
	)
	if err != nil {
		tb.Fatalf("create synthetic resource cache: %v", err)
	}
	return cache
}

func countPrometheusSamples(body string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}
	return count
}
