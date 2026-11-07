// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSnapshotRuntimeRefsPreservedOnInventoryFailure(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotReconcilerNodes, func(int, string) node.NodeList {
		return node.NodeList{&node.Node{InsID: "worker", IP: "10.0.0.1", Healthy: true}}
	})
	patches.ApplyFunc(sandbox.ListSandbox, func(context.Context, *sandboxtypes.ListCubeSandboxReq) *sandboxtypes.ListCubeSandboxRes {
		t.Fatal("reconciliation must not use partial inventory")
		return nil
	})
	patches.ApplyFunc(sandbox.ListSandboxWithFailOnError, func(context.Context, *sandboxtypes.ListCubeSandboxReq) *sandboxtypes.ListCubeSandboxRes {
		return &sandboxtypes.ListCubeSandboxRes{Ret: &sandboxtypes.Ret{RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed), RetMsg: "worker restarting"}}
	})
	marked := false
	patches.ApplyFunc(UpdateSnapshotRuntimeRefsNodeError, func(context.Context, string, string, string) error { marked = true; return nil })
	patches.ApplyFunc(RefreshSnapshotRuntimeRefsFromNode, func(context.Context, string, string, []SnapshotRuntimeRefInfo) error {
		t.Fatal("failed inventory must not release runtime refs")
		return nil
	})
	require.Error(t, reconcileSnapshotRuntimeRefs(context.Background()))
	require.True(t, marked)
}
