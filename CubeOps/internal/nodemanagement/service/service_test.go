// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func newTestService(t *testing.T) (*service.NodeService, *fakeNodeStore) {
	t.Helper()
	fs := newFakeNodeStore()
	svc := service.NewNodeService(fs, service.DeclaredVersionInfo{
		Primary: map[string]string{"cubelet": "v1.0"},
	})
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return svc, fs
}

func TestRegisterNode(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	req := &model.RegisterNodeRequest{
		NodeID:       "node-1",
		HostIP:       "10.0.0.1",
		Capacity:     model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable:  model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		InstanceType: "cubebox",
	}
	snap, err := svc.RegisterNode(ctx, req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if snap.NodeID != "node-1" {
		t.Errorf("nodeID = %s", snap.NodeID)
	}
	if snap.HostIP != "10.0.0.1" {
		t.Errorf("hostIP = %s", snap.HostIP)
	}
	reg, err := fs.GetRegistration(ctx, "node-1")
	if err != nil {
		t.Fatalf("get reg: %v", err)
	}
	if reg.HostIP != "10.0.0.1" {
		t.Errorf("reg hostIP = %s", reg.HostIP)
	}
}

func TestRegisterNode_RejectsSchedulingLabel(t *testing.T) {
	svc, _ := newTestService(t)
	req := &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{model.LabelSchedulingDisabled: "true"},
	}
	if _, err := svc.RegisterNode(context.Background(), req); err != service.ErrSchedulingLabelRejected {
		t.Errorf("got %v, want ErrSchedulingLabelRejected", err)
	}
}

func TestRegisterNode_RejectsInvalidLabels(t *testing.T) {
	svc, _ := newTestService(t)
	req := &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{"": "v"},
	}
	_, err := svc.RegisterNode(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty label key")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("err=%v, want contains 'invalid'", err)
	}
}

func TestUpdateNodeStatus(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	now := time.Now()
	snap, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime: now,
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !snap.Healthy {
		t.Errorf("expected healthy")
	}
}

func TestUpdateNodeStatus_NotReady(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	snap, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "False"}},
		HeartbeatTime: time.Now(),
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if snap.Healthy {
		t.Errorf("expected unhealthy")
	}
	if snap.UnhealthyReason != model.ReasonReportedNotReady {
		t.Errorf("reason = %s", snap.UnhealthyReason)
	}
}

func TestIsolateNode(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	snap, err := svc.SetNodeSchedulingDisabled(ctx, "node-1", true, "admin", "")
	if err != nil {
		t.Fatalf("isolate: %v", err)
	}
	if !snap.SchedulingDisabled {
		t.Errorf("expected scheduling disabled")
	}
	snap, err = svc.SetNodeSchedulingDisabled(ctx, "node-1", false, "admin", "")
	if err != nil {
		t.Fatalf("unisolate: %v", err)
	}
	if snap.SchedulingDisabled {
		t.Errorf("expected scheduling enabled")
	}
}

