// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestFillPauseBindingInfoIncludesLifecycleTimeout(t *testing.T) {
	const (
		sandboxID = "sbx-paused-timeout"
		endAt     = int64(1_700_000_120_000)
	)
	timeout := 120
	provider := &recordingTimeoutProvider{
		lookupEndAt:   endAt,
		lookupTimeout: &timeout,
	}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{
			SandboxID: sandboxID,
			HostIP:    "127.0.0.1",
			SandboxIP: "10.0.0.2",
		}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{
			SandboxID:  sandboxID,
			SnapshotID: "snap-paused-timeout",
			Status:     "READY",
		}, nil
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) {
		return nil, false
	})

	rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
	filled := fillPauseBindingInfoFromMaster(context.Background(), &types.GetCubeSandboxReq{
		SandboxID: sandboxID,
	}, rsp)

	require.True(t, filled)
	require.Len(t, rsp.Data, 1)
	assert.Equal(t, endAt, rsp.Data[0].EndAt)
	require.NotNil(t, rsp.Data[0].TimeoutSeconds)
	assert.Equal(t, timeout, *rsp.Data[0].TimeoutSeconds)
	assert.Equal(t, 1, provider.lookupCalls)
	assert.Equal(t, sandboxID, provider.sandboxID)
}
