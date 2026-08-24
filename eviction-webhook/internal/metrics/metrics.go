// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package metrics defines Prometheus metrics for eviction-webhook.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// EvictionInterceptedTotal counts total evictions intercepted by the webhook.
var EvictionInterceptedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "eviction_webhook_intercepted_total",
		Help: "Total number of evictions intercepted by the webhook",
	},
	[]string{"node", "instance_type", "reason"},
)

// RecoveryDurationSeconds measures sandbox recovery duration in seconds.
var RecoveryDurationSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "eviction_webhook_recovery_duration_seconds",
		Help:    "Duration of sandbox recovery in seconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 8),
	},
	[]string{"node", "status"},
)

// CubeMasterAPILatencySeconds measures CubeMaster API call latency.
var CubeMasterAPILatencySeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "eviction_webhook_cubemaster_api_latency_seconds",
		Help:    "CubeMaster API call latency in seconds",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"method", "status"},
)

// CubeMasterAPIErrorsTotal counts CubeMaster API errors.
var CubeMasterAPIErrorsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "eviction_webhook_cubemaster_errors_total",
		Help: "Total number of CubeMaster API errors",
	},
	[]string{"method", "error_code"},
)

// IsolatedNodesTotal counts total nodes that have been isolated.
var IsolatedNodesTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "eviction_webhook_isolated_nodes_total",
		Help: "Total number of nodes that have been isolated",
	},
)

// WebhookLatencySeconds measures webhook request latency.
var WebhookLatencySeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "eviction_webhook_request_latency_seconds",
		Help:    "Webhook request latency in seconds",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"status"},
)

// WebhookRequestsTotal counts total webhook requests received.
var WebhookRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "eviction_webhook_requests_total",
		Help: "Total number of webhook requests received",
	},
	[]string{"operation", "allowed"},
)