func TestSetNodeSchedulingDisabled_DetailLength(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-detail"})
	// Reset to non-isolated state for the test.
	_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-detail", false, "admin", "")

	tests := []struct {
		name    string
		detail  string
		wantErr bool
	}{
		// Pure ASCII letters — 200 chars pass, 201 rejected
		{"english within limit", strings.Repeat("a", 200), false},
		{"english over limit", strings.Repeat("a", 201), true},

		// Pure digits — 200 chars pass, 201 rejected
		{"digits within limit", strings.Repeat("1", 200), false},
		{"digits over limit", strings.Repeat("1", 201), true},

		// Pure CJK — rune-counted, 200 chars pass, 201 rejected
		{"chinese within limit", strings.Repeat("测", 200), false},
		{"chinese over limit", strings.Repeat("测", 201), true},

		// Pure ASCII punctuation
		{"ascii punctuation within limit", strings.Repeat("!", 200), false},
		{"ascii punctuation over limit", strings.Repeat("!", 201), true},

		// Mixed — "abc测试123" = 8 runes, 25x = 200 chars (pass), 26x = 208 chars (reject)
		{"mixed within limit", strings.Repeat("abc测试123", 25), false},
		{"mixed over limit", strings.Repeat("abc测试123", 26), true},

		// Empty — passes (falls back to default detail)
		{"empty detail", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure starting from non-isolated state so each subtest triggers a real change.
			_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-detail", false, "admin", "")
			_, err := svc.SetNodeSchedulingDisabled(ctx, "node-detail", true, "admin", tt.detail)
			if tt.wantErr {
				if !errors.Is(err, service.ErrDetailTooLong) {
					t.Fatalf("expected ErrDetailTooLong, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestUpdateLabels(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1", Labels: map[string]string{"zone": "gz"}})

	if err := svc.UpdateNodeLabels(ctx, "node-1", map[string]string{"rack": "r1"}, "admin"); err != nil {
		t.Fatalf("update labels: %v", err)
	}
	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if snap.Labels["zone"] != "gz" {
		t.Errorf("zone label lost")
	}
	if snap.Labels["rack"] != "r1" {
		t.Errorf("rack label missing")
	}
}

func TestUpdateLabels_ReservedRejected(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	err := svc.UpdateNodeLabels(ctx, "node-1", map[string]string{model.LabelSchedulingDisabled: "true"}, "admin")
	if err == nil {
		t.Errorf("expected error for reserved label")
	}
}

func TestListSchedulerNodes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1", HostIP: "10.0.0.1"})

	nodes, err := svc.ListSchedulerNodes(ctx)
	if err != nil {
		t.Fatalf("list scheduler nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len = %d", len(nodes))
	}
	if nodes[0].ID() != "node-1" {
		t.Errorf("id = %s", nodes[0].ID())
	}
	if nodes[0].HostIP() != "10.0.0.1" {
		t.Errorf("ip = %s", nodes[0].HostIP())
	}
}

func TestGetNode_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.GetNode(context.Background(), "missing")
	if err != store.ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestVersionMatrix(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID:   "node-1",
		Versions: []model.ComponentVersion{{Component: "cubelet", Version: "v1.0"}},
	})

	matrix, err := svc.GetVersionMatrix(ctx)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if matrix.ControlPlane["cubelet"] != "v1.0" {
		t.Errorf("control plane version = %s", matrix.ControlPlane["cubelet"])
	}
	if len(matrix.Components) != 1 {
		t.Errorf("components = %d", len(matrix.Components))
	}
}

func TestRegisterNode_DuplicateID(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	req := &model.RegisterNodeRequest{
		NodeID:       "node-1",
		HostIP:       "10.0.0.1",
		Capacity:     model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		Allocatable:  model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
		InstanceType: "cubebox",
	}
	if _, err := svc.RegisterNode(ctx, req); err != nil {
		t.Fatalf("first register: %v", err)
	}

	req.HostIP = "10.0.0.2"
	req.InstanceType = "cubebox-pro"
	snap, err := svc.RegisterNode(ctx, req)
	if err != nil {
		t.Fatalf("duplicate register: %v", err)
	}
	if snap.HostIP != "10.0.0.2" {
		t.Errorf("hostIP = %s, want 10.0.0.2", snap.HostIP)
	}
	if snap.InstanceType != "cubebox-pro" {
		t.Errorf("instanceType = %s, want cubebox-pro", snap.InstanceType)
	}
	reg, err := fs.GetRegistration(ctx, "node-1")
	if err != nil {
		t.Fatalf("get reg: %v", err)
	}
	if reg.HostIP != "10.0.0.2" {
		t.Errorf("reg hostIP = %s, want 10.0.0.2", reg.HostIP)
	}
}

func TestRegisterNode_WithLocalTemplates(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	req := &model.RegisterNodeRequest{
		NodeID:   "node-1",
		HostIP:   "10.0.0.1",
		Capacity: model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192},
	}
	if _, err := svc.RegisterNode(ctx, req); err != nil {
		t.Fatalf("register: %v", err)
	}

	now := time.Now()
	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:     []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime:  now,
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-1"}, {TemplateID: "tpl-2"}},
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}

	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(snap.LocalTemplates) != 2 {
		t.Fatalf("local templates = %d, want 2", len(snap.LocalTemplates))
	}
	if snap.LocalTemplates[0].TemplateID != "tpl-1" || snap.LocalTemplates[1].TemplateID != "tpl-2" {
		t.Errorf("local templates = %+v", snap.LocalTemplates)
	}
}

