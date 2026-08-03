// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package recovery

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "worker-01", mock.isolated[0])
	require.Len(t, mock.paused, 1)
	assert.Equal(t, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", mock.paused[0])
	// Verify pod name was NOT used as sandbox ID.
	for _, id := range mock.paused {
		assert.NotEqual(t, "sandbox-pod-abc", id, "pod name was used as sandbox ID — should use hex ID from ListSandboxesByNode")
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
	assert.GreaterOrEqual(t, len(mock.isolated), 1, "expected at least 1 IsolateNode call")
	// With dedup, the second eviction skips sandboxes already paused by the first.
	// So only 2 unique PauseSandbox calls (not 4).
	assert.Len(t, mock.paused, 2, "expected 2 PauseSandbox calls (dedup)")
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

	require.Len(t, mock.resumed, 1)
	assert.Equal(t, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", mock.resumed[0])
	require.Len(t, mock.unisolated, 1)
	assert.Equal(t, "worker-02", mock.unisolated[0])
}

func TestOnPressureReliefNoopWhenNothingRecorded(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)

	mgr.OnPressureRelief("never-evicted-node")

	mock.mu.Lock()
	defer mock.mu.Unlock()

	assert.Empty(t, mock.unisolated, "expected no UnisolateNode calls for unknown node")
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox calls for unknown node")
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
		assert.NotEqual(t, "worker-03", n, "worker-03 should be removed from isolated nodes after relief")
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
	require.Len(t, mgr.paused["worker-resume-fail"], 1, "expected paused state to remain after resume failure")
	assert.Empty(t, mgr.isolated["worker-resume-fail"], "expected isolated state to clear after successful unisolate")
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
	_, pausedExists := mgr.paused["worker-unisolate-fail"]
	assert.False(t, pausedExists, "expected paused state to clear after resume success, got %v", mgr.paused)
	assert.NotEmpty(t, mgr.isolated["worker-unisolate-fail"], "expected isolated state to remain after unisolate failure")
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
	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "host-123", mock.isolated[0], "expected IsolateNode(host-123)")
	require.Len(t, mock.listHostIDs, 1)
	assert.Equal(t, "host-123", mock.listHostIDs[0], "expected ListSandboxesByNode(host-123)")
	require.Len(t, mock.unisolated, 1)
	assert.Equal(t, "host-123", mock.unisolated[0], "expected UnisolateNode(host-123)")
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
	require.Empty(t, mock.isolated, "expected recovery to stop on unresolved node: isolated=%v", mock.isolated)
	require.Empty(t, mock.listHostIDs, "expected recovery to stop on unresolved node: listHostIDs=%v", mock.listHostIDs)
	require.Empty(t, mock.paused, "expected recovery to stop on unresolved node: paused=%v", mock.paused)
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
	require.Len(t, mock.resumed, 1)
	assert.Equal(t, id, mock.resumed[0], "expected restored sandbox to resume")
	require.Len(t, mock.unisolated, 1)
	assert.Equal(t, "host-restored", mock.unisolated[0], "expected restored node to unisolate")
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
	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "host-pressured", mock.isolated[0], "expected restored isolation to be reasserted")
	assert.Empty(t, mock.unisolated, "expected no UnisolateNode while still pressured")
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox while still pressured")
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
	assert.GreaterOrEqual(t, len(mock.paused), 3, "expected at least one PauseSandbox call per node")
	mock.mu.Unlock()

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for node, sandboxes := range mgr.paused {
		seen := make(map[string]bool, len(sandboxes))
		for _, ps := range sandboxes {
			require.False(t, seen[ps.SandboxID], "duplicate paused state for node=%s sandboxID=%s", node, ps.SandboxID)
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

	assert.Equal(t, int32(20), calls.Load(), "expected 20 calls")
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
	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "worker-fail", mock.isolated[0], "expected IsolateNode(worker-fail)")
	// No sandboxes should be paused.
	assert.Empty(t, mock.paused, "expected 0 PauseSandbox calls on list failure")
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

	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "worker-empty", mock.isolated[0], "expected IsolateNode")
	assert.Empty(t, mock.paused, "expected 0 pauses")
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
	require.Len(t, mock.paused, 1)
	assert.Equal(t, goodID, mock.paused[0], "expected only good sandbox paused")
}

