// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// sandboxSpecPersistFailureTotal counts persistSandboxSpec failures, labelled
// by the (intentionally low-cardinality) failure reason. Persistence is
// best-effort by design (per ADR 0001): the count exists so operators can
// alert on continuous drift between the master's canonical spec store and
// the live sandbox table, without blocking the create hot path.
var sandboxSpecPersistFailureTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "sandbox_spec_persist_failure_total",
	Help: "Total best-effort sandbox spec persistence failures, by reason.",
}, []string{"reason"})

// recordPersistFailure increments the failure counter with the given reason
// label. Empty reason strings are normalized to "unknown" so the metric is
// always queryable.
func recordPersistFailure(reason string) {
	if reason == "" {
		reason = "unknown"
	}
	sandboxSpecPersistFailureTotal.WithLabelValues(reason).Inc()
}
