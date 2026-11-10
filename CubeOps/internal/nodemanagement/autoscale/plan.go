// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package autoscale sizes the compute-node pool through the Kubernetes Cluster
// Autoscaler (issue #2193).
//
// CubeOps decides how many nodes to keep and which node to retire; the Cluster
// Autoscaler adds and removes machines. The handshake is a placeholder Pod:
//
//   - every node CubeOps wants to keep carries one placeholder Pod, so the
//     Cluster Autoscaler never sees it as empty (Cube MicroVMs are not Pods);
//   - a Pending placeholder asks the Cluster Autoscaler for one more node;
//   - a placeholder is deleted only after its node was isolated in Cube and
//     holds no running or paused sandbox, so the machine the Cluster
//     Autoscaler removes is exactly the drained one;
//   - once the machine is gone, the Cube node metadata is deleted so lookups
//     of sandboxes that lived there return "not found" instead of an error.
//
// All state is derived on every pass from Cube node metadata, the Cube node
// operation log, and the placeholder Pods, so a restart resumes where the
// previous leader stopped.
package autoscale

import (
	"math"
	"sort"
	"time"
)

// Operator is recorded on every Cube operation the autoscaler performs; an
// isolation is owned by the autoscaler only when its latest isolate operation
// carries this operator.
const Operator = "cubeops-autoscaler"

// NodeState classifies one pool node for planning.
type NodeState string

const (
	// StateServing: registered in Cube, healthy, schedulable.
	StateServing NodeState = "serving"
	// StateStarting: in the Kubernetes pool but not yet serving in Cube
	// (not registered, unhealthy, or not ready).
	StateStarting NodeState = "starting"
	// StateDraining: isolated by the autoscaler, placeholder still present.
	StateDraining NodeState = "draining"
	// StateReleased: isolated by the autoscaler, placeholder removed; waiting
	// for the Cluster Autoscaler to delete the machine.
	StateReleased NodeState = "released"
	// StateForeign: isolated by someone else; never touched or counted.
	StateForeign NodeState = "foreign"
)

// PoolNode is the merged Kubernetes + Cube view of one pool node.
type PoolNode struct {
	Name            string
	State           NodeState
	HasPlaceholder  bool
	QuotaMilliCPU   int64
	QuotaMemoryMB   int64
	UsedMilliCPU    int64
	UsedMemoryMB    int64
	MvmNum          int64
	IsolatedSince   time.Time // set for draining/released
	InKubernetes    bool
	InCube          bool
	CubeHealthy     bool
	CubeIsolated    bool
	OwnedIsolation  bool
	PlaceholderName string
}

// Sizing holds the knobs the plan needs.
type Sizing struct {
	MinNodes          int
	MaxNodes          int
	TargetUtilization float64
	HeadroomMilliCPU  int64
	HeadroomMemoryMB  int64
}

// Demand summarises usage across the pool.
type Demand struct {
	UsedMilliCPU    int64
	UsedMemoryMB    int64
	NodeMilliCPU    int64 // per-node scheduler quota used for sizing
	NodeMemoryMB    int64
	ServingNodes    int
	DesiredNodes    int
	CPUBoundNodes   int
	MemoryBoundNode int
}

