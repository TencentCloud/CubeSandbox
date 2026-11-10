// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package autoscale

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)

func serving(name string, usedMilli int64, mvm int64) PoolNode {
	return PoolNode{Name: name, State: StateServing, HasPlaceholder: true, PlaceholderName: "cube-capacity-" + name,
		InKubernetes: true, InCube: true, CubeHealthy: true,
		QuotaMilliCPU: 16000, QuotaMemoryMB: 32000, UsedMilliCPU: usedMilli, UsedMemoryMB: usedMilli * 2, MvmNum: mvm}
}

func bound(nodes ...PoolNode) []Placeholder {
	var out []Placeholder
	for _, n := range nodes {
		if n.HasPlaceholder {
			out = append(out, Placeholder{Name: n.PlaceholderName, NodeName: n.Name, CreatedAt: t0.Add(-time.Hour)})
		}
	}
	return out
}

func sizing() Sizing {
	return Sizing{MinNodes: 1, MaxNodes: 5, TargetUtilization: 0.75}
}

func kinds(p Plan) []ActionKind {
	var out []ActionKind
	for _, a := range p.Actions {
		out = append(out, a.Kind)
	}
	return out
}

func TestComputeDemandSizesForTargetUtilization(t *testing.T) {
	nodes := []PoolNode{serving("a", 12000, 6), serving("b", 12000, 6)}
	d := ComputeDemand(nodes, sizing())
	// 24000m used / (16000m * 0.75) = 2 nodes; memory is equally bound.
	assert.Equal(t, 2, d.DesiredNodes)
	assert.Equal(t, int64(16000), d.NodeMilliCPU)
}

func TestComputeDemandCountsDrainingUsageAndClamps(t *testing.T) {
	dr := serving("c", 8000, 4)
	dr.State = StateDraining
	nodes := []PoolNode{serving("a", 16000, 8), serving("b", 16000, 8), dr}
	d := ComputeDemand(nodes, Sizing{MinNodes: 1, MaxNodes: 3, TargetUtilization: 0.75})
	// 40000m / 12000m = 4 -> clamped to max 3.
	assert.Equal(t, 3, d.DesiredNodes)
}

func TestDecideAdoptsEveryExistingNodeBeforeScalingIn(t *testing.T) {
	// Enabling on a running pool: no placeholders yet. Each serving node must
	// be pinned first; nothing is isolated in the same pass.
	nodes := []PoolNode{serving("a", 0, 0), serving("b", 0, 0), serving("c", 0, 0)}
	for i := range nodes {
		nodes[i].HasPlaceholder, nodes[i].PlaceholderName = false, ""
	}
	p := Decide(Input{Now: t0, Nodes: nodes, Sizing: sizing(), ScaleInCooldown: time.Minute})
	require.Equal(t, []ActionKind{ActCreatePlaceholders}, kinds(p))
	assert.Equal(t, 3, p.Actions[0].Count)
}

func TestDecideRequestsCapacityWhenDemandExceedsPool(t *testing.T) {
	nodes := []PoolNode{serving("a", 15000, 8), serving("b", 15000, 8)}
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing()})
	require.Equal(t, []ActionKind{ActCreatePlaceholders}, kinds(p))
	assert.Equal(t, 1, p.Actions[0].Count) // 30000/12000 -> 3 nodes
}

func TestDecideDoesNotRequestTwiceWhileNodeIsStarting(t *testing.T) {
	nodes := []PoolNode{serving("a", 15000, 8), serving("b", 15000, 8),
		{Name: "c", State: StateStarting, InKubernetes: true, HasPlaceholder: true, PlaceholderName: "cube-capacity-c"}}
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing()})
	assert.Empty(t, p.Actions)
}

func TestDecideWithdrawsNewestPendingRequestWhenDemandFalls(t *testing.T) {
	nodes := []PoolNode{serving("a", 1000, 1), serving("b", 1000, 1)}
	phs := append(bound(nodes...),
		Placeholder{Name: "cube-capacity-old", CreatedAt: t0.Add(-10 * time.Minute)},
		Placeholder{Name: "cube-capacity-new", CreatedAt: t0.Add(-time.Minute)})
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: phs, Sizing: sizing()})
	require.Equal(t, []ActionKind{ActDeletePending, ActDeletePending}, kinds(p))
	assert.Equal(t, "cube-capacity-new", p.Actions[0].Pod)
}

