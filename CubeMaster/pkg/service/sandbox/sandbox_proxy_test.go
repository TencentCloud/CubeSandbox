// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSetProxyToRedisPropagatesMaskRequestHost(t *testing.T) {
	originalSet := setSandboxProxyMapFn
	defer func() { setSandboxProxyMapFn = originalSet }()

	var stored *proxytypes.SandboxProxyMap
	setSandboxProxyMapFn = func(_ context.Context, proxy *proxytypes.SandboxProxyMap) error {
		stored = proxy
		return nil
	}

	mask := "localhost:${PORT}"
	ctx := withCreateOriginRequest(context.Background(), &sandboxtypes.CreateCubeSandboxReq{
		CubeNetworkConfig: &sandboxtypes.CubeNetworkConfig{
			MaskRequestHost: &mask,
		},
	})
	createCtx := &createSandboxContext{
		ctx:        ctx,
		selectHost: &node.Node{InsID: "node-1", IP: "10.0.0.1"},
		masterRsp: &sandboxtypes.CreateCubeSandboxRes{
			SandboxID: "sandbox-1",
			SandboxIP: "192.168.0.2",
		},
		cubeletReq: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
		},
	}

	if err := createCtx.setProxyToRedis(); err != nil {
		t.Fatalf("setProxyToRedis failed: %v", err)
	}
	if stored == nil {
		t.Fatal("expected proxy metadata to be stored")
	}
	if stored.MaskRequestHost != mask {
		t.Fatalf("MaskRequestHost=%q, want %q", stored.MaskRequestHost, mask)
	}
}
