// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLLMEgressRuleProtectsAPIKeyWhenFormatted(t *testing.T) {
	const apiKey = "sk-DO-NOT-LOG"

	rule, err := LLMEgressRule(&LLMConfig{
		Provider: "test",
		BaseURL:  "https://llm.example.test/v1",
		APIKey:   apiKey,
	})
	if err != nil {
		t.Fatalf("LLMEgressRule: %v", err)
	}

	action, ok := rule["action"].(map[string]interface{})
	if !ok {
		t.Fatal("action has unexpected type")
	}
	inject, ok := action["inject"].([]map[string]interface{})
	if !ok || len(inject) != 1 {
		t.Fatalf("inject has unexpected value: %#v", action["inject"])
	}
	if got := fmt.Sprint(inject[0]["secret"]); got != "***REDACTED***" {
		t.Fatalf("formatted secret = %q, want redacted value", got)
	}

	payload, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(payload), apiKey) {
		t.Fatalf("transport payload does not contain the API key required by CubeEgress: %s", payload)
	}
}
