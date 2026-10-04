// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestUpdateResumeRejectsTimeoutBelowNever(t *testing.T) {
	ctx := context.Background()
	invalid := -2

	t.Run("rejects before sandbox id resolution", func(t *testing.T) {
		rsp := Update(ctx, &types.UpdateRequest{
			RequestID:    "req-resume-invalid-timeout-and-id",
			SandboxID:    " ",
			InstanceType: "cubebox",
			Action:       "resume",
			Timeout:      &invalid,
		})
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
			rsp.Ret.RetMsg != "timeout must be >= -1 (use -1 for never timeout)" {
			t.Fatalf("timeout validation should take precedence, got ret=%+v", rsp.Ret)
		}
	})

	t.Run("rejects timeout below never-timeout sentinel", func(t *testing.T) {
		rsp := Update(ctx, &types.UpdateRequest{
			RequestID:    "req-resume-timeout-minus-two",
			SandboxID:    "sb-resume-timeout-minus-two",
			InstanceType: "cubebox",
			Action:       "resume",
			Timeout:      &invalid,
		})
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) ||
			rsp.Ret.RetMsg != "timeout must be >= -1 (use -1 for never timeout)" {
			t.Fatalf("timeout=-2 should be rejected as params error, got ret=%+v", rsp.Ret)
		}
	})

	t.Run("never-timeout is not rejected by validation", func(t *testing.T) {
		never := types.NeverTimeout
		rsp := Update(ctx, &types.UpdateRequest{
			RequestID:    "req-resume-never-with-blank-id",
			SandboxID:    " ",
			InstanceType: "cubebox",
			Action:       "resume",
			Timeout:      &never,
		})
		if rsp.Ret.RetMsg == "timeout must be >= -1 (use -1 for never timeout)" {
			t.Fatalf("timeout=-1 must not fail timeout validation, got ret=%+v", rsp.Ret)
		}
	})
}

func TestPublishUpdateTimeoutValueMatrix(t *testing.T) {
	const sandboxID = "sb-publish-update-timeout-matrix"
	ctx := context.Background()

	cases := []struct {
		name        string
		req         *types.UpdateRequest
		wantCalled  bool
		wantTimeout int
	}{
		{name: "nil request", req: nil},
		{
			name: "pause ignores timeout",
			req: &types.UpdateRequest{
				SandboxID: sandboxID,
				Action:    "pause",
				Timeout:   types.TimeoutPtr(types.NeverTimeout),
			},
		},
		{
			name: "nil timeout preserves stored",
			req:  &types.UpdateRequest{SandboxID: sandboxID, Action: "resume"},
		},
		{
			name: "zero timeout preserves stored",
			req: &types.UpdateRequest{
				SandboxID: sandboxID,
				Action:    "resume",
				Timeout:   types.TimeoutPtr(0),
			},
		},
		{
			name:        "never timeout publishes -1",
			req:         &types.UpdateRequest{SandboxID: sandboxID, Action: "resume", Timeout: types.TimeoutPtr(types.NeverTimeout)},
			wantCalled:  true,
			wantTimeout: types.NeverTimeout,
		},
		{
			name:        "positive timeout publishes N",
			req:         &types.UpdateRequest{SandboxID: sandboxID, Action: "resume", Timeout: types.TimeoutPtr(120)},
			wantCalled:  true,
			wantTimeout: 120,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &mockTimeoutProvider{}
			SetTimeoutProvider(provider)
			defer SetTimeoutProvider(nil)

			publishUpdateTimeout(ctx, tc.req)
			if provider.called != tc.wantCalled {
				t.Fatalf("provider called=%v, want %v", provider.called, tc.wantCalled)
			}
			if tc.wantCalled && provider.lastTimeoutSeconds != tc.wantTimeout {
				t.Fatalf("provider timeout=%d, want %d", provider.lastTimeoutSeconds, tc.wantTimeout)
			}
			if tc.wantCalled && provider.lastSandboxID != sandboxID {
				t.Fatalf("provider sandboxID=%s, want %s", provider.lastSandboxID, sandboxID)
			}
		})
	}
}

