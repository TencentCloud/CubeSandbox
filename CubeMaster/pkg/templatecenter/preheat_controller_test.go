// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

// ---------------------------------------------------------------------------
// matchNodeSelector
// ---------------------------------------------------------------------------

func TestMatchNodeSelector_EmptySelector(t *testing.T) {
	node := &nodemeta.NodeSnapshot{NodeID: "n1", Labels: map[string]string{"zone": "a"}}
	if !matchNodeSelector(node, nil) {
		t.Fatal("empty selector should match all nodes")
	}
	if !matchNodeSelector(node, map[string]string{}) {
		t.Fatal("empty map selector should match all nodes")
	}
}

func TestMatchNodeSelector_LabelMatch(t *testing.T) {
	node := &nodemeta.NodeSnapshot{NodeID: "n1", Labels: map[string]string{"zone": "a", "role": "compute"}}
	tests := []struct {
		name    string
		selector map[string]string
		want    bool
	}{
		{"single match", map[string]string{"zone": "a"}, true},
		{"multi match", map[string]string{"zone": "a", "role": "compute"}, true},
		{"mismatch value", map[string]string{"zone": "b"}, false},
		{"missing key", map[string]string{"pool": "x"}, false},
		{"partial match", map[string]string{"zone": "a", "pool": "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchNodeSelector(node, tt.selector); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchNodeSelector_NilLabels(t *testing.T) {
	node := &nodemeta.NodeSnapshot{NodeID: "n1", Labels: nil}
	if matchNodeSelector(node, map[string]string{"zone": "a"}) {
		t.Fatal("non-empty selector should not match node with nil labels")
	}
	if !matchNodeSelector(node, nil) {
		t.Fatal("empty selector should match even nil-label node")
	}
}

// ---------------------------------------------------------------------------
// filterNodesForTemplate
// ---------------------------------------------------------------------------

func TestFilterNodesForTemplate_InstanceTypeFilter(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1", InstanceType: "cubebox", Healthy: true},
		{NodeID: "n2", InstanceType: "gpu", Healthy: true},
		{NodeID: "n3", InstanceType: "cubebox", Healthy: true},
	}
	out := filterNodesForTemplate(nodes, nil, "cubebox")
	if len(out) != 2 {
		t.Fatalf("expected 2 matching nodes, got %d", len(out))
	}
	for _, n := range out {
		if n.InstanceType != "cubebox" {
			t.Fatalf("filtered node %s has wrong instance type %s", n.NodeID, n.InstanceType)
		}
	}
}

func TestFilterNodesForTemplate_SelectorPlusInstanceType(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1", InstanceType: "cubebox", Labels: map[string]string{"zone": "a"}, Healthy: true},
		{NodeID: "n2", InstanceType: "cubebox", Labels: map[string]string{"zone": "b"}, Healthy: true},
		{NodeID: "n3", InstanceType: "gpu", Labels: map[string]string{"zone": "a"}, Healthy: true},
	}
	out := filterNodesForTemplate(nodes, map[string]string{"zone": "a"}, "cubebox")
	if len(out) != 1 || out[0].NodeID != "n1" {
		t.Fatalf("expected only n1 matching, got %v", out)
	}
}

func TestFilterNodesForTemplate_EmptySelectorAllOfType(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1", InstanceType: "cubebox", Labels: map[string]string{"x": "1"}, Healthy: true},
		{NodeID: "n2", InstanceType: "cubebox", Labels: nil, Healthy: true},
		{NodeID: "n3", InstanceType: "gpu", Healthy: true},
	}
	out := filterNodesForTemplate(nodes, nil, "cubebox")
	if len(out) != 2 {
		t.Fatalf("empty selector should match all cubebox nodes, got %d", len(out))
	}
}

func TestFilterNodesForTemplate_NilNodes(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{nil, {NodeID: "n1", InstanceType: "cubebox"}}
	out := filterNodesForTemplate(nodes, nil, "cubebox")
	if len(out) != 1 {
		t.Fatalf("nil nodes should be skipped, got %d", len(out))
	}
}

// ---------------------------------------------------------------------------
// selectCandidates
// ---------------------------------------------------------------------------

