// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package autoscale

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

type fakeNodes struct {
	snaps   map[string]*model.NodeSnapshot
	ops     map[string][]model.NodeOperation
	deleted []string
	now     time.Time
}

func (f *fakeNodes) ListNodes(context.Context) ([]*model.NodeSnapshot, error) {
	var out []*model.NodeSnapshot
	for _, s := range f.snaps {
		c := *s
		out = append(out, &c)
	}
	return out, nil
}

func (f *fakeNodes) GetNode(_ context.Context, id string) (*model.NodeSnapshot, error) {
	s, ok := f.snaps[id]
	if !ok {
		return nil, errors.New("not found")
	}
	c := *s
	return &c, nil
}

func (f *fakeNodes) SetNodeSchedulingDisabled(_ context.Context, id string, disabled bool, operator, detail string) (*model.NodeSnapshot, error) {
	s := f.snaps[id]
	if s.SchedulingDisabled == disabled {
		return s, nil
	}
	s.SchedulingDisabled = disabled
	typ := model.OpUnisolate
	if disabled {
		typ = model.OpIsolate
	}
	f.ops[id] = append([]model.NodeOperation{{NodeID: id, Type: typ, Operator: operator, Detail: detail, CreatedAt: f.now}}, f.ops[id]...)
	return s, nil
}

func (f *fakeNodes) ListOperationsByTypes(_ context.Context, id string, _ []string, limit int) ([]model.NodeOperation, error) {
	ops := f.ops[id]
	if len(ops) > limit {
		ops = ops[:limit]
	}
	return ops, nil
}

func (f *fakeNodes) DeleteNode(_ context.Context, id string, _ bool) (*model.NodeSnapshot, error) {
	f.deleted = append(f.deleted, id)
	delete(f.snaps, id)
	return nil, nil
}

type fakeInv map[string]int

func (f fakeInv) CountNodeSandboxes(_ context.Context, id string) (int, error) { return f[id], nil }

type fakeCluster struct {
	nodes   []string
	phs     []Placeholder
	seq     int
	created [][]string
	deleted []string
}

func (f *fakeCluster) PoolNodes(context.Context) ([]string, error)         { return f.nodes, nil }
func (f *fakeCluster) Placeholders(context.Context) ([]Placeholder, error) { return f.phs, nil }
func (f *fakeCluster) CreatePlaceholder(_ context.Context, avoid []string) error {
	f.seq++
	f.created = append(f.created, avoid)
	f.phs = append(f.phs, Placeholder{Name: fmt.Sprintf("cube-capacity-%d", f.seq)})
	return nil
}
func (f *fakeCluster) DeletePlaceholder(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	out := f.phs[:0]
	for _, p := range f.phs {
		if p.Name != name {
			out = append(out, p)
		}
	}
	f.phs = out
	return nil
}

// bindPending schedules Pending placeholders onto nodes without one.
func (f *fakeCluster) bindPending() {
	used := map[string]bool{}
	for _, p := range f.phs {
		if p.NodeName != "" {
			used[p.NodeName] = true
		}
	}
	for i := range f.phs {
		if f.phs[i].NodeName != "" {
			continue
		}
		for _, n := range f.nodes {
			if !used[n] {
				f.phs[i].NodeName, used[n] = n, true
				break
			}
		}
	}
}

func snap(id string, usedMilli int64) *model.NodeSnapshot {
	return &model.NodeSnapshot{NodeID: id, Healthy: true, QuotaCPU: 16000, QuotaMemMB: 32000,
		QuotaCpuUsage: usedMilli, QuotaMemUsage: usedMilli}
}

func newTestReconciler(nodes *fakeNodes, inv fakeInv, cl *fakeCluster, now *time.Time) *Reconciler {
	cfg := config.AutoscaleConfig{Enable: true, MinNodes: 1, MaxNodes: 4, TargetUtilization: 0.75,
		ScaleInCooldown: 10 * time.Minute, DrainTimeout: time.Hour, ProvisionTimeout: 15 * time.Minute,
		NodeSelector: map[string]string{"pool": "cube"}}
	r := New(cfg, nodes, inv, cl, nil)
	r.now = func() time.Time { return *now }
	return r
}

func TestReconcilerScaleInDrainsReleasesAndCleansUp(t *testing.T) {
	now := t0
	nodes := &fakeNodes{snaps: map[string]*model.NodeSnapshot{"a": snap("a", 1000), "b": snap("b", 0)},
		ops: map[string][]model.NodeOperation{}, now: now}
	inv := fakeInv{"a": 1, "b": 1}
	cl := &fakeCluster{nodes: []string{"a", "b"}}
	r := newTestReconciler(nodes, inv, cl, &now)
	ctx := context.Background()

	// Adopt: pin both nodes.
	r.RunOnce(ctx)
	require.Len(t, cl.phs, 2)
	cl.bindPending()

	// Cooldown starts, then passes; "b" (fewest MicroVMs, least used) is isolated.
	nodes.snaps["b"].MvmNum = 0
	nodes.snaps["a"].MvmNum = 1
	r.RunOnce(ctx)
	now = now.Add(11 * time.Minute)
	nodes.now = now
	r.RunOnce(ctx)
	require.True(t, nodes.snaps["b"].SchedulingDisabled)
	assert.False(t, nodes.snaps["a"].SchedulingDisabled)

	// Sandbox still on "b": placeholder stays.
	r.RunOnce(ctx)
	assert.Empty(t, cl.deleted)

	// Empty: placeholder released.
	inv["b"] = 0
	r.RunOnce(ctx)
	require.Len(t, cl.deleted, 1)
	st := r.Status()
	assert.Empty(t, st.LastError)

	// While the machine is still there, the node is not re-pinned and new
	// placeholders would avoid it.
	r.RunOnce(ctx)
	assert.Len(t, cl.phs, 1)

	// Cluster Autoscaler removes the machine; Cube heartbeat stops.
	cl.nodes = []string{"a"}
	nodes.snaps["b"].Healthy = false
	r.RunOnce(ctx)
	assert.Equal(t, []string{"b"}, nodes.deleted)
}

func TestReconcilerNeverReleasesOperatorIsolatedNode(t *testing.T) {
	now := t0
	nodes := &fakeNodes{snaps: map[string]*model.NodeSnapshot{"a": snap("a", 0), "b": snap("b", 0)},
		ops: map[string][]model.NodeOperation{}, now: now}
	_, _ = nodes.SetNodeSchedulingDisabled(context.Background(), "b", true, "alice", "maintenance")
	cl := &fakeCluster{nodes: []string{"a", "b"}}
	r := newTestReconciler(nodes, fakeInv{}, cl, &now)
	for i := 0; i < 5; i++ {
		r.RunOnce(context.Background())
		cl.bindPending()
		now = now.Add(20 * time.Minute)
	}
	assert.Empty(t, cl.deleted)
	assert.Empty(t, nodes.deleted)
	assert.Len(t, cl.phs, 2, "operator-isolated node stays pinned")
}

func TestReconcilerDryRunChangesNothing(t *testing.T) {
	now := t0
	nodes := &fakeNodes{snaps: map[string]*model.NodeSnapshot{"a": snap("a", 0)}, ops: map[string][]model.NodeOperation{}, now: now}
	cl := &fakeCluster{nodes: []string{"a"}}
	r := newTestReconciler(nodes, fakeInv{}, cl, &now)
	r.cfg.DryRun = true
	r.RunOnce(context.Background())
	assert.Empty(t, cl.phs)
	assert.NotEmpty(t, r.Status().Actions)
}
