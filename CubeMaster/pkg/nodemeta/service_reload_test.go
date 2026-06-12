package nodemeta

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// newTestService returns a service with a pre-populated in-memory node map,
// suitable for applyReloadResult unit tests (no DB required).
func newTestService(snaps ...*NodeSnapshot) *service {
	s := &service{nodes: make(map[string]*NodeSnapshot, len(snaps))}
	for _, snap := range snaps {
		s.nodes[snap.NodeID] = snap
	}
	return s
}

func TestApplyReloadResultUpdatesRegistrationFields(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID:       "node-a",
		Labels:       map[string]string{"zone": "old"},
		InstanceType: "old-type",
		HostIP:       "1.1.1.1",
		GRPCPort:     9000,
		QuotaCPU:     100,
		QuotaMemMB:   512,
	})

	next := map[string]*NodeSnapshot{
		"node-a": {
			NodeID:       "node-a",
			Labels:       map[string]string{"zone": "new", "env": "prod"},
			InstanceType: "new-type",
			HostIP:       "2.2.2.2",
			GRPCPort:     9001,
			QuotaCPU:     200,
			QuotaMemMB:   1024,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-a"]
	s.mu.RUnlock()

	if snap.Labels["zone"] != "new" || snap.Labels["env"] != "prod" {
		t.Fatalf("Labels not updated: %v", snap.Labels)
	}
	if snap.InstanceType != "new-type" {
		t.Fatalf("InstanceType not updated: %s", snap.InstanceType)
	}
	if snap.HostIP != "2.2.2.2" {
		t.Fatalf("HostIP not updated: %s", snap.HostIP)
	}
	if snap.GRPCPort != 9001 {
		t.Fatalf("GRPCPort not updated: %d", snap.GRPCPort)
	}
	if snap.QuotaCPU != 200 {
		t.Fatalf("QuotaCPU not updated: %d", snap.QuotaCPU)
	}
	if snap.QuotaMemMB != 1024 {
		t.Fatalf("QuotaMemMB not updated: %d", snap.QuotaMemMB)
	}
}

func TestApplyReloadResultPreservesInMemoryHeartbeatWhenFresher(t *testing.T) {
	inMemoryTime := time.Now()
	dbTime := inMemoryTime.Add(-5 * time.Second)

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-b",
		HeartbeatTime: inMemoryTime,
		ReportedReady: true,
		Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}},
	})

	next := map[string]*NodeSnapshot{
		"node-b": {
			NodeID:        "node-b",
			HeartbeatTime: dbTime,
			ReportedReady: false,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-b"]
	s.mu.RUnlock()

	if !snap.HeartbeatTime.Equal(inMemoryTime) {
		t.Fatalf("HeartbeatTime regressed: got %v want %v", snap.HeartbeatTime, inMemoryTime)
	}
	if !snap.ReportedReady {
		t.Fatal("ReportedReady regressed: in-memory value should be preserved")
	}
}

func TestApplyReloadResultTakesDBHeartbeatWhenFresher(t *testing.T) {
	oldTime := time.Now().Add(-10 * time.Second)
	newTime := time.Now()

	s := newTestService(&NodeSnapshot{
		NodeID:        "node-c",
		HeartbeatTime: oldTime,
		ReportedReady: false,
	})

	next := map[string]*NodeSnapshot{
		"node-c": {
			NodeID:        "node-c",
			HeartbeatTime: newTime,
			ReportedReady: true,
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-c"]
	s.mu.RUnlock()

	if !snap.HeartbeatTime.Equal(newTime) {
		t.Fatalf("HeartbeatTime not updated: got %v want %v", snap.HeartbeatTime, newTime)
	}
	if !snap.ReportedReady {
		t.Fatal("ReportedReady not updated from DB")
	}
}

func TestApplyReloadResultSyncsVersionsForExistingNode(t *testing.T) {
	s := newTestService(&NodeSnapshot{
		NodeID: "node-d",
		Versions: []ComponentVersion{
			{Component: "cubelet", Version: "v1.0.0"},
		},
		versionsHash: "oldhash",
	})

	newVersions := []ComponentVersion{
		{Component: "cubelet", Version: "v2.0.0"},
		{Component: "cube-agent", Version: "v1.5.0"},
	}
	next := map[string]*NodeSnapshot{
		"node-d": {
			NodeID:       "node-d",
			Versions:     newVersions,
			versionsHash: versionsHash(newVersions),
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap := s.nodes["node-d"]
	s.mu.RUnlock()

	if len(snap.Versions) != 2 {
		t.Fatalf("Versions length = %d, want 2", len(snap.Versions))
	}
	found := false
	for _, v := range snap.Versions {
		if v.Component == "cubelet" && v.Version == "v2.0.0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("updated cubelet version not found in %v", snap.Versions)
	}
	wantHash := versionsHash(newVersions)
	if snap.versionsHash != wantHash {
		t.Fatalf("versionsHash = %s, want %s", snap.versionsHash, wantHash)
	}
}

func TestApplyReloadResultAddsNewNodeFromDB(t *testing.T) {
	s := newTestService(&NodeSnapshot{NodeID: "node-existing"})

	newTime := time.Now()
	next := map[string]*NodeSnapshot{
		"node-existing": {NodeID: "node-existing"},
		"node-new": {
			NodeID:        "node-new",
			HostIP:        "3.3.3.3",
			Labels:        map[string]string{"region": "us-east"},
			HeartbeatTime: newTime,
			ReportedReady: true,
		},
	}
	s.applyReloadResult(next)

	s.mu.RLock()
	snap, ok := s.nodes["node-new"]
	s.mu.RUnlock()

	if !ok {
		t.Fatal("new node from DB not added to in-memory map")
	}
	if snap.HostIP != "3.3.3.3" {
		t.Fatalf("HostIP = %s, want 3.3.3.3", snap.HostIP)
	}
	if snap.Labels["region"] != "us-east" {
		t.Fatalf("Labels = %v, want region=us-east", snap.Labels)
	}
}
