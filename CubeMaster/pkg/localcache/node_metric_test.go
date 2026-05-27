// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestUpdateNodeMetricInProcess(t *testing.T) {
	origCache := l.cache
	t.Cleanup(func() { l.cache = origCache })

	l.cache = cache.New(0, 0)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true})

	metricTime := time.Unix(1700000000, 0).UTC()
	if err := UpdateNodeMetricInProcess(&NodeMetric{
		NodeID:              "node-a",
		MetricTime:          metricTime,
		HasAllocated:        true,
		MilliCPUUsage:       2500,
		MemoryMBUsage:       4096,
		MvmNum:              7,
		NicQueues:           5,
		HasDisk:             true,
		DataDiskUsagePer:    10.5,
		StorageDiskUsagePer: 20.5,
		SysDiskUsagePer:     30.5,
	}); err != nil {
		t.Fatalf("UpdateNodeMetricInProcess: %v", err)
	}

	got, ok := GetNode("node-a")
	if !ok {
		t.Fatal("node-a missing after metric update")
	}
	if got.QuotaCpuUsage != 2500 || got.QuotaMemUsage != 4096 || got.MvmNum != 7 ||
		got.NicQueues != 5 || got.DataDiskUsagePer != 10.5 || got.StorageDiskUsagePer != 20.5 ||
		got.SysDiskUsagePer != 30.5 || !got.MetricUpdate.Equal(metricTime) {
		t.Fatalf("node-a metric mismatch: %+v", got)
	}
}

func TestUpdateNodeMetricInProcessReturnsErrorForUnknownNode(t *testing.T) {
	origCache := l.cache
	t.Cleanup(func() { l.cache = origCache })

	l.cache = cache.New(0, 0)
	if err := UpdateNodeMetricInProcess(&NodeMetric{
		NodeID: "ghost", HasAllocated: true,
	}); err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestUpdateNodeMetricInProcessRejectsNil(t *testing.T) {
	if err := UpdateNodeMetricInProcess(nil); err == nil {
		t.Fatal("expected error for nil metric")
	}
}

// TestUpdateNodeMetricInProcessDoesNotClobberMissingGroup is the
// regression guard for the partial-heartbeat bug: a disk-only update
// must leave a prior allocated snapshot intact, and vice versa.
func TestUpdateNodeMetricInProcessDoesNotClobberMissingGroup(t *testing.T) {
	origCache := l.cache
	t.Cleanup(func() { l.cache = origCache })

	l.cache = cache.New(0, 0)
	priorMetric := time.Unix(1700000000, 0).UTC()
	prior := &node.Node{
		InsID:               "node-a",
		IP:                  "10.0.0.1",
		Healthy:             true,
		QuotaCpuUsage:       2500,
		QuotaMemUsage:       4096,
		MvmNum:              7,
		NicQueues:           5,
		DataDiskUsagePer:    10.5,
		StorageDiskUsagePer: 20.5,
		SysDiskUsagePer:     30.5,
		MetricUpdate:        priorMetric,
	}
	l.cache.SetDefault("node-a", prior)

	diskOnlyTime := priorMetric.Add(time.Second)
	if err := UpdateNodeMetricInProcess(&NodeMetric{
		NodeID:              "node-a",
		MetricTime:          diskOnlyTime,
		HasDisk:             true,
		DataDiskUsagePer:    77.7,
		StorageDiskUsagePer: 88.8,
		SysDiskUsagePer:     99.9,
	}); err != nil {
		t.Fatalf("disk-only update failed: %v", err)
	}
	got, _ := GetNode("node-a")
	if got.QuotaCpuUsage != 2500 || got.QuotaMemUsage != 4096 || got.MvmNum != 7 || got.NicQueues != 5 {
		t.Fatalf("disk-only update clobbered allocated group: %+v", got)
	}
	if got.DataDiskUsagePer != 77.7 || got.StorageDiskUsagePer != 88.8 || got.SysDiskUsagePer != 99.9 {
		t.Fatalf("disk-only update did not apply disk values: %+v", got)
	}
	if !got.MetricUpdate.Equal(diskOnlyTime) {
		t.Fatalf("MetricUpdate not advanced: %v", got.MetricUpdate)
	}

	allocOnlyTime := diskOnlyTime.Add(time.Second)
	if err := UpdateNodeMetricInProcess(&NodeMetric{
		NodeID:        "node-a",
		MetricTime:    allocOnlyTime,
		HasAllocated:  true,
		MilliCPUUsage: 1000,
		MemoryMBUsage: 2048,
		MvmNum:        3,
		NicQueues:     2,
	}); err != nil {
		t.Fatalf("allocated-only update failed: %v", err)
	}
	got, _ = GetNode("node-a")
	if got.DataDiskUsagePer != 77.7 || got.StorageDiskUsagePer != 88.8 || got.SysDiskUsagePer != 99.9 {
		t.Fatalf("allocated-only update clobbered disk group: %+v", got)
	}
	if got.QuotaCpuUsage != 1000 || got.QuotaMemUsage != 2048 || got.MvmNum != 3 || got.NicQueues != 2 {
		t.Fatalf("allocated-only update did not apply allocated values: %+v", got)
	}
}

// TestUpdateNodeMetricInProcessIgnoresEmptyMetric protects the partial
// path from acting on a NodeMetric where neither group was reported:
// such a payload would otherwise advance MetricUpdate while leaving the
// underlying values stale, which would silently extend MetricUpdateTimeout.
func TestUpdateNodeMetricInProcessIgnoresEmptyMetric(t *testing.T) {
	origCache := l.cache
	t.Cleanup(func() { l.cache = origCache })

	l.cache = cache.New(0, 0)
	priorMetric := time.Unix(1700000000, 0).UTC()
	prior := &node.Node{InsID: "node-a", QuotaCpuUsage: 4242, MetricUpdate: priorMetric}
	l.cache.SetDefault("node-a", prior)

	if err := UpdateNodeMetricInProcess(&NodeMetric{
		NodeID:     "node-a",
		MetricTime: priorMetric.Add(time.Minute),
	}); err != nil {
		t.Fatalf("unexpected error from empty metric: %v", err)
	}
	got, _ := GetNode("node-a")
	if got.QuotaCpuUsage != 4242 || !got.MetricUpdate.Equal(priorMetric) {
		t.Fatalf("empty metric should be a no-op, got %+v", got)
	}
}