// ComputeDemand returns the node count the pool should have. Usage on every
// node that still holds sandboxes (serving or draining) counts as demand;
// only serving nodes define per-node capacity.
func ComputeDemand(nodes []PoolNode, s Sizing) Demand {
	var d Demand
	var cpuQuotas, memQuotas []int64
	for _, n := range nodes {
		switch n.State {
		case StateServing:
			d.ServingNodes++
			cpuQuotas = append(cpuQuotas, n.QuotaMilliCPU)
			memQuotas = append(memQuotas, n.QuotaMemoryMB)
			d.UsedMilliCPU += n.UsedMilliCPU
			d.UsedMemoryMB += n.UsedMemoryMB
		case StateDraining:
			d.UsedMilliCPU += n.UsedMilliCPU
			d.UsedMemoryMB += n.UsedMemoryMB
		}
	}
	d.NodeMilliCPU = median(cpuQuotas)
	d.NodeMemoryMB = median(memQuotas)

	target := s.TargetUtilization
	if target <= 0 || target > 1 {
		target = 1
	}
	d.CPUBoundNodes = nodesFor(d.UsedMilliCPU+s.HeadroomMilliCPU, d.NodeMilliCPU, target)
	d.MemoryBoundNode = nodesFor(d.UsedMemoryMB+s.HeadroomMemoryMB, d.NodeMemoryMB, target)
	want := d.CPUBoundNodes
	if d.MemoryBoundNode > want {
		want = d.MemoryBoundNode
	}
	if d.NodeMilliCPU == 0 && d.NodeMemoryMB == 0 {
		// No serving node to size from: hold the current footprint.
		want = d.ServingNodes
	}
	d.DesiredNodes = clamp(want, s.MinNodes, s.MaxNodes)
	return d
}

func nodesFor(need, perNode int64, target float64) int {
	if need <= 0 || perNode <= 0 {
		return 0
	}
	return int(math.Ceil(float64(need) / (float64(perNode) * target)))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		v = lo
	}
	if hi > 0 && v > hi {
		v = hi
	}
	return v
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// Placeholder is the planner's view of one placeholder Pod.
type Placeholder struct {
	Name      string
	NodeName  string // empty while Pending
	CreatedAt time.Time
}

// Action is one step the reconciler executes. At most one Cube mutation
// (isolate / unisolate / release / delete) is planned per pass.
type Action struct {
	Kind   ActionKind
	Node   string
	Pod    string
	Count  int
	Reason string
}

// ActionKind enumerates reconciler steps.
type ActionKind string

const (
	ActCreatePlaceholders ActionKind = "create-placeholders"
	ActDeletePending      ActionKind = "delete-pending-placeholder"
	ActIsolate            ActionKind = "isolate"
	ActCancelDrain        ActionKind = "cancel-drain"
	ActCheckDrained       ActionKind = "check-drained"
	ActDeleteCubeNode     ActionKind = "delete-cube-node"
)

// Input is everything one planning pass needs.
type Input struct {
	Now          time.Time
	Nodes        []PoolNode
	Placeholders []Placeholder
	Sizing       Sizing
	// BelowSince is when demand first dropped below the serving pool; zero if
	// it is not below now. The reconciler keeps it across passes.
	BelowSince       time.Time
	ScaleInCooldown  time.Duration
	DrainTimeout     time.Duration
	ProvisionTimeout time.Duration
}

// Plan is the decision for one pass.
type Plan struct {
	Demand     Demand
	BelowSince time.Time
	Actions    []Action
	// Warnings are surfaced in logs and the status endpoint.
	Warnings []string
}

