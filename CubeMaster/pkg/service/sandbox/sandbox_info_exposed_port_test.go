// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// decorateSandboxInfo must resolve the host_port endpoint whenever the
// request carries no caller host IP (no X-Cube-Caller-Host-IP header and no
// remote address in ctx). CubeAPI relies on this: it forwards container_port
// but never the caller address, so same-node sandboxes also get the
// hostIP:hostPort form (reachable via CubeVS hairpin).
func TestDecorateSandboxInfoExposedPortWithoutCallerIP(t *testing.T) {
	const sandboxID = "sb-exposed-port-no-caller"
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(_ context.Context, got string) (*basetypes.SandboxProxyMap, bool) {
		require.Equal(t, sandboxID, got)
		return &basetypes.SandboxProxyMap{
			SandboxID: sandboxID,
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
			ContainerToHostPorts: map[string]string{
				"8080": "32000",
			},
		}, true
	})

	newInfo := func() (*types.GetCubeSandboxReq, *types.GetCubeSandboxRes) {
		req := &types.GetCubeSandboxReq{SandboxID: sandboxID, ContainerPort: 8080}
		rsp := &types.GetCubeSandboxRes{Data: []*types.SandboxData{{SandboxID: sandboxID}}}
		return req, rsp
	}

	t.Run("no caller host in ctx resolves host port", func(t *testing.T) {
		req, rsp := newInfo()
		require.NoError(t, decorateSandboxInfo(context.Background(), req, rsp))
		item := rsp.Data[0]
		assert.Equal(t, ExposedPortModeHostPort, item.ExposedPortMode)
		assert.Equal(t, "10.0.0.1:32000", item.ExposedPortEndpoint)
		assert.Equal(t, int32(32000), item.ExposedPortHostPort)
		assert.Equal(t, "10.0.0.1", item.HostIP)
		assert.Equal(t, "192.168.0.2", item.SandboxIP)
		assert.Equal(t, int32(8080), item.RequestedContainerPort)
	})

	t.Run("unspecified sentinel forces host port despite topology", func(t *testing.T) {
		req, rsp := newInfo()
		ctx := constants.WithHostIP(context.Background(), "0.0.0.0")
		require.NoError(t, decorateSandboxInfo(ctx, req, rsp))
		item := rsp.Data[0]
		assert.Equal(t, ExposedPortModeHostPort, item.ExposedPortMode)
		assert.Equal(t, "10.0.0.1:32000", item.ExposedPortEndpoint)
		assert.Equal(t, int32(32000), item.ExposedPortHostPort)
	})

	t.Run("same-node caller host still selects tap ip as control", func(t *testing.T) {
		req, rsp := newInfo()
		ctx := constants.WithHostIP(context.Background(), "10.0.0.1")
		require.NoError(t, decorateSandboxInfo(ctx, req, rsp))
		item := rsp.Data[0]
		assert.Equal(t, ExposedPortModeTapIP, item.ExposedPortMode)
		assert.Equal(t, "192.168.0.2:8080", item.ExposedPortEndpoint)
		assert.Equal(t, int32(0), item.ExposedPortHostPort)
	})
}
