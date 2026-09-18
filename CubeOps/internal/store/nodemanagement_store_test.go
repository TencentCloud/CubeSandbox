// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	nmstore "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func TestNodeStore_RegistrationLifecycle(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	reg := &nmstore.NodeRegistration{
		NodeID:       "node-1",
		HostIP:       "10.0.0.1",
		CapacityJSON: model.MustJSON(model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192}),
	}
	if err := s.UpsertRegistration(ctx, reg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.GetRegistration(ctx, "node-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.HostIP != "10.0.0.1" {
		t.Errorf("hostIP = %s", got.HostIP)
	}

	if err := s.UpdateLabels(ctx, "node-1", map[string]string{"zone": "gz"}); err != nil {
		t.Fatalf("update labels: %v", err)
	}
	got, err = s.GetRegistration(ctx, "node-1")
	if err != nil {
		t.Fatalf("get after labels: %v", err)
	}
	labels, err := nmstore.ParseLabelsJSON(got.LabelsJSON)
	if err != nil {
		t.Fatalf("parse labels: %v", err)
	}
	if labels["zone"] != "gz" {
		t.Errorf("zone = %s", labels["zone"])
	}
}

func TestNodeStore_StatusLifecycle(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	st := &nmstore.NodeStatus{
		NodeID:        "node-1",
		HeartbeatUnix: time.Now().Unix(),
		Healthy:       true,
	}
	if err := s.UpsertStatus(ctx, st); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetStatus(ctx, "node-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Healthy != true {
		t.Errorf("healthy = %v", got.Healthy)
	}
}

func TestNodeStore_ComponentVersions(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	_ = s.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "node-1"})

	versions := []model.ComponentVersion{
		{Component: "cubelet", Version: "v1.0"},
		{Component: "guest-image", Version: "v2.0"},
	}
	if err := s.WriteComponentVersions(ctx, "node-1", versions, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	rows, err := s.ListComponentVersions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d", len(rows))
	}

	if err := s.WriteComponentVersions(ctx, "node-1", []model.ComponentVersion{{Component: "cubelet", Version: "v1.1"}}, true); err != nil {
		t.Fatalf("write incomplete: %v", err)
	}
	rows, _ = s.ListComponentVersions(ctx)
	if len(rows) != 2 {
		t.Errorf("after incomplete expected 2 rows, got %d", len(rows))
	}
}

