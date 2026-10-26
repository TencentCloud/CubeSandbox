// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

type HealthStatus struct {
	Healthy         bool
	UnhealthyReason string
}

func ReadyConditionTrue(conditions []model.NodeCondition) bool {
	for _, c := range conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func EvaluateHealth(reportedReady bool, heartbeatTime, now time.Time, timeout time.Duration) HealthStatus {
	if heartbeatTime.IsZero() || now.Sub(heartbeatTime) > timeout {
		return HealthStatus{Healthy: false, UnhealthyReason: model.ReasonHeartbeatExpired}
	}
	if !reportedReady {
		return HealthStatus{Healthy: false, UnhealthyReason: model.ReasonReportedNotReady}
	}
	return HealthStatus{Healthy: true}
}

// MetadataTimeout returns the cubelet heartbeat expiry window (15s),
// independent of CubeMaster's sync interval.
func MetadataTimeout() time.Duration {
	return 15 * time.Second
}
