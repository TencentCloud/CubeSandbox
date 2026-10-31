// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestResolveFullSandboxIDUsesDurableRouteWithColdCache(t *testing.T) {
	const sid = "abcdef0123456789abcdef0123456789"
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, id string) (*basetypes.SandboxProxyMap, bool) {
		require.Equal(t, sid, id)
		return &basetypes.SandboxProxyMap{SandboxID: sid, HostIP: "10.0.0.1"}, true
	})
	patches.ApplyFunc(localcache.ListKnownSandboxIDs, func() []string { t.Fatal("must not require a healthy-node scan for a known full ID"); return nil })
	got, err := ResolveSandboxID(context.Background(), strings.ToUpper(sid))
	require.NoError(t, err)
	require.Equal(t, sid, got)
}

func TestSandboxInfoMissingWorkerIsNotMissingSandbox(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache { return nil })
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, id string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{SandboxID: id, HostIP: "10.0.0.1"}, true
	})
	patches.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return nil, false })
	patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) { return nil, false })
	for _, host := range []string{"", "node-1"} {
		rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
		endpoint, _, _, ok := checkValidAndGetReq(context.Background(), &types.GetCubeSandboxReq{SandboxID: "known", HostID: host}, &cubebox.ListCubeSandboxRequest{}, rsp)
		require.Empty(t, endpoint)
		require.False(t, ok)
		require.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), rsp.Ret.RetCode)
	}
}
