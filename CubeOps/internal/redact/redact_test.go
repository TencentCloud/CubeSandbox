// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestValue_RedactsTopLevelSecret(t *testing.T) {
	in := map[string]interface{}{
		"apiKey": "sk-very-secret",
		"model":  "deepseek-chat",
	}
	out := Value(in).(map[string]interface{})

	if out["apiKey"] != redacted {
		t.Errorf("apiKey = %v, want %q", out["apiKey"], redacted)
	}
	if out["model"] != "deepseek-chat" {
		t.Errorf("model = %v, want deepseek-chat", out["model"])
	}
}

func TestValue_DoesNotMutateInput(t *testing.T) {
	in := map[string]interface{}{
		"secret": "leak-me",
		"safe":   "ok",
	}
	_ = Value(in)

	if in["secret"] != "leak-me" {
		t.Errorf("input was mutated: secret = %v", in["secret"])
	}
}

func TestValue_RedactsNestedMap(t *testing.T) {
	in := map[string]interface{}{
		"cube_network_config": map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"name": "agenthub-llm",
					"action": map[string]interface{}{
						"inject": []interface{}{
							map[string]interface{}{
								"header": "Authorization",
								"secret": "sk-VERY-SECRET-KEY",
								"format": "Bearer ${SECRET}",
							},
						},
					},
				},
			},
		},
		"labels": map[string]interface{}{"app": "cube"},
	}
	out := Value(in).(map[string]interface{})

	// Walk down to verify the secret is redacted but other fields are intact.
	cnc := out["cube_network_config"].(map[string]interface{})
	rules := cnc["rules"].([]interface{})
	rule0 := rules[0].(map[string]interface{})
	action := rule0["action"].(map[string]interface{})
	inject := action["inject"].([]interface{})
	inj0 := inject[0].(map[string]interface{})

	if inj0["secret"] != redacted {
		t.Errorf("nested secret = %v, want %q", inj0["secret"], redacted)
	}
	if inj0["format"] != "Bearer ${SECRET}" {
		t.Errorf("format was redacted by mistake: %v", inj0["format"])
	}
	if inj0["header"] != "Authorization" {
		t.Errorf("header was redacted by mistake: %v", inj0["header"])
	}
	// Unrelated sibling map at the top level survives untouched.
	labels := out["labels"].(map[string]interface{})
	if labels["app"] != "cube" {
		t.Errorf("unrelated sibling map was corrupted: labels.app = %v", labels["app"])
	}
}

func TestValue_CaseInsensitive(t *testing.T) {
	cases := []string{
		"LLMApiKey",
		"llm_api_key",
		"ApiKey",
		"API_KEY",
		"clientSecret",
		"refreshToken",
		"Authorization",
		"private_key",
	}
	for _, k := range cases {
		in := map[string]interface{}{k: "value"}
		out := Value(in).(map[string]interface{})
		if out[k] != redacted {
			t.Errorf("key %q: value = %v, want %q", k, out[k], redacted)
		}
	}
}

func TestValue_LeavesNonSensitiveFieldsAlone(t *testing.T) {
	in := map[string]interface{}{
		"model":         "deepseek-chat",
		"host":          "api.deepseek.com",
		"allowInternet": true,
		"count":         42,
	}
	out := Value(in).(map[string]interface{})
	for k, v := range in {
		if out[k] != v {
			t.Errorf("non-sensitive field %q was modified: got %v, want %v", k, out[k], v)
		}
	}
}

func TestValue_RedactsInSlice(t *testing.T) {
	in := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"name": "a", "token": "t1"},
			map[string]interface{}{"name": "b", "token": "t2"},
		},
	}
	out := Value(in).(map[string]interface{})
	items := out["items"].([]interface{})
	for i, raw := range items {
		m := raw.(map[string]interface{})
		if m["token"] != redacted {
			t.Errorf("items[%d].token = %v, want %q", i, m["token"], redacted)
		}
		if m["name"] == redacted {
			t.Errorf("items[%d].name was redacted by mistake", i)
		}
	}
}

func TestSecret_FormatsRedactedButMarshalsPlaintext(t *testing.T) {
	const value = "sk-DO-NOT-LOG"
	secret := Secret(value)

	if got := fmt.Sprint(secret); got != redacted {
		t.Fatalf("formatted secret = %q, want %q", got, redacted)
	}
	if got := fmt.Sprintf("%#v", secret); got != redacted {
		t.Fatalf("Go-formatted secret = %q, want %q", got, redacted)
	}

	encoded, err := json.Marshal(map[string]interface{}{"secret": secret})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if got := string(encoded); got != `{"secret":"sk-DO-NOT-LOG"}` {
		t.Fatalf("encoded secret = %s, want plaintext transport value", got)
	}
}

