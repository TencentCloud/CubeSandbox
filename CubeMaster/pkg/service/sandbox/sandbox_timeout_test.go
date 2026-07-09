// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSetTimeoutValidationAllowsZero(t *testing.T) {
	const sandboxID = "sb-timeout-zero-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-zero",
		SandboxID: sandboxID,
		Timeout:   0,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Greater(t, rsp.EndAt, int64(0))
}

func TestSetTimeoutValidationRejectsNegative(t *testing.T) {
	const sandboxID = "sb-timeout-negative-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	// Only -1 (NeverTimeout) is accepted as a valid negative value.
	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-negative",
		SandboxID: sandboxID,
		Timeout:   -2,
	})

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}

func TestRefreshValidationRejectsNonPositiveDuration(t *testing.T) {
	for _, duration := range []int32{0, -1} {
		rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
			RequestID: "req-invalid-duration",
			SandboxID: "sandbox-does-not-need-resolution",
			Duration:  duration,
		})

		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) {
			t.Fatalf("duration=%d should be rejected as params error, got ret=%+v", duration, rsp.Ret)
		}
	}
}

func TestTimeoutValidationPrecedesSandboxIDResolution(t *testing.T) {
	const unresolvedSandboxID = " "
	ctx := context.Background()
	// A whitespace-only ID passes the initial non-empty check, but normalization
	// deterministically rejects it before consulting cache or cluster state.

	t.Run("set timeout", func(t *testing.T) {
		rsp := SetTimeout(ctx, &types.SetTimeoutRequest{
			RequestID: "req-invalid-timeout-and-sandbox-id",
			SandboxID: unresolvedSandboxID,
			Timeout:   -2,
		})

		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
			rsp.Ret.RetMsg != "timeout must be >= -1 (use -1 for never timeout)" {
			t.Fatalf("timeout validation should take precedence, got ret=%+v", rsp.Ret)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		rsp := Refresh(ctx, &types.RefreshSandboxRequest{
			RequestID: "req-invalid-duration-and-sandbox-id",
			SandboxID: unresolvedSandboxID,
			Duration:  0,
		})

		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
			rsp.Ret.RetMsg != "duration must be positive (seconds)" {
			t.Fatalf("duration validation should take precedence, got ret=%+v", rsp.Ret)
		}
	})
}

func TestSetTimeoutValidationAllowsNeverTimeout(t *testing.T) {
	const sandboxID = "sb-timeout-never-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-never",
		SandboxID: sandboxID,
		Timeout:   types.NeverTimeout,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
}

func TestRefreshValidationAllowsZero(t *testing.T) {
	const sandboxID = "sb-refresh-zero-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-zero",
		SandboxID: sandboxID,
		Duration:  0,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Greater(t, rsp.EndAt, int64(0))
}

func TestRefreshValidationAllowsNeverTimeout(t *testing.T) {
	const sandboxID = "sb-refresh-never-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-never",
		SandboxID: sandboxID,
		Duration:  types.NeverTimeout,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
}

func TestRefreshValidationRejectsInvalidNegative(t *testing.T) {
	const sandboxID = "sb-refresh-negative-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-negative",
		SandboxID: sandboxID,
		Duration:  -2,
	})

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}
