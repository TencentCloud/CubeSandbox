package nodehealth

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	ReasonReportedNotReady = "ReportedNotReady"
	ReasonHeartbeatExpired = "HeartbeatExpired"
)

type Status struct {
	Healthy         bool
	UnhealthyReason string
}

func MetadataTimeout(syncMetaDataInterval time.Duration) time.Duration {
	return syncMetaDataInterval + 10*time.Second
}

func ReadyConditionTrue(conditions []corev1.NodeCondition) bool {
	for _, cond := range conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func EvaluateFromFacts(reportedReady bool, heartbeatTime, now time.Time, timeout time.Duration) Status {
	if heartbeatTime.IsZero() || now.Sub(heartbeatTime) > timeout {
		return Status{Healthy: false, UnhealthyReason: ReasonHeartbeatExpired}
	}
	if !reportedReady {
		return Status{Healthy: false, UnhealthyReason: ReasonReportedNotReady}
	}
	return Status{Healthy: true}
}

func Evaluate(conditions []corev1.NodeCondition, heartbeatTime, now time.Time, timeout time.Duration) Status {
	return EvaluateFromFacts(ReadyConditionTrue(conditions), heartbeatTime, now, timeout)
}
