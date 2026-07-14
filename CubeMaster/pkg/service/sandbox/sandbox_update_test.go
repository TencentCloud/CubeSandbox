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

type updateTimeoutProvider struct {
	sandboxID string
	timeout   int
	calls     int
}

func (p *updateTimeoutProvider) RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	p.sandboxID = sandboxID
	p.timeout = timeoutSeconds
	p.calls++
	return 12345, nil
}

func (p *updateTimeoutProvider) LookupEndAt(ctx context.Context, sandboxID string) (int64, error) {
	return 0, nil
}

func TestUpdateNormalizesNegativeResumeTimeout(t *testing.T) {
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

	require.NotNil(t, req.Timeout)
	assert.Equal(t, types.NeverTimeout, *req.Timeout)
	assert.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
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

	provider := &updateTimeoutProvider{}
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

	provider := &updateTimeoutProvider{}
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