func TestUpdateNodeStatus_WritesMetricToRedis(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	var captured *nodemetric.NodeMetric
	cleanup := nodemetric.SetWriteNodeMetricHook(func(m *nodemetric.NodeMetric) error {
		captured = m
		return nil
	})
	defer cleanup()

	metricTime := time.Now().Add(-time.Minute)
	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime: time.Now(),
		Allocated: &model.AllocatedResources{
			MilliCPU: 1000,
			MemoryMB: 2048,
			MvmNum:   5,
		},
		DiskUsage: &model.DiskUsage{
			DataDiskUsagePer: 30.5,
		},
		MetricTime: metricTime,
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if captured == nil {
		t.Fatal("expected metric to be written")
	}
	if captured.NodeID != "node-1" {
		t.Errorf("nodeID = %s", captured.NodeID)
	}
	if !captured.MetricTime.Equal(metricTime) {
		t.Errorf("metricTime = %v, want %v", captured.MetricTime, metricTime)
	}
	if captured.MilliCPUUsage != 1000 || captured.MemoryMBUsage != 2048 || captured.MvmNum != 5 {
		t.Errorf("allocated usage mismatch: %+v", captured)
	}
	if !captured.HasDisk || captured.DataDiskUsagePer != 30.5 {
		t.Errorf("disk usage mismatch: %+v", captured)
	}
}

func TestUpdateNodeStatus_MetricTimeFallback(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	var captured *nodemetric.NodeMetric
	cleanup := nodemetric.SetWriteNodeMetricHook(func(m *nodemetric.NodeMetric) error {
		captured = m
		return nil
	})
	defer cleanup()

	before := time.Now()
	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime: time.Now(),
		Allocated:     &model.AllocatedResources{MilliCPU: 1000},
		MetricTime:    time.Time{},
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	after := time.Now()
	if captured == nil {
		t.Fatal("expected metric to be written")
	}
	if captured.MetricTime.Before(before) || captured.MetricTime.After(after) {
		t.Errorf("metricTime fallback out of range: %v", captured.MetricTime)
	}
}

func TestUpdateNodeStatus_LocalTemplatesReconciled(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:     []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime:  time.Now(),
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-1"}, {TemplateID: "tpl-2"}},
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, err = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:     []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime:  time.Now(),
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-2"}, {TemplateID: "tpl-3"}},
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}

	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	ids := make([]string, 0, len(snap.LocalTemplates))
	for _, t := range snap.LocalTemplates {
		ids = append(ids, t.TemplateID)
	}
	want := []string{"tpl-2", "tpl-3"}
	if len(ids) != len(want) {
		t.Fatalf("templates = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("template[%d] = %s, want %s", i, ids[i], want[i])
		}
	}
}

func TestUpdateNodeStatus_PartialHeartbeat(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	var captured *nodemetric.NodeMetric
	cleanup := nodemetric.SetWriteNodeMetricHook(func(m *nodemetric.NodeMetric) error {
		captured = m
		return nil
	})
	defer cleanup()

	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime: time.Now(),
		Allocated:     &model.AllocatedResources{MilliCPU: 1000},
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if captured == nil {
		t.Fatal("expected metric to be written")
	}
	if !captured.HasAllocated || captured.HasDisk {
		t.Errorf("hasAllocated=%v, hasDisk=%v", captured.HasAllocated, captured.HasDisk)
	}
}

func TestUpdateNodeStatus_NoMetricWriteOnEmptyAllocated(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	var captured bool
	cleanup := nodemetric.SetWriteNodeMetricHook(func(m *nodemetric.NodeMetric) error {
		captured = true
		return nil
	})
	defer cleanup()

	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
		HeartbeatTime: time.Now(),
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if captured {
		t.Error("expected no Redis metric write when metrics are empty")
	}
}

func TestUpdateNodeStatus_EmptyConditions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	snap, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		HeartbeatTime: time.Now(),
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if snap.Healthy {
		t.Error("expected unhealthy with empty conditions")
	}
	if snap.UnhealthyReason != model.ReasonReportedNotReady {
		t.Errorf("reason = %s, want %s", snap.UnhealthyReason, model.ReasonReportedNotReady)
	}
}

func TestUpdateNodeStatus_Concurrent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	const workers = 20
	done := make(chan struct{}, workers)
	for i := range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
				Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
				HeartbeatTime: time.Now(),
				Allocated:     &model.AllocatedResources{MilliCPU: int64(i * 100)},
			})
		}()
	}
	for range workers {
		<-done
	}

	_, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
}

