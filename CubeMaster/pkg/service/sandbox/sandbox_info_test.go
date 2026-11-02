// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestSandboxInfoErrorReason(t *testing.T) {
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) {
		return &node.Node{IP: "127.0.0.1", Healthy: true}, true
	})
	var stored string
	var requestedStorage bool
	patches.ApplyFunc(cubelet.List, func(_ context.Context, _ string, req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
		requestedStorage = req.GetOption().GetPrivateWithCubeboxStore()
		item := &cubebox.CubeSandbox{Id: "sandbox-id"}
		if requestedStorage {
			item.PrivateCubeboxStorageData = []byte(stored)
		}
		return &cubebox.ListCubeSandboxResponse{Items: []*cubebox.CubeSandbox{item}}, nil
	})
	for _, tc := range []struct {
		name   string
		stored string
		want   string
	}{
		{"primary container", `{"first_container_name":"main","ContainersMap":{"ContainerMap":{"main":{"status":{"Status":{"Reason":"OOMKilled"}}},"sandbox-id":{"status":{"Status":{"Reason":"OtherContainer"}}}}},"private_token":"not-public"}`, "OOMKilled"},
		{"legacy sandbox ID", `{"ContainersMap":{"ContainerMap":{"sandbox-id":{"status":{"Status":{"Reason":"Error"}}}}}}`, "Error"},
		{"missing primary", `{"first_container_name":"missing","ContainersMap":{"ContainerMap":{"sandbox-id":{"status":{"Status":{"Reason":"OtherContainer"}}}}}}`, ""},
		{"empty reason", `{"ContainersMap":{"ContainerMap":{"sandbox-id":{"status":{"Status":{"Reason":""}}}}}}`, ""},
		{"missing status", `{"ContainersMap":{"ContainerMap":{"sandbox-id":{}}}}`, ""},
		{"null container", `{"ContainersMap":{"ContainerMap":{"sandbox-id":null}}}`, ""},
		{"missing storage", "", ""},
		{"null storage", "null", ""},
		{"invalid storage", "not json", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored = tc.stored
			requestedStorage = false
			ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{RequestID: "req-info"})
			rsp := SandboxInfo(ctx, &types.GetCubeSandboxReq{RequestID: "req-info", HostID: "node-1"})
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
			require.Len(t, rsp.Data, 1)
			encoded, err := json.Marshal(rsp)
			require.NoError(t, err)
			var wire struct {
				Data []map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(encoded, &wire))
			if tc.want == "" {
				require.NotContains(t, wire.Data[0], "error_reason")
			} else {
				require.Equal(t, tc.want, wire.Data[0]["error_reason"])
			}
			require.True(t, requestedStorage, "Info must request the stored container status")
			require.NotContains(t, string(encoded), "not-public")
			require.NotContains(t, string(encoded), "ContainersMap")
		})
	}
}
