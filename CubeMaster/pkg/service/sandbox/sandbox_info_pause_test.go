// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestFillPauseBindingInfoIncludesLifecycleEndAt(t *testing.T) {
	const sandboxID = "sb-paused-info"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, gotSandboxID string) (*basetypes.SandboxProxyMap, bool) {
		require.Equal(t, sandboxID, gotSandboxID)
		return &basetypes.SandboxProxyMap{
			SandboxID: sandboxID,
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
		}, true
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(_ context.Context, gotSandboxID string) (*pausesnap.Record, error) {
		require.Equal(t, sandboxID, gotSandboxID)
		return &pausesnap.Record{
			SandboxID:  sandboxID,
			SnapshotID: "snap-paused-info",
			Status:     pausesnap.StatusReady,
		}, nil
	})

	for _, tc := range []struct {
		name         string
		endAt        int64
		cubeletEndAt int64
		wantLookups  int
	}{
		{name: "finite timeout", endAt: 123456789, wantLookups: 1},
		{name: "never timeout", endAt: 0, wantLookups: 1},
		{name: "reuse cubelet deadline", endAt: 123456789, cubeletEndAt: 987654321},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &mockTimeoutProvider{returnEndAt: tc.endAt}
			previousProvider := getTimeoutProvider()
			SetTimeoutProvider(provider)
			t.Cleanup(func() { SetTimeoutProvider(previousProvider) })

			rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
			if tc.cubeletEndAt != 0 {
				rsp.Data = []*types.SandboxData{{SandboxID: sandboxID, EndAt: tc.cubeletEndAt}}
			}
			filled := fillPauseBindingInfoFromMaster(context.Background(), &types.GetCubeSandboxReq{
				SandboxID: sandboxID,
			}, rsp)

			require.True(t, filled)
			require.Len(t, rsp.Data, 1)
			wantEndAt := tc.endAt
			if tc.cubeletEndAt != 0 {
				wantEndAt = tc.cubeletEndAt
			}
			require.Equal(t, wantEndAt, rsp.Data[0].EndAt)
			require.Equal(t, tc.wantLookups, provider.lookupCalls)
		})
	}
}