func TestUpdateResumePublishesTimeoutOnlyOnSuccess(t *testing.T) {
	const sandboxID = "sb-update-resume-publish-on-success"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	provider := &mockTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)

	t.Run("success publishes timeout", func(t *testing.T) {
		provider.called = false
		stubResumeUpdate(t, successResumeRes())
		rsp := Update(context.Background(), resumeTimeoutReq(sandboxID, 120))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			t.Fatalf("successful resume should stay success, got ret=%+v", rsp.Ret)
		}
		if !provider.called || provider.lastTimeoutSeconds != 120 {
			t.Fatalf("successful resume should publish timeout=120, called=%v timeout=%d",
				provider.called, provider.lastTimeoutSeconds)
		}
	})

	t.Run("failed resume does not publish", func(t *testing.T) {
		provider.called = false
		provider.lastTimeoutSeconds = 0
		stubResumeUpdate(t, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterInternalError),
				RetMsg:  "resume failed",
			},
		})
		rsp := Update(context.Background(), resumeTimeoutReq(sandboxID, 120))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterInternalError) {
			t.Fatalf("failed resume should keep its error, got ret=%+v", rsp.Ret)
		}
		if provider.called {
			t.Fatal("failed resume must not rewrite timeout metadata")
		}
	})
}

func TestUpdateResumeSucceedsWhenTimeoutProviderFails(t *testing.T) {
	const sandboxID = "sb-update-resume-provider-error"
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    "127.0.0.1",
	})
	defer localcache.DeleteSandboxCache(sandboxID)

	provider := &mockTimeoutProvider{returnErr: errors.New("mock provider error")}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)
	stubResumeUpdate(t, successResumeRes())

	rsp := Update(context.Background(), resumeTimeoutReq(sandboxID, types.NeverTimeout))
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		t.Fatalf("provider failure must not change a successful resume, got ret=%+v", rsp.Ret)
	}
	if !provider.called {
		t.Fatal("expected RefreshTimeout to be attempted")
	}
}

func TestUpdateCompletedResumeStillPublishesTimeoutOnSyncFailure(t *testing.T) {
	const sid = "sb-resume-sync-pending"
	localcache.SetSandboxCache(sid, &localcache.SandboxCache{SandboxID: sid, HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache(sid)
	provider := &mockTimeoutProvider{}
	SetTimeoutProvider(provider)
	defer SetTimeoutProvider(nil)
	for _, timeout := range []int{300, types.NeverTimeout} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			provider.called = false
			stubResumeUpdate(t, &types.Res{ResumeCompleted: true, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterInternalError), RetMsg: "resume completed; Redis synchronization failed"}})
			rsp := Update(context.Background(), resumeTimeoutReq(sid, timeout))
			if !rsp.ResumeCompleted || rsp.Ret.RetCode != int(errorcode.ErrorCode_MasterInternalError) {
				t.Fatal("partial success was lost or incorrectly reported as full success")
			}
			if !provider.called || provider.lastTimeoutSeconds != timeout {
				t.Fatal("completed restore lost timeout update")
			}
		})
	}
}

func resumeTimeoutReq(sandboxID string, timeout int) *types.UpdateRequest {
	return &types.UpdateRequest{
		RequestID:    "req-resume-timeout",
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
		Action:       "resume",
		Timeout:      types.TimeoutPtr(timeout),
	}
}

func successResumeRes() *types.Res {
	return &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
}

func stubResumeUpdate(t *testing.T, resumeRsp *types.Res) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, sandboxID string, opts sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	patches.ApplyFunc(resumeFromPauseSnapshot, func(ctx context.Context, req *types.UpdateRequest, hostIP string) *types.Res {
		return resumeRsp
	})
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}}
	})
}
