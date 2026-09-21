// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import "github.com/docker/go-metrics"

var (
	cleanupAttempts    metrics.LabeledCounter
	cleanupScheduler   metrics.LabeledCounter
	quarantinedSandbox metrics.Gauge
)

func init() {
	ns := metrics.NewNamespace("cubebox", "gc", nil)

	cleanupAttempts = ns.NewLabeledCounter("cleanup_attempts", "cleanup rounds by outcome", "outcome")
	cleanupScheduler = ns.NewLabeledCounter(
		"cleanup_scheduler",
		"cleanup scheduler decisions by outcome",
		"outcome",
	)
	// Not a rate: this is how much of the host's tap/IP/volume capacity is
	// currently stuck and will not come back without an operator. It should be
	// alerted on as a level. It goes down when any successful cleanup path
	// removes the corresponding quarantined record.
	quarantinedSandbox = ns.NewGauge("quarantined_sandboxes", "sandboxes given up on, still holding host resources", metrics.Total)

	metrics.Register(ns)
}

const (
	outcomeSuccess     = "success"
	outcomeFail        = "fail"
	outcomeQuarantined = "quarantined"

	schedulerStarted   = "started"
	schedulerReadError = "read_error"
)