func TestListSchedulerNodes_Concurrent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	for i := range 10 {
		_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{
			NodeID: fmt.Sprintf("node-%d", i),
			HostIP: fmt.Sprintf("10.0.0.%d", i),
		})
	}

	const workers = 20
	done := make(chan struct{}, workers)
	for i := range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			if i%2 == 0 {
				_, _ = svc.ListSchedulerNodes(ctx)
			} else {
				_, _ = svc.UpdateNodeStatus(ctx, fmt.Sprintf("node-%d", i%10), &model.UpdateNodeStatusRequest{
					Conditions:    []model.NodeCondition{{Type: "Ready", Status: "True"}},
					HeartbeatTime: time.Now(),
				})
			}
		}()
	}
	for range workers {
		<-done
	}
}

func TestRegisterNode_HostFacts(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()

	facts := &model.HostFacts{
		CPUVendor:         "AuthenticAMD",
		CPUModel:          "AMD EPYC 7K62",
		CPUIDHash:         "sha256:abc",
		HostKernelRelease: "5.15.0",
		KVMAPIVersion:     12,
	}
	req := &model.RegisterNodeRequest{
		NodeID:    "node-hf",
		HostIP:    "10.0.0.1",
		HostFacts: facts,
	}
	snap, err := svc.RegisterNode(ctx, req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if snap.HostFacts == nil {
		t.Fatal("snap.HostFacts is nil")
	}
	if snap.HostFacts.CPUIDHash != "sha256:abc" {
		t.Errorf("cpuid_hash = %s", snap.HostFacts.CPUIDHash)
	}

	// Verify HostFacts persisted to store.
	reg, err := fs.GetRegistration(ctx, "node-hf")
	if err != nil {
		t.Fatalf("get reg: %v", err)
	}
	if reg.HostFactsJSON == "" {
		t.Error("HostFactsJSON is empty")
	}
	if reg.CPUIDHash != "sha256:abc" {
		t.Errorf("reg.CPUIDHash = %s", reg.CPUIDHash)
	}
	if reg.HostKernelRelease != "5.15.0" {
		t.Errorf("reg.HostKernelRelease = %s", reg.HostKernelRelease)
	}
}

func TestUpdateNodeStatus_HostFacts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// Register first.
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-hf"})

	// Update with HostFacts.
	facts := &model.HostFacts{
		CPUVendor:         "AuthenticAMD",
		CPUIDHash:         "sha256:xyz",
		HostKernelRelease: "6.6.0",
		KVMAPIVersion:     12,
	}
	snap, err := svc.UpdateNodeStatus(ctx, "node-hf", &model.UpdateNodeStatusRequest{
		HostFacts:     facts,
		HeartbeatTime: time.Now(),
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if snap.HostFacts == nil {
		t.Fatal("snap.HostFacts is nil after update")
	}
	if snap.HostFacts.CPUIDHash != "sha256:xyz" {
		t.Errorf("cpuid_hash = %s", snap.HostFacts.CPUIDHash)
	}
}

func TestUpdateNodeStatus_NilHostFactsPreservesExisting(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// Register with HostFacts.
	facts := &model.HostFacts{CPUIDHash: "sha256:original", HostKernelRelease: "5.15.0", KVMAPIVersion: 12}
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-hf", HostFacts: facts})

	// Heartbeat without HostFacts (nil) should NOT wipe existing facts.
	snap, err := svc.UpdateNodeStatus(ctx, "node-hf", &model.UpdateNodeStatusRequest{
		HeartbeatTime: time.Now(),
	})
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if snap.HostFacts == nil {
		t.Fatal("snap.HostFacts was wiped by nil heartbeat")
	}
	if snap.HostFacts.CPUIDHash != "sha256:original" {
		t.Errorf("cpuid_hash = %s, want sha256:original", snap.HostFacts.CPUIDHash)
	}
}

func TestToSchedulerNode_HostFacts(t *testing.T) {
	facts := &model.HostFacts{
		CPUVendor:         "AuthenticAMD",
		CPUIDHash:         "sha256:abc",
		HostKernelRelease: "5.15.0",
		KVMAPIVersion:     12,
	}
	snap := &model.NodeSnapshot{
		NodeID:    "node-1",
		HostIP:    "10.0.0.1",
		HostFacts: facts,
	}
	n := service.ToSchedulerNode(snap)
	if n == nil {
		t.Fatal("expected node")
	}
	if n.HostFacts == nil {
		t.Fatal("HostFacts is nil")
	}
	if n.HostFacts.CPUIDHash != "sha256:abc" {
		t.Errorf("cpuid_hash = %s", n.HostFacts.CPUIDHash)
	}
	if n.HostFacts.CPUVendor != "AuthenticAMD" {
		t.Errorf("cpu_vendor = %s", n.HostFacts.CPUVendor)
	}
}

func TestHostFacts_IsZero(t *testing.T) {
	if !(*model.HostFacts)(nil).IsZero() {
		t.Error("nil should be zero")
	}
	if !(&model.HostFacts{}).IsZero() {
		t.Error("empty should be zero")
	}
	if (&model.HostFacts{CPUVendor: "x"}).IsZero() {
		t.Error("vendor set should not be zero")
	}
	if (&model.HostFacts{KVMAPIVersion: 12}).IsZero() {
		t.Error("kvm set should not be zero")
	}
}

// --- T1: DeleteNodeLabel ---

func TestDeleteNodeLabel(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{"zone": "gz", "rack": "r1"},
	})

	if err := svc.DeleteNodeLabel(ctx, "node-1", "rack", "admin"); err != nil {
		t.Fatalf("delete label: %v", err)
	}
	snap, _ := svc.GetNode(ctx, "node-1")
	if _, ok := snap.Labels["rack"]; ok {
		t.Error("rack label should be deleted")
	}
	if snap.Labels["zone"] != "gz" {
		t.Error("zone label should remain")
	}
}

