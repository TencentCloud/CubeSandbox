// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeegress

import (
	"encoding/json"
	"reflect"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestRenderEgressPolicyNilInput(t *testing.T) {
	body, err := RenderEgressPolicy("sb-1", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if body != nil {
		t.Fatalf("body=%s, want nil for nil input", body)
	}
}

func TestRenderEgressPolicyEmptyRules(t *testing.T) {
	in := &PolicyInput{} // no rules
	body, err := RenderEgressPolicy("sb-1", in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// L3/L4-only configs have no L7 rules and don't produce a CubeEgress
	// policy. CubeEgress is the L7 policy plane; allow_out / deny_out /
	// allow_internet_access stay in the cubevs eBPF datapath alone.
	if body != nil {
		t.Fatalf("body=%s, want nil for empty rules", body)
	}
}

func TestRenderEgressPolicyNameToID(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "deepseek_api",
			Match:  &MatchInput{Host: strPtr("api.deepseek.com")},
			Action: &ActionInput{Allow: true},
		}},
	}
	body := decodePolicy(t, "sb-deepseek", in)
	if got := body["policy_id"]; got != "sb-deepseek" {
		t.Fatalf("policy_id=%v, want sb-deepseek", got)
	}
	rules := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules len=%d", len(rules))
	}
	rule := rules[0].(map[string]any)
	if got := rule["id"]; got != "deepseek_api" {
		t.Fatalf("rules[0].id=%v, want deepseek_api (rule.Name)", got)
	}
	// `name` must NOT leak through — CubeEgress's policy.lua doesn't
	// know about it and would treat the rule as missing rule.id.
	if _, hasName := rule["name"]; hasName {
		t.Fatalf("rules[0] should not have 'name' key (was renamed to 'id')")
	}
}

func TestRenderEgressPolicyMatchSnakeCase(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name: "r1",
			Match: &MatchInput{
				SNI:    strPtr("api.example.com"),
				Host:   strPtr("api.example.com"),
				Method: []string{"GET", "POST"},
				Path:   strPtr("/v1/chat"),
				Scheme: strPtr("https"),
			},
			Action: &ActionInput{Allow: true},
		}},
	}
	body := decodePolicy(t, "sb-1", in)
	rule := body["rules"].([]any)[0].(map[string]any)
	match := rule["match"].(map[string]any)

	want := map[string]any{
		"sni":    "api.example.com",
		"host":   "api.example.com",
		"method": []any{"GET", "POST"},
		"path":   "/v1/chat",
		"scheme": "https",
	}
	if !reflect.DeepEqual(match, want) {
		t.Fatalf("match=%v\nwant=%v", match, want)
	}
}

func TestRenderEgressPolicyActionInject(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:  "r1",
			Match: &MatchInput{Host: strPtr("api.example.com")},
			Action: &ActionInput{
				Allow: true,
				Audit: strPtr("metadata"),
				Inject: []InjectInput{
					{
						Header: "Authorization",
						Secret: "sk_xxx",
						Format: strPtr("Bearer ${SECRET}"),
					},
					{
						// format omitted → CubeEgress defaults to "${SECRET}".
						Header: "X-Token",
						Secret: "abc",
					},
				},
			},
		}},
	}
	body := decodePolicy(t, "sb-1", in)
	rule := body["rules"].([]any)[0].(map[string]any)
	action := rule["action"].(map[string]any)

	if got := action["allow"]; got != true {
		t.Fatalf("allow=%v, want true", got)
	}
	if got := action["audit"]; got != "metadata" {
		t.Fatalf("audit=%v, want metadata", got)
	}
	inject := action["inject"].([]any)
	if len(inject) != 2 {
		t.Fatalf("inject len=%d, want 2", len(inject))
	}
	first := inject[0].(map[string]any)
	wantFirst := map[string]any{
		"header": "Authorization",
		"secret": "sk_xxx",
		"format": "Bearer ${SECRET}",
	}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Fatalf("inject[0]=%v\nwant=%v", first, wantFirst)
	}
	second := inject[1].(map[string]any)
	wantSecond := map[string]any{
		"header": "X-Token",
		"secret": "abc",
	}
	if !reflect.DeepEqual(second, wantSecond) {
		t.Fatalf("inject[1]=%v\nwant=%v (no format key when omitted)", second, wantSecond)
	}
}

func TestRenderEgressPolicyDenyAction(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "block_evil",
			Match:  &MatchInput{Host: strPtr("evil.com")},
			Action: &ActionInput{Allow: false},
		}},
	}
	body := decodePolicy(t, "sb-1", in)
	action := body["rules"].([]any)[0].(map[string]any)["action"].(map[string]any)
	if got := action["allow"]; got != false {
		t.Fatalf("allow=%v, want false", got)
	}
	if _, hasInject := action["inject"]; hasInject {
		t.Fatalf("deny rule should not carry inject")
	}
}

func TestRenderEgressPolicyNilMatchEmitsEmptyObject(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "catchall",
			Match:  nil,
			Action: &ActionInput{Allow: true},
		}},
	}
	body := decodePolicy(t, "sb-1", in)
	rule := body["rules"].([]any)[0].(map[string]any)
	match, ok := rule["match"].(map[string]any)
	if !ok {
		t.Fatalf("match=%v, want object", rule["match"])
	}
	if len(match) != 0 {
		t.Fatalf("match=%v, want empty object", match)
	}
}

func TestRenderEgressPolicyNilActionDefaultsToDeny(t *testing.T) {
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "weird",
			Match:  &MatchInput{Host: strPtr("x.com")},
			Action: nil,
		}},
	}
	body := decodePolicy(t, "sb-1", in)
	action := body["rules"].([]any)[0].(map[string]any)["action"].(map[string]any)
	// nil action fails CubeEgress validation if we propagate it
	// faithfully. The renderer fills it as deny so the upstream caller
	// sees a deterministic 200 OK rather than a CubeEgress 400.
	if got := action["allow"]; got != false {
		t.Fatalf("allow=%v, want false (nil action → deny default)", got)
	}
}

// decodePolicy renders, then decodes back into a generic map so the
// tests can poke at the wire shape without depending on Go struct order.
func decodePolicy(t *testing.T, sandboxID string, in *PolicyInput) map[string]any {
	t.Helper()
	raw, err := RenderEgressPolicy(sandboxID, in)
	if err != nil {
		t.Fatalf("RenderEgressPolicy err=%v", err)
	}
	if raw == nil {
		t.Fatalf("RenderEgressPolicy body=nil, want a body for input with rules")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, raw)
	}
	return got
}
