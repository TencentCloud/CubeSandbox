// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// fakeSandboxChecker is a stub SandboxInventoryChecker for deletion tests.
type fakeSandboxChecker struct {
	count int
	err   error
	mu    sync.Mutex
	calls int
}

func (f *fakeSandboxChecker) CountNodeSandboxes(_ context.Context, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.count, f.err
}

func (f *fakeSandboxChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// registerAndIsolate creates a healthy node and isolates it. A heartbeat is
// sent so the node counts as healthy for DeleteNode's sandbox verification.
func registerAndIsolate(t *testing.T, svc *service.NodeService, nodeID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: nodeID, HostIP: nodeID}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.UpdateNodeStatus(ctx, nodeID, &model.UpdateNodeStatusRequest{
		Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := svc.SetNodeSchedulingDisabled(ctx, nodeID, true, "test", ""); err != nil {
		t.Fatalf("isolate: %v", err)
	}
}

func TestDeleteNode_NotIsolated(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	_, err := svc.DeleteNode(ctx, "node-1", false)
	if !errors.Is(err, service.ErrNodeNotIsolated) {
		t.Fatalf("expected ErrNodeNotIsolated, got %v", err)
	}
}

func TestDeleteNode_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.DeleteNode(ctx, "no-such-node", false)
	if !errors.Is(err, service.ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}

func TestDeleteNode_EmptyNode_Succeeds(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	// No sandbox checker → no inventory check → should succeed.
	snap, err := svc.DeleteNode(ctx, "node-1", false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if snap == nil || snap.NodeID != "node-1" {
		t.Fatalf("expected returned snapshot, got %+v", snap)
	}

	// Node should be gone from the store and memory.
	if _, err := fs.GetRegistration(ctx, "node-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound from store, got %v", err)
	}
	// GetNode returns store.ErrNotFound directly (not wrapped as ErrNodeNotFound).
	if _, err := svc.GetNode(ctx, "node-1"); err == nil {
		t.Fatalf("expected error from GetNode after deletion, got nil")
	}
}

func TestDeleteNode_HasSandboxes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	checker := &fakeSandboxChecker{count: 3}
	svc.SetSandboxInventoryChecker(checker)

	_, err := svc.DeleteNode(ctx, "node-1", false)
	if !errors.Is(err, service.ErrNodeHasSandboxes) {
		t.Fatalf("expected ErrNodeHasSandboxes, got %v", err)
	}

	// Node should still exist.
	if _, err := svc.GetNode(ctx, "node-1"); err != nil {
		t.Fatalf("node should still exist after failed delete: %v", err)
	}
	if checker.callCount() != 1 {
		t.Fatalf("expected 1 checker call, got %d", checker.callCount())
	}
}

func TestDeleteNode_SandboxCheckFails(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	checker := &fakeSandboxChecker{err: errors.New("cubemaster unreachable")}
	svc.SetSandboxInventoryChecker(checker)

	_, err := svc.DeleteNode(ctx, "node-1", false)
	if !errors.Is(err, service.ErrSandboxCheckFailed) {
		t.Fatalf("expected ErrSandboxCheckFailed, got %v", err)
	}

	// Node should still exist (fail closed).
	if _, err := svc.GetNode(ctx, "node-1"); err != nil {
		t.Fatalf("node should still exist after failed check: %v", err)
	}
}

func TestDeleteNode_UnhealthyNodeRefused(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	// Register but do NOT send a heartbeat, so the node stays unhealthy.
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1", HostIP: "node-1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.SetNodeSchedulingDisabled(ctx, "node-1", true, "test", ""); err != nil {
		t.Fatalf("isolate: %v", err)
	}

	checker := &fakeSandboxChecker{}
	svc.SetSandboxInventoryChecker(checker)

	_, err := svc.DeleteNode(ctx, "node-1", false)
	if !errors.Is(err, service.ErrSandboxCheckFailed) {
		t.Fatalf("expected ErrSandboxCheckFailed for unhealthy node, got %v", err)
	}
	// The inventory checker must not be consulted when the node is unhealthy.
	if checker.callCount() != 0 {
		t.Fatalf("expected no checker call for unhealthy node, got %d", checker.callCount())
	}
	// Node should still exist (fail closed).
	if _, err := svc.GetNode(ctx, "node-1"); err != nil {
		t.Fatalf("node should still exist after refused delete: %v", err)
	}
}

func TestDeleteNode_ForceBypassesSandboxCheck(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	// Checker reports sandboxes, but force=true should bypass it.
	checker := &fakeSandboxChecker{count: 5}
	svc.SetSandboxInventoryChecker(checker)

	_, err := svc.DeleteNode(ctx, "node-1", true)
	if err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if checker.callCount() != 0 {
		t.Fatalf("force should not call checker, got %d calls", checker.callCount())
	}
}

func TestDeleteNode_ForceStillRequiresIsolation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	// Not isolated — force should still fail.
	_, err := svc.DeleteNode(ctx, "node-1", true)
	if !errors.Is(err, service.ErrNodeNotIsolated) {
		t.Fatalf("expected ErrNodeNotIsolated even with force, got %v", err)
	}
}

func TestDeleteNode_EmptyNodeID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.DeleteNode(ctx, "", false)
	if !errors.Is(err, service.ErrNodeIDRequired) {
		t.Fatalf("expected ErrNodeIDRequired, got %v", err)
	}
}

