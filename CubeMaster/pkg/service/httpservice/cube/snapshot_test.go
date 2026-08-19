// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestCreateSnapshotSuccessResponse(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot host resolution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName string) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{
			JobID:        "op-1",
			TemplateID:   "snap-1",
			RequestID:    requestID,
			SandboxID:    sandboxID,
			ResourceType: "snapshot",
			ResourceID:   "snap-1",
			Operation:    "SNAPSHOT_CREATE",
			Status:       "READY",
			Phase:        "REGISTERING",
		}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{
			SnapshotID:      snapshotID,
			Status:          "READY",
			OriginSandboxID: knownSandboxTestID,
			StorageBackend:  "cubecow",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"requestID":"req-1",
		"sandbox_id":"`+knownSandboxTestID+`",
		"display_name":"snap-name"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)

	got, ok := resp.(*snapshotResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	if assert.NotNil(t, got.Snapshot) {
		assert.Equal(t, "snap-1", got.Snapshot.SnapshotID)
		assert.Equal(t, knownSandboxTestID, got.Snapshot.OriginSandboxID)
	}
	if assert.NotNil(t, got.Operation) {
		assert.Equal(t, "op-1", got.Operation.OperationID)
		assert.Equal(t, "snap-1", got.Operation.SnapshotID)
		assert.Equal(t, "READY", got.Operation.Status)
	}
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestSnapshotErrorCodeMapsMySQLLockErrorsToDBError(t *testing.T) {
	for _, err := range []error{
		&mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"},
		&mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"},
	} {
		assert.Equal(t, int(errorcode.ErrorCode_DBError), snapshotErrorCode(err))
	}
}

func TestCreateSnapshotAcceptsSnakeCaseRequestID(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName string) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{JobID: "op-2", TemplateID: "snap-2", RequestID: requestID}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{SnapshotID: snapshotID}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-snake",
		"sandbox_id":"`+knownSandboxTestID+`"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)

	got := resp.(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-snake", got.RequestID)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestCreateSnapshotDetachesExecutionFromCanceledRequest(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})

	resolveSnapshotHostFn = func(ctx context.Context, requestID, sandboxID string) (string, string, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot host resolution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(ctx context.Context, requestID, sandboxID, nodeID, nodeIP, displayName string) (*types.TemplateImageJobInfo, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot execution context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return &types.TemplateImageJobInfo{
			JobID:      "op-detached",
			TemplateID: "snap-detached",
			RequestID:  requestID,
			Status:     "READY",
		}, nil
	}
	getSnapshotInfoFn = func(ctx context.Context, snapshotID string, includeRequest bool) (*templatecenter.SnapshotInfo, error) {
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot info lookup context should not be canceled with the HTTP request: %v", ctx.Err())
		default:
		}
		return &templatecenter.SnapshotInfo{SnapshotID: snapshotID, Status: "READY"}, nil
	}

	baseReq := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-detached",
		"sandbox_id":"`+knownSandboxTestID+`"
	}`))
	canceledCtx, cancel := context.WithCancel(baseReq.Context())
	cancel()
	req := baseReq.WithContext(canceledCtx)

	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)
	got := resp.(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
}

func TestSnapshotExecutionContextDetachesFromParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := snapshotExecutionContext(parent, map[string]any{"RequestId": "req-ctx"})
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("snapshot HTTP execution context should not inherit parent cancellation: %v", ctx.Err())
	default:
	}
}

func TestHandleSnapshotOperationMapsNotFound(t *testing.T) {
	origGetSnapshotOperationFn := getSnapshotOperationFn
	t.Cleanup(func() {
		getSnapshotOperationFn = origGetSnapshotOperationFn
	})
	getSnapshotOperationFn = func(ctx context.Context, operationID string) (*templatecenter.SnapshotOperationInfo, error) {
		return nil, templatecenter.ErrSnapshotOperationNotFound
	}

	rt := &CubeLog.RequestTrace{}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/cube/operation/op-missing", nil).WithContext(ctx)
	gc.Params = gin.Params{{Key: "operation_id", Value: "op-missing"}}
	handleSnapshotOperationAction(gc)

	var got operationResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_NotFound), rt.RetCode)
}

func TestGetSnapshotListSupportsFiltersAndPagination(t *testing.T) {
	origListSnapshotsFn := listSnapshotsFn
	t.Cleanup(func() {
		listSnapshotsFn = origListSnapshotsFn
	})
	listSnapshotsFn = func(ctx context.Context, opts *templatecenter.ListSnapshotsOptions) ([]templatecenter.SnapshotInfo, string, error) {
		assert.Equal(t, "snap-1", opts.SnapshotID)
		assert.Equal(t, "sb-1", opts.SandboxID)
		assert.Equal(t, "READY", opts.Status)
		assert.Equal(t, 1, opts.Limit)
		assert.Equal(t, "1", opts.NextToken)
		return []templatecenter.SnapshotInfo{{SnapshotID: "snap-1", OriginSandboxID: "sb-1", Status: "READY"}}, "2", nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot?snapshot_id=snap-1&sandbox_id=sb-1&status=READY&limit=1&next_token=1&request_id=req-list", nil)
	rt := &CubeLog.RequestTrace{}
	resp := getSnapshot(req, rt, "")

	got := resp.(*snapshotListResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-list", got.RequestID)
	assert.Equal(t, "2", got.NextToken)
	if assert.Len(t, got.Data, 1) {
		assert.Equal(t, "snap-1", got.Data[0].SnapshotID)
	}
}

func TestHandleSandboxRollbackActionUsesPathSandboxID(t *testing.T) {
	registerKnownSandboxTestID(t)

	origRollbackSnapshotFn := rollbackSnapshotFn
	t.Cleanup(func() {
		rollbackSnapshotFn = origRollbackSnapshotFn
	})
	rollbackSnapshotFn = func(ctx context.Context, requestID, sandboxID, snapshotID, instanceType string) (*types.TemplateImageJobInfo, error) {
		assert.Equal(t, "req-rb", requestID)
		assert.Equal(t, knownSandboxTestID, sandboxID)
		assert.Equal(t, "snap-1", snapshotID)
		return &types.TemplateImageJobInfo{
			JobID:      "op-rb",
			RequestID:  requestID,
			SandboxID:  sandboxID,
			ResourceID: snapshotID,
			Status:     "READY",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+knownSandboxTestID+"/rollback", strings.NewReader(`{
		"request_id":"req-rb",
		"snapshot_id":"snap-1"
	}`))
	rt := &CubeLog.RequestTrace{}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = req.WithContext(ctx)
	gc.Params = gin.Params{{Key: "sandbox_id", Value: knownSandboxTestID}}
	handleSandboxRollbackAction(gc)

	var got operationResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-rb", got.RequestID)
	assert.Equal(t, "op-rb", got.Operation.OperationID)
	assert.Equal(t, "READY", got.Operation.Status)
}

func TestConstrainSnapshotCreateScopeIntersectsRequestedScope(t *testing.T) {
	origResolveSnapshotReadyNodeScopeFn := resolveSnapshotReadyNodeScopeFn
	t.Cleanup(func() {
		resolveSnapshotReadyNodeScopeFn = origResolveSnapshotReadyNodeScopeFn
	})
	resolveSnapshotReadyNodeScopeFn = func(ctx context.Context, snapshotID string) ([]string, error) {
		return []string{"node-a", "node-b"}, nil
	}

	req := &types.CreateCubeSandboxReq{
		DistributionScope: []string{"node-b", "node-c"},
	}
	err := constrainSnapshotCreateScope(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("constrainSnapshotCreateScope failed: %v", err)
	}
	assert.Equal(t, []string{"node-b"}, req.DistributionScope)
}

// TestBindSnapshotCreateReplicaInjectsRuntimeAnnotations verifies the v4
// contract: master sets only the logical snapshot id + attached_at
// annotations, never the physical memory_vol/memory_dev. Any stale physical
// annotation present in the caller-supplied request must be stripped so it
// cannot reach the cubelet.
func TestBindSnapshotCreateReplicaInjectsRuntimeAnnotations(t *testing.T) {
	origResolveSnapshotReadyNodeScopeFn := resolveSnapshotReadyNodeScopeFn
	origResolveSnapshotReadyReplicaFn := resolveSnapshotReadyReplicaFn
	t.Cleanup(func() {
		resolveSnapshotReadyNodeScopeFn = origResolveSnapshotReadyNodeScopeFn
		resolveSnapshotReadyReplicaFn = origResolveSnapshotReadyReplicaFn
	})
	resolveSnapshotReadyNodeScopeFn = func(ctx context.Context, snapshotID string) ([]string, error) {
		return []string{"node-a", "node-b"}, nil
	}
	resolveSnapshotReadyReplicaFn = func(ctx context.Context, snapshotID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		assert.Equal(t, "snap-1", snapshotID)
		assert.Equal(t, "node-a", preferredNodeID)
		// v5: ReplicaStatus is now thin — no physical fields available to
		// accidentally propagate.
		return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
	}

	req := &types.CreateCubeSandboxReq{
		Annotations:       map[string]string{},
		DistributionScope: []string{"node-a", "node-b"},
	}
	err := bindSnapshotCreateReplica(context.Background(), "snap-1", req)
	if err != nil {
		t.Fatalf("bindSnapshotCreateReplica failed: %v", err)
	}
	assert.Equal(t, []string{"node-a"}, req.DistributionScope)
	assert.Equal(t, "snap-1", req.Annotations[constants.CubeAnnotationRuntimeSnapshotID])
	assert.NotEmpty(t, req.Annotations[constants.CubeAnnotationRuntimeSnapshotAttachedAt])
}

// TestBindAppSnapshotTemplateReplicaRequiresReadyReplica verifies that even
// though no physical refs are written, the replica resolution still runs as
// a fail-fast gate (no ready replica -> error).
func TestBindAppSnapshotTemplateReplicaRequiresReadyReplica(t *testing.T) {
	origResolveTemplateReadyReplicaFn := resolveTemplateReadyReplicaFn
	t.Cleanup(func() {
		resolveTemplateReadyReplicaFn = origResolveTemplateReadyReplicaFn
	})
	resolveTemplateReadyReplicaFn = func(ctx context.Context, templateID, preferredNodeID string) (templatecenter.ReplicaStatus, error) {
		return templatecenter.ReplicaStatus{}, assert.AnError
	}

	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}}
	err := bindAppSnapshotTemplateReplica(context.Background(), "tpl-missing", req)
	assert.Error(t, err)
}

func TestCompatibleNodesBySnapshot(t *testing.T) {
	origFn := listCompatibleNodesForSnapshotFn
	t.Cleanup(func() { listCompatibleNodesForSnapshotFn = origFn })
	listCompatibleNodesForSnapshotFn = func(ctx context.Context, snapshotID string, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "snap-1", snapshotID)
		assert.False(t, includeAll)
		return &templatecenter.CompatibleNodesResult{
			SnapshotID: snapshotID,
			OriginNode: "node-a",
			Nodes:      []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/snap-1/compatible-nodes?request_id=req-1", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "snap-1").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "snap-1", got.SnapshotID)
	assert.Equal(t, "node-a", got.OriginNode)
	if assert.Len(t, got.Nodes, 1) {
		assert.Equal(t, "node-b", got.Nodes[0].NodeID)
	}
}

func TestCompatibleNodesByFactors(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "sha256:cpu", factors.CPUIDHash)
		assert.Equal(t, "5.15.0", factors.HostKernelRelease)
		assert.True(t, includeAll)
		return &templatecenter.CompatibleNodesResult{
			Nodes: []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?cpuid_hash=sha256:cpu&host_kernel_release=5.15.0&include_all=true", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Len(t, got.Nodes, 1)
}

// The dedicated /compatible-nodes-by-factors route serves the bare-factor mode
// without a dummy snapshot_id segment.
func TestCompatibleNodesByFactorsDedicatedRoute(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		assert.Equal(t, "sha256:cpu", factors.CPUIDHash)
		assert.Equal(t, "5.15.0", factors.HostKernelRelease)
		return &templatecenter.CompatibleNodesResult{
			Nodes: []templatecenter.CompatibleNode{{NodeID: "node-b", Compatible: true}},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/compatible-nodes-by-factors?cpuid_hash=sha256:cpu&host_kernel_release=5.15.0", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodesByFactors(req, rt).(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Len(t, got.Nodes, 1)
}

// The dedicated route rejects an under-specified query the same way the shared
// mode does, without reaching the listing fn.
func TestCompatibleNodesByFactorsDedicatedRouteUnderspecified(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	called := false
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		called = true
		return &templatecenter.CompatibleNodesResult{}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/compatible-nodes-by-factors?cpuid_hash=sha256:cpu", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodesByFactors(req, rt).(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.False(t, called, "factors fn must not be called for under-specified query")
}

func TestCompatibleNodesMissingParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot//compatible-nodes", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
}

func TestCompatibleNodesFactorsErrorMapsToParamsError(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		return nil, templatecenter.ErrRestoreCompatNoFactors
	}

	req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?cpuid_hash=x&host_kernel_release=5.15.0", nil)
	rt := &CubeLog.RequestTrace{}
	got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
}

// A bare-factor query supplying only one of the two required factors must be
// rejected with a params error rather than silently returning an empty list
// (the missing required dimension would otherwise fail closed against every
// candidate). The factors fn must never be reached.
func TestCompatibleNodesBareFactorUnderspecified(t *testing.T) {
	origFn := listCompatibleNodesForFactorsFn
	t.Cleanup(func() { listCompatibleNodesForFactorsFn = origFn })
	called := false
	listCompatibleNodesForFactorsFn = func(ctx context.Context, factors *nodemeta.HostFacts, includeAll bool) (*templatecenter.CompatibleNodesResult, error) {
		called = true
		return &templatecenter.CompatibleNodesResult{}, nil
	}

	for _, q := range []string{"cpuid_hash=sha256:cpu", "host_kernel_release=5.15.0"} {
		req := httptest.NewRequest(http.MethodGet, "/cube/snapshot/ignored/compatible-nodes?"+q, nil)
		rt := &CubeLog.RequestTrace{}
		got := compatibleNodes(req, rt, "ignored").(*compatibleNodesResponse)

		assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
		assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
	}
	assert.False(t, called, "factors fn must not be called for under-specified query")
}
