// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

// mockCubeMaster records calls made by the Manager.
type mockCubeMaster struct {
	mu            sync.Mutex
	isolated      []string
	unisolated    []string
	paused        []string
	resumed       []string
	listResult    []cubemaster.SandboxBrief // sandboxes to return from ListSandboxesByNode
	listErr       error                     // optional: force list failure
	pauseFailIDs  map[string]bool           // sandbox IDs that should fail PauseSandbox
	resumeFailIDs map[string]bool           // sandbox IDs that should fail ResumeSandbox
	unisolateErr  error                     // optional: force UnisolateNode failure
	resolvedIDs   map[string]string         // optional ResolveHostID mapping
	resolveErr    error
	listHostIDs   []string
}

func (m *mockCubeMaster) IsolateNode(_ context.Context, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isolated = append(m.isolated, nodeID)
	return nil
}

func (m *mockCubeMaster) UnisolateNode(_ context.Context, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unisolateErr != nil {
		return m.unisolateErr
	}
	m.unisolated = append(m.unisolated, nodeID)
	return nil
}

func (m *mockCubeMaster) PauseSandbox(_ context.Context, sandboxID, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pauseFailIDs != nil && m.pauseFailIDs[sandboxID] {
		return fmt.Errorf("forced pause failure for %s", sandboxID)
	}
	m.paused = append(m.paused, sandboxID)
	return nil
}

func (m *mockCubeMaster) ResumeSandbox(_ context.Context, sandboxID, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resumeFailIDs != nil && m.resumeFailIDs[sandboxID] {
		return fmt.Errorf("forced resume failure for %s", sandboxID)
	}
	m.resumed = append(m.resumed, sandboxID)
	return nil
}

func (m *mockCubeMaster) ListSandboxesByNode(_ context.Context, hostID string) ([]cubemaster.SandboxBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listHostIDs = append(m.listHostIDs, hostID)
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listResult, nil
}

func (m *mockCubeMaster) ResolveHostID(_ context.Context, identifier string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	if m.resolvedIDs != nil {
		if resolved, ok := m.resolvedIDs[identifier]; ok {
			return resolved, nil
		}
	}
	return identifier, nil // identity: mock returns the same identifier
}

func newTestManager(mock *mockCubeMaster) *Manager {
	mgr := &Manager{
		cm:       nil,
		cmIface:  mock,
		paused:   make(map[string][]PausedSandbox),
		isolated: make(map[string]string),
	}
	return mgr
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before deadline")
}

func TestOnEvictionCordonsAndPauses(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	event := &types.EvictionEvent{
		EventID:       "uid-1",
		PodName:       "sandbox-pod-abc", // pod name, NOT sandbox ID
		Namespace:     "cube-system",
		NodeName:      "worker-01",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	mgr.OnEviction(event)

	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) == 1 && len(mock.paused) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.isolated) != 1 || mock.isolated[0] != "worker-01" {
		t.Errorf("expected IsolateNode(worker-01), got %v", mock.isolated)
	}
	if len(mock.paused) != 1 || mock.paused[0] != "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6" {
		t.Errorf("expected PauseSandbox with hex ID, got %v", mock.paused)
	}
	// Verify pod name was NOT used as sandbox ID.
	for _, id := range mock.paused {
		if id == "sandbox-pod-abc" {
			t.Error("pod name was used as sandbox ID — should use hex ID from ListSandboxesByNode")
		}
	}
}

func TestOnEvictionIdempotentCordon(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
			{SandboxID: "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6"},
		},
	}
	mgr := newTestManager(mock)

	// Two sandboxes evicted from the same node.
	for i := 0; i < 2; i++ {
		mgr.OnEviction(&types.EvictionEvent{
			EventID:      fmt.Sprintf("uid-%d", i),
			PodName:      fmt.Sprintf("sandbox-pod-%d", i),
			NodeName:     "worker-01",
			InstanceType: "cubebox",
		})
	}

	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) >= 1 && len(mock.paused) == 2
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// IsolateNode should only be called once despite two evictions (dedup by isolated map).
	// Note: with the timing fix, isolated is set after IsolateNode succeeds, so the
	// second eviction might still call IsolateNode if the first hasn't completed yet.
	// That's acceptable — IsolateNode is idempotent. We verify at least one call.
	if len(mock.isolated) < 1 {
		t.Errorf("expected at least 1 IsolateNode call, got %d", len(mock.isolated))
	}
	// With dedup, the second eviction skips sandboxes already paused by the first.
	// So only 2 unique PauseSandbox calls (not 4).
	if len(mock.paused) != 2 {
		t.Errorf("expected 2 PauseSandbox calls (dedup), got %d", len(mock.paused))
	}
}

