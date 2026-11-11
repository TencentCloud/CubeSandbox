// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"google.golang.org/protobuf/proto"
)

func TestSandboxInfoErrorReason(t *testing.T) {
	items := []*cubebox.CubeSandbox{
		{Id: "11111111111111111111111111111111", ErrorReason: "OOMKilled"},
		{Id: "22222222222222222222222222222222", ErrorReason: "PauseFailed"},
		{Id: "33333333333333333333333333333333"},
	}
	for _, item := range items {
		localcache.SetSandboxCache(item.Id, &localcache.SandboxCache{SandboxID: item.Id, HostIP: "127.0.0.1"})
		t.Cleanup(func() { localcache.DeleteSandboxCache(item.Id) })
	}
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return &node.Node{IP: "127.0.0.1", Healthy: true}, true
	})
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return nil, false
	})
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return nil, pausesnap.ErrNotReady
	})
	var requestedID string
	var requestedStorage bool
	patches.ApplyFunc(cubelet.List, func(_ context.Context, _ string, req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		requestedID = req.GetId()
		requestedStorage = req.GetOption().GetPrivateWithCubeboxStore()
		rsp := &cubebox.ListCubeSandboxResponse{}
		for _, item := range items {
			if requestedID == "" || requestedID == item.Id {
				rsp.Items = append(rsp.Items, item)
			}
		}
		encoded, err := proto.Marshal(rsp)
		require.NoError(t, err)
		wire := &cubebox.ListCubeSandboxResponse{}
		require.NoError(t, proto.Unmarshal(encoded, wire))
		return wire, nil
	})
	for _, tc := range []struct {
		name string
		id   string
		want []*cubebox.CubeSandbox
	}{
		{"host list", "", items},
		{"terminated", items[0].Id, items[:1]},
		{"pause failed", items[1].Id, items[1:2]},
		{"no reason or older cubelet", items[2].Id, items[2:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestedID = "not called"
			requestedStorage = true
			ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-info"})
			rsp := SandboxInfo(ctx, &types.GetCubeSandboxReq{RequestID: "req-info", HostID: "node-1", SandboxID: tc.id})
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
			require.Equal(t, tc.id, requestedID)
			require.False(t, requestedStorage, "Info must not request private store JSON")
			encoded, err := json.Marshal(rsp)
			require.NoError(t, err)
			var wire struct {
				Data []map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(encoded, &wire))
			require.Len(t, wire.Data, len(tc.want))
			for i, item := range tc.want {
				require.Equal(t, item.Id, wire.Data[i]["sandbox_id"])
				if item.ErrorReason == "" {
					require.NotContains(t, wire.Data[i], "error_reason")
				} else {
					require.Equal(t, item.ErrorReason, wire.Data[i]["error_reason"])
				}
			}
		})
	}
}