// Decide plans one pass. It never removes a placeholder from a node that has
// not been drained, and never isolates more than one node at a time.
func Decide(in Input) Plan {
	p := Plan{Demand: ComputeDemand(in.Nodes, in.Sizing)}
	desired := p.Demand.DesiredNodes

	var serving, starting, draining, foreign []PoolNode
	for _, n := range in.Nodes {
		switch n.State {
		case StateServing:
			serving = append(serving, n)
		case StateStarting:
			starting = append(starting, n)
		case StateDraining:
			draining = append(draining, n)
		case StateForeign:
			// Isolated by an operator: keep it pinned, never retire it.
			foreign = append(foreign, n)
		case StateReleased:
			// Machine gone from Kubernetes: drop the Cube metadata so
			// sandbox lookups resolve to "not found". The node was verified
			// empty before its placeholder was released.
			if !n.InKubernetes && n.InCube && !n.CubeHealthy {
				p.Actions = append(p.Actions, Action{Kind: ActDeleteCubeNode, Node: n.Name, Reason: "machine removed after drain"})
			}
		}
	}

	// Every node that should keep running needs a placeholder; extra
	// placeholders ask for new nodes. Only released nodes get none.
	keep := len(serving) + len(starting) + len(draining) + len(foreign)
	pending := 0
	for _, ph := range in.Placeholders {
		if ph.NodeName == "" {
			pending++
		}
	}
	bound := len(in.Placeholders) - pending
	want := keep
	if desired > want {
		want = desired // desired is already clamped to MaxNodes
	}

	switch have := bound + pending; {
	case have < want:
		p.Actions = append(p.Actions, Action{Kind: ActCreatePlaceholders, Count: want - have,
			Reason: "keep every serving node and request capacity"})
	case have > want && pending > 0:
		// Demand fell before the new node arrived: withdraw requests only.
		extra := have - want
		for _, ph := range newestFirst(in.Placeholders) {
			if extra == 0 {
				break
			}
			if ph.NodeName == "" {
				p.Actions = append(p.Actions, Action{Kind: ActDeletePending, Pod: ph.Name, Reason: "request no longer needed"})
				extra--
			}
		}
	}
	for _, ph := range in.Placeholders {
		if ph.NodeName == "" && in.ProvisionTimeout > 0 && in.Now.Sub(ph.CreatedAt) > in.ProvisionTimeout {
			p.Warnings = append(p.Warnings, "placeholder "+ph.Name+" pending beyond provision timeout; check the node pool maximum and Cluster Autoscaler events")
		}
	}
	for _, n := range starting {
		if n.HasPlaceholder && !n.InCube {
			p.Warnings = append(p.Warnings, "node "+n.Name+" has not registered with Cube")
		}
	}

	// Drain bookkeeping: one node at a time.
	if len(draining) > 0 {
		n := draining[0]
		switch {
		case len(serving)+len(starting) < desired:
			p.Actions = append(p.Actions, Action{Kind: ActCancelDrain, Node: n.Name, Reason: "demand rose during drain"})
		case in.DrainTimeout > 0 && !n.IsolatedSince.IsZero() && in.Now.Sub(n.IsolatedSince) > in.DrainTimeout:
			p.Actions = append(p.Actions, Action{Kind: ActCancelDrain, Node: n.Name, Reason: "drain timeout; sandboxes are never stopped"})
		default:
			p.Actions = append(p.Actions, Action{Kind: ActCheckDrained, Node: n.Name, Pod: n.PlaceholderName})
		}
		p.BelowSince = time.Time{}
		return p
	}

	// Scale-in: demand below the serving pool for the whole cooldown, no
	// node still starting, and nothing pending.
	if len(serving) > desired && len(starting) == 0 && pending == 0 {
		p.BelowSince = in.BelowSince
		if p.BelowSince.IsZero() {
			p.BelowSince = in.Now
		}
		if in.Now.Sub(p.BelowSince) >= in.ScaleInCooldown {
			victim := pickVictim(serving)
			p.Actions = append(p.Actions, Action{Kind: ActIsolate, Node: victim.Name, Pod: victim.PlaceholderName,
				Reason: "demand below pool for the scale-in cooldown"})
			p.BelowSince = time.Time{}
		}
	} else {
		p.BelowSince = time.Time{}
	}
	return p
}

// pickVictim prefers the node with the fewest MicroVMs, then the least used
// quota, then the name (stable).
func pickVictim(nodes []PoolNode) PoolNode {
	c := append([]PoolNode(nil), nodes...)
	sort.Slice(c, func(i, j int) bool {
		if c[i].MvmNum != c[j].MvmNum {
			return c[i].MvmNum < c[j].MvmNum
		}
		if c[i].UsedMilliCPU != c[j].UsedMilliCPU {
			return c[i].UsedMilliCPU < c[j].UsedMilliCPU
		}
		return c[i].Name < c[j].Name
	})
	return c[0]
}

// newestFirst withdraws the most recent request first; older ones are the
// likeliest to be close to getting their node.
func newestFirst(in []Placeholder) []Placeholder {
	c := append([]Placeholder(nil), in...)
	sort.Slice(c, func(i, j int) bool { return c[i].CreatedAt.After(c[j].CreatedAt) })
	return c
}
