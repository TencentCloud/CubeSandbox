// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetrics_AreCollectable guards the promauto registration set: every
// metric must be readable via the standard collector interface without
// panicking (duplicate registration would fail at init).
func TestMetrics_AreCollectable(t *testing.T) {
	if testutil.ToFloat64(deliveryResultTotal.WithLabelValues(ResultSucceeded)) != 0 {
		t.Fatal("new counter should read 0")
	}
	if testutil.ToFloat64(ssrfRejectedTotal) != 0 {
		t.Fatal("new counter should read 0")
	}
	if testutil.ToFloat64(redirectRejectedTotal) != 0 {
		t.Fatal("new counter should read 0")
	}
	if testutil.ToFloat64(keepPendingDeadTotal) != 0 {
		t.Fatal("new counter should read 0")
	}
}
