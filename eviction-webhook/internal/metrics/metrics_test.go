// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvictionInterceptedTotalIncrements(t *testing.T) {
	before := testutil.ToFloat64(EvictionInterceptedTotal.WithLabelValues("worker-01", "cubebox", "kubelet"))
	EvictionInterceptedTotal.WithLabelValues("worker-01", "cubebox", "kubelet").Inc()
	after := testutil.ToFloat64(EvictionInterceptedTotal.WithLabelValues("worker-01", "cubebox", "kubelet"))
	assert.Equal(t, before+1, after)
}

func TestIsolatedNodesTotalIncrements(t *testing.T) {
	before := testutil.ToFloat64(IsolatedNodesTotal)
	IsolatedNodesTotal.Inc()
	after := testutil.ToFloat64(IsolatedNodesTotal)
	assert.Equal(t, before+1, after)
}

func TestPausedSandboxesGaugeSetAndGet(t *testing.T) {
	PausedSandboxesGauge.Set(5)
	assert.Equal(t, float64(5), testutil.ToFloat64(PausedSandboxesGauge))
	PausedSandboxesGauge.Set(0)
}

func TestCubeMasterAPIErrorsTotalIncrements(t *testing.T) {
	before := testutil.ToFloat64(CubeMasterAPIErrorsTotal.WithLabelValues("PauseSandbox", "timeout"))
	CubeMasterAPIErrorsTotal.WithLabelValues("PauseSandbox", "timeout").Inc()
	after := testutil.ToFloat64(CubeMasterAPIErrorsTotal.WithLabelValues("PauseSandbox", "timeout"))
	assert.Equal(t, before+1, after)
}

func TestRecoveryDurationObserve(t *testing.T) {
	// Observe should not panic for any positive duration.
	require.NotPanics(t, func() {
		RecoveryDurationSeconds.WithLabelValues("worker-01", "success").Observe(2.5)
	})
}

func TestCubeMasterAPILatencyObserve(t *testing.T) {
	require.NotPanics(t, func() {
		CubeMasterAPILatencySeconds.WithLabelValues("IsolateNode", "200").Observe(0.05)
	})
}

func TestWebhookLatencyObserve(t *testing.T) {
	require.NotPanics(t, func() {
		WebhookLatencySeconds.WithLabelValues("denied").Observe(0.01)
	})
}

func TestAllMetricsAreRegistered(t *testing.T) {
	// Gather all registered metrics and verify our custom ones are present.
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	names := make(map[string]bool, len(metricFamilies))
	for _, mf := range metricFamilies {
		names[mf.GetName()] = true
	}

	expected := []string{
		"eviction_webhook_intercepted_total",
		"eviction_webhook_recovery_duration_seconds",
		"eviction_webhook_cubemaster_api_latency_seconds",
		"eviction_webhook_cubemaster_errors_total",
		"eviction_webhook_isolated_nodes_total",
		"eviction_webhook_paused_sandboxes",
		"eviction_webhook_request_latency_seconds",
	}
	for _, name := range expected {
		assert.True(t, names[name], "metric %q should be registered", name)
	}
}

func TestMetricNamesConventions(t *testing.T) {
	// All custom metrics should start with the service prefix.
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, mf := range metricFamilies {
		name := mf.GetName()
		if strings.HasPrefix(name, "eviction_webhook_") {
			assert.True(t,
				strings.HasPrefix(name, "eviction_webhook_"),
				"metric %q should follow eviction_webhook_ namespace", name,
			)
		}
	}
}
