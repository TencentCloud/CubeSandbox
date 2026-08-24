// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func TestToSchedulerNode_LocalTemplatesIncluded(t *testing.T) {
	snap := &model.NodeSnapshot{
		NodeID:         "node-1",
		HostIP:         "10.0.0.1",
		Capacity:       model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable:    model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-1"}, {TemplateID: "tpl-2"}},
	}
	n := ToSchedulerNode(snap)
	if n == nil {
		t.Fatal("expected node")
	}
	want := []string{"tpl-1", "tpl-2"}
	if len(n.LocalTemplates) != len(want) {
		t.Fatalf("local templates = %v, want %v", n.LocalTemplates, want)
	}
	for i := range want {
		if n.LocalTemplates[i] != want[i] {
			t.Errorf("template[%d] = %s, want %s", i, n.LocalTemplates[i], want[i])
		}
	}
}

func TestToSchedulerNode_LocalTemplatesRoundTrip(t *testing.T) {
	snap := &model.NodeSnapshot{
		NodeID:         "node-1",
		HostIP:         "10.0.0.1",
		Capacity:       model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable:    model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-a"}, {TemplateID: "tpl-b"}},
	}
	n := ToSchedulerNode(snap)
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tpls, ok := m["LocalTemplates"].([]any)
	if !ok || len(tpls) != 2 {
		t.Fatalf("LocalTemplates = %v", m["LocalTemplates"])
	}
}

func TestToSchedulerNode_ReportedReadyRoundTrip(t *testing.T) {
	tests := []struct {
		name          string
		reportedReady bool
	}{
		{"ready", true},
		{"not ready", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &model.NodeSnapshot{
				NodeID:        "node-1",
				Capacity:      model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
				ReportedReady: tt.reportedReady,
			}
			n := ToSchedulerNode(snap)
			if n.ReportedReady != tt.reportedReady {
				t.Errorf("ReportedReady = %v, want %v", n.ReportedReady, tt.reportedReady)
			}
		})
	}
}

func TestToSchedulerNode_EmptyInstanceType(t *testing.T) {
	snap := &model.NodeSnapshot{
		NodeID:      "node-1",
		Capacity:    model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable: model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
	}
	n := ToSchedulerNode(snap)
	if n.InstanceType != model.DefaultInstanceTypeName {
		t.Errorf("instanceType = %s, want %s", n.InstanceType, model.DefaultInstanceTypeName)
	}
}

func TestToSchedulerNode_MetricTimestamps(t *testing.T) {
	metricUpdate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	metricLocal := time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC)
	snap := &model.NodeSnapshot{
		NodeID:              "node-1",
		HostIP:              "10.0.0.1",
		MetricUpdate:        metricUpdate,
		MetricLocalUpdateAt: metricLocal,
	}
	n := ToSchedulerNode(snap)
	if !n.MetricUpdate.Equal(metricUpdate) {
		t.Errorf("MetricUpdate = %v, want %v", n.MetricUpdate, metricUpdate)
	}
	if !n.MetricLocalUpdateAt.Equal(metricLocal) {
		t.Errorf("MetricLocalUpdateAt = %v, want %v", n.MetricLocalUpdateAt, metricLocal)
	}
}

func TestSchedulerNodeScoreView_MetricTimestamps(t *testing.T) {
	metricUpdate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	metricLocal := time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC)
	snap := &model.NodeSnapshot{
		NodeID:              "node-1",
		Score:               0.75,
		MetricUpdate:        metricUpdate,
		MetricLocalUpdateAt: metricLocal,
		HeartbeatTime:       time.Date(2026, 1, 2, 3, 6, 5, 0, time.UTC),
	}
	n := SchedulerNodeScoreView(snap)
	if n.Score != 0.75 {
		t.Errorf("score = %v, want 0.75", n.Score)
	}
	if !n.MetricUpdate.Equal(metricUpdate) {
		t.Errorf("MetricUpdate = %v, want %v", n.MetricUpdate, metricUpdate)
	}
	if !n.MetricLocalUpdateAt.Equal(metricLocal) {
		t.Errorf("MetricLocalUpdateAt = %v, want %v", n.MetricLocalUpdateAt, metricLocal)
	}
	if !n.MetaDataUpdateAt.Equal(snap.HeartbeatTime) {
		t.Errorf("MetaDataUpdateAt = %v, want %v", n.MetaDataUpdateAt, snap.HeartbeatTime)
	}
}

