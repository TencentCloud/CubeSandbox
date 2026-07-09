package sandbox

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
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
