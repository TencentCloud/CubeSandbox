// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhooktest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func CaptureAccepted(t *testing.T) float64 {
	t.Helper()
	metrics, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range metrics {
		if family.GetName() != "cubemaster_webhook_operations_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["operation"] == "capture" && labels["result"] == "accepted" {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