func TestDeleteNodeLabel_RejectsReservedLabel(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	err := svc.DeleteNodeLabel(ctx, "node-1", model.LabelSchedulingDisabled, "admin")
	if err == nil {
		t.Fatal("expected error for reserved label")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("err=%v, want contains 'reserved'", err)
	}
}

func TestDeleteNodeLabel_RejectsInvalidKey(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	if err := svc.DeleteNodeLabel(ctx, "node-1", "", "admin"); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestDeleteNodeLabel_NodeNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	err := svc.DeleteNodeLabel(context.Background(), "missing", "zone", "admin")
	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

// --- T2: DB failure injection ---

func TestDeleteNodeLabel_StoreFailure(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1", Labels: map[string]string{"zone": "gz"}})

	dbErr := errors.New("db connection lost")
	fs.failOnGetRegistration = dbErr
	err := svc.DeleteNodeLabel(ctx, "node-1", "zone", "admin")
	if !errors.Is(err, dbErr) {
		t.Errorf("err=%v, want dbErr", err)
	}
}

func TestUpdateLabels_StoreFailure(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	dbErr := errors.New("db write failed")
	fs.failOnUpdateLabels = dbErr
	err := svc.UpdateNodeLabels(ctx, "node-1", map[string]string{"zone": "gz"}, "admin")
	if !errors.Is(err, dbErr) {
		t.Errorf("err=%v, want dbErr", err)
	}
}

// --- T3: label limit boundary ---

func TestRegisterNode_LabelLimitBoundary(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// 64 labels should succeed.
	labels := make(map[string]string, 64)
	for i := 0; i < 64; i++ {
		labels[fmt.Sprintf("key-%d", i)] = "v"
	}
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-64", Labels: labels}); err != nil {
		t.Fatalf("64 labels should succeed: %v", err)
	}

	// 65 labels should fail.
	labels["overflow"] = "v"
	_, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-65", Labels: labels})
	if err == nil {
		t.Fatal("65 labels should fail")
	}
}

// --- T4: surviving exported functions ---

func TestGetNodeComponentVersions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})
	_, _ = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Versions: []model.ComponentVersion{
			{Component: "guest-image", Version: "v2.0"},
			{Component: "cube-agent", Version: "v1.5"},
		},
		HeartbeatTime: time.Now(),
	})

	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(snap.Versions) != 2 {
		t.Errorf("versions count=%d, want 2", len(snap.Versions))
	}
}

func TestGetVersionMatrix(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})
	_, _ = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Versions:      []model.ComponentVersion{{Component: "guest-image", Version: "v2.0"}},
		HeartbeatTime: time.Now(),
	})

	vm, err := svc.GetVersionMatrix(ctx)
	if err != nil {
		t.Fatalf("get version matrix: %v", err)
	}
	if vm == nil {
		t.Fatal("version matrix should not be nil")
	}
}

func TestListOperations(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})
	_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-1", true, "admin", "test")

	ops, err := svc.ListOperations(ctx, "node-1", 10)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) == 0 {
		t.Fatal("expected at least one operation")
	}
}