func TestDecideScaleInWaitsForCooldownThenIsolatesEmptiestNode(t *testing.T) {
	nodes := []PoolNode{serving("a", 2000, 2), serving("b", 1000, 1), serving("c", 3000, 3)}
	in := Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing(), ScaleInCooldown: 15 * time.Minute}

	p := Decide(in)
	assert.Empty(t, p.Actions, "first observation only starts the cooldown")
	assert.Equal(t, t0, p.BelowSince)

	in.Now, in.BelowSince = t0.Add(10*time.Minute), p.BelowSince
	assert.Empty(t, Decide(in).Actions)

	in.Now = t0.Add(16 * time.Minute)
	p = Decide(in)
	require.Equal(t, []ActionKind{ActIsolate}, kinds(p))
	assert.Equal(t, "b", p.Actions[0].Node)
}

func TestDecideCooldownResetsWhenDemandReturns(t *testing.T) {
	nodes := []PoolNode{serving("a", 11000, 6), serving("b", 11000, 6)}
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing(),
		BelowSince: t0.Add(-time.Hour), ScaleInCooldown: time.Minute})
	assert.Empty(t, p.Actions)
	assert.True(t, p.BelowSince.IsZero())
}

func TestDecideNeverScalesBelowMinimum(t *testing.T) {
	nodes := []PoolNode{serving("a", 0, 0), serving("b", 0, 0)}
	s := sizing()
	s.MinNodes = 2
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: s,
		BelowSince: t0.Add(-time.Hour), ScaleInCooldown: time.Minute})
	assert.Empty(t, p.Actions)
}

func TestDecideDrainingNodeIsCheckedOneAtATime(t *testing.T) {
	dr := serving("b", 500, 1)
	dr.State, dr.IsolatedSince, dr.OwnedIsolation = StateDraining, t0.Add(-5*time.Minute), true
	nodes := []PoolNode{serving("a", 500, 1), dr, serving("c", 0, 0)}
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing(),
		BelowSince: t0.Add(-time.Hour), ScaleInCooldown: time.Minute, DrainTimeout: time.Hour})
	require.Equal(t, []ActionKind{ActCheckDrained}, kinds(p), "no second isolation while one drains")
	assert.Equal(t, "cube-capacity-b", p.Actions[0].Pod)
}

func TestDecideCancelsDrainOnTimeoutOrRisingDemand(t *testing.T) {
	dr := serving("b", 4000, 2)
	dr.State, dr.IsolatedSince = StateDraining, t0.Add(-2*time.Hour)
	nodes := []PoolNode{serving("a", 4000, 2), dr}
	p := Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing(), DrainTimeout: time.Hour})
	require.Equal(t, []ActionKind{ActCancelDrain}, kinds(p))

	dr.IsolatedSince = t0.Add(-time.Minute)
	nodes = []PoolNode{serving("a", 12000, 6), dr} // demand needs 2 serving nodes
	p = Decide(Input{Now: t0, Nodes: nodes, Placeholders: bound(nodes...), Sizing: sizing(), DrainTimeout: time.Hour})
	require.Equal(t, []ActionKind{ActCancelDrain}, kinds(p))
	assert.Contains(t, p.Actions[0].Reason, "demand rose")
}

func TestDecideReleasedNodeGetsNoPlaceholderAndIsCleanedUpOnceGone(t *testing.T) {
	a := serving("a", 0, 0)
	rel := PoolNode{Name: "b", State: StateReleased, InKubernetes: true, InCube: true, CubeHealthy: true, OwnedIsolation: true}
	p := Decide(Input{Now: t0, Nodes: []PoolNode{a, rel}, Placeholders: bound(a), Sizing: sizing()})
	assert.Empty(t, p.Actions, "released node is not re-pinned; machine still present")

	rel.InKubernetes, rel.CubeHealthy = false, false
	p = Decide(Input{Now: t0, Nodes: []PoolNode{a, rel}, Placeholders: bound(a), Sizing: sizing()})
	require.Equal(t, []ActionKind{ActDeleteCubeNode}, kinds(p))
	assert.Equal(t, "b", p.Actions[0].Node)
}

func TestDecideKeepsOperatorIsolatedNodePinned(t *testing.T) {
	a := serving("a", 0, 0)
	f := PoolNode{Name: "b", State: StateForeign, InKubernetes: true, InCube: true, CubeHealthy: true}
	p := Decide(Input{Now: t0, Nodes: []PoolNode{a, f}, Placeholders: bound(a), Sizing: sizing(),
		BelowSince: t0.Add(-time.Hour), ScaleInCooldown: time.Minute})
	require.Equal(t, []ActionKind{ActCreatePlaceholders}, kinds(p))
	assert.Equal(t, 1, p.Actions[0].Count)
}
