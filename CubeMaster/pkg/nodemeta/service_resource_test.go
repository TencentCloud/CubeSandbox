// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
	corev1 "k8s.io/api/core/v1"
)

func init() {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		os.Setenv("CUBE_MASTER_CONFIG_PATH", filepath.Clean(filepath.Join(mydir, "../../conf.yaml")))
	}
	if _, err := config.Init(); err != nil {
		panic(err)
	}
}

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

func TestToSchedulerNodeCopiesHostFacts(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID:        "node-facts",
		HostIP:        "10.0.0.2",
		HeartbeatTime: time.Now(),
		Healthy:       true,
		HostFacts: &HostFacts{
			CPUVendor:             "GenuineIntel",
			CPUModel:              "Xeon 8255C",
			CPUIDHash:             "sha256:cpu",
			HostKernelRelease:     "5.15.0",
			HostKernelFingerprint: "sha256:kernel",
			KVMAPIVersion:         12,
			KVMModuleFingerprint:  "sha256:kvmmod",
			KVMModuleTaint:        "EO",
		},
	}
	n := toSchedulerNode(snap)
	if n == nil {
		t.Fatal("toSchedulerNode returned nil")
	}
	if n.HostFacts == nil {
		t.Fatal("HostFacts must be copied to the scheduler node")
	}
	if n.HostFacts.CPUVendor != "GenuineIntel" ||
		n.HostFacts.CPUIDHash != "sha256:cpu" ||
		n.HostFacts.HostKernelFingerprint != "sha256:kernel" ||
		n.HostFacts.KVMAPIVersion != 12 ||
		n.HostFacts.KVMModuleFingerprint != "sha256:kvmmod" ||
		n.HostFacts.KVMModuleTaint != "EO" {
		t.Errorf("HostFacts mismatch: %+v", n.HostFacts)
	}
	// Must be an independent copy, not the snapshot's pointer.
	n.HostFacts.CPUVendor = "AuthenticAMD"
	if snap.HostFacts.CPUVendor != "GenuineIntel" {
		t.Errorf("scheduler node must not alias the snapshot HostFacts pointer")
	}
}

func TestToSchedulerNodeNilHostFacts(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID:        "node-no-facts",
		HostIP:        "10.0.0.3",
		HeartbeatTime: time.Now(),
		Healthy:       true,
	}
	n := toSchedulerNode(snap)
	if n == nil {
		t.Fatal("toSchedulerNode returned nil")
	}
	if n.HostFacts != nil {
		t.Errorf("nil snapshot HostFacts must yield nil node HostFacts, got %+v", n.HostFacts)
	}
}

func TestToSchedulerNodeUsesPrecomputedHealth(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID:          "node-health",
		HostIP:          "10.0.0.9",
		HeartbeatTime:   time.Now(),
		ReportedReady:   true,
		Healthy:         false,
		UnhealthyReason: nodehealth.ReasonHeartbeatExpired,
	}
	n := toSchedulerNode(snap)
	if n == nil {
		t.Fatal("toSchedulerNode returned nil")
	}
	if n.Healthy {
		t.Fatal("toSchedulerNode should preserve snapshot health state")
	}
	if n.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", n.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
}

func TestCurrentHealthStatusExpiresStaleHeartbeat(t *testing.T) {
	now := time.Now()

	t.Run("fresh ready heartbeat stays healthy", func(t *testing.T) {
		snap := &NodeSnapshot{
			NodeID:        "node-a",
			HeartbeatTime: now.Add(-5 * time.Second),
			ReportedReady: true,
		}
		got := currentHealthStatus(snap, now)
		if !got.Healthy {
			t.Fatalf("Healthy=%v want true", got.Healthy)
		}
		if got.UnhealthyReason != "" {
			t.Fatalf("UnhealthyReason=%s want empty", got.UnhealthyReason)
		}
	})

	t.Run("stale ready heartbeat becomes unhealthy", func(t *testing.T) {
		snap := &NodeSnapshot{
			NodeID:        "node-b",
			HeartbeatTime: now.Add(-(healthTimeout() + time.Second)),
			ReportedReady: true,
		}
		got := currentHealthStatus(snap, now)
		if got.Healthy {
			t.Fatalf("Healthy=%v want false", got.Healthy)
		}
		if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
			t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
		}
	})

	t.Run("missing heartbeat is treated as expired", func(t *testing.T) {
		snap := &NodeSnapshot{
			NodeID:        "node-missing",
			ReportedReady: false,
		}
		got := currentHealthStatus(snap, now)
		if got.Healthy {
			t.Fatalf("Healthy=%v want false", got.Healthy)
		}
		if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
			t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
		}
	})
}

func TestUpdateNodeStatusStoresLastReportedReadyButExportsCurrentHealth(t *testing.T) {
	req := &UpdateNodeStatusRequest{
		Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}},
		HeartbeatTime: time.Now().Add(-(healthTimeout() + time.Second)),
	}

	reportedReady := nodehealth.ReadyConditionTrue(req.Conditions)
	if !reportedReady {
		t.Fatal("reported ready should be true")
	}
	snap := &NodeSnapshot{
		NodeID:        "node-c",
		Conditions:    req.Conditions,
		HeartbeatTime: req.HeartbeatTime,
		ReportedReady: reportedReady,
	}
	applyCurrentHealth(snap, time.Now())

	if !snap.ReportedReady {
		t.Fatal("ReportedReady should preserve the last reported Ready=True fact")
	}
	if snap.Healthy {
		t.Fatal("Healthy should reflect current stale heartbeat state, want false")
	}
	if snap.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", snap.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
}

func TestCloneSnapshotWithCurrentHealthRefreshesStaleNode(t *testing.T) {
	now := time.Now()
	snap := &NodeSnapshot{
		NodeID:        "node-d",
		HeartbeatTime: now.Add(-(healthTimeout() + time.Second)),
		ReportedReady: true,
		Healthy:       true,
	}

	got := cloneSnapshotWithCurrentHealth(snap, now)
	if got == nil {
		t.Fatal("cloneSnapshotWithCurrentHealth returned nil")
	}
	if got.Healthy {
		t.Fatal("stale heartbeat should be refreshed to unhealthy on snapshot reads")
	}
	if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
	if !snap.Healthy {
		t.Fatal("source snapshot should not be mutated by cloneSnapshotWithCurrentHealth")
	}
}

func TestNodeSnapshotJSONIncludesHealthyFalseAndHidesReportedReady(t *testing.T) {
	snap := &NodeSnapshot{
		NodeID:          "node-json",
		ReportedReady:   true,
		Healthy:         false,
		UnhealthyReason: nodehealth.ReasonHeartbeatExpired,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	payload := string(data)
	if !strings.Contains(payload, `"healthy":false`) {
		t.Fatalf("payload %s should include healthy=false", payload)
	}
	if strings.Contains(payload, `"reported_ready"`) {
		t.Fatalf("payload %s should not expose reported_ready", payload)
	}
	if !strings.Contains(payload, `"unhealthy_reason":"HeartbeatExpired"`) {
		t.Fatalf("payload %s should include unhealthy_reason", payload)
	}
}
