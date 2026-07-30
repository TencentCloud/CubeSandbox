// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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
		duration := int32(0)
		rsp := Refresh(ctx, &types.RefreshSandboxRequest{
			RequestID: "req-invalid-duration-and-sandbox-id",
			SandboxID: unresolvedSandboxID,
			Duration:  &duration,
		})

		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
			rsp.Ret.RetMsg != "duration must be -1 or positive" {
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
	assert.Equal(t, int64(0), rsp.EndAt, "never-timeout should return endAt=0")
}

func TestRefreshTimeoutMetaFallbackReturnsZeroForNeverTimeout(t *testing.T) {
	SetTimeoutProvider(nil)

	endAt := refreshTimeoutMeta(context.Background(), "sb-timeout-never-fallback", types.NeverTimeout)

	assert.Equal(t, int64(0), endAt)
}

func TestRefreshValidationRejectsZero(t *testing.T) {
	const sandboxID = "sb-refresh-zero-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	duration := int32(0)
	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-zero",
		SandboxID: sandboxID,
		Duration:  &duration,
	})

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}

func TestRefreshValidationAllowsNeverTimeout(t *testing.T) {
	const sandboxID = "sb-refresh-never-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	duration := int32(types.NeverTimeout)
	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-never",
		SandboxID: sandboxID,
		Duration:  &duration,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Equal(t, int64(0), rsp.EndAt, "never-timeout should return endAt=0")
}

func TestRefreshOmittedDurationUsesClusterDefault(t *testing.T) {
	const sandboxID = "sb-refresh-default-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldDefaultTimeout := config.GetConfig().CubeletConf.DefaultTimeoutInsec
	config.GetConfig().CubeletConf.DefaultTimeoutInsec = 7200
	defer func() {
		config.GetConfig().CubeletConf.DefaultTimeoutInsec = oldDefaultTimeout
	}()

	provider := &recordingTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-default",
		SandboxID: sandboxID,
	})

	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, 7200, provider.timeout)
}

func TestRefreshValidationRejectsInvalidNegative(t *testing.T) {
	const sandboxID = "sb-refresh-negative-validation"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	duration := int32(-2)
	rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{
		RequestID: "req-refresh-negative",
		SandboxID: sandboxID,
		Duration:  &duration,
	})

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
}
