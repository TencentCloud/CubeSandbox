// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestMergeEgressRulesPrependsRequestRulesAndKeepsSameNamedTemplateRules(t *testing.T) {
	templateRules := []*types.EgressRule{
		{
			Name: "shared",
			Match: &types.EgressRuleMatch{
				Host: strPtr("template.example.com"),
			},
			Action: &types.EgressRuleAction{Allow: false},
		},
		{
			Name: "template-only",
			Match: &types.EgressRuleMatch{
				Host: strPtr("template-only.example.com"),
			},
			Action: &types.EgressRuleAction{Allow: true},
		},
	}
	requestRules := []*types.EgressRule{
		{
			Name: "shared",
			Match: &types.EgressRuleMatch{
				Host: strPtr("request.example.com"),
			},
			Action: &types.EgressRuleAction{Allow: true},
		},
		{
			Name: "request-only",
			Match: &types.EgressRuleMatch{
				Host: strPtr("request-only.example.com"),
			},
			Action: &types.EgressRuleAction{Allow: false},
		},
	}

	got := mergeEgressRules(templateRules, requestRules)
	if len(got) != 4 {
		t.Fatalf("expected 4 merged rules, got %d", len(got))
	}
	if got[0].Name != "shared" || got[1].Name != "request-only" || got[2].Name != "shared" || got[3].Name != "template-only" {
		t.Fatalf("unexpected merged rule order: %#v", []string{got[0].Name, got[1].Name, got[2].Name, got[3].Name})
	}
	if got[0] == requestRules[0] || got[1] == requestRules[1] || got[2] == templateRules[0] || got[3] == templateRules[1] {
		t.Fatal("expected merged rules to be cloned, got shared pointers")
	}
	if got[0].Match == nil || got[0].Match.Host == nil || *got[0].Match.Host != "request.example.com" {
		t.Fatalf("expected request rule to stay first for shared name, got %+v", got[0])
	}
	if got[2].Match == nil || got[2].Match.Host == nil || *got[2].Match.Host != "template.example.com" {
		t.Fatalf("expected template rule with shared name to remain after request rules, got %+v", got[2])
	}
}

func TestMergeEgressRulesPrependsRequestRulesWithoutNameConflict(t *testing.T) {
	templateRules := []*types.EgressRule{
		{Name: "template-a", Action: &types.EgressRuleAction{Allow: true}},
		{Name: "template-b", Action: &types.EgressRuleAction{Allow: false}},
	}
	requestRules := []*types.EgressRule{
		{Name: "request-a", Action: &types.EgressRuleAction{Allow: true}},
		{Name: "request-b", Action: &types.EgressRuleAction{Allow: false}},
	}

	got := mergeEgressRules(templateRules, requestRules)
	if len(got) != 4 {
		t.Fatalf("expected 4 merged rules, got %d", len(got))
	}
	gotNames := []string{got[0].Name, got[1].Name, got[2].Name, got[3].Name}
	wantNames := []string{"request-a", "request-b", "template-a", "template-b"}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("unexpected merged rule order: got=%v want=%v", gotNames, wantNames)
		}
	}
	if got[0] == requestRules[0] || got[1] == requestRules[1] || got[2] == templateRules[0] || got[3] == templateRules[1] {
		t.Fatal("expected merged rules to be cloned, got shared pointers")
	}
}

func TestMergeEgressRulesHandlesEmptySides(t *testing.T) {
	templateRules := []*types.EgressRule{
		{Name: "template-a", Match: &types.EgressRuleMatch{Host: strPtr("template.example.com")}},
	}
	requestRules := []*types.EgressRule{
		{Name: "request-a", Match: &types.EgressRuleMatch{Host: strPtr("request.example.com")}},
	}

	gotTemplateOnly := mergeEgressRules(templateRules, nil)
	if len(gotTemplateOnly) != 1 || gotTemplateOnly[0].Name != "template-a" {
		t.Fatalf("unexpected template-only merge result: %+v", gotTemplateOnly)
	}
	if gotTemplateOnly[0] == templateRules[0] {
		t.Fatal("expected template-only merge to clone rules")
	}

	gotRequestOnly := mergeEgressRules(nil, requestRules)
	if len(gotRequestOnly) != 1 || gotRequestOnly[0].Name != "request-a" {
		t.Fatalf("unexpected request-only merge result: %+v", gotRequestOnly)
	}
	if gotRequestOnly[0] == requestRules[0] {
		t.Fatal("expected request-only merge to clone rules")
	}
}

