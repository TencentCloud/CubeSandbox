// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"testing"
)

func ueStrPtr(s string) *string { return &s }

// ueRule builds a minimal inject rule whose injected secret is the one thing a
// credential refresh changes, so tests can assert what UpdateEgressRule persisted.
func ueRule(name, secret string) *EgressRule {
	return &EgressRule{
		Name:  name,
		Match: &EgressRuleMatch{Path: ueStrPtr("/api/*")},
		Action: &EgressRuleAction{
			Allow:  true,
			Inject: []*EgressRuleInject{{Header: "Authorization", Secret: secret}},
		},
	}
}

// ueService builds a localService with a real on-disk store (so persistence is
// exercised for real) and an injectable fake CubeEgress client.
func ueService(t *testing.T) (*localService, *fakeEgress) {
	t.Helper()
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	fake := newFakeEgress()
	return &localService{
		store:  store,
		egress: fake,
		states: map[string]*managedState{},
	}, fake
}

func ueSeed(s *localService, key, sandboxID, sandboxIP string, rules ...*EgressRule) {
	s.states[key] = &managedState{
		persistedState: persistedState{
			SandboxID:         sandboxID,
			SandboxIP:         sandboxIP,
			CubeNetworkConfig: &CubeNetworkConfig{Rules: rules},
		},
	}
}

// ueLoadRules reads the rule set back from the store to prove it was persisted,
// not merely mutated in memory.
func ueLoadRules(t *testing.T, s *localService, sandboxID string) []*EgressRule {
	t.Helper()
	all, err := s.store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error=%v", err)
	}
	for _, st := range all {
		if st.SandboxID == sandboxID {
			if st.CubeNetworkConfig == nil {
				return nil
			}
			return st.CubeNetworkConfig.Rules
		}
	}
	t.Fatalf("sandbox %q not found in store", sandboxID)
	return nil
}

func TestUpdateEgressRuleAppendsNewRule(t *testing.T) {
	s, fake := ueService(t)
	ueSeed(s, "sb-1", "sb-1", "192.168.0.10")

	resp, err := s.UpdateEgressRule(context.Background(), &UpdateEgressRuleRequest{
		SandboxID: "sb-1",
		Rule:      ueRule("gitlab_token", "tok-A"),
	})
	if err != nil {
		t.Fatalf("UpdateEgressRule error=%v", err)
	}
	if !resp.Applied || resp.Pending {
		t.Fatalf("want applied on a healthy push, got applied=%v pending=%v", resp.Applied, resp.Pending)
	}
	if resp.RuleCount != 1 {
		t.Fatalf("RuleCount=%d, want 1", resp.RuleCount)
	}
	if resp.SandboxIP != "192.168.0.10" {
		t.Fatalf("SandboxIP=%q, want 192.168.0.10", resp.SandboxIP)
	}
	if fake.putCount() != 1 {
		t.Fatalf("put count=%d, want 1 (live re-push)", fake.putCount())
	}
	rules := ueLoadRules(t, s, "sb-1")
	if len(rules) != 1 || rules[0].Name != "gitlab_token" {
		t.Fatalf("persisted rules=%+v, want one gitlab_token rule", rules)
	}
	if got := rules[0].Action.Inject[0].Secret; got != "tok-A" {
		t.Fatalf("persisted secret=%q, want tok-A", got)
	}
}

func TestUpdateEgressRuleReplacesByNameInPlace(t *testing.T) {
	s, fake := ueService(t)
	ueSeed(s, "sb-1", "sb-1", "192.168.0.10",
		ueRule("api_bearer", "old"),
		ueRule("git_basic", "keep-me"),
	)

	resp, err := s.UpdateEgressRule(context.Background(), &UpdateEgressRuleRequest{
		SandboxID: "sb-1",
		Rule:      ueRule("api_bearer", "fresh"),
	})
	if err != nil {
		t.Fatalf("UpdateEgressRule error=%v", err)
	}
	// Replace, not append: count stays 2 and order is preserved.
	if resp.RuleCount != 2 {
		t.Fatalf("RuleCount=%d, want 2 (replace-by-name, not append)", resp.RuleCount)
	}
	rules := ueLoadRules(t, s, "sb-1")
	if len(rules) != 2 {
		t.Fatalf("persisted %d rules, want 2", len(rules))
	}
	if rules[0].Name != "api_bearer" || rules[1].Name != "git_basic" {
		t.Fatalf("rule order changed: %q, %q", rules[0].Name, rules[1].Name)
	}
	if got := rules[0].Action.Inject[0].Secret; got != "fresh" {
		t.Fatalf("api_bearer secret=%q, want fresh (refreshed in place)", got)
	}
	if got := rules[1].Action.Inject[0].Secret; got != "keep-me" {
		t.Fatalf("git_basic secret=%q, want keep-me (untouched)", got)
	}
	if n := fake.puts[len(fake.puts)-1].rules; n != 2 {
		t.Fatalf("re-pushed %d rules, want the full set of 2", n)
	}
}