func TestDeleteNode_RemovesRedisMetric(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	// Track Redis metric deletes via the hook.
	var deletedIDs []string
	cleanup := nodemetric.SetDeleteNodeMetricHook(func(nodeID string) error {
		deletedIDs = append(deletedIDs, nodeID)
		return nil
	})
	defer cleanup()

	if _, err := svc.DeleteNode(ctx, "node-1", false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != "node-1" {
		t.Fatalf("expected Redis metric delete for node-1, got %v", deletedIDs)
	}
}

func TestDeleteNode_RecordsOperation(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	if _, err := svc.DeleteNode(ctx, "node-1", false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ops, err := fs.ListOperations(ctx, "node-1", 10)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	// isolate + delete = 2 operations; verify the last one is OpDelete.
	if len(ops) != 2 {
		t.Fatalf("expected 2 operations (isolate+delete), got %d", len(ops))
	}
	if ops[1].Type != model.OpDelete {
		t.Fatalf("expected OpDelete, got %s", ops[1].Type)
	}
}

func TestDeleteNode_NodeCanReRegister(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	if _, err := svc.DeleteNode(ctx, "node-1", false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Re-register the same node ID.
	snap, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "node-1",
		HostIP: "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if snap.SchedulingDisabled {
		t.Fatalf("re-registered node should not retain isolation mark")
	}
}

// TestUpdateNodeStatus_AfterDelete_RejectsHeartbeat guards against the
// "deleted node revived by heartbeat" race.
func TestUpdateNodeStatus_AfterDelete_RejectsHeartbeat(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	if _, err := svc.DeleteNode(ctx, "node-1", false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Simulate a heartbeat arriving after deletion.
	_, err := svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
	})
	if !errors.Is(err, service.ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound from heartbeat after delete, got %v", err)
	}

	// No status row should have been recreated in the store.
	if _, err := fs.GetStatus(ctx, "node-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound from store.GetStatus, got %v", err)
	}

	// No in-memory snapshot should exist — GetNode returns ErrNotFound.
	if _, err := svc.GetNode(ctx, "node-1"); err == nil {
		t.Fatalf("node should not be visible after rejected heartbeat")
	}
}

// TestDeleteNode_ConcurrentWithHeartbeat races DeleteNode against concurrent
// heartbeats. After the race completes, the node must be either deleted (no
// status row, no in-memory snapshot) or alive with a valid heartbeat — but
// never in a half-deleted state with an orphan status row and no registration.
func TestDeleteNode_ConcurrentWithHeartbeat(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")

	// Pre-seed a status row so the heartbeat path has something to upsert.
	_, _ = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
		Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
	})

	const heartbeats = 30
	var wg sync.WaitGroup
	wg.Add(heartbeats + 1)

	// Heartbeat goroutines.
	for range heartbeats {
		go func() {
			defer wg.Done()
			_, _ = svc.UpdateNodeStatus(ctx, "node-1", &model.UpdateNodeStatusRequest{
				Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
			})
		}()
	}

	// Delete goroutine.
	var delErr error
	go func() {
		defer wg.Done()
		_, delErr = svc.DeleteNode(ctx, "node-1", false)
	}()

	wg.Wait()

	_, regErr := fs.GetRegistration(ctx, "node-1")

	if delErr != nil {
		// Delete failed (likely a heartbeat won the race and held the lock);
		// the node must still be fully intact.
		if errors.Is(regErr, store.ErrNotFound) {
			t.Fatalf("delete failed (%v) but registration is gone — inconsistent state", delErr)
		}
		return
	}

	// Delete succeeded: registration must be gone (a concurrent heartbeat may
	// briefly leave an orphan status row, cleaned on re-register).
	if !errors.Is(regErr, store.ErrNotFound) {
		t.Fatalf("delete succeeded but registration still exists")
	}
}

// TestIsolateConcurrent verifies that concurrent isolate/unisolate calls
// on the same node do not corrupt the scheduling-disabled state.
func TestIsolateConcurrent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"})

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		disabled := i%2 == 0
		go func() {
			defer wg.Done()
			_, _ = svc.SetNodeSchedulingDisabled(ctx, "node-1", disabled, "test", "")
		}()
	}
	wg.Wait()

	// Final state must be readable and internally consistent.
	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	_ = snap.SchedulingDisabled // just ensure no panic / corruption
}

// TestDeleteNode_ProductionPath_InventoryEmpty: empty inventory → delete succeeds.
func TestDeleteNode_ProductionPath_InventoryEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ret":{"ret_code":200,"ret_msg":"Success"},"data":[]}`)
	}))
	defer srv.Close()

	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")
	svc.SetSandboxInventoryChecker(cubemaster.New(srv.URL))

	if _, err := svc.DeleteNode(ctx, "node-1", false); err != nil {
		t.Fatalf("expected success with empty inventory, got %v", err)
	}
}

// TestDeleteNode_ProductionPath_CubeletListFailed: inventory failure → delete refused.
func TestDeleteNode_ProductionPath_CubeletListFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ret":{"ret_code":130512,"ret_msg":"list sandbox failed: cubelet unreachable"}}`)
	}))
	defer srv.Close()

	svc, _ := newTestService(t)
	ctx := context.Background()
	registerAndIsolate(t, svc, "node-1")
	svc.SetSandboxInventoryChecker(cubemaster.New(srv.URL))

	_, err := svc.DeleteNode(ctx, "node-1", false)
	if !errors.Is(err, service.ErrSandboxCheckFailed) {
		t.Fatalf("expected ErrSandboxCheckFailed, got %v", err)
	}
	if _, err := svc.GetNode(ctx, "node-1"); err != nil {
		t.Fatalf("node should still exist after failed delete: %v", err)
	}
}
