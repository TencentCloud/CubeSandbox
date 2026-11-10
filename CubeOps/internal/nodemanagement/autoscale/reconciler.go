// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package autoscale

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// LockName serialises reconcile passes across CubeOps replicas.
const LockName = "cubeops_node_autoscale_v1"

// NodeService is the subset of the node domain service the reconciler uses.
type NodeService interface {
	ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error)
	GetNode(ctx context.Context, nodeID string) (*model.NodeSnapshot, error)
	SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error)
	ListOperationsByTypes(ctx context.Context, nodeID string, types []string, limit int) ([]model.NodeOperation, error)
	DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error)
}

// Inventory counts running and paused sandboxes on a node.
type Inventory interface {
	CountNodeSandboxes(ctx context.Context, hostID string) (int, error)
}

// Locker runs fn only on the replica that holds the named lock.
type Locker func(ctx context.Context, name string, fn func(context.Context) error) error

// Status is the last pass, served by the status endpoint.
type Status struct {
	Enabled    bool       `json:"enabled"`
	DryRun     bool       `json:"dry_run"`
	LastRun    time.Time  `json:"last_run"`
	LastError  string     `json:"last_error,omitempty"`
	Demand     Demand     `json:"demand"`
	Nodes      []PoolNode `json:"nodes"`
	Pending    int        `json:"pending_placeholders"`
	BelowSince time.Time  `json:"below_since,omitempty"`
	Actions    []Action   `json:"actions"`
	Warnings   []string   `json:"warnings,omitempty"`
}

// Reconciler drives the pool toward the planned size.
type Reconciler struct {
	cfg     config.AutoscaleConfig
	nodes   NodeService
	inv     Inventory
	cluster Cluster
	lock    Locker
	now     func() time.Time

	mu         sync.Mutex
	belowSince time.Time
	status     Status
}

// New builds a reconciler. lock may be nil (single replica / tests).
func New(cfg config.AutoscaleConfig, nodes NodeService, inv Inventory, cluster Cluster, lock Locker) *Reconciler {
	return &Reconciler{cfg: cfg, nodes: nodes, inv: inv, cluster: cluster, lock: lock, now: time.Now,
		status: Status{Enabled: cfg.Enable, DryRun: cfg.DryRun}}
}

// Run reconciles every Interval until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	logging.G(ctx).Infof("autoscale: started: min=%d max=%d target=%.2f interval=%s dry_run=%t",
		r.cfg.MinNodes, r.cfg.MaxNodes, r.cfg.TargetUtilization, r.cfg.Interval, r.cfg.DryRun)
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce performs one pass under the cluster lock.
func (r *Reconciler) RunOnce(ctx context.Context) {
	pass := func(ctx context.Context) error {
		err := r.reconcile(ctx)
		r.mu.Lock()
		r.status.LastRun = r.now()
		r.status.LastError = ""
		if err != nil {
			r.status.LastError = err.Error()
		}
		r.mu.Unlock()
		if err != nil {
			logging.G(ctx).Warnf("autoscale: pass failed: %v", err)
		}
		return nil
	}
	if r.lock == nil {
		_ = pass(ctx)
		return
	}
	if err := r.lock(ctx, LockName, pass); err != nil {
		logging.G(ctx).Warnf("autoscale: lock: %v", err)
	}
}

