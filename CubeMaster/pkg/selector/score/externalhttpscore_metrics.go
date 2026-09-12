// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
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

var (
	externalHTTPScoreFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_scheduler_external_http_score_failure_total",
		Help: "Total external_http_score fail-open failures by sanitized category.",
	}, []string{"category"})

	externalHTTPScoreWarnMu   sync.Mutex
	externalHTTPScoreLastWarn = map[string]time.Time{}
	// Test seam: when non-nil, overrides time.Now for warn rate-limit tests.
	externalHTTPScoreNow func() time.Time
	// Test seam: counts Warn emissions after rate limiting.
	externalHTTPScoreWarnCount atomic.Uint64
)

func observeExternalHTTPScoreFailure(category string) {
	if category == "" {
		category = "unknown_error"
	}
	externalHTTPScoreFailuresTotal.WithLabelValues(category).Inc()
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
