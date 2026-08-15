// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package webhook implements the CubeOps in-process webhook delivery worker:
// Redis Stream consumer → SQL delivery ledger → HMAC-signed HTTP POST with
// retry / dead-letter semantics. See proposal/webhook-delivery-spec.md.
package webhook

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Result classes for delivery outcomes.
const (
	ResultSucceeded = "succeeded"
	ResultRetryable = "retryable"
	ResultPermanent = "permanent"
	ResultShutdown  = "shutdown"
	ResultDead      = "dead"
)

var (
	deliveryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cubeops_webhook_delivery_duration_seconds",
		Help:    "Webhook delivery HTTP duration, by result class.",
		Buckets: prometheus.DefBuckets,
	}, []string{"result"})

	deliveryResultTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cubeops_webhook_delivery_result_total",
		Help: "Webhook delivery outcomes (succeeded/retryable/permanent/shutdown).",
	}, []string{"result"})

	httpStatusTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cubeops_webhook_http_status_total",
		Help: "Delivery HTTP status code distribution.",
	}, []string{"status"})

	backlogByStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cubeops_webhook_backlog_rows",
		Help: "Actionable delivery backlog rows by status (pending/retryable failed).",
	}, []string{"status"})

	perSubscriptionBacklog = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cubeops_webhook_subscription_backlog_rows",
		Help: "Per-subscription actionable backlog. Cardinality is capped by webhook.per_subscription_metrics_max; beyond it only aggregate metrics are emitted.",
	}, []string{"subscription_id"})

	leaseContentionTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_lease_contention_total",
		Help: "Conditional claim/complete updates that affected 0 rows (lease lost or task re-claimed).",
	})

	lateResultDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_late_result_dropped_total",
		Help: "Completion updates dropped because the lease was no longer owned.",
	})

	workerSaturation = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cubeops_webhook_worker_saturation",
		Help: "Current in-flight sends / worker_concurrency (0..1).",
	})

	keepPendingDeadTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_keep_pending_dead_total",
		Help: "Rows converted from failed to dead by the keep-pending retry window sweep.",
	})

	decryptFailureTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_secret_decrypt_failure_total",
		Help: "Delivery secret decryption failures (classified as permanent).",
	})

	ssrfRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_ssrf_rejected_total",
		Help: "Deliveries rejected by the SSRF address policy.",
	})

	redirectRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cubeops_webhook_redirect_rejected_total",
		Help: "Deliveries rejected because the endpoint attempted an HTTP redirect.",
	})
)
