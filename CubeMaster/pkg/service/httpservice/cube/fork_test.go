// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const testForkSandboxID = "sb-fork-src"
const testForkSnapshotID = "snap-fork-1"

// forkReclaimRecord captures the reclaim's snapshot-delete call
// (synchronized: the reclaim runs in a goroutine).
type forkReclaimRecord struct {
	mu        sync.Mutex
	called    bool
	requestID string
}

func (r *forkReclaimRecord) record(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	r.requestID = requestID
}

func (r *forkReclaimRecord) get() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called, r.requestID
}

func waitReclaimed(t *testing.T, rec *forkReclaimRecord) {
	t.Helper()
	require.Eventually(t, func() bool {
		called, _ := rec.get()
		return called
	}, 2*time.Second, 10*time.Millisecond, "the reclaim DELETE must run")
}

// stubForkCreateSeams stubs the external seams of forkSandboxFromSource.
// `reclaim` records the temporary-snapshot reclaim call, if any.
func stubForkCreateSeams(t *testing.T, reclaim *forkReclaimRecord) {
	t.Helper()
	origResolve := resolveSnapshotHostFn
	origCreate := createSnapshotFn
	origDelete := deleteSnapshotFn
	origDeal := forkDealCubeboxCreateReqWithTemplateFn
	origCreateRun := forkCreateSandboxRunFn
	t.Cleanup(func() {
		resolveSnapshotHostFn = origResolve
		createSnapshotFn = origCreate
		deleteSnapshotFn = origDelete
		forkDealCubeboxCreateReqWithTemplateFn = origDeal
		forkCreateSandboxRunFn = origCreateRun
	})

	resolveSnapshotHostFn = func(_ context.Context, _, _ string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(_ context.Context, _, sandboxID, _, _, _, _ string) (*types.TemplateImageJobInfo, error) {
		return &types.TemplateImageJobInfo{
			TemplateID: testForkSnapshotID,
			SandboxID:  sandboxID,
			Status:     "READY",
		}, nil
	}
	deleteSnapshotFn = func(ctx context.Context, requestID, _, _ string) (*types.TemplateImageJobInfo, error) {
		// The reclaim must use a fresh ctx: the overall fork deadline may be
		// exactly exhausted when an all-failure fan-out unwinds.
		if err := ctx.Err(); err != nil {
			t.Errorf("snapshot reclaim ctx already done: %v", err)
		}
		reclaim.record(requestID)
		return &types.TemplateImageJobInfo{TemplateID: testForkSnapshotID}, nil
	}
	forkDealCubeboxCreateReqWithTemplateFn = func(_ context.Context, _ *types.CreateCubeSandboxReq) error {
		return nil
	}

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(templatecenter.GetTemplateRequest, func(_ context.Context, _ string) (*types.CreateCubeSandboxReq, error) {
		return nil, nil
	})
	patches.ApplyFunc(templatecenter.RegisterSnapshotRuntimeRefForCreatedSandbox,
		func(_ context.Context, _, _, _, _ string) error { return nil })
}

// TestForkFromSourceReclaimsSnapshotOnTimeout verifies a timed-out fan-out
// reclaims the temp snapshot; the stalled slot surfaces as a per-fork error.
func TestForkFromSourceReclaimsSnapshotOnTimeout(t *testing.T) {
	var reclaim forkReclaimRecord
	stubForkCreateSeams(t, &reclaim)
	forkCreateSandboxRunFn = func(ctx context.Context, _ *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		<-ctx.Done() // stall until the fork deadline expires
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterInternalError),
			RetMsg:  ctx.Err().Error(),
		}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp := &forkSandboxResponse{Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}}
	err := forkSandboxFromSource(ctx, "req-fork", testForkSandboxID, 1, nil, resp)

	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Nil(t, resp.Results[0].Sandbox)
	if assert.NotNil(t, resp.Results[0].Ret) {
		assert.NotEqual(t, int(errorcode.ErrorCode_Success), resp.Results[0].Ret.RetCode)
	}
	waitReclaimed(t, &reclaim)
}

// TestForkFromSourceReclaimUsesDistinctRequestID verifies the reclaim uses a
// fresh requestID, since DeleteSnapshot rejects one bound to the create job.
func TestForkFromSourceReclaimUsesDistinctRequestID(t *testing.T) {
	var reclaim forkReclaimRecord
	stubForkCreateSeams(t, &reclaim)
	forkCreateSandboxRunFn = func(_ context.Context, _ *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "derivation rejected",
		}}
	}

	ctx := context.Background()
	resp := &forkSandboxResponse{Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}}
	err := forkSandboxFromSource(ctx, "req-fork", testForkSandboxID, 1, nil, resp)

	require.NoError(t, err)
	waitReclaimed(t, &reclaim)
	_, id := reclaim.get()
	assert.Equal(t, "req-fork-reclaim", id)
}

// TestForkDeriveSeedsNodeAffinity verifies each derivation ctx carries the
// node-affinity selector, as in the standalone create path.
func TestForkDeriveSeedsNodeAffinity(t *testing.T) {
	withNodeAffinitySelectorAllowedKeys(t, "gpu")
	var reclaim forkReclaimRecord
	stubForkCreateSeams(t, &reclaim)
	forkDealCubeboxCreateReqWithTemplateFn = func(_ context.Context, req *types.CreateCubeSandboxReq) error {
		// The real template merge populates affinity annotations; emulate it.
		if req.Annotations == nil {
			req.Annotations = map[string]string{}
		}
		req.Annotations[constants.AnnotationsNodeAffinitySelector] = `[{"key":"gpu","operator":"Exists"}]`
		return nil
	}
	selectorCh := make(chan bool, 1)
	forkCreateSandboxRunFn = func(ctx context.Context, _ *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		selectorCh <- constants.GetNodeSelector(ctx) != nil
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}

	ctx := context.Background()
	resp := &forkSandboxResponse{Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}}
	err := forkSandboxFromSource(ctx, "req-fork", testForkSandboxID, 1, nil, resp)

	require.NoError(t, err)
	require.NotNil(t, resp.Results[0].Sandbox)
	assert.True(t, <-selectorCh, "derivation ctx must carry the node selector")
	waitReclaimed(t, &reclaim)
}

