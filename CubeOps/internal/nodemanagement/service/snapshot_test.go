// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
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
