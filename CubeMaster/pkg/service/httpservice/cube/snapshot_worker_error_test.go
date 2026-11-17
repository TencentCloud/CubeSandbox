// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"fmt"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSnapshotWorkerFailureIsNotAClientParameterError(t *testing.T) {
	err := fmt.Errorf("lookup source: %w", errSandboxWorkerUnavailable)
	if code := snapshotErrorCode(err); code != int(errorcode.ErrorCode_CubeletUnHealthy) {
		t.Fatalf("worker availability was classified as code %d", code)
	}
}

func TestResolveSandboxHostPreservesWorkerAvailability(t *testing.T) {
	for _, message := range []string{"", " ", "CubeletUnHealthy", "worker disconnected"} {
		t.Run(fmt.Sprintf("message=%q", message), func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache { return nil })
			patches.ApplyFunc(sandbox.SandboxInfo, func(context.Context, *types.GetCubeSandboxReq) *types.GetCubeSandboxRes {
				return &types.GetCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_CubeletUnHealthy), RetMsg: message}}
			})
			_, _, err := resolveSandboxHost(context.Background(), "request", "sandbox")
			require.ErrorIs(t, err, errSandboxWorkerUnavailable)
			require.NotContains(t, err.Error(), "not found")
			if message != "" && message != " " {
				require.ErrorContains(t, err, message)
			}
			require.Equal(t, int(errorcode.ErrorCode_CubeletUnHealthy), snapshotErrorCode(err))
		})
	}
}