func TestSelectCandidates_BudgetTemplates(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1"},
		{NodeID: "n2"},
		{NodeID: "n3"},
	}
	counts := map[string]int{"n1": 19, "n2": 20, "n3": 0}
	bytes := map[string]int64{"n1": 0, "n2": 0, "n3": 0}
	cfg := &config.TemplatePreheatConf{PerNodeMaxTemplates: 20, PerNodeMaxBytes: 100}

	out := selectCandidates(nodes, map[string]struct{}{}, 3, counts, bytes, cfg, 10)
	// n2 has 20 templates already (at cap), n1 has 19 (can fit 1), n3 has 0
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates (n1, n3), got %d: %v", len(out), out)
	}
	ids := []string{out[0].NodeID, out[1].NodeID}
	if ids[0] != "n1" || ids[1] != "n3" {
		t.Fatalf("expected n1,n3, got %v", ids)
	}
}

func TestSelectCandidates_BudgetBytes(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1"},
		{NodeID: "n2"},
	}
	counts := map[string]int{"n1": 0, "n2": 0}
	bytes := map[string]int64{"n1": 90, "n2": 50}
	cfg := &config.TemplatePreheatConf{PerNodeMaxTemplates: 100, PerNodeMaxBytes: 100}

	// templateSize=20: n1 (90+20=110>100) excluded, n2 (50+20=70<=100) included
	out := selectCandidates(nodes, map[string]struct{}{}, 2, counts, bytes, cfg, 20)
	if len(out) != 1 || out[0].NodeID != "n2" {
		t.Fatalf("expected only n2, got %v", out)
	}
}

func TestSelectCandidates_ExistingReadyExcluded(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1"},
		{NodeID: "n2"},
	}
	existing := map[string]struct{}{"n1": {}}
	counts := map[string]int{"n1": 0, "n2": 0}
	bytes := map[string]int64{"n1": 0, "n2": 0}
	cfg := &config.TemplatePreheatConf{PerNodeMaxTemplates: 20, PerNodeMaxBytes: 100}

	out := selectCandidates(nodes, existing, 2, counts, bytes, cfg, 10)
	if len(out) != 1 || out[0].NodeID != "n2" {
		t.Fatalf("expected only n2 (n1 has existing replica), got %v", out)
	}
}

func TestSelectCandidates_AdmitCountCap(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1"}, {NodeID: "n2"}, {NodeID: "n3"}, {NodeID: "n4"},
	}
	counts := map[string]int{}
	bytes := map[string]int64{}
	cfg := &config.TemplatePreheatConf{PerNodeMaxTemplates: 20, PerNodeMaxBytes: 100}

	out := selectCandidates(nodes, map[string]struct{}{}, 2, counts, bytes, cfg, 10)
	if len(out) != 2 {
		t.Fatalf("admitCount=2 should cap at 2, got %d", len(out))
	}
}

func TestSelectCandidates_OptimisticIncrement(t *testing.T) {
	// Two templates evaluated in the same pass.
	// After first template picks n1, the second should see n1's count incremented.
	nodes := []*nodemeta.NodeSnapshot{{NodeID: "n1"}, {NodeID: "n2"}}
	counts := map[string]int{"n1": 0, "n2": 0}
	bytes := map[string]int64{"n1": 0, "n2": 0}
	cfg := &config.TemplatePreheatConf{PerNodeMaxTemplates: 1, PerNodeMaxBytes: 1000}

	// Template A picks n1
	out1 := selectCandidates(nodes, map[string]struct{}{}, 1, counts, bytes, cfg, 50)
	if len(out1) != 1 || out1[0].NodeID != "n1" {
		t.Fatalf("expected n1 for first template, got %v", out1)
	}
	// n1's count is now 1 (at cap). Template B should pick n2.
	out2 := selectCandidates(nodes, map[string]struct{}{}, 1, counts, bytes, cfg, 50)
	if len(out2) != 1 || out2[0].NodeID != "n2" {
		t.Fatalf("expected n2 for second template (n1 at cap), got %v", out2)
	}
}

// ---------------------------------------------------------------------------
// cooldownElapsed
// ---------------------------------------------------------------------------

func TestCooldownElapsed_NeverSubmitted(t *testing.T) {
	lastRedoSubmitByTemplate = make(map[string]time.Time)
	if !cooldownElapsed("tpl-1", 10*time.Minute) {
		t.Fatal("never-submitted template should pass cooldown")
	}
}

