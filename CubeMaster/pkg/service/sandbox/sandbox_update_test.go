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
