// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// externalHTTPScoreWarnInterval bounds how often each sanitized failure
// category is logged at Warn on the create path. Further failures in the same
// interval are Debug-only; the Prometheus counter still increments every time.
const externalHTTPScoreWarnInterval = time.Minute

// Fixed low-cardinality reason labels for Prometheus and the topic1 accept harness.
// Log lines keep detailed sanitize categories; metrics map into this enum only.
const (
	externalHTTPScoreReasonSuccess          = "success"
	externalHTTPScoreReasonTimeout          = "timeout"
	externalHTTPScoreReasonConnection       = "connection"
	externalHTTPScoreReasonHTTPStatus       = "http_status"
	externalHTTPScoreReasonInvalidJSON      = "invalid_json"
	externalHTTPScoreReasonMissingCandidate = "missing_candidate"
	externalHTTPScoreReasonCircuitOpen      = "circuit_open"
	externalHTTPScoreReasonOther            = "other"
)

var externalHTTPScoreMetricReasonAllowlist = map[string]struct{}{
	externalHTTPScoreReasonSuccess:          {},
	externalHTTPScoreReasonTimeout:          {},
	externalHTTPScoreReasonConnection:       {},
	externalHTTPScoreReasonHTTPStatus:       {},
	externalHTTPScoreReasonInvalidJSON:      {},
	externalHTTPScoreReasonMissingCandidate: {},
	externalHTTPScoreReasonCircuitOpen:      {},
	externalHTTPScoreReasonOther:            {},
}

var (
	externalHTTPScoreOutcomesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_scheduler_external_http_score_outcomes_total",
		Help: "Total external_http_score outcomes by fixed reason (success, timeout, connection, http_status, invalid_json, missing_candidate, circuit_open, other).",
	}, []string{"reason"})

	externalHTTPScoreRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cube_scheduler_external_http_score_request_duration_seconds",
		Help:    "External HTTP score round-trip latency by fixed outcome reason.",
		Buckets: prometheus.DefBuckets,
	}, []string{"reason"})

	// Label is host:port only (no userinfo, path, or query) so credentials
	// never appear and cardinality stays bounded by distinct sidecar hosts.
	externalHTTPScoreCircuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cube_scheduler_external_http_score_circuit_state",
		Help: "external_http_score circuit breaker state by sidecar host:port: 0=closed, 1=half-open, 2=open.",
	}, []string{"target"})

	externalHTTPScoreWarnMu   sync.Mutex
	externalHTTPScoreLastWarn = map[string]time.Time{}
	// Test seam: when non-nil, overrides time.Now for warn rate-limit tests.
	externalHTTPScoreNow func() time.Time
	// Test seam: counts Warn emissions after rate limiting.
	externalHTTPScoreWarnCount atomic.Uint64
	// Test seam: last circuit state written (0/1/2); multi-host uses the GaugeVec.
	externalHTTPScoreCircuitValue atomic.Int64
)

// externalHTTPScoreMetricReason maps a log-safe sanitize category to a fixed
// Prometheus / accept-harness reason label (never includes endpoints or tokens).
func externalHTTPScoreMetricReason(category string) string {
	switch category {
	case externalHTTPScoreReasonSuccess:
		return externalHTTPScoreReasonSuccess
	case "external_http_score missing_candidate":
		return externalHTTPScoreReasonMissingCandidate
	case "external_http_score unexpected_status":
		return externalHTTPScoreReasonHTTPStatus
	case "external_http_score malformed_response", "external_http_score empty_scores":
		return externalHTTPScoreReasonInvalidJSON
	case "external_http_score circuit_open":
		return externalHTTPScoreReasonCircuitOpen
	case "http_request_failed", "unknown_error",
		"external_http_score response_too_large",
		"external_http_score invalid_candidate_score":
		// Generic / non-transport / non-JSON-shape failures must not page as
		// connection or invalid_json — keep connection a true dial/transport signal.
		return externalHTTPScoreReasonOther
	}
	if isAllowListedHTTPFailureCategory(category) {
		switch {
		case strings.HasSuffix(category, "_timeout"):
			return externalHTTPScoreReasonTimeout
		case strings.HasSuffix(category, "_connection_refused"),
			strings.HasSuffix(category, "_transport_failed"):
			return externalHTTPScoreReasonConnection
		default:
			// _canceled is usually caller/create cancel, not sidecar reachability.
			// Generic _failed is also not a dial/transport signal.
			return externalHTTPScoreReasonOther
		}
	}
	return externalHTTPScoreReasonOther
}

func setExternalHTTPScoreCircuitState(target string, state int) {
	if target == "" {
		target = "invalid"
	}
	externalHTTPScoreCircuitValue.Store(int64(state))
	externalHTTPScoreCircuitState.WithLabelValues(target).Set(float64(state))
}

func externalHTTPScoreCircuitStateValue() float64 {
	return float64(externalHTTPScoreCircuitValue.Load())
}

func resetExternalHTTPScoreCircuitMetrics() {
	externalHTTPScoreCircuitState.Reset()
	externalHTTPScoreCircuitValue.Store(circuitStateClosed)
}

func observeExternalHTTPScoreFailure(category string) {
	reason := externalHTTPScoreMetricReason(category)
	if _, ok := externalHTTPScoreMetricReasonAllowlist[reason]; !ok {
		reason = externalHTTPScoreReasonOther
	}
	externalHTTPScoreOutcomesTotal.WithLabelValues(reason).Inc()
}

func observeExternalHTTPScoreSuccess(duration time.Duration) {
	externalHTTPScoreOutcomesTotal.WithLabelValues(externalHTTPScoreReasonSuccess).Inc()
	if duration < 0 {
		duration = 0
	}
	externalHTTPScoreRequestDuration.WithLabelValues(externalHTTPScoreReasonSuccess).Observe(duration.Seconds())
}

func observeExternalHTTPScoreRequestFailure(duration time.Duration, category string) {
	reason := externalHTTPScoreMetricReason(category)
	if _, ok := externalHTTPScoreMetricReasonAllowlist[reason]; !ok {
		reason = externalHTTPScoreReasonOther
	}
	// Single counter increment per HTTP attempt; config-only failures use observeExternalHTTPScoreFailure.
	externalHTTPScoreOutcomesTotal.WithLabelValues(reason).Inc()
	if duration < 0 {
		duration = 0
	}
	externalHTTPScoreRequestDuration.WithLabelValues(reason).Observe(duration.Seconds())
}

func shouldWarnExternalHTTPScoreFailure(category string) bool {
	now := time.Now()
	if externalHTTPScoreNow != nil {
		now = externalHTTPScoreNow()
	}
	externalHTTPScoreWarnMu.Lock()
	defer externalHTTPScoreWarnMu.Unlock()
	last, ok := externalHTTPScoreLastWarn[category]
	if ok && now.Sub(last) < externalHTTPScoreWarnInterval {
		return false
	}
	externalHTTPScoreLastWarn[category] = now
	return true
}

func resetExternalHTTPScoreFailureLogStateForTest() {
	externalHTTPScoreWarnMu.Lock()
	externalHTTPScoreLastWarn = map[string]time.Time{}
	externalHTTPScoreWarnMu.Unlock()
	externalHTTPScoreWarnCount.Store(0)
	externalHTTPScoreNow = nil
}