func TestCooldownElapsed_WithinCooldown(t *testing.T) {
	lastRedoSubmitByTemplate = map[string]time.Time{
		"tpl-1": time.Now(),
	}
	if cooldownElapsed("tpl-1", 10*time.Minute) {
		t.Fatal("template submitted just now should not pass cooldown")
	}
}

func TestCooldownElapsed_PastCooldown(t *testing.T) {
	lastRedoSubmitByTemplate = map[string]time.Time{
		"tpl-1": time.Now().Add(-15 * time.Minute),
	}
	if !cooldownElapsed("tpl-1", 10*time.Minute) {
		t.Fatal("template submitted 15m ago with 10m cooldown should pass")
	}
}

func TestCooldownElapsed_ZeroCooldown(t *testing.T) {
	lastRedoSubmitByTemplate = map[string]time.Time{
		"tpl-1": time.Now(),
	}
	if !cooldownElapsed("tpl-1", 0) {
		t.Fatal("zero cooldown should always pass")
	}
}

// ---------------------------------------------------------------------------
// sortPinnedTemplates
// ---------------------------------------------------------------------------

func TestSortPinnedTemplates_ByPriority(t *testing.T) {
	pinned := []config.PinnedTemplateConf{
		{TemplateID: "low", Priority: 10},
		{TemplateID: "high", Priority: 100},
		{TemplateID: "mid", Priority: 50},
	}
	out := sortPinnedTemplates(pinned)
	if out[0].TemplateID != "high" || out[1].TemplateID != "mid" || out[2].TemplateID != "low" {
		t.Fatalf("expected high,mid,low ordering, got %s,%s,%s",
			out[0].TemplateID, out[1].TemplateID, out[2].TemplateID)
	}
}

func TestSortPinnedTemplates_TieBreakOnID(t *testing.T) {
	pinned := []config.PinnedTemplateConf{
		{TemplateID: "zzz", Priority: 50},
		{TemplateID: "aaa", Priority: 50},
	}
	out := sortPinnedTemplates(pinned)
	if out[0].TemplateID != "aaa" {
		t.Fatalf("tie-break should sort by template_id asc, got %s first", out[0].TemplateID)
	}
}

func TestSortPinnedTemplates_DoesNotMutateInput(t *testing.T) {
	pinned := []config.PinnedTemplateConf{
		{TemplateID: "low", Priority: 10},
		{TemplateID: "high", Priority: 100},
	}
	_ = sortPinnedTemplates(pinned)
	if pinned[0].TemplateID != "low" {
		t.Fatal("sortPinnedTemplates should not mutate the input slice")
	}
}

// ---------------------------------------------------------------------------
// filterHealthyNodes
// ---------------------------------------------------------------------------

func TestFilterHealthyNodes(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1", Healthy: true},
		{NodeID: "n2", Healthy: false},
		{NodeID: "n3", Healthy: true},
		nil,
	}
	out := filterHealthyNodes(nodes)
	if len(out) != 2 {
		t.Fatalf("expected 2 healthy nodes, got %d", len(out))
	}
}

// ---------------------------------------------------------------------------
// signalPreheatReconcile
// ---------------------------------------------------------------------------

func TestSignalPreheatReconcile_NonBlocking(t *testing.T) {
	// Drain any pending trigger from other tests
	select {
	case <-preheatTriggerCh:
	default:
	}

	signalPreheatReconcile()
	signalPreheatReconcile() // should not block — coalesced
	signalPreheatReconcile()

	select {
	case <-preheatTriggerCh:
		// good — one trigger pending
	default:
		t.Fatal("expected at least one trigger in channel")
	}

	// Channel should have at most 1 (cap=1)
	select {
	case <-preheatTriggerCh:
		t.Fatal("expected channel to be empty after one read (cap=1)")
	default:
		// good
	}
}

// ---------------------------------------------------------------------------
// nodeSnapshotIDSet
// ---------------------------------------------------------------------------

func TestNodeSnapshotIDSet(t *testing.T) {
	nodes := []*nodemeta.NodeSnapshot{
		{NodeID: "n1"},
		{NodeID: "n2"},
		nil,
	}
	set := nodeSnapshotIDSet(nodes)
	if len(set) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(set))
	}
	if _, ok := set["n1"]; !ok {
		t.Fatal("n1 missing from set")
	}
	if _, ok := set["n2"]; !ok {
		t.Fatal("n2 missing from set")
	}
}
