// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapconcurrent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestNodeQuotaRejectionUsesCapacityError(t *testing.T) {
	before := l
	l = &local{CreateRetryConf: &wrapconcurrent.ConcurrentHandle{}}
	t.Cleanup(func() { l = before })
	for _, pinned := range []bool{true, false} {
		c := &createSandboxContext{
			ctx: context.Background(), directHost: pinned,
			retryTimes: l.CreateRetryConf.MaxRetry() + 1,
			masterRsp:  &types.CreateCubeSandboxRes{Ret: &types.Ret{}},
			cubeletRsp: &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_HostResourcesNotEnough, RetMsg: "host quota exhausted",
			}},
		}
		require.False(t, c.errorCodeRetry())
		require.Equal(t, int(errorcode.ErrorCode_SelectNodesNoRes), c.masterRsp.Ret.RetCode)
		require.Equal(t, "host quota exhausted", c.masterRsp.Ret.RetMsg)
		require.False(t, c.loopRetry)
	}
}

func TestNodeQuotaRejectionCanRescheduleUnpinnedCreate(t *testing.T) {
	before := l
	l = &local{CreateRetryConf: &wrapconcurrent.ConcurrentHandle{}}
	t.Cleanup(func() { l = before })
	c := &createSandboxContext{
		ctx: context.Background(), selctx: selctx.New("quota"), selectHost: &node.Node{InsID: "full"},
		masterRsp:  &types.CreateCubeSandboxRes{Ret: &types.Ret{}},
		cubeletRsp: &cubebox.RunCubeSandboxResponse{Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_HostResourcesNotEnough}},
	}
	require.True(t, c.errorCodeRetry())
	require.True(t, c.reschedule)
	require.False(t, c.loopRetry)
	require.False(t, c.hasSetRet)
}
