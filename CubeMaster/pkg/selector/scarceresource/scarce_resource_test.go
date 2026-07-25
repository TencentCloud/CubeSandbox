// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scarceresource

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func patchScarceResources(t *testing.T, resources []config.ScarceResourceDef) {
	t.Helper()
	restore := SetEffectiveResourcesFnForTest(func() []config.ScarceResourceDef {
		return resources
	})
	t.Cleanup(restore)
}

func gpuNode(id string) *node.Node {
	return &node.Node{
		InsID:      id,
		IP:         "10.0.0." + id,
		NodeLabels: map[string]string{"gpu": "true"},
	}
}

func cpuNode(id string) *node.Node {
	return &node.Node{
		InsID: id,
		IP:    "10.0.0." + id,
	}
}

func selCtxWithGPURequired(t *testing.T) *selctx.SelectorCtx {
	t.Helper()
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.Affinity.NodeSelector = gpuExistsSelector(t)
	return ctx
}

func gpuExistsSelector(t *testing.T) affinity.NodeSelector {
	t.Helper()
	ns, err := affinity.NewNodeSelector([]affinity.NodeSelectorTerm{{
		MatchExpressions: []affinity.NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: affinity.NodeSelectorOpExists,
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	return ns
}

func TestFilterNodesUsesExplicitRequestSelector(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	// Backoff path: NodeSelector unset; GPU demand comes from BackoffNodeSelector only.
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", gpuExistsSelector(t))
	if got.Len() != 2 {
		t.Fatalf("expected both nodes when backoff selector requests gpu, got %d", got.Len())
	}
}

func TestFilterNodesNilNodeSelectorWithoutExplicitRequest(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 1 {
		t.Fatalf("expected cpu node only when no selector declares gpu, got %d", got.Len())
	}
}

func TestFilterNodesDisabled(t *testing.T) {
	patchScarceResources(t, nil)
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 2 {
		t.Fatalf("expected 2 nodes, got %d", got.Len())
	}
}

func TestFilterNodesExcludesGPUWhenNotRequested(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	in := node.NodeList{gpuNode("1"), cpuNode("2"), cpuNode("3")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 2 {
		t.Fatalf("expected 2 cpu nodes, got %d", got.Len())
	}
	for _, n := range got {
		if _, ok := n.NodeLabels["gpu"]; ok {
			t.Fatalf("gpu node should be filtered out: %s", n.ID())
		}
	}
}

func TestFilterNodesKeepsGPUWhenRequested(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ctx := selCtxWithGPURequired(t)
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 2 {
		t.Fatalf("expected both nodes when gpu is requested, got %d", got.Len())
	}
}

func TestFilterNodesKeepsGPUWhenClusterIDAnded(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ns, err := affinity.NewNodeSelector([]affinity.NodeSelectorTerm{{
		MatchExpressions: []affinity.NodeSelectorRequirement{
			{
				Key:      constants.AffinityKeyClusterID,
				Operator: affinity.NodeSelectorOpIn,
				Values:   map[string]any{"cluster-a": struct{}{}},
			},
			{
				Key:      "gpu",
				Operator: affinity.NodeSelectorOpExists,
			},
		},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.Affinity.NodeSelector = ns
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 2 {
		t.Fatalf("expected both nodes when gpu Exists is ANDed with cluster-id, got %d", got.Len())
	}
}

func TestFilterNodesKeepsGPUWhenInMatchesSecondScarceValue(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"a100", "h100"},
	}})
	ns, err := affinity.NewNodeSelector([]affinity.NodeSelectorTerm{{
		MatchExpressions: []affinity.NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: affinity.NodeSelectorOpIn,
			Values:   map[string]any{"h100": struct{}{}},
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	ctx.Affinity.NodeSelector = ns
	in := node.NodeList{gpuNode("1"), cpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 2 {
		t.Fatalf("expected both nodes when gpu In matches scarce value, got %d", got.Len())
	}
}

func TestFilterNodesAllGPUFailsClosed(t *testing.T) {
	patchScarceResources(t, []config.ScarceResourceDef{{
		LabelKey: "gpu", LabelValues: []string{"true"},
	}})
	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	in := node.NodeList{gpuNode("1"), gpuNode("2")}
	got := FilterNodes(ctx, in, "test", nil)
	if got.Len() != 0 {
		t.Fatalf("expected empty node list, got %d", got.Len())
	}
}

func TestNodeLabelMatchesScarce(t *testing.T) {
	def := config.ScarceResourceDef{LabelKey: "gpu", LabelValues: []string{"true"}}
	if !nodeLabelMatchesScarce(map[string]string{"gpu": "true"}, def) {
		t.Fatal("expected gpu=true to match")
	}
	if nodeLabelMatchesScarce(map[string]string{"gpu": "false"}, def) {
		t.Fatal("gpu=false should not match")
	}
	if nodeLabelMatchesScarce(map[string]string{}, def) {
		t.Fatal("missing label should not match")
	}
}

func TestNodeLabelMatchesScarceEmptyLabelValuesHeuristic(t *testing.T) {
	def := config.ScarceResourceDef{LabelKey: "gpu"}
	if !nodeLabelMatchesScarce(map[string]string{"gpu": "true"}, def) {
		t.Fatal("expected any truthy value when label_values empty")
	}
	if nodeLabelMatchesScarce(map[string]string{"gpu": "false"}, def) {
		t.Fatal("gpu=false should not match under empty label_values heuristic")
	}
}

func TestNodeLabelMatchesScarceEmptyLabelValuesNonGPU(t *testing.T) {
	def := config.ScarceResourceDef{LabelKey: "spot"}
	if !nodeLabelMatchesScarce(map[string]string{"spot": "0"}, def) {
		t.Fatal("non-gpu empty label_values should treat 0 as scarce")
	}
	if !nodeLabelMatchesScarce(map[string]string{"spot": "true"}, def) {
		t.Fatal("expected spot=true to match under empty label_values")
	}
}

func TestNodeLabelMatchesScarceExplicitFalseValue(t *testing.T) {
	def := config.ScarceResourceDef{LabelKey: "tier", LabelValues: []string{"false"}}
	if !nodeLabelMatchesScarce(map[string]string{"tier": "false"}, def) {
		t.Fatal("explicit label_values should use exact match, including false")
	}
}

func TestEffectiveResourcesDefaultGPU(t *testing.T) {
	conf := &config.ScarceResourceFilterConf{Enable: true}
	got := conf.EffectiveResources()
	if len(got) != 1 || got[0].LabelKey != "gpu" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}
