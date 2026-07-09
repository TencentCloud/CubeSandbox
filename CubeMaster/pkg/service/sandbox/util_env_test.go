package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

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
