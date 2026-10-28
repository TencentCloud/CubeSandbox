package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestSafePrintCreateRedactsCreateTimeEnvValues(t *testing.T) {
	const secret = "example-sensitive-value"
	req := &types.CreateCubeSandboxReq{
		CreateTimeEnvVars: map[string]string{"TOKEN": secret},
		Annotations:       map[string]string{constants.CubeAnnotationCreateTimeEnvVars: secret},
		Containers:        []*types.Container{{Envs: []*types.KeyValue{{Key: "TOKEN", Value: secret}}}},
	}
	if strings.Contains(safePrintCreateCubeSandboxReqWithDiagnostics(req, true), secret) {
		t.Fatal("create-time environment value appeared in request log")
	}
	if !strings.Contains(safePrintCreateCubeSandboxReqWithDiagnostics(req, false), secret) {
		t.Fatal("disabled diagnostics must preserve existing request logging behavior")
	}
	if req.CreateTimeEnvVars["TOKEN"] != secret {
		t.Fatal("logging must not change the original request")
	}
	if req.Containers[0].Envs[0].Value != secret || req.Annotations[constants.CubeAnnotationCreateTimeEnvVars] != secret {
		t.Fatal("logging must not change container envs or annotations")
	}
}

func TestSetCreateTimeEnvVarsAnnotation(t *testing.T) {
	out := map[string]string{}
	envVars := map[string]string{
		"SESSION_ID": "user-session-test",
		"USER_ID":    "42",
	}

	if err := setCreateTimeEnvVarsAnnotation(out, envVars); err != nil {
		t.Fatalf("setCreateTimeEnvVarsAnnotation err=%v", err)
	}

	raw := out[constants.CubeAnnotationCreateTimeEnvVars]
	if raw == "" {
		t.Fatalf("missing %s annotation", constants.CubeAnnotationCreateTimeEnvVars)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal create time env vars annotation: %v", err)
	}
	if decoded["SESSION_ID"] != "user-session-test" {
		t.Fatalf("SESSION_ID=%q, want user-session-test", decoded["SESSION_ID"])
	}
	if decoded["USER_ID"] != "42" {
		t.Fatalf("USER_ID=%q, want 42", decoded["USER_ID"])
	}
}

func TestSetCreateTimeEnvVarsAnnotationRejectsOversizedPayload(t *testing.T) {
	out := map[string]string{}
	envVars := map[string]string{
		"OVERSIZED": strings.Repeat("x", maxCreateTimeEnvVarsAnnotationBytes),
	}

	err := setCreateTimeEnvVarsAnnotation(out, envVars)
	if err == nil {
		t.Fatal("expected oversized payload error")
	}
	if !strings.Contains(err.Error(), "annotation payload too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}