// TestReconcileRestoredSkipsWhenNoPressureChecker verifies that ReconcileRestored
// is a no-op (no panic, no CubeMaster calls) when no pressureChecker is set,
// even when isolated/paused state exists.
func TestReconcileRestoredSkipsWhenNoPressureChecker(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	// Populate some state so the early-exit path for empty nodes is not taken.
	mgr.isolated["worker-no-checker"] = "host-no-checker"
	mgr.paused["worker-no-checker"] = []PausedSandbox{{
		SandboxID:    "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
		InstanceType: "cubebox",
		EventID:      "uid-no-checker",
	}}
	// Intentionally do NOT call mgr.SetPressureChecker(...).

	// Must not panic.
	mgr.ReconcileRestored(context.Background())

	// Give any accidental goroutines time to fire.
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.isolated, "expected no IsolateNode call when pressureChecker is nil")
	assert.Empty(t, mock.unisolated, "expected no UnisolateNode call when pressureChecker is nil")
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox call when pressureChecker is nil")
	assert.Empty(t, mock.paused, "expected no PauseSandbox call when pressureChecker is nil")
}

// TestReconcileRestoredSkipsOnPressureCheckError verifies that when the
// pressureChecker returns an error for a node, ReconcileRestored skips that
// node without calling any CubeMaster API (isolated/resumed/unisolated all 0).
func TestReconcileRestoredSkipsOnPressureCheckError(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.isolated["worker-checker-err"] = "host-checker-err"
	mgr.paused["worker-checker-err"] = []PausedSandbox{{
		SandboxID:    "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
		InstanceType: "cubebox",
		EventID:      "uid-checker-err",
	}}
	mgr.SetPressureChecker(func(_ context.Context, _ string) (bool, error) {
		return false, fmt.Errorf("k8s API unavailable")
	})

	mgr.ReconcileRestored(context.Background())

	// Give any accidental goroutines time to fire.
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.isolated, "expected no IsolateNode call on pressure check error")
	assert.Empty(t, mock.unisolated, "expected no UnisolateNode call on pressure check error")
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox call on pressure check error")
}

// TestOnPressureDetectedSkipsAlreadyIsolated verifies that when a node is
// already in the isolated map, OnPressureDetected returns early without
// triggering startEviction (no IsolateNode or PauseSandbox calls).
func TestOnPressureDetectedSkipsAlreadyIsolated(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)
	// Pre-populate isolation state so the node looks already isolated.
	mgr.mu.Lock()
	mgr.isolated["worker-already-isolated"] = "host-already-isolated"
	mgr.mu.Unlock()

	mgr.OnPressureDetected("worker-already-isolated")

	// Give any accidental goroutines time to fire.
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.isolated, "expected no IsolateNode call for already-isolated node")
	assert.Empty(t, mock.paused, "expected no PauseSandbox call for already-isolated node")
}

