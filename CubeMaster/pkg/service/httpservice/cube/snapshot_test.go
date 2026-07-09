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

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestCreateSnapshotSuccessResponse(t *testing.T) {
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
			OriginSandboxID: "sb-1",
			StorageBackend:  "cubecow",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"requestID":"req-1",
		"sandbox_id":"sb-1",
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
		assert.Equal(t, "sb-1", got.Snapshot.OriginSandboxID)
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
		"sandbox_id":"sb-1"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := createSnapshot(req, rt)

	got := resp.(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-snake", got.RequestID)
	assert.Equal(t, int64(errorcode.ErrorCode_Success), rt.RetCode)
}

func TestCreateSnapshotDetachesExecutionFromCanceledRequest(t *testing.T) {
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
		"sandbox_id":"sb-1"
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

	req := httptest.NewRequest(http.MethodGet, "/cube/operation/op-missing", nil)
	rt := &CubeLog.RequestTrace{}
	resp := handleSnapshotOperationAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*operationResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
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
	resp := getSnapshot(req, rt)

	got := resp.(*snapshotListResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Ret.RetCode)
	assert.Equal(t, "req-list", got.RequestID)
	assert.Equal(t, "2", got.NextToken)
	if assert.Len(t, got.Data, 1) {
		assert.Equal(t, "snap-1", got.Data[0].SnapshotID)
	}
}

func TestHandleSandboxRollbackActionUsesPathSandboxID(t *testing.T) {
	origRollbackSnapshotFn := rollbackSnapshotFn
	t.Cleanup(func() {
		rollbackSnapshotFn = origRollbackSnapshotFn
	})
	rollbackSnapshotFn = func(ctx context.Context, requestID, sandboxID, snapshotID, instanceType string) (*types.TemplateImageJobInfo, error) {
		assert.Equal(t, "req-rb", requestID)
		assert.Equal(t, "sb-path", sandboxID)
		assert.Equal(t, "snap-1", snapshotID)
		return &types.TemplateImageJobInfo{
			JobID:      "op-rb",
			RequestID:  requestID,
			SandboxID:  sandboxID,
			ResourceID: snapshotID,
			Status:     "READY",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/sb-path/rollback", strings.NewReader(`{
		"request_id":"req-rb",
		"snapshot_id":"snap-1"
	}`))
	rt := &CubeLog.RequestTrace{}
	resp := handleSandboxRollbackAction(httptest.NewRecorder(), req, rt)

	got := resp.(*operationResponse)
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
