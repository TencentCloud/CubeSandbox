// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
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

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=-1 (NeverTimeout) should be accepted, got ret=%+v", rsp.Ret)
	}
	if rsp.EndAt != 0 {
		t.Fatalf("timeout=-1 (NeverTimeout) must return EndAt=0 (never expires), got %d", rsp.EndAt)
	}
}

type mockTimeoutProvider struct {
	called             bool
	lastSandboxID      string
	lastTimeoutSeconds int
	returnEndAt        int64
	returnErr          error
}

func (m *mockTimeoutProvider) RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	m.called = true
	m.lastSandboxID = sandboxID
	m.lastTimeoutSeconds = timeoutSeconds
	return m.returnEndAt, m.returnErr
}

func (m *mockTimeoutProvider) LookupEndAt(ctx context.Context, sandboxID string) (int64, error) {
	return m.returnEndAt, m.returnErr
}

func TestSetTimeoutWithInstalledProviderAllowsNeverTimeout(t *testing.T) {
	const sandboxID = "sb-timeout-never-provider"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	provider := &mockTimeoutProvider{returnEndAt: 0}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-never-provider",
		SandboxID: sandboxID,
		Timeout:   types.NeverTimeout,
	})

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=-1 should be accepted, got ret=%+v", rsp.Ret)
	}
	if rsp.EndAt != 0 {
		t.Fatalf("expected EndAt=0, got %d", rsp.EndAt)
	}
	if provider.lastSandboxID != sandboxID {
		t.Fatalf("expected provider called with sandboxID=%s, got %s", sandboxID, provider.lastSandboxID)
	}
	if provider.lastTimeoutSeconds != types.NeverTimeout {
		t.Fatalf("expected provider called with timeoutSeconds=-1, got %d", provider.lastTimeoutSeconds)
	}
}

func TestSetTimeoutWithInstalledProviderErrorAllowsNeverTimeout(t *testing.T) {
	const sandboxID = "sb-timeout-never-provider-err"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	provider := &mockTimeoutProvider{returnErr: errors.New("mock provider error")}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-never-provider-err",
		SandboxID: sandboxID,
		Timeout:   types.NeverTimeout,
	})

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=-1 should be accepted, got ret=%+v", rsp.Ret)
	}
	if rsp.EndAt != 0 {
		t.Fatalf("expected EndAt=0 even on provider error, got %d", rsp.EndAt)
	}
}

func TestSetTimeoutWithInstalledProviderNonZeroEndAtNormalizesToZero(t *testing.T) {
	const sandboxID = "sb-timeout-never-provider-nonzero"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	// Even if a custom provider returns a spurious non-zero EndAt for -1, SetTimeout normalizes it to 0.
	provider := &mockTimeoutProvider{returnEndAt: 123456789}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{
		RequestID: "req-never-provider-nonzero",
		SandboxID: sandboxID,
		Timeout:   types.NeverTimeout,
	})

	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("timeout=-1 should be accepted, got ret=%+v", rsp.Ret)
	}
	if rsp.EndAt != 0 {
		t.Fatalf("expected EndAt=0 normalized despite provider returning %d, got %d", provider.returnEndAt, rsp.EndAt)
	}
}
