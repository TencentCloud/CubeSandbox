package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestEffectiveArtifactDownloadBaseURLPrefersConfiguredMasterAddr(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://cube-master.cube-system.svc:8089")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://0.0.0.0:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://cube-master.cube-system.svc:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLRewritesLoopbackEnvWithSharedNodeIP(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.0.0.8")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://10.0.0.8:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLSkipsUnrewritableLoopbackEnv(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://fallback.example"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLFallsBackToArtifactRow(t *testing.T) {
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("", artifact), "http://master-from-row:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLRewritesLoopbackArtifactRowWithSharedNodeIP(t *testing.T) {
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.0.0.8")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://127.0.0.1:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("", artifact), "http://10.0.0.8:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

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
