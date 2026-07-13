// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

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

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=0 should be accepted, got ret=%+v", rsp.Ret)
	}
	if rsp.EndAt <= 0 {
		t.Fatalf("timeout=0 should return an immediate endAt, got %d", rsp.EndAt)
	}
}

func TestSetTimeoutValidationRejectsNegative(t *testing.T) {
	// Only -1 (NeverTimeout) is accepted as a valid negative value.
	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-negative",
		SandboxID: "sb-timeout-negative-validation",
		Timeout:   -2,
	})

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) {
		t.Fatalf("timeout<-1 should be rejected as params error, got ret=%+v", rsp.Ret)
	}
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

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=-1 (NeverTimeout) should be accepted, got ret=%+v", rsp.Ret)
	}
}
