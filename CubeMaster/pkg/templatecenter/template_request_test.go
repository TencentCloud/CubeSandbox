package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCloneEgressRuleDeepCopiesPort(t *testing.T) {
	port := 8443
	rule := &types.EgressRule{
		Name: "custom-https",
		Match: &types.EgressRuleMatch{
			Port: &port,
		},
	}

	cloned := rule.DeepCopy()
	if cloned == nil || cloned.Match == nil || cloned.Match.Port == nil {
		t.Fatalf("cloned rule lost port: %+v", cloned)
	}
	if *cloned.Match.Port != port {
		t.Fatalf("cloned port=%d, want %d", *cloned.Match.Port, port)
	}
	if cloned.Match.Port == rule.Match.Port {
		t.Fatal("cloned port aliases source pointer")
	}

	*cloned.Match.Port = 443
	if *rule.Match.Port != 8443 {
		t.Fatalf("source port changed through clone: %d", *rule.Match.Port)
	}
}