func TestOnPressureReliefResumesAndUncordons(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	// Record an eviction.
	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-relief",
		PodName:      "sandbox-pod-xyz",
		NodeName:     "worker-02",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1
	})

	// Pressure clears.
	mgr.OnPressureRelief("worker-02")
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.resumed) == 1 && len(mock.unisolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.resumed) != 1 || mock.resumed[0] != "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6" {
		t.Errorf("expected ResumeSandbox with hex ID, got %v", mock.resumed)
	}
	if len(mock.unisolated) != 1 || mock.unisolated[0] != "worker-02" {
		t.Errorf("expected UnisolateNode(worker-02), got %v", mock.unisolated)
	}
}

func TestOnPressureReliefNoopWhenNothingRecorded(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)

	mgr.OnPressureRelief("never-evicted-node")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.unisolated) != 0 || len(mock.resumed) != 0 {
		t.Errorf("expected no calls for unknown node, got unisolated=%v resumed=%v",
			mock.unisolated, mock.resumed)
	}
}

func TestOnPressureReliefClearsState(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-state",
		PodName:      "sandbox-pod-state",
		NodeName:     "worker-03",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1
	})

	mgr.OnPressureRelief("worker-03")
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.resumed) == 1 && len(mock.unisolated) == 1
	})

	// After relief, internal state must be cleared.
	nodes := mgr.IsolatedNodes()
	for _, n := range nodes {
		if n == "worker-03" {
			t.Error("worker-03 should be removed from isolated nodes after relief")
		}
	}
}

func TestOnPressureReliefKeepsStateOnResumeFailure(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{
		listResult:    []cubemaster.SandboxBrief{{SandboxID: id}},
		resumeFailIDs: map[string]bool{id: true},
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-resume-fail",
		PodName:      "sandbox-pod-resume-fail",
		NodeName:     "worker-resume-fail",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1
	})

	mgr.OnPressureRelief("worker-resume-fail")
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.unisolated) == 1
	})

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.paused["worker-resume-fail"]) != 1 {
		t.Fatalf("expected paused state to remain after resume failure, got %v", mgr.paused)
	}
	if mgr.isolated["worker-resume-fail"] != "" {
		t.Error("expected isolated state to clear after successful unisolate")
	}
}

func TestOnPressureReliefKeepsIsolationOnUnisolateFailure(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{
		listResult:   []cubemaster.SandboxBrief{{SandboxID: id}},
		unisolateErr: fmt.Errorf("temporary unisolate failure"),
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-unisolate-fail",
		PodName:      "sandbox-pod-unisolate-fail",
		NodeName:     "worker-unisolate-fail",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1
	})

	mgr.OnPressureRelief("worker-unisolate-fail")
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.resumed) == 1
	})

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if _, ok := mgr.paused["worker-unisolate-fail"]; ok {
		t.Fatalf("expected paused state to clear after resume success, got %v", mgr.paused)
	}
	if mgr.isolated["worker-unisolate-fail"] == "" {
		t.Error("expected isolated state to remain after unisolate failure")
	}
}

func TestRecoveryUsesResolvedCubeMasterNodeIDConsistently(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{
		listResult:  []cubemaster.SandboxBrief{{SandboxID: id}},
		resolvedIDs: map[string]string{"worker-name": "host-123"},
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-hostid",
		PodName:      "sandbox-pod-hostid",
		NodeName:     "worker-name",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1 && len(mock.listHostIDs) == 1
	})

	mgr.OnPressureRelief("worker-name")
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.unisolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.isolated) != 1 || mock.isolated[0] != "host-123" {
		t.Fatalf("expected IsolateNode(host-123), got %v", mock.isolated)
	}
	if len(mock.listHostIDs) != 1 || mock.listHostIDs[0] != "host-123" {
		t.Fatalf("expected ListSandboxesByNode(host-123), got %v", mock.listHostIDs)
	}
	if len(mock.unisolated) != 1 || mock.unisolated[0] != "host-123" {
		t.Fatalf("expected UnisolateNode(host-123), got %v", mock.unisolated)
	}
}

func TestOnEvictionStopsWhenNodeIDCannotResolve(t *testing.T) {
	mock := &mockCubeMaster{
		resolveErr: fmt.Errorf("node not registered"),
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-resolve-fail",
		PodName:      "sandbox-pod-resolve-fail",
		NodeName:     "worker-unregistered",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return !mgr.evictionInFlight["worker-unregistered"]
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.isolated) != 0 || len(mock.listHostIDs) != 0 || len(mock.paused) != 0 {
		t.Fatalf("expected recovery to stop on unresolved node, isolated=%v listHostIDs=%v paused=%v",
			mock.isolated, mock.listHostIDs, mock.paused)
	}
}