func TestMergeEgressRulesSkipsNilEntries(t *testing.T) {
	templateRules := []*types.EgressRule{
		nil,
		{Name: "template-a", Action: &types.EgressRuleAction{Allow: true}},
	}
	requestRules := []*types.EgressRule{
		nil,
		{Name: "request-a", Action: &types.EgressRuleAction{Allow: false}},
	}

	got := mergeEgressRules(templateRules, requestRules)
	if len(got) != 2 {
		t.Fatalf("expected nil entries to be skipped, got %d merged rules", len(got))
	}
	if got[0].Name != "request-a" || got[1].Name != "template-a" {
		t.Fatalf("unexpected merged rule order with nil entries: %#v", []string{got[0].Name, got[1].Name})
	}
}

func TestMergeCubeNetworkConfigsMergesRulesWithoutAliasingTemplate(t *testing.T) {
	templateAllow := false
	requestAllow := true
	templateCfg := &types.CubeNetworkConfig{
		AllowInternetAccess: &templateAllow,
		AllowOut:            []string{"10.0.0.0/8"},
		DenyOut:             []string{"192.168.0.0/16"},
		Rules: []*types.EgressRule{
			{Name: "template-only", Match: &types.EgressRuleMatch{Host: strPtr("template.example.com")}},
		},
	}
	requestCfg := &types.CubeNetworkConfig{
		AllowInternetAccess: &requestAllow,
		AllowOut:            []string{"172.16.0.0/12"},
		DenyOut:             []string{"100.64.0.0/10"},
		Rules: []*types.EgressRule{
			{Name: "request-only", Match: &types.EgressRuleMatch{Host: strPtr("request.example.com")}},
		},
	}

	got := mergeCubeNetworkConfigs(templateCfg, requestCfg)
	if got == nil {
		t.Fatal("expected merged config, got nil")
	}
	if got.AllowInternetAccess == nil || !*got.AllowInternetAccess {
		t.Fatalf("expected request allowInternetAccess override, got %+v", got.AllowInternetAccess)
	}
	if len(got.AllowOut) != 2 || got.AllowOut[0] != "10.0.0.0/8" || got.AllowOut[1] != "172.16.0.0/12" {
		t.Fatalf("unexpected merged allowOut: %#v", got.AllowOut)
	}
	if len(got.DenyOut) != 2 || got.DenyOut[0] != "192.168.0.0/16" || got.DenyOut[1] != "100.64.0.0/10" {
		t.Fatalf("unexpected merged denyOut: %#v", got.DenyOut)
	}
	if len(got.Rules) != 2 || got.Rules[0].Name != "request-only" || got.Rules[1].Name != "template-only" {
		t.Fatalf("unexpected merged rules: %#v", []string{got.Rules[0].Name, got.Rules[1].Name})
	}
	if got.Rules[0] == requestCfg.Rules[0] || got.Rules[1] == templateCfg.Rules[0] {
		t.Fatal("expected merged rules to be cloned, got shared pointers")
	}
}

func TestMergeCubeNetworkConfigsClonesTemplateRulesWhenRequestRulesEmpty(t *testing.T) {
	templateAllow := false
	requestAllow := true
	templateCfg := &types.CubeNetworkConfig{
		AllowInternetAccess: &templateAllow,
		Rules: []*types.EgressRule{
			{Name: "template-only", Match: &types.EgressRuleMatch{Host: strPtr("template.example.com")}},
		},
	}
	requestCfg := &types.CubeNetworkConfig{
		AllowInternetAccess: &requestAllow,
	}

	got := mergeCubeNetworkConfigs(templateCfg, requestCfg)
	if got == nil {
		t.Fatal("expected merged config, got nil")
	}
	if got.AllowInternetAccess == nil || !*got.AllowInternetAccess {
		t.Fatalf("expected request allowInternetAccess override, got %+v", got.AllowInternetAccess)
	}
	if len(got.Rules) != 1 || got.Rules[0].Name != "template-only" {
		t.Fatalf("unexpected merged rules: %#v", got.Rules)
	}
	if got.Rules[0] == templateCfg.Rules[0] {
		t.Fatal("expected template rules to be cloned when request has no rules")
	}
}

func strPtr(s string) *string {
	return &s
}
