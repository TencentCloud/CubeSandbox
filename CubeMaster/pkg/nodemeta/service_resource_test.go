// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"testing"
	"time"
)

func TestToSchedulerNodeDoesNotForgeMetricUpdate(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID:        "node-a",
		HostIP:        "10.0.0.1",
		HeartbeatTime: time.Unix(1700000000, 0).UTC(),
		Healthy:       true,
	}
	n := toSchedulerNode(snap)
	if n == nil {
		t.Fatal("toSchedulerNode returned nil")
	}
	if !n.MetricUpdate.IsZero() {
		t.Fatalf("MetricUpdate must not inherit heartbeat time: %v", n.MetricUpdate)
	}
	if !n.MetricLocalUpdateAt.IsZero() {
		t.Fatalf("MetricLocalUpdateAt must not inherit heartbeat time: %v", n.MetricLocalUpdateAt)
	}
	if !n.MetaDataUpdateAt.Equal(snap.HeartbeatTime) {
		t.Fatalf("MetaDataUpdateAt %v want %v", n.MetaDataUpdateAt, snap.HeartbeatTime)
	}
}