func TestMergeIncomingHostFacts(t *testing.T) {
	prev := &model.HostFacts{
		CPUIDHash:            "sha256:cpu",
		KVMModuleFingerprint: "sha256:prev-module",
		KVMModuleTaint:       "taint-prev",
	}

	t.Run("scanned=true adopts incoming module state", func(t *testing.T) {
		incoming := &model.HostFacts{
			CPUIDHash: "sha256:cpu", KVMModuleScanned: true,
			KVMModuleFingerprint: "sha256:new-module", KVMModuleTaint: "taint-new",
		}
		out := mergeIncomingHostFacts(prev, incoming)
		assert.Equal(t, "sha256:new-module", out.KVMModuleFingerprint)
		assert.Equal(t, "taint-new", out.KVMModuleTaint)
		assert.False(t, out.KVMModuleScanned, "Scanned must be cleared")
	})

	t.Run("scanned=false preserves prev module state", func(t *testing.T) {
		incoming := &model.HostFacts{
			CPUIDHash: "sha256:cpu", KVMModuleScanned: false,
			KVMModuleFingerprint: "", KVMModuleTaint: "",
		}
		out := mergeIncomingHostFacts(prev, incoming)
		assert.Equal(t, "sha256:prev-module", out.KVMModuleFingerprint)
		assert.Equal(t, "taint-prev", out.KVMModuleTaint)
		assert.False(t, out.KVMModuleScanned)
	})

	t.Run("nil incoming returns prev", func(t *testing.T) {
		out := mergeIncomingHostFacts(prev, nil)
		assert.Equal(t, prev, out)
	})

	t.Run("zero incoming returns prev", func(t *testing.T) {
		out := mergeIncomingHostFacts(prev, &model.HostFacts{})
		assert.Equal(t, prev, out)
	})

	t.Run("nil prev takes incoming with Scanned cleared", func(t *testing.T) {
		incoming := &model.HostFacts{
			CPUIDHash: "sha256:cpu", KVMModuleScanned: true,
			KVMModuleFingerprint: "sha256:module",
		}
		out := mergeIncomingHostFacts(nil, incoming)
		assert.Equal(t, "sha256:module", out.KVMModuleFingerprint)
		assert.False(t, out.KVMModuleScanned)
	})
}

func TestMarshalHostFactsClearsScanned(t *testing.T) {
	f := &model.HostFacts{
		CPUIDHash: "sha256:cpu", KVMModuleScanned: true,
	}
	raw := marshalHostFacts(f)
	var decoded model.HostFacts
	_ = json.Unmarshal([]byte(raw), &decoded)
	assert.False(t, decoded.KVMModuleScanned, "Scanned must not reach DB JSON")
}

func TestToSchedulerNode_PreservesSchedulingFields(t *testing.T) {
	snap := &model.NodeSnapshot{
		NodeID:                "node-1",
		HostIP:                "10.0.0.1",
		Capacity:              model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable:           model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Zone:                  "gz-3",
		Region:                "gz",
		CPUType:               "intel-ice-lake",
		DeviceClass:           "GPU",
		DeviceID:              42,
		MachineHostIP:         "10.0.0.10",
		InstanceFamily:        "CVM.S5",
		DedicatedClusterID:    "cluster-1",
		VirtualNodeQuotaArray: []int64{4, 8},
		SystemDiskSize:        100,
		DataDiskSize:          200,
	}
	n := ToSchedulerNode(snap)
	if n == nil {
		t.Fatal("expected node")
	}
	if n.Zone != "gz-3" {
		t.Errorf("Zone = %s, want gz-3", n.Zone)
	}
	if n.Region != "gz" {
		t.Errorf("Region = %s, want gz", n.Region)
	}
	if n.CPUType != "intel-ice-lake" {
		t.Errorf("CPUType = %s, want intel-ice-lake", n.CPUType)
	}
	if n.DeviceClass != "GPU" {
		t.Errorf("DeviceClass = %s, want GPU", n.DeviceClass)
	}
	if n.DeviceID != 42 {
		t.Errorf("DeviceID = %d, want 42", n.DeviceID)
	}
	if n.MachineHostIP != "10.0.0.10" {
		t.Errorf("MachineHostIP = %s, want 10.0.0.10", n.MachineHostIP)
	}
	if n.InstanceFamily != "CVM.S5" {
		t.Errorf("InstanceFamily = %s, want CVM.S5", n.InstanceFamily)
	}
	if n.DedicatedClusterId != "cluster-1" {
		t.Errorf("DedicatedClusterId = %s, want cluster-1", n.DedicatedClusterId)
	}
	if len(n.VirtualNodeQuotaArray) != 2 || n.VirtualNodeQuotaArray[0] != 4 || n.VirtualNodeQuotaArray[1] != 8 {
		t.Errorf("VirtualNodeQuotaArray = %v, want [4 8]", n.VirtualNodeQuotaArray)
	}
	if n.SystemDiskSize != 100 || n.DataDiskSize != 200 {
		t.Errorf("disk sizes = %d/%d, want 100/200", n.SystemDiskSize, n.DataDiskSize)
	}
}

