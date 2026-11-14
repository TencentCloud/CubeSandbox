package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplySandboxConfigEchoesPersistedSpec(t *testing.T) {
	allowInternet := true
	specs := map[string]*types.CreateCubeSandboxReq{
		"sb-full":           {AutoPause: true, AutoResume: true, CubeNetworkConfig: &types.CubeNetworkConfig{AllowInternetAccess: &allowInternet}},
		"sb-lifecycle-only": {AutoPause: false},
	}
	items := []*types.SandboxData{
		{SandboxID: "sb-full"},
		{SandboxID: "sb-lifecycle-only"},
		{SandboxID: "sb-no-spec"},
		nil,
	}

	applySandboxConfig(items, specs)

	full := items[0]
	if full.AutoPause == nil || !*full.AutoPause || full.AutoResume == nil || !*full.AutoResume {
		t.Fatalf("sb-full lifecycle echo = %v/%v, want true/true", full.AutoPause, full.AutoResume)
	}
	if full.CubeNetworkConfig == nil || full.CubeNetworkConfig.AllowInternetAccess == nil || !*full.CubeNetworkConfig.AllowInternetAccess {
		t.Fatalf("sb-full network echo = %+v, want allowInternetAccess=true", full.CubeNetworkConfig)
	}

	lifecycleOnly := items[1]
	if lifecycleOnly.AutoPause == nil || *lifecycleOnly.AutoPause {
		t.Fatalf("sb-lifecycle-only AutoPause = %v, want explicit false", lifecycleOnly.AutoPause)
	}
	if lifecycleOnly.CubeNetworkConfig != nil {
		t.Fatalf("sb-lifecycle-only network = %+v, want nil", lifecycleOnly.CubeNetworkConfig)
	}

	noSpec := items[2]
	if noSpec.AutoPause != nil || noSpec.AutoResume != nil || noSpec.CubeNetworkConfig != nil {
		t.Fatalf("sb-no-spec fields = %v/%v/%+v, want all unset", noSpec.AutoPause, noSpec.AutoResume, noSpec.CubeNetworkConfig)
	}

	if items[3] != nil {
		t.Fatalf("nil item mutated: %+v", items[3])
	}
}
