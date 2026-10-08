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
	// outcomeDeferred counts cleanup rounds that failed for a reason expected
	// to clear on its own (a shim-spawn intent still inside its TTL). They are
	// neither successes nor failed attempts: they must not move the sandbox
	// toward quarantine, so they are reported separately to keep the budget
	// visible in metrics without being spent.
	outcomeDeferred = "deferred"

	schedulerStarted   = "started"
	schedulerReadError = "read_error"
)
