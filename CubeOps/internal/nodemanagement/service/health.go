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

// MetadataTimeout returns the health-check timeout (syncInterval + 10s slack).
// Default 5s aligns with CubeMaster's SyncMetaDataInterval → 15s window.
func MetadataTimeout(syncInterval time.Duration) time.Duration {
	if syncInterval <= 0 {
		syncInterval = 5 * time.Second
	}
	return syncInterval + 10*time.Second
}