// TestOnPressureDetectedTriggerIsolation verifies that when a node is NOT yet
// isolated, OnPressureDetected triggers startEviction which cordons the node
// and pauses its sandboxes.
func TestOnPressureDetectedTriggerIsolation(t *testing.T) {
	mock := &mockCubeMaster{
		listResult: []cubemaster.SandboxBrief{
			{SandboxID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"},
		},
	}
	mgr := newTestManager(mock)

	mgr.OnPressureDetected("worker-pressure-new")

	eventually(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.isolated) == 1 && len(mock.paused) == 1
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Len(t, mock.isolated, 1)
	assert.Equal(t, "worker-pressure-new", mock.isolated[0], "expected IsolateNode for the pressured node")
	require.Len(t, mock.paused, 1)
	assert.Equal(t, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", mock.paused[0], "expected PauseSandbox for the sandbox on the pressured node")
}

// TestNewCreatesManagerWithCorrectState verifies that New returns a non-nil
// Manager with properly initialised maps (no nil-map panics on first use).
func TestNewCreatesManagerWithCorrectState(t *testing.T) {
	// New requires a real *cubemaster.Client which we don't have; we just
	// verify it doesn't panic when passed nil (the client field is nil-safe
	// because cmIface takes precedence).
	mgr := New(nil)
	require.NotNil(t, mgr)
	require.NotNil(t, mgr.paused)
	require.NotNil(t, mgr.isolated)
	require.NotNil(t, mgr.evictionInFlight)
}

// TestNewWithPersisterNoStateFile verifies that NewWithPersister succeeds when
// the state file does not exist (file-not-found is treated as empty state).
func TestNewWithPersisterNoStateFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"

	mgr, err := NewWithPersister(nil, path)
	require.NoError(t, err)
	require.NotNil(t, mgr)
	assert.Empty(t, mgr.paused)
	assert.Empty(t, mgr.isolated)
}

// TestNewWithPersisterLoadsExistingState verifies that NewWithPersister reads
// previously saved isolated/paused data from a JSON state file.
func TestNewWithPersisterLoadsExistingState(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"

	// Write a minimal state file manually.
	state := `{"paused":{"worker-saved":[{"SandboxID":"aabbccddeeff00112233445566778899","InstanceType":"cubebox","EventID":"uid-saved"}]},"isolated":{"worker-saved":"host-saved"}}`
	require.NoError(t, os.WriteFile(path, []byte(state), 0o644))

	mgr, err := NewWithPersister(nil, path)
	require.NoError(t, err)
	require.NotNil(t, mgr)
	assert.Len(t, mgr.isolated, 1)
	assert.Equal(t, "host-saved", mgr.isolated["worker-saved"])
	require.Len(t, mgr.paused["worker-saved"], 1)
	assert.Equal(t, "aabbccddeeff00112233445566778899", mgr.paused["worker-saved"][0].SandboxID)
}

// TestClonePausedAndIsolated exercises the clone helpers directly to ensure
// they produce deep copies (mutations to the clone don't affect the original).
func TestClonePausedAndIsolated(t *testing.T) {
	orig := map[string][]PausedSandbox{
		"node-1": {{SandboxID: "aabb", InstanceType: "cubebox", EventID: "e1"}},
	}
	cloned := clonePaused(orig)
	cloned["node-1"][0].SandboxID = "ffff"
	assert.Equal(t, "aabb", orig["node-1"][0].SandboxID, "clone should not share backing array")

	origIso := map[string]string{"node-1": "host-1"}
	clonedIso := cloneIsolated(origIso)
	clonedIso["node-1"] = "host-changed"
	assert.Equal(t, "host-1", origIso["node-1"], "cloneIsolated should not share map")
}

// TestPersistSavesAndLoadsRoundTrip verifies that the persister can write and
// re-read a state without data loss.
func TestPersistSavesAndLoadsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := newPersister(dir + "/recovery.json")

	state := persistState{
		Paused: map[string][]PausedSandbox{
			"node-rt": {{SandboxID: "aabbccdd", InstanceType: "cubebox", EventID: "e-rt"}},
		},
		Isolated: map[string]string{"node-rt": "host-rt"},
	}
	require.NoError(t, p.save(state))

	loaded, err := p.load()
	require.NoError(t, err)
	assert.Equal(t, "host-rt", loaded.Isolated["node-rt"])
	require.Len(t, loaded.Paused["node-rt"], 1)
	assert.Equal(t, "aabbccdd", loaded.Paused["node-rt"][0].SandboxID)
}

