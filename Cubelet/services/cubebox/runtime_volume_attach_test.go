// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestRuntimeVolumeAttachUpdate(t *testing.T) {
	ctx := t.Context()
	sb := newCubeboxWithStatusForTest("running-attach", cubeboxstore.Status{StartedAt: 1})
	manager := &fakeCubeboxAPI{cb: sb}
	s := &service{
		engine:                &workflow.Engine{},
		cubeboxMgr:            &local{cubeboxManger: manager},
		sandboxLifecycleLocks: utils.NewResourceLocks(),
	}
	req := &cubebox.UpdateCubeSandboxRequest{
		RequestID: "attach-request",
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.MasterAnnotationsUpdateAction: constants.UpdateActionAttachRuntimeVolume,
		},
	}

	rsp, err := s.Update(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Conflict, rsp.GetRet().GetRetCode())
	require.Equal(t, "runtime volume publication is unavailable", rsp.GetRet().GetRetMsg())
	require.Zero(t, manager.getCalls)

	resolved := 0
	s.resolveRuntimeVolumeAttach = func(ctx context.Context, gotReq *cubebox.UpdateCubeSandboxRequest, gotSandbox *cubeboxstore.CubeBox) (volume.AttachRequest, runtimeVolumePublisher, error) {
		resolved++
		require.Same(t, req, gotReq)
		require.Same(t, sb, gotSandbox)
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		unlock, err := s.sandboxLifecycleLocks.LockContext(waitCtx, sb.ID)
		if unlock != nil {
			unlock()
		}
		require.ErrorIs(t, err, context.DeadlineExceeded)
		return volume.AttachRequest{}, nil, nil
	}
	rsp, err = s.Update(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Conflict, rsp.GetRet().GetRetCode())
	require.Equal(t, "runtime volume attach binding or publication is unavailable", rsp.GetRet().GetRetMsg())
	require.Empty(t, rsp.GetExtInfo())
	require.Equal(t, 1, resolved)
	require.Zero(t, s.sandboxLifecycleLocks.Len())

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	rsp, err = s.Update(canceled, req)
	require.NoError(t, err)
	require.Equal(t, canceled.Err().Error(), rsp.GetRet().GetRetMsg())
	require.Equal(t, 1, resolved)

	now := time.Now()
	sb.UserMarkDeletedTime = &now
	rsp, err = s.Update(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Conflict, rsp.GetRet().GetRetCode())
	require.Equal(t, "runtime volume attach requires a running sandbox outside a lifecycle transition", rsp.GetRet().GetRetMsg())
	require.Equal(t, 1, resolved)
}