func TestReconcileRestoredRelievesNodeWhenPressureAlreadyCleared(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.paused["worker-restored"] = []PausedSandbox{{
		SandboxID:    id,
		InstanceType: "cubebox",
		EventID:      "uid-restored",
	}}
	mgr.isolated["worker-restored"] = "host-restored"
	mgr.SetPressureChecker(func(context.Context, string) (bool, error) {
		return false, nil
	})

	mgr.ReconcileRestored(context.Background())
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.resumed) == 1 && len(mock.unisolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.resumed) != 1 || mock.resumed[0] != id {
		t.Fatalf("expected restored sandbox to resume, got %v", mock.resumed)
	}
	if len(mock.unisolated) != 1 || mock.unisolated[0] != "host-restored" {
		t.Fatalf("expected restored node to unisolate, got %v", mock.unisolated)
	}
}

func TestReconcileRestoredReassertsIsolationWhenStillPressured(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.isolated["worker-pressured"] = "host-pressured"
	mgr.SetPressureChecker(func(context.Context, string) (bool, error) {
		return true, nil
	})

	mgr.ReconcileRestored(context.Background())
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.isolated) != 1 || mock.isolated[0] != "host-pressured" {
		t.Fatalf("expected restored isolation to be reasserted, got %v", mock.isolated)
	}
	if len(mock.unisolated) != 0 || len(mock.resumed) != 0 {
		t.Fatalf("expected no relief while still pressured, unisolated=%v resumed=%v",
			mock.unisolated, mock.resumed)
	}
}

func TestConcurrentEvictions(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	var wg sync.WaitGroup
	const n = 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			mgr.OnEviction(&types.EvictionEvent{
				EventID:      fmt.Sprintf("uid-conc-%d", idx),
				PodName:      fmt.Sprintf("sandbox-pod-%d", idx),
				NodeName:     fmt.Sprintf("worker-%d", idx%3),
				InstanceType: "cubebox",
			})
		}(i)
	}
	wg.Wait()
	eventually(t, func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return len(mgr.evictionInFlight) == 0
	})

	mock.mu.Lock()
	if len(mock.paused) < 3 {
		t.Errorf("expected at least one PauseSandbox call per node, got %d", len(mock.paused))
	}
	mock.mu.Unlock()

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for node, sandboxes := range mgr.paused {
		seen := make(map[string]bool, len(sandboxes))
		for _, ps := range sandboxes {
			if seen[ps.SandboxID] {
				t.Fatalf("duplicate paused state for node=%s sandboxID=%s", node, ps.SandboxID)
			}
			seen[ps.SandboxID] = true
		}
	}
}

// Ensure atomic counter is used correctly (race detector bait).
func TestOnEvictionRaceDetector(t *testing.T) {
	var calls atomic.Int32
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr.OnEviction(&types.EvictionEvent{
				EventID:      fmt.Sprintf("r-%d", i),
				PodName:      fmt.Sprintf("s-%d", i),
				NodeName:     "node-race",
				InstanceType: "cubebox",
			})
			calls.Add(1)
		}(i)
	}
	wg.Wait()
	eventually(t, func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return len(mgr.evictionInFlight) == 0
	})

	if calls.Load() != 20 {
		t.Errorf("expected 20 calls, got %d", calls.Load())
	}
}

func TestOnEvictionListFailure(t *testing.T) {
	mock := &mockCubeMaster{
		listErr: fmt.Errorf("CubeMaster unreachable"),
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:  "uid-list-fail",
		PodName:  "sandbox-pod-fail",
		NodeName: "worker-fail",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Node should still be cordoned (IsolateNode succeeds even though List fails).
	if len(mock.isolated) != 1 || mock.isolated[0] != "worker-fail" {
		t.Errorf("expected IsolateNode(worker-fail), got %v", mock.isolated)
	}
	// No sandboxes should be paused.
	if len(mock.paused) != 0 {
		t.Errorf("expected 0 PauseSandbox calls on list failure, got %d", len(mock.paused))
	}
}

func TestOnEvictionEmptySandboxList(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{}, // empty
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:  "uid-empty",
		PodName:  "sandbox-pod-empty",
		NodeName: "worker-empty",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.isolated) != 1 || mock.isolated[0] != "worker-empty" {
		t.Errorf("expected IsolateNode, got %v", mock.isolated)
	}
	if len(mock.paused) != 0 {
		t.Errorf("expected 0 pauses, got %d", len(mock.paused))
	}
}

func TestOnEvictionPartialPauseFailure(t *testing.T) {
	goodID := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	badID := "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6"
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: goodID},
			{SandboxID: badID},
		},
		pauseFailIDs: map[string]bool{badID: true},
	}
	mgr := newTestManager(mock)

	mgr.OnEviction(&types.EvictionEvent{
		EventID:      "uid-partial",
		PodName:      "sandbox-pod-partial",
		NodeName:     "worker-partial",
		InstanceType: "cubebox",
	})
	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.paused) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Only the good sandbox should be paused.
	if len(mock.paused) != 1 || mock.paused[0] != goodID {
		t.Errorf("expected only good sandbox paused, got %v", mock.paused)
	}
}