func TestApplyHostMetaToSnapshot(t *testing.T) {
	hosts := map[string]*store.HostInfo{
		"node-1": {
			InsID:           "node-1",
			Zone:            "gz-3",
			Region:          "gz",
			InstanceType:    "CVM.S5",
			ClusterLabel:    "default",
			QuotaCPU:        4000,
			QuotaMemMB:      8192,
			OssClusterLabel: "oss-1",
			SysDiskGB:       100,
			DataDiskGB:      200,
		},
	}
	hostTypes := map[string]*store.HostTypeInfo{
		"CVM.S5": {InstanceType: "CVM.S5", CPUType: "intel-ice-lake"},
	}
	subHosts := map[string]*store.SubHostInfo{
		"node-1": {
			InsID:              "node-1",
			HostIP:             "10.0.0.10",
			DeviceClass:        "GPU",
			DeviceID:           7,
			InstanceFamily:     "CVM.S5",
			DedicatedClusterID: "cluster-1",
			VirtualNodeQuota:   "[2,4]",
		},
	}

	snap := &model.NodeSnapshot{NodeID: "node-1"}
	applyHostMetaToSnapshot(snap, hosts, hostTypes, subHosts)

	if snap.Zone != "gz-3" || snap.Region != "gz" {
		t.Errorf("zone/region = %s/%s, want gz-3/gz", snap.Zone, snap.Region)
	}
	if snap.InstanceType != "CVM.S5" {
		t.Errorf("InstanceType = %s, want CVM.S5", snap.InstanceType)
	}
	if snap.CPUType != "intel-ice-lake" {
		t.Errorf("CPUType = %s, want intel-ice-lake", snap.CPUType)
	}
	if snap.DeviceClass != "GPU" || snap.DeviceID != 7 {
		t.Errorf("device = %s/%d, want GPU/7", snap.DeviceClass, snap.DeviceID)
	}
	if snap.MachineHostIP != "10.0.0.10" {
		t.Errorf("MachineHostIP = %s, want 10.0.0.10", snap.MachineHostIP)
	}
	if snap.InstanceFamily != "CVM.S5" {
		t.Errorf("InstanceFamily = %s, want CVM.S5", snap.InstanceFamily)
	}
	if snap.DedicatedClusterID != "cluster-1" {
		t.Errorf("DedicatedClusterID = %s, want cluster-1", snap.DedicatedClusterID)
	}
	if len(snap.VirtualNodeQuotaArray) != 2 || snap.VirtualNodeQuotaArray[0] != 2 || snap.VirtualNodeQuotaArray[1] != 4 {
		t.Errorf("VirtualNodeQuotaArray = %v, want [2 4]", snap.VirtualNodeQuotaArray)
	}
	if snap.SystemDiskSize != 100 || snap.DataDiskSize != 200 {
		t.Errorf("disk sizes = %d/%d, want 100/200", snap.SystemDiskSize, snap.DataDiskSize)
	}
}

func TestApplyHostMetaToSnapshot_EmptyQuota(t *testing.T) {
	hosts := map[string]*store.HostInfo{"node-1": {InsID: "node-1", Zone: "gz-3"}}
	subHosts := map[string]*store.SubHostInfo{
		"node-1": {InsID: "node-1", VirtualNodeQuota: "[]"},
	}
	snap := &model.NodeSnapshot{NodeID: "node-1"}
	applyHostMetaToSnapshot(snap, hosts, nil, subHosts)
	if snap.Zone != "gz-3" {
		t.Errorf("Zone = %s, want gz-3", snap.Zone)
	}
	if len(snap.VirtualNodeQuotaArray) != 0 {
		t.Errorf("VirtualNodeQuotaArray = %v, want empty", snap.VirtualNodeQuotaArray)
	}
}
