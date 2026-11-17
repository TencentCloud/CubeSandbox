package sandbox

import (
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplySandboxConfigEchoesPersistedSpec(t *testing.T) {
	allowInternet := true
	secret := "topsecret"
	specs := map[string]*types.CreateCubeSandboxReq{
		"sb-full": {AutoPause: true, AutoResume: true, CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowInternetAccess: &allowInternet,
			Rules: []*types.EgressRule{{
				Name: "inject-cred",
				Action: &types.EgressRuleAction{
					Inject: []*types.EgressRuleInject{{Header: "Authorization", Secret: secret}},
				},
			}},
		}},
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
	if len(full.CubeNetworkConfig.Rules) != 0 {
		t.Fatalf("echoed network config must drop rules (inject secrets): %+v", full.CubeNetworkConfig.Rules)
	}
	if len(specs["sb-full"].CubeNetworkConfig.Rules) != 1 {
		t.Fatalf("echo must copy the config, spec rules = %+v, want untouched", specs["sb-full"].CubeNetworkConfig.Rules)
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

func TestEndAtStatusFor(t *testing.T) {
	never := types.NeverTimeout
	finite := 600
	for name, tc := range map[string]struct {
		endAt   int64
		timeout *int
		want    string
	}{
		"real deadline":        {endAt: 1_700_000_000_000, timeout: &finite, want: types.EndAtStatusDeadline},
		"true never-timeout":   {timeout: &never, want: types.EndAtStatusNever},
		"finite but no meta":   {timeout: &finite, want: types.EndAtStatusUnknown},
		"no timeout persisted": {want: types.EndAtStatusUnknown},
	} {
		if got := endAtStatusFor(tc.endAt, tc.timeout); got != tc.want {
			t.Fatalf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestApplySandboxConfigSetsEndAtStatus(t *testing.T) {
	never := types.NeverTimeout
	items := []*types.SandboxData{
		{SandboxID: "sb-deadline", EndAt: 1_700_000_000_000},
		{SandboxID: "sb-never"},
		{SandboxID: "sb-missing-meta", EndAt: 0},
	}
	specs := map[string]*types.CreateCubeSandboxReq{
		"sb-deadline":     {Timeout: nil},
		"sb-never":        {Timeout: &never},
		"sb-missing-meta": {Timeout: func() *int { v := 600; return &v }()},
	}
	applySandboxConfig(items, specs)
	if items[0].EndAtStatus != types.EndAtStatusDeadline {
		t.Fatalf("deadline: got %q", items[0].EndAtStatus)
	}
	if items[1].EndAtStatus != types.EndAtStatusNever {
		t.Fatalf("never: got %q", items[1].EndAtStatus)
	}
	if items[2].EndAtStatus != types.EndAtStatusUnknown {
		t.Fatalf("unknown: got %q", items[2].EndAtStatus)
	}
}

func TestResponseMarshalScrubsEgressRules(t *testing.T) {
	secret := "topsecret"
	cfg := &types.CubeNetworkConfig{
		AllowInternetAccess: &[]bool{false}[0],
		Rules: []*types.EgressRule{{
			Name:   "inject-cred",
			Action: &types.EgressRuleAction{Inject: []*types.EgressRuleInject{{Header: "Authorization", Secret: secret}}},
		}},
	}
	detail := &types.SandboxData{SandboxID: "sb-1", CubeNetworkConfig: cfg}
	brief := &types.SandboxBriefData{SandboxID: "sb-1", CubeNetworkConfig: cfg}

	for name, item := range map[string]interface{}{
		"SandboxData":      detail,
		"SandboxBriefData": brief,
	} {
		raw, err := types.FastestJsoniter.Marshal(item)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		if strings.Contains(string(raw), "rules") {
			t.Fatalf("%s: rules leaked onto the wire: %s", name, raw)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s: inject secret leaked onto the wire", name)
		}
		var decoded map[string]interface{}
		if err := types.FastestJsoniter.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if decoded["sandbox_id"] != "sb-1" {
			t.Fatalf("%s: sandbox_id lost in marshal: %v", name, decoded)
		}
		if decoded["cube_network_config"] == nil {
			t.Fatalf("%s: cube_network_config lost in marshal", name)
		}
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("source config rules mutated: %+v", cfg.Rules)
	}
}

func TestEchoNetworkConfigMarksRulesPresence(t *testing.T) {
	secret := "topsecret"
	spec := &types.CreateCubeSandboxReq{
		CubeNetworkConfig: &types.CubeNetworkConfig{
			Rules: []*types.EgressRule{{
				Name:   "inject-cred",
				Action: &types.EgressRuleAction{Inject: []*types.EgressRuleInject{{Header: "Authorization", Secret: secret}}},
			}},
		},
	}
	items := []*types.SandboxData{{SandboxID: "sb-1"}}
	applySandboxConfig(items, map[string]*types.CreateCubeSandboxReq{"sb-1": spec})

	raw, err := types.FastestJsoniter.Marshal(items[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"rules_present":true`) {
		t.Fatalf("rules_present missing on the wire: %s", raw)
	}
	if strings.Contains(string(raw), "rules") && strings.Contains(string(raw), secret) {
		t.Fatalf("rules leaked onto the wire: %s", raw)
	}
	var decoded map[string]interface{}
	if err := types.FastestJsoniter.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	net, _ := decoded["cube_network_config"].(map[string]interface{})
	if _, ok := net["rules"]; ok {
		t.Fatalf("rules key present on the wire: %v", net)
	}
	if net["rules_present"] != true {
		t.Fatalf("rules_present not true: %v", net)
	}
}
