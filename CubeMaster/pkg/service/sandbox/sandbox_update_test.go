// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestUpdateRejectsInvalidNegativeResumeTimeout(t *testing.T) {
	const sandboxID = "sbx-update-timeout"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldMockUpdateAction := config.GetConfig().Common.MockUpdateAction
	config.GetConfig().Common.MockUpdateAction = true
	defer func() {
		config.GetConfig().Common.MockUpdateAction = oldMockUpdateAction
	}()

	timeout := -2
	req := &types.UpdateRequest{
		RequestID:    "req-update-timeout",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      &timeout,
	}

	rsp := Update(context.Background(), req)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), rsp.Ret.RetCode)
	assert.Equal(t, "timeout must be >= -1 (use -1 for never timeout)", rsp.Ret.RetMsg)
	assert.Equal(t, -2, *req.Timeout)
}

func TestUpdateResumePublishesTimeoutMetadata(t *testing.T) {
	const sandboxID = "sbx-update-timeout-metadata"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldMockUpdateAction := config.GetConfig().Common.MockUpdateAction
	config.GetConfig().Common.MockUpdateAction = true
	defer func() {
		config.GetConfig().Common.MockUpdateAction = oldMockUpdateAction
	}()

	provider := &recordingTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	timeout := 600
	rsp := Update(context.Background(), &types.UpdateRequest{
		RequestID:    "req-update-timeout-metadata",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      &timeout,
	})

	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, sandboxID, provider.sandboxID)
	assert.Equal(t, timeout, provider.timeout)
}

func TestUpdateResumeNeverTimeoutPublishesMetadata(t *testing.T) {
	const sandboxID = "sbx-update-never-timeout-metadata"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldMockUpdateAction := config.GetConfig().Common.MockUpdateAction
	config.GetConfig().Common.MockUpdateAction = true
	defer func() {
		config.GetConfig().Common.MockUpdateAction = oldMockUpdateAction
	}()

	provider := &recordingTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	timeout := types.NeverTimeout
	rsp := Update(context.Background(), &types.UpdateRequest{
		RequestID:    "req-update-never-timeout-metadata",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      &timeout,
	})

	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, sandboxID, provider.sandboxID)
	assert.Equal(t, types.NeverTimeout, provider.timeout)
}

func TestUpdateResumePreservedTimeoutRebasesMetadata(t *testing.T) {
	const sandboxID = "sbx-update-preserved-timeout-rebase"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldMockUpdateAction := config.GetConfig().Common.MockUpdateAction
	config.GetConfig().Common.MockUpdateAction = true
	defer func() {
		config.GetConfig().Common.MockUpdateAction = oldMockUpdateAction
	}()

	defer SetTimeoutProvider(nil)

	zero := 0
	for _, tc := range []struct {
		name    string
		timeout *int
	}{
		{name: "omitted", timeout: nil},
		{name: "zero", timeout: &zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &recordingTimeoutProvider{}
			SetTimeoutProvider(provider)

			rsp := Update(context.Background(), &types.UpdateRequest{
				RequestID:    "req-update-preserved-timeout-rebase",
				SandboxID:    sandboxID,
				InstanceType: "cubebox",
				Action:       "resume",
				Timeout:      tc.timeout,
			})

			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
			assert.Equal(t, 0, provider.calls)
			assert.Equal(t, 1, provider.rebaseCalls)
			assert.Equal(t, sandboxID, provider.sandboxID)
		})
	}
}

func TestUpdatePauseDoesNotPublishTimeoutMetadata(t *testing.T) {
	const sandboxID = "sbx-update-pause-timeout-metadata"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	oldMockUpdateAction := config.GetConfig().Common.MockUpdateAction
	config.GetConfig().Common.MockUpdateAction = true
	defer func() {
		config.GetConfig().Common.MockUpdateAction = oldMockUpdateAction
	}()

	provider := &recordingTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	timeout := 600
	rsp := Update(context.Background(), &types.UpdateRequest{
		RequestID:    "req-update-pause-timeout-metadata",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "pause",
		Timeout:      &timeout,
	})

	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	assert.Equal(t, 0, provider.calls)
}