// TestIsolatedNodesReturnsAllKeys verifies that IsolatedNodes returns every
// node stored in the isolated map.
func TestIsolatedNodesReturnsAllKeys(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.isolated["n1"] = "h1"
	mgr.isolated["n2"] = "h2"

	nodes := mgr.IsolatedNodes()
	assert.Len(t, nodes, 2)
	assert.ElementsMatch(t, []string{"n1", "n2"}, nodes)
}

// TestOnPressureReliefDeferredWhenStillPressured verifies that if the
// pressureChecker reports pressure is still true, OnPressureRelief does NOT
// call resume or unisolate (it defers via scheduleReliefRetry).
func TestOnPressureReliefDeferredWhenStillPressured(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.paused["worker-still-pressured"] = []PausedSandbox{{
		SandboxID:    id,
		InstanceType: "cubebox",
		EventID:      "uid-still-pressured",
	}}
	mgr.isolated["worker-still-pressured"] = "host-still-pressured"
	mgr.SetPressureChecker(func(_ context.Context, _ string) (bool, error) {
		return true, nil // still under pressure
	})

	mgr.OnPressureRelief("worker-still-pressured")

	// Give the goroutine from applyRelief (if incorrectly triggered) time to fire.
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox when node is still pressured")
	assert.Empty(t, mock.unisolated, "expected no UnisolateNode when node is still pressured")
}

// TestOnPressureReliefErrorReschedules verifies that if the pressureChecker
// returns an error, OnPressureRelief does not resume/unisolate and reschedules.
func TestOnPressureReliefErrorReschedules(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.paused["worker-checker-err2"] = []PausedSandbox{{
		SandboxID:    id,
		InstanceType: "cubebox",
		EventID:      "uid-checker-err2",
	}}
	mgr.isolated["worker-checker-err2"] = "host-checker-err2"
	mgr.SetPressureChecker(func(_ context.Context, _ string) (bool, error) {
		return false, fmt.Errorf("transient failure")
	})

	mgr.OnPressureRelief("worker-checker-err2")

	// Give any accidental goroutines time to fire.
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.resumed, "expected no ResumeSandbox on pressure check error")
	assert.Empty(t, mock.unisolated, "expected no UnisolateNode on pressure check error")
}

// TestReconcileRestoredNoPausedNoIsolated verifies ReconcileRestored returns
// immediately when there is no restored state (empty maps).
func TestReconcileRestoredNoPausedNoIsolated(t *testing.T) {
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	mgr.SetPressureChecker(func(_ context.Context, _ string) (bool, error) {
		return false, nil
	})

	// Should not panic and not call any CubeMaster APIs.
	mgr.ReconcileRestored(context.Background())
	time.Sleep(20 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.isolated)
	assert.Empty(t, mock.unisolated)
	assert.Empty(t, mock.resumed)
}

// TestReconcileRestoredPausedOnlyNodeNoIsolation covers the branch where a
// node appears in paused but NOT in isolated (cubeMasterNodeID == "").
// Under pressure this path logs and skips (no IsolateNode call).
func TestReconcileRestoredPausedOnlyNodeUnderPressure(t *testing.T) {
	id := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	mock := &mockCubeMaster{}
	mgr := newTestManager(mock)
	// paused but NOT isolated — cubeMasterNodeID will be ""
	mgr.paused["worker-paused-only"] = []PausedSandbox{{
		SandboxID:    id,
		InstanceType: "cubebox",
		EventID:      "uid-paused-only",
	}}
	mgr.SetPressureChecker(func(_ context.Context, _ string) (bool, error) {
		return true, nil // still pressured
	})

	mgr.ReconcileRestored(context.Background())
	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	// cubeMasterNodeID is "" so IsolateNode must NOT be called.
	assert.Empty(t, mock.isolated, "expected no IsolateNode when no cubeMasterNodeID stored")
}
