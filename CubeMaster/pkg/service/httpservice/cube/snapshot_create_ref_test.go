// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestSnapshotCreateReservationBeforeRPC(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			oldDeal, oldRun, oldReserve := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, reserveSnapshotCreateFn
			t.Cleanup(func() {
				createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, reserveSnapshotCreateFn = oldDeal, oldRun, oldReserve
			})
			p := gomonkey.NewPatches()
			t.Cleanup(p.Reset)
			p.ApplyFunc(runInsReq2Affinity, func(ctx context.Context, _ *types.CreateCubeSandboxReq) (context.Context, error) { return ctx, nil })
			createSandboxDealCubeboxCreateReqWithTemplateFn = func(_ context.Context, req *types.CreateCubeSandboxReq) error {
				req.Annotations[constants.CubeAnnotationRuntimeSnapshotID] = "snap-source"
				req.DistributionScope = []string{"node-target"}
				return nil
			}
			reservedID := ""
			reserveSnapshotCreateFn = func(_ context.Context, snapshot, sandbox, node, ip string) error {
				require.Equal(t, "snap-source", snapshot)
				require.Equal(t, "node-target", node)
				require.Len(t, sandbox, 32)
				if reject {
					return templatecenter.ErrSnapshotExpired
				}
				reservedID = sandbox
				return nil
			}
			called := false
			createSandboxRunFn = func(_ context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
				called = true
				require.NotEmpty(t, reservedID)
				require.Equal(t, reservedID, req.Annotations[constants.CubeAnnotationDesiredSandboxID])
				return &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterUnavailable)}}
			}
			trace := &CubeLog.RequestTrace{}
			createSandbox(httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"instanceType":"cubebox"}`)), trace)
			require.Equal(t, !reject, called)
			if reject {
				require.EqualValues(t, errorcode.ErrorCode_Conflict, trace.RetCode)
			}
		})
	}
}

func TestSnapshotExpiredCreateAndForkConflict(t *testing.T) {
	oldCreate, oldFork := createSandboxDealCubeboxCreateReqWithTemplateFn, forkDealCubeboxCreateReqWithTemplateFn
	t.Cleanup(func() {
		createSandboxDealCubeboxCreateReqWithTemplateFn, forkDealCubeboxCreateReqWithTemplateFn = oldCreate, oldFork
	})
	reject := func(context.Context, *types.CreateCubeSandboxReq) error {
		return fmt.Errorf("source: %w", templatecenter.ErrSnapshotExpired)
	}
	createSandboxDealCubeboxCreateReqWithTemplateFn, forkDealCubeboxCreateReqWithTemplateFn = reject, reject
	trace := &CubeLog.RequestTrace{}
	createSandbox(httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"instanceType":"cubebox"}`)), trace)
	require.EqualValues(t, errorcode.ErrorCode_Conflict, trace.RetCode)
	_, ret := singleForkDerive(context.Background(), "request", "snap-source", nil, nil, "", snapshotmode.Mode("full"))
	require.EqualValues(t, errorcode.ErrorCode_Conflict, ret.RetCode)
}

func TestReleaseUndispatchedSnapshotCreate(t *testing.T) {
	oldDetach := detachSnapshotCreateRefFn
	t.Cleanup(func() { detachSnapshotCreateRefFn = oldDetach })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		constants.CubeAnnotationRuntimeSnapshotID: "source",
		constants.CubeAnnotationDesiredSandboxID:  "reserved",
	}}
	for _, tc := range []struct {
		name       string
		notStarted bool
		code       int
		want       int
	}{
		{"undispatched", true, int(errorcode.ErrorCode_SelectNodesFailed), 1},
		{"unknown", false, int(errorcode.ErrorCode_ReqCubeAPIFailed), 0},
		{"success", true, int(errorcode.ErrorCode_Success), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			detachSnapshotCreateRefFn = func(ctx context.Context, sandbox, binding, reason string) error {
				calls++
				require.NoError(t, ctx.Err(), "request cancellation must not cancel cleanup")
				require.Equal(t, "reserved", sandbox)
				require.Equal(t, templatecenter.SnapshotRuntimeBindingCreatePending, binding)
				require.NotEmpty(t, reason)
				return nil
			}
			releaseUndispatchedSnapshotCreate(ctx, req, &types.CreateCubeSandboxRes{CreateNotStarted: tc.notStarted, Ret: &types.Ret{RetCode: tc.code}})
			require.Equal(t, tc.want, calls)
		})
	}
}

func TestSnapshotCreateReservationIPOnlyNode(t *testing.T) {
	old := reserveSnapshotCreateFn
	t.Cleanup(func() { reserveSnapshotCreateFn = old })
	for _, withReplica := range []bool{false, true} {
		ctx := context.Background()
		if withReplica {
			ctx = withTemplateResolveResult(ctx, &templateResolveResult{HasChosenReplica: true, ChosenReplica: templatecenter.ReplicaStatus{NodeIP: "192.0.2.10"}})
		}
		called := false
		reserveSnapshotCreateFn = func(_ context.Context, snapshot, sandbox, nodeID, nodeIP string) error {
			called = true
			require.Empty(t, nodeID)
			require.Equal(t, "192.0.2.10", nodeIP)
			return nil
		}
		req := &types.CreateCubeSandboxReq{DistributionScope: []string{"192.0.2.10"}, Annotations: map[string]string{constants.CubeAnnotationRuntimeSnapshotID: "source"}}
		require.NoError(t, reserveCreatedSandboxRuntimeRef(ctx, req))
		require.True(t, called)
		require.Len(t, req.Annotations[constants.CubeAnnotationDesiredSandboxID], 32)
	}
}
