package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestDestroySandboxMissingSandboxReturnsNotFound(t *testing.T) {
	ResetAfterDestroySandboxSuccessHooks()
	defer ResetAfterDestroySandboxSuccessHooks()

	hookCalled := false
	RegisterAfterDestroySandboxSuccessHook(func(_ context.Context, _ string) error {
		hookCalled = true
		return nil
	})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}}
	})
	// Keep ID as-is so the missing-sandbox path is exercised after resolve.
	patches.ApplyFunc(ResolveSandboxID, func(_ context.Context, input string) (string, error) {
		return input, nil
	})
	patches.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache {
		return nil
	})
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(context.Context, string) (*basetypes.SandboxProxyMap, bool) {
		return nil, false
	})

	got := DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{
		RequestID:    "req-missing-delete",
		SandboxID:    "sandbox-does-not-exist",
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Filter: &types.CubeSandboxFilter{
			LabelSelector: map[string]string{},
		},
	})

	assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
	assert.Equal(t, "no such sandbox", got.Ret.RetMsg)
	assert.False(t, hookCalled, "after-destroy success hook should not run for missing sandbox")
}

func TestSetSyncDestroyFailurePreservesDeleteAutoResumeBusinessCodes(t *testing.T) {
	tests := []struct {
		name string
		code errorcode.ErrorCode
		msg  string
	}{
		{
			name: "capacity admission conflict",
			code: errorcode.ErrorCode_Conflict,
			msg:  "resume rejected by paused_resource_release_ratio policy: node capacity is unavailable",
		},
		{
			name: "pausing state",
			code: errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid),
			msg:  "sandbox is pausing; retry DELETE after 2 seconds",
		},
		{
			name: "unproven resume",
			code: errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskResumeFailed),
			msg:  "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rsp := &types.DeleteCubeSandboxRes{Ret: &types.Ret{}}

			setSyncDestroyFailure(rsp, ret.Err(tt.code, tt.msg))

			assert.Equal(t, int(tt.code), rsp.Ret.RetCode)
			assert.Equal(t, tt.msg, rsp.Ret.RetMsg)
		})
	}
}

func TestSetSyncDestroyFailurePreservesTransportFailureContract(t *testing.T) {
	rsp := &types.DeleteCubeSandboxRes{Ret: &types.Ret{}}
	err := errors.New("cubelet connection reset")

	setSyncDestroyFailure(rsp, err)

	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), rsp.Ret.RetCode)
	assert.Equal(t, "cubelet connection reset", rsp.Ret.RetMsg)
}

func TestSetSyncDestroyFailureKeepsTypedConnectionFailureInternal(t *testing.T) {
	rsp := &types.DeleteCubeSandboxRes{Ret: &types.Ret{}}
	err := ret.Err(errorcode.ErrorCode_ConnHostFailed, "cubelet connection reset")

	setSyncDestroyFailure(rsp, err)

	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), rsp.Ret.RetCode)
	assert.Equal(t, "cubelet connection reset", rsp.Ret.RetMsg)
}