// TestForkFromSourceReclaimsSnapshotOnPartialSuccess verifies a partial
// success still issues the reclaim DELETE.
func TestForkFromSourceReclaimsSnapshotOnPartialSuccess(t *testing.T) {
	var reclaim forkReclaimRecord
	stubForkCreateSeams(t, &reclaim)
	var call atomic.Int32
	forkCreateSandboxRunFn = func(_ context.Context, _ *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		if call.Add(1) == 1 {
			return &types.CreateCubeSandboxRes{
				Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
				SandboxID: "sb-fork-ok",
			}
		}
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "derivation rejected",
		}}
	}

	ctx := context.Background()
	resp := &forkSandboxResponse{Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}}
	err := forkSandboxFromSource(ctx, "req-fork", testForkSandboxID, 2, nil, resp)

	require.NoError(t, err)
	require.Len(t, resp.Results, 2)
	waitReclaimed(t, &reclaim)
	successes := 0
	failures := 0
	for _, r := range resp.Results {
		if r != nil && r.Sandbox != nil {
			successes++
		} else if r != nil && r.Ret != nil {
			failures++
		}
	}
	assert.Equal(t, 1, successes, "the successful fork must be kept")
	assert.Equal(t, 1, failures, "the failed fork must surface as an error")
}

// TestHandleForkSandboxAppliesOverallTimeout verifies the handler bounds the
// whole fork with forkOverallTimeout.
func TestHandleForkSandboxAppliesOverallTimeout(t *testing.T) {
	var reclaim forkReclaimRecord
	stubForkCreateSeams(t, &reclaim)
	forkCreateSandboxRunFn = func(ctx context.Context, _ *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		<-ctx.Done()
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterInternalError),
			RetMsg:  ctx.Err().Error(),
		}}
	}

	origTimeout := forkOverallTimeout
	forkOverallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { forkOverallTimeout = origTimeout })

	rt := &CubeLog.RequestTrace{}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+testForkSandboxID+"/fork",
		strings.NewReader(`{"request_id":"req-fork","count":1}`))
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = req.WithContext(ctx)
	gc.Params = gin.Params{{Key: "sandbox_id", Value: testForkSandboxID}}
	handleForkSandboxFromSandboxAction(gc)

	var got forkSandboxResponse
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, int(errorcode.ErrorCode_Success), got.Res.Ret.RetCode)
	require.Len(t, got.Results, 1)
	assert.Nil(t, got.Results[0].Sandbox)
	if assert.NotNil(t, got.Results[0].Ret) {
		assert.NotEqual(t, int(errorcode.ErrorCode_Success), got.Results[0].Ret.RetCode)
	}
	waitReclaimed(t, &reclaim)
}

// TestHandleForkSandboxRejectsCountOutOfRange verifies out-of-range count is
// rejected instead of silently clamped to a default.
func TestHandleForkSandboxRejectsCountOutOfRange(t *testing.T) {
	for _, count := range []string{"0", "-3", "101", "500"} {
		rt := &CubeLog.RequestTrace{}
		ctx := CubeLog.WithRequestTrace(context.Background(), rt)
		req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/"+testForkSandboxID+"/fork",
			strings.NewReader(`{"request_id":"req-fork","count":`+count+`}`))
		w := httptest.NewRecorder()
		gc, _ := gin.CreateTestContext(w)
		gc.Request = req.WithContext(ctx)
		gc.Params = gin.Params{{Key: "sandbox_id", Value: testForkSandboxID}}
		handleForkSandboxFromSandboxAction(gc)

		var got forkSandboxResponse
		require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Res.Ret.RetCode)
		assert.Contains(t, got.Res.Ret.RetMsg, "count must be between 1 and 100")
	}
}

// TestForkSandboxNotFoundMapsTo404 verifies a missing source sandbox surfaces
// as NotFound (not a generic params error) at the master endpoint.
func TestForkSandboxNotFoundMapsTo404(t *testing.T) {
	origResolve := resolveSnapshotHostFn
	t.Cleanup(func() { resolveSnapshotHostFn = origResolve })
	resolveSnapshotHostFn = func(_ context.Context, _, sandboxID string) (string, string, error) {
		return "", "", fmt.Errorf("%w: %s", errSandboxNotFound, sandboxID)
	}

	ctx := context.Background()
	resp := &forkSandboxResponse{Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}}
	err := forkSandboxFromSource(ctx, "req-fork", testForkSandboxID, 1, nil, resp)

	require.Error(t, err)
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), snapshotErrorCode(err))
}

// TestSnapshotErrorCodeMapsSandboxNotFound verifies errSandboxNotFound routes to
// NotFound, shared by create-snapshot and fork via resolveSandboxHost.
func TestSnapshotErrorCodeMapsSandboxNotFound(t *testing.T) {
	assert.Equal(t, int(errorcode.ErrorCode_NotFound),
		snapshotErrorCode(fmt.Errorf("%w: sb-missing", errSandboxNotFound)))
}

// TestSnapshotErrorCodeMapsContextDeadlineToInternal verifies a server-side
// deadline maps to an internal error, not a client params error.
func TestSnapshotErrorCodeMapsContextDeadlineToInternal(t *testing.T) {
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError),
		snapshotErrorCode(context.DeadlineExceeded))
}