// TestSecret_LogValueIsRedacted verifies slog handlers render the placeholder,
// not the plaintext MarshalJSON would produce.
func TestSecret_LogValueIsRedacted(t *testing.T) {
	var lv slog.LogValuer = Secret("sk-DO-NOT-LOG")
	if got := lv.LogValue().String(); got != redacted {
		t.Errorf("LogValue() = %q, want %q", got, redacted)
	}
}

// TestValue_RedactsSecretTypedLeafUnderSafeKey verifies a Secret under a
// harmless-looking key is still replaced, since the log codec honours
// MarshalJSON and would otherwise emit it verbatim.
func TestValue_RedactsSecretTypedLeafUnderSafeKey(t *testing.T) {
	in := map[string]interface{}{
		"header": "Authorization",
		"value":  Secret("sk-LEAK-ME"),
	}
	out := Value(in).(map[string]interface{})

	if out["value"] != redacted {
		t.Errorf("Secret leaf under safe key = %v, want %q", out["value"], redacted)
	}
	if out["header"] != "Authorization" {
		t.Errorf("non-sensitive sibling was modified: %v", out["header"])
	}
}

// TestJSON_RedactsSecretTypedLeaf verifies JSON() never emits the plaintext of
// an embedded Secret, even though json.Marshal on the raw Secret would.
func TestJSON_RedactsSecretTypedLeaf(t *testing.T) {
	in := map[string]interface{}{
		"action": map[string]interface{}{
			"inject": []interface{}{
				map[string]interface{}{"header": "Authorization", "value": Secret("sk-LEAK-ME")},
			},
		},
	}
	b, err := JSON(in)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(b), "sk-LEAK-ME") {
		t.Errorf("JSON output leaked the Secret plaintext: %s", b)
	}
}

// TestJSON_RedactsSecretInTypedSlice uses the exact shape LLMEgressRule
// produces — inject is a []map[string]interface{}, not a []interface{} — which
// the non-reflective fast paths would return verbatim, leaking the API key.
func TestJSON_RedactsSecretInTypedSlice(t *testing.T) {
	in := map[string]interface{}{
		"action": map[string]interface{}{
			"inject": []map[string]interface{}{
				{"header": "Authorization", "secret": Secret("sk-LEAK-ME"), "format": "Bearer ${SECRET}"},
			},
		},
	}
	b, err := JSON(in)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(b), "sk-LEAK-ME") {
		t.Errorf("JSON output leaked the API key: %s", b)
	}
	if !strings.Contains(string(b), "Bearer ${SECRET}") {
		t.Errorf("non-sensitive sibling was dropped: %s", b)
	}
}

// TestValue_RedactsTypedStringMap covers map[string]string, another concrete
// container type the fast paths miss.
func TestValue_RedactsTypedStringMap(t *testing.T) {
	out := Value(map[string]string{"apiKey": "sk-LEAK-ME", "model": "deepseek"})
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("Value returned %T, want a traversed map", out)
	}
	if m["apiKey"] != redacted {
		t.Errorf("apiKey = %v, want %q", m["apiKey"], redacted)
	}
	if m["model"] != "deepseek" {
		t.Errorf("model = %v, want deepseek", m["model"])
	}
}

// TestValue_LeavesByteSliceAlone guards the reflection path from mangling
// []byte, which marshals as a JSON string rather than a container.
func TestValue_LeavesByteSliceAlone(t *testing.T) {
	b, err := JSON(map[string]interface{}{"raw": []byte("hello")})
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(string(b), `"raw":"aGVsbG8="`) {
		t.Errorf("[]byte was not preserved as a JSON string: %s", b)
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	in := map[string]interface{}{
		"cube_network_config": map[string]interface{}{
			"action": map[string]interface{}{
				"inject": []interface{}{
					map[string]interface{}{"secret": "sk-LEAK-ME", "header": "Authorization"},
				},
			},
		},
		"labels": map[string]interface{}{"app": "cube"},
	}
	b, err := JSON(in)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "sk-LEAK-ME") {
		t.Errorf("JSON output contains plaintext secret: %s", s)
	}
	if !strings.Contains(s, redacted) {
		t.Errorf("JSON output missing %q placeholder: %s", redacted, s)
	}
	if !strings.Contains(s, `"labels"`) {
		t.Errorf("JSON output dropped unrelated field: %s", s)
	}

	// Sanity: result is still valid JSON.
	var back map[string]interface{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v", err)
	}
}