// Status returns a copy of the last pass.
func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Nodes = append([]PoolNode(nil), s.Nodes...)
	s.Actions = append([]Action(nil), s.Actions...)
	s.Warnings = append([]string(nil), s.Warnings...)
	return s
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	pool, err := r.observe(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	below := r.belowSince
	r.mu.Unlock()

	plan := Decide(Input{
		Now:          r.now(),
		Nodes:        pool.nodes,
		Placeholders: pool.placeholders,
		Sizing: Sizing{
			MinNodes:          r.cfg.MinNodes,
			MaxNodes:          r.cfg.MaxNodes,
			TargetUtilization: r.cfg.TargetUtilization,
			HeadroomMilliCPU:  r.cfg.HeadroomMilliCPU,
			HeadroomMemoryMB:  r.cfg.HeadroomMemoryMB,
		},
		BelowSince:       below,
		ScaleInCooldown:  r.cfg.ScaleInCooldown,
		DrainTimeout:     r.cfg.DrainTimeout,
		ProvisionTimeout: r.cfg.ProvisionTimeout,
	})

	pending := 0
	for _, ph := range pool.placeholders {
		if ph.NodeName == "" {
			pending++
		}
	}
	r.mu.Lock()
	r.belowSince = plan.BelowSince
	r.status.Demand = plan.Demand
	r.status.Nodes = pool.nodes
	r.status.Pending = pending
	r.status.BelowSince = plan.BelowSince
	r.status.Actions = plan.Actions
	r.status.Warnings = plan.Warnings
	r.mu.Unlock()

	for _, w := range plan.Warnings {
		logging.G(ctx).Warnf("autoscale: %s", w)
	}
	var firstErr error
	for _, a := range plan.Actions {
		if err := r.execute(ctx, a, pool.avoid); err != nil {
			logging.G(ctx).Warnf("autoscale: %s node=%s pod=%s failed: %v", a.Kind, a.Node, a.Pod, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

type observation struct {
	nodes        []PoolNode
	placeholders []Placeholder
	avoid        []string // nodes new placeholders must not land on
}

// observe merges Kubernetes pool nodes, Cube node metadata and placeholders.
func (r *Reconciler) observe(ctx context.Context) (*observation, error) {
	kubeNodes, err := r.cluster.PoolNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pool nodes: %w", err)
	}
	phs, err := r.cluster.Placeholders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list placeholders: %w", err)
	}
	snaps, err := r.nodes.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cube nodes: %w", err)
	}

	inKube := make(map[string]bool, len(kubeNodes))
	for _, n := range kubeNodes {
		inKube[n] = true
	}
	byCube := make(map[string]*model.NodeSnapshot, len(snaps))
	for _, s := range snaps {
		byCube[s.NodeID] = s
	}
	phByNode := make(map[string]string)
	for _, ph := range phs {
		if ph.NodeName != "" {
			phByNode[ph.NodeName] = ph.Name
		}
	}

	names := map[string]bool{}
	for _, n := range kubeNodes {
		names[n] = true
	}
	for id, s := range byCube {
		// Cube nodes outside the pool matter only when the autoscaler
		// isolated them (machine already removed, metadata to clean up).
		if !inKube[id] && s.SchedulingDisabled {
			names[id] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	obs := &observation{placeholders: phs}
	for _, name := range sorted {
		s := byCube[name]
		n := PoolNode{Name: name, InKubernetes: inKube[name], InCube: s != nil}
		n.PlaceholderName, n.HasPlaceholder = phByNode[name], phByNode[name] != ""
		if s != nil {
			n.CubeHealthy = s.Healthy
			n.CubeIsolated = s.SchedulingDisabled
			n.QuotaMilliCPU, n.QuotaMemoryMB = s.QuotaCPU, s.QuotaMemMB
			n.UsedMilliCPU, n.UsedMemoryMB = s.QuotaCpuUsage, s.QuotaMemUsage
			n.MvmNum = s.MvmNum
		}
		switch {
		case n.CubeIsolated:
			owned, since, err := r.ownedIsolation(ctx, name)
			if err != nil {
				return nil, err
			}
			n.OwnedIsolation, n.IsolatedSince = owned, since
			switch {
			case !owned:
				n.State = StateForeign
			case n.HasPlaceholder:
				n.State = StateDraining
			default:
				n.State = StateReleased
			}
		case !n.InKubernetes:
			continue
		case n.InCube && n.CubeHealthy:
			n.State = StateServing
		default:
			n.State = StateStarting
		}
		if !n.InKubernetes && n.State != StateReleased {
			continue
		}
		if n.State == StateReleased || n.State == StateDraining {
			obs.avoid = append(obs.avoid, name)
		}
		obs.nodes = append(obs.nodes, n)
	}
	return obs, nil
}

// ownedIsolation reports whether the latest isolation of node was made by
// the autoscaler, and when.
func (r *Reconciler) ownedIsolation(ctx context.Context, node string) (bool, time.Time, error) {
	ops, err := r.nodes.ListOperationsByTypes(ctx, node, []string{model.OpIsolate, model.OpUnisolate}, 1)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("list operations node=%s: %w", node, err)
	}
	if len(ops) == 0 || ops[0].Type != model.OpIsolate || ops[0].Operator != Operator {
		return false, time.Time{}, nil
	}
	return true, ops[0].CreatedAt, nil
}

func (r *Reconciler) execute(ctx context.Context, a Action, avoid []string) error {
	log := logging.G(ctx)
	if r.cfg.DryRun {
		log.Infof("autoscale: dry-run %s node=%s pod=%s count=%d reason=%q", a.Kind, a.Node, a.Pod, a.Count, a.Reason)
		return nil
	}
	switch a.Kind {
	case ActCreatePlaceholders:
		for i := 0; i < a.Count; i++ {
			if err := r.cluster.CreatePlaceholder(ctx, avoid); err != nil {
				return err
			}
		}
		log.Infof("autoscale: created %d placeholder(s): %s", a.Count, a.Reason)
	case ActDeletePending:
		if err := r.cluster.DeletePlaceholder(ctx, a.Pod); err != nil {
			return err
		}
		log.Infof("autoscale: withdrew pending placeholder %s: %s", a.Pod, a.Reason)
	case ActIsolate:
		if _, err := r.nodes.SetNodeSchedulingDisabled(ctx, a.Node, true, Operator, "autoscale: drain for scale-in"); err != nil {
			return err
		}
		log.Infof("autoscale: isolated node=%s for scale-in", a.Node)
	case ActCancelDrain:
		if _, err := r.nodes.SetNodeSchedulingDisabled(ctx, a.Node, false, Operator, "autoscale: "+a.Reason); err != nil {
			return err
		}
		log.Infof("autoscale: returned node=%s to service: %s", a.Node, a.Reason)
	case ActCheckDrained:
		return r.releaseIfDrained(ctx, a.Node, a.Pod)
	case ActDeleteCubeNode:
		if _, err := r.nodes.DeleteNode(ctx, a.Node, true); err != nil {
			return err
		}
		log.Infof("autoscale: deleted cube metadata for removed node=%s", a.Node)
	default:
		return fmt.Errorf("unknown action %q", a.Kind)
	}
	return nil
}

// releaseIfDrained removes the node's placeholder once it is isolated by the
// autoscaler and holds no running or paused sandbox. Any doubt keeps waiting.
func (r *Reconciler) releaseIfDrained(ctx context.Context, node, pod string) error {
	snap, err := r.nodes.GetNode(ctx, node)
	if err != nil {
		return err
	}
	if !snap.Healthy {
		return fmt.Errorf("node %s unhealthy; cannot verify it is empty", node)
	}
	n, err := r.inv.CountNodeSandboxes(ctx, node)
	if err != nil {
		return fmt.Errorf("count sandboxes: %w", err)
	}
	if n != 0 {
		logging.G(ctx).Infof("autoscale: draining node=%s sandboxes=%d", node, n)
		return nil
	}
	// Re-check ownership right before the irreversible step.
	owned, _, err := r.ownedIsolation(ctx, node)
	if err != nil {
		return err
	}
	snap, err = r.nodes.GetNode(ctx, node)
	if err != nil {
		return err
	}
	if !owned || !snap.SchedulingDisabled {
		return fmt.Errorf("node %s no longer isolated by the autoscaler; not releasing", node)
	}
	if pod == "" {
		return fmt.Errorf("node %s has no placeholder to release", node)
	}
	if err := r.cluster.DeletePlaceholder(ctx, pod); err != nil {
		return err
	}
	logging.G(ctx).Infof("autoscale: node=%s drained; released placeholder %s to the cluster autoscaler", node, pod)
	return nil
}