func TestNodeStore_HostFacts(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	// Register with HostFacts.
	factsJSON := `{"cpu_vendor":"AuthenticAMD","cpuid_hash":"sha256:abc","host_kernel_release":"5.15.0","kvm_api_version":12}`
	reg := &nmstore.NodeRegistration{
		NodeID:            "node-hf",
		HostIP:            "10.0.0.1",
		HostFactsJSON:     factsJSON,
		CPUIDHash:         "sha256:abc",
		HostKernelRelease: "5.15.0",
	}
	if err := s.UpsertRegistration(ctx, reg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.GetRegistration(ctx, "node-hf")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.HostFactsJSON != factsJSON {
		t.Errorf("host_facts_json = %s, want %s", got.HostFactsJSON, factsJSON)
	}
	if got.CPUIDHash != "sha256:abc" {
		t.Errorf("cpuid_hash = %s", got.CPUIDHash)
	}
	if got.HostKernelRelease != "5.15.0" {
		t.Errorf("host_kernel_release = %s", got.HostKernelRelease)
	}

	// Update HostFacts via UpdateHostFacts.
	newFactsJSON := `{"cpu_vendor":"AuthenticAMD","cpuid_hash":"sha256:xyz","host_kernel_release":"6.6.0","kvm_api_version":12}`
	if err := s.UpdateHostFacts(ctx, "node-hf", newFactsJSON, "sha256:xyz", "6.6.0", 16, 65536); err != nil {
		t.Fatalf("update host facts: %v", err)
	}
	got, err = s.GetRegistration(ctx, "node-hf")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.HostFactsJSON != newFactsJSON {
		t.Errorf("host_facts_json after update = %s", got.HostFactsJSON)
	}
	if got.CPUIDHash != "sha256:xyz" {
		t.Errorf("cpuid_hash after update = %s", got.CPUIDHash)
	}
	if got.HostKernelRelease != "6.6.0" {
		t.Errorf("host_kernel_release after update = %s", got.HostKernelRelease)
	}
}

func TestNodeStore_Operations(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	if err := s.CreateOperation(ctx, &nmstore.NodeOperation{NodeID: "node-1", Type: "isolate", Operator: "admin"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ops, err := s.ListOperations(ctx, "node-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("len = %d", len(ops))
	}
	if ops[0].Type != "isolate" {
		t.Errorf("type = %s", ops[0].Type)
	}
}

func TestNodeStore_DeleteRegistration(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	if err := s.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "node-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Delete succeeds.
	if err := s.DeleteRegistration(ctx, "node-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Second delete returns ErrNotFound.
	if err := s.DeleteRegistration(ctx, "node-1"); !errors.Is(err, nmstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestNodeStore_DeleteNode_RemovesAllRows(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	// Seed: registration + status + versions + operations.
	s.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "node-1", HostIP: "10.0.0.1"})
	s.UpsertStatus(ctx, &nmstore.NodeStatus{NodeID: "node-1", HeartbeatUnix: time.Now().Unix(), Healthy: true})
	s.WriteComponentVersions(ctx, "node-1", []model.ComponentVersion{
		{Component: "cubelet", Version: "v1.0"},
	}, false)
	s.CreateOperation(ctx, &nmstore.NodeOperation{NodeID: "node-1", Type: "isolate", Operator: "admin"})

	if err := s.DeleteNode(ctx, "node-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Registration gone.
	if _, err := s.GetRegistration(ctx, "node-1"); !errors.Is(err, nmstore.ErrNotFound) {
		t.Fatalf("registration should be gone, got %v", err)
	}
	// Status gone.
	if _, err := s.GetStatus(ctx, "node-1"); !errors.Is(err, nmstore.ErrNotFound) {
		t.Fatalf("status should be gone, got %v", err)
	}
	// Versions gone.
	rows, _ := s.ListComponentVersions(ctx)
	for _, r := range rows {
		if r.NodeID == "node-1" {
			t.Fatalf("component version still exists: %+v", r)
		}
	}
	// Operations preserved as audit trail.
	ops, err := s.ListOperations(ctx, "node-1", 10)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 operation preserved, got %d", len(ops))
	}
}

func TestNodeStore_DeleteNode_NotFound(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	if err := s.DeleteNode(ctx, "no-such-node"); !errors.Is(err, nmstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestNodeStore_DeleteNode_AllowsReRegister(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	// Register, then delete.
	s.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "node-1", HostIP: "10.0.0.1"})
	if err := s.DeleteNode(ctx, "node-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Re-register with the same ID — should start fresh.
	if err := s.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "node-1", HostIP: "10.0.0.2"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, err := s.GetRegistration(ctx, "node-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.HostIP != "10.0.0.2" {
		t.Fatalf("hostIP = %s, want 10.0.0.2", got.HostIP)
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestNodeStore_QuotaSpecLifecycle(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	// CreateQuotaSpecIfAbsent: first insert succeeds.
	row := &nmstore.NodeQuotaSpec{NodeID: "node-a", MCpuLimit: 64000, MemLimit: "64Gi", Revision: 1, UpdatedBy: "alice", NodeManaged: true}
	if err := s.CreateQuotaSpecIfAbsent(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Second insert of the same node must surface ErrQuotaSpecExists.
	if err := s.CreateQuotaSpecIfAbsent(ctx, &nmstore.NodeQuotaSpec{NodeID: "node-a"}); !errors.Is(err, nmstore.ErrQuotaSpecExists) {
		t.Fatalf("want ErrQuotaSpecExists, got %v", err)
	}

	// GetQuotaSpec returns the row.
	got, err := s.GetQuotaSpec(ctx, "node-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MCpuLimit != 64000 || got.MemLimit != "64Gi" || got.Revision != 1 {
		t.Fatalf("got = %+v", got)
	}
	// Missing node returns ErrNotFound.
	if _, err := s.GetQuotaSpec(ctx, "ghost"); !errors.Is(err, nmstore.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// UpdateQuotaSpecIfRevision: matching revision bumps and applies.
	next := &nmstore.NodeQuotaSpec{NodeID: "node-a", MCpuLimit: 80000, MemLimit: "64Gi", UpdatedBy: "bob", NodeManaged: true}
	applied, err := s.UpdateQuotaSpecIfRevision(ctx, next, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !applied {
		t.Fatal("matching revision must apply")
	}
	got, _ = s.GetQuotaSpec(ctx, "node-a")
	if got.MCpuLimit != 80000 || got.Revision != 2 {
		t.Fatalf("after update got = %+v", got)
	}

	// Stale revision must be a no-op (RowsAffected == 0).
	stale := &nmstore.NodeQuotaSpec{NodeID: "node-a", MCpuLimit: 99999, UpdatedBy: "carol", NodeManaged: true}
	applied, err = s.UpdateQuotaSpecIfRevision(ctx, stale, 1)
	if err != nil {
		t.Fatalf("stale update: %v", err)
	}
	if applied {
		t.Fatal("stale revision must not apply")
	}
	got, _ = s.GetQuotaSpec(ctx, "node-a")
	if got.MCpuLimit != 80000 || got.Revision != 2 {
		t.Fatalf("stale update changed row: %+v", got)
	}
}

func TestNodeStore_QuotaSpecUpsertBumpsRevision(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	if err := s.UpsertQuotaSpec(ctx, &nmstore.NodeQuotaSpec{NodeID: "node-a", MCpuLimit: 64000, Revision: 1, UpdatedBy: "alice"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Upsert on the same node_id bumps revision and replaces fields.
	if err := s.UpsertQuotaSpec(ctx, &nmstore.NodeQuotaSpec{NodeID: "node-a", MCpuLimit: 80000, UpdatedBy: "bob"}); err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	got, err := s.GetQuotaSpec(ctx, "node-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MCpuLimit != 80000 || got.Revision != 2 {
		t.Fatalf("got = %+v", got)
	}
}

func TestNodeStore_QuotaSpecFollowersAndBump(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()

	ctx := context.Background()
	s := nmstore.NewNodeStore(env.store.DB())

	// Sentinel (cluster default) with an explicit ratio.
	s.UpsertQuotaSpec(ctx, &nmstore.NodeQuotaSpec{NodeID: nmstore.ClusterQuotaSentinelID, PausedReleaseRatio: floatPtr(0.5), Revision: 1})
	// Follower with NULL ratio (inherits cluster default).
	s.UpsertQuotaSpec(ctx, &nmstore.NodeQuotaSpec{NodeID: "node-a", Revision: 1})
	// Node-managed row with an explicit ratio (not a follower).
	s.UpsertQuotaSpec(ctx, &nmstore.NodeQuotaSpec{NodeID: "node-b", PausedReleaseRatio: floatPtr(0.8), Revision: 1})

	// Followers: only the NULL-ratio, non-sentinel row.
	followers, err := s.ListClusterQuotaFollowers(ctx)
	if err != nil {
		t.Fatalf("list followers: %v", err)
	}
	if len(followers) != 1 || followers[0].NodeID != "node-a" {
		t.Fatalf("followers = %+v", followers)
	}

	// Bump advances only follower rows.
	bumped, err := s.BumpClusterQuotaFollowers(ctx)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if bumped != 1 {
		t.Fatalf("bumped = %d, want 1", bumped)
	}
	got, _ := s.GetQuotaSpec(ctx, "node-a")
	if got.Revision != 2 {
		t.Fatalf("follower revision = %d, want 2", got.Revision)
	}

	// ListQuotaSpecs returns every row (sentinel included).
	all, err := s.ListQuotaSpecs(ctx)
	if err != nil {
		t.Fatalf("list specs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("specs = %d, want 3", len(all))
	}
}