// TestUpdateEgressRulePersistsEvenWhenPushFails is the durability guarantee:
// the rule is saved to disk BEFORE the CubeEgress push, so a transient push
// failure still leaves the refreshed credential persisted for the retry loop.
func TestUpdateEgressRulePersistsEvenWhenPushFails(t *testing.T) {
	s, fake := ueService(t)
	fake.putErrs["192.168.0.10"] = []error{errors.New("connection refused")}
	ueSeed(s, "sb-1", "sb-1", "192.168.0.10")

	resp, err := s.UpdateEgressRule(context.Background(), &UpdateEgressRuleRequest{
		SandboxID: "sb-1",
		Rule:      ueRule("gitlab_token", "tok-A"),
	})
	if err != nil {
		t.Fatalf("a transient push failure must not fail the call, got err=%v", err)
	}
	if resp.Applied || !resp.Pending {
		t.Fatalf("want pending on a transient push failure, got applied=%v pending=%v", resp.Applied, resp.Pending)
	}
	rules := ueLoadRules(t, s, "sb-1")
	if len(rules) != 1 || rules[0].Action.Inject[0].Secret != "tok-A" {
		t.Fatalf("rule not persisted despite push failure: %+v", rules)
	}
}

func TestUpdateEgressRuleFindsStateByNetworkHandle(t *testing.T) {
	s, _ := ueService(t)
	ueSeed(s, "nh-1", "sb-1", "192.168.0.10")

	resp, err := s.UpdateEgressRule(context.Background(), &UpdateEgressRuleRequest{
		NetworkHandle: "nh-1",
		Rule:          ueRule("gitlab_token", "tok-A"),
	})
	if err != nil {
		t.Fatalf("UpdateEgressRule via network handle error=%v", err)
	}
	if resp.RuleCount != 1 {
		t.Fatalf("RuleCount=%d, want 1", resp.RuleCount)
	}
}

func TestUpdateEgressRuleValidation(t *testing.T) {
	s, _ := ueService(t)
	ueSeed(s, "sb-1", "sb-1", "192.168.0.10")

	cases := []struct {
		name string
		req  *UpdateEgressRuleRequest
	}{
		{"nil request", nil},
		{"nil rule", &UpdateEgressRuleRequest{SandboxID: "sb-1"}},
		{"empty rule name", &UpdateEgressRuleRequest{SandboxID: "sb-1", Rule: &EgressRule{}}},
		{"unknown sandbox", &UpdateEgressRuleRequest{SandboxID: "nope", Rule: ueRule("r", "v")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.UpdateEgressRule(context.Background(), tc.req); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestUpdateEgressRuleClonesCallerRule guards the aliasing hazard: the stored
// and persisted state must be independent of the caller's rule pointer, so a
// later mutation of it cannot silently diverge in-memory state from disk.
func TestUpdateEgressRuleClonesCallerRule(t *testing.T) {
	s, _ := ueService(t)
	ueSeed(s, "sb-1", "sb-1", "192.168.0.10")

	rule := ueRule("gitlab_token", "tok-A")
	if _, err := s.UpdateEgressRule(context.Background(), &UpdateEgressRuleRequest{
		SandboxID: "sb-1",
		Rule:      rule,
	}); err != nil {
		t.Fatalf("UpdateEgressRule error=%v", err)
	}

	// Mutate the caller's rule after the call returned.
	rule.Name = "renamed"
	rule.Action.Inject[0].Secret = "MUTATED"

	stored := s.states["sb-1"].CubeNetworkConfig.Rules[0]
	if stored.Name != "gitlab_token" || stored.Action.Inject[0].Secret != "tok-A" {
		t.Fatalf("in-memory state aliased the caller's rule: name=%q secret=%q",
			stored.Name, stored.Action.Inject[0].Secret)
	}
	persisted := ueLoadRules(t, s, "sb-1")
	if persisted[0].Name != "gitlab_token" || persisted[0].Action.Inject[0].Secret != "tok-A" {
		t.Fatalf("persisted state aliased the caller's rule: name=%q secret=%q",
			persisted[0].Name, persisted[0].Action.Inject[0].Secret)
	}
}
