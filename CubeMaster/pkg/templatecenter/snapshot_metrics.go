// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	snapshotCommitTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_snapshot_commit_total",
		Help: "Total snapshot commit operations by result.",
	}, []string{"result"})
	snapshotRollbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_snapshot_rollback_total",
		Help: "Total snapshot rollback operations by result.",
	}, []string{"result"})
	snapshotDeleteTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_snapshot_delete_total",
		Help: "Total snapshot delete operations by result.",
	}, []string{"result"})
	snapshotOrphanCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cube_snapshot_orphan_count",
		Help: "Current number of snapshot replicas detected with missing persisted objects.",
	})
	snapshotStorageModeGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cube_snapshot_storage_mode",
		Help: "Current snapshot storage mode for each node.",
	}, []string{"node_id", "node_ip", "mode"})
)

var snapshotStorageModes = []snapshotStorageMode{
	snapshotStorageModeHealthy,
	snapshotStorageModeWarn,
	snapshotStorageModeReject,
	snapshotStorageModeDeleteOnly,
	snapshotStorageModeUnknown,
}

func recordSnapshotCommitResult(success bool) {
	snapshotCommitTotal.WithLabelValues(snapshotMetricResult(success)).Inc()
}

func recordSnapshotRollbackResult(success bool) {
	snapshotRollbackTotal.WithLabelValues(snapshotMetricResult(success)).Inc()
}

func recordSnapshotDeleteResult(success bool) {
	snapshotDeleteTotal.WithLabelValues(snapshotMetricResult(success)).Inc()
}

func setSnapshotOrphanGauge(count int) {
	if count < 0 {
		count = 0
	}
	snapshotOrphanCount.Set(float64(count))
}

func setSnapshotStorageModeMetric(state snapshotStorageState) {
	nodeID := firstNonEmpty(state.NodeID, "")
	nodeIP := firstNonEmpty(state.NodeIP, "")
	for _, mode := range snapshotStorageModes {
		value := 0.0
		if state.Mode == mode {
			value = 1.0
		}
		snapshotStorageModeGauge.WithLabelValues(nodeID, nodeIP, string(mode)).Set(value)
	}
}

func snapshotMetricResult(success bool) string {
	if success {
		return "success"
	}
	return "error"
}
