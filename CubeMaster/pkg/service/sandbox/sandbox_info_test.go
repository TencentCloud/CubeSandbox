// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestReadyPauseBindingPreservesRunningNodeState(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return &basetypes.SandboxProxyMap{}, true
	})
	// A successful resume is allowed to leave this record when Delete fails.
	patches.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{SnapshotID: "snap", Status: "READY"}, nil
	})
	for _, tc := range []struct {
		name       string
		state      cubebox.ContainerState
		empty      bool
		synthesize bool
	}{
		{"resumed with leftover binding", cubebox.ContainerState_CONTAINER_RUNNING, false, false},
		{"paused", cubebox.ContainerState_CONTAINER_PAUSED, false, true},
		{"exited flicker", cubebox.ContainerState_CONTAINER_EXITED, false, true},
		{"empty node response", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rsp := &types.GetCubeSandboxRes{Ret: &types.Ret{}}
			if !tc.empty {
				rsp.Data = []*types.SandboxData{{SandboxID: "sbx", Status: int32(tc.state)}}
			}
			got := fillPauseBindingInfoFromMaster(context.Background(), &types.GetCubeSandboxReq{SandboxID: "sbx"}, rsp)
			if got != tc.synthesize {
				t.Fatalf("synthesized=%v want %v", got, tc.synthesize)
			}
			want := int32(cubebox.ContainerState_CONTAINER_PAUSED)
			if !tc.synthesize {
				want = int32(cubebox.ContainerState_CONTAINER_RUNNING)
			}
			if len(rsp.Data) != 1 || rsp.Data[0].Status != want {
				t.Fatalf("unexpected Info: %+v", rsp.Data)
			}
		})
	}
}
