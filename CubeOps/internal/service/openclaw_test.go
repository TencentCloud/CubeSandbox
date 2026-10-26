// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/redact"
)

// TestLLMEgressRule_PlaintextForTransportAndRedactedForLog verifies that
// LLMEgressRule returns the plaintext API key for transport while redact.Value()
// masks it in logs.
func TestLLMEgressRule_PlaintextForTransportAndRedactedForLog(t *testing.T) {
	const apiKey = "sk-DO-NOT-LOG"

	rule, err := LLMEgressRule(&LLMConfig{
		Provider: "test",
		BaseURL:  "https://llm.example.test/v1",
		APIKey:   apiKey,
	})
	if err != nil {
		t.Fatalf("LLMEgressRule: %v", err)
	}

	// 1. Transport payload must carry the plaintext API key.
	payload, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(payload), apiKey) {
		t.Fatalf("transport payload missing the API key: %s", payload)
	}

	// 2. After redact.Value(), the rule must be safe to log.
	redactedRule := redact.Value(rule).(map[string]interface{})
	action := redactedRule["action"].(map[string]interface{})
	inject := action["inject"].([]interface{})
	inj0 := inject[0].(map[string]interface{})

	if got := inj0["secret"]; got != "***REDACTED***" {
		t.Errorf("redacted secret leaf = %v, want \"***REDACTED***\"", got)
	}

	redactedPayload, err := json.Marshal(redactedRule)
	if err != nil {
		t.Fatalf("json.Marshal(redacted): %v", err)
	}
	if strings.Contains(string(redactedPayload), apiKey) {
		t.Errorf("redacted payload leaked the API key: %s", redactedPayload)
	}
	if !strings.Contains(string(redactedPayload), "Bearer ${SECRET}") {
		t.Errorf("redacted payload dropped the \"format\" field: %s", redactedPayload)
	}
}
