// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func seedMachineTypeLabel(t *testing.T, fs *fakeNodeStore, nodeID, value string) {
	t.Helper()
	ctx := context.Background()
	reg, err := fs.GetRegistration(ctx, nodeID)
	if err != nil {
		t.Fatalf("get registration: %v", err)
	}
	labels, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		t.Fatalf("parse labels: %v", err)
	}
	if value == "" {
		delete(labels, model.LabelMachineType)
	} else {
		labels[model.LabelMachineType] = value
	}
	if err := fs.UpdateLabels(ctx, nodeID, labels); err != nil {
		t.Fatalf("seed machine type: %v", err)
	}
}

func registerFactNode(t *testing.T, svc *service.NodeService, id, cpuid, kernel string) {
	t.Helper()
	_, err := svc.RegisterNode(context.Background(), &model.RegisterNodeRequest{
		NodeID: id,
		HostIP: id,
		HostFacts: &model.HostFacts{
			CPUVendor:         "GenuineIntel",
			CPUModel:          "test-cpu",
			CPUIDHash:         cpuid,
			HostKernelRelease: kernel,
		},
	})
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestMachineTypeCatalogAndBinding(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	registerFactNode(t, svc, "n2", "cpu-a", "6.6.8")
	registerFactNode(t, svc, "n3", "cpu-b", "6.6.1")

	view, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{
		Name:        "intel-k66",
		DisplayName: "Intel K66",
		Description: "same cpu, mixed kernel",
		NodeIDs:     []string{"n1", "n2"},
	}, "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.NodeCount != 2 {
		t.Fatalf("members = %d, want 2 (%+v)", view.NodeCount, view.Nodes)
	}
	if len(view.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", view.Warnings)
	}
	if len(view.KernelReleases) != 2 {
		t.Fatalf("kernel groups = %d, want 2", len(view.KernelReleases))
	}

	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "intel-k66"}, "admin"); !errors.Is(err, service.ErrMachineTypeExists) {
		t.Fatalf("duplicate = %v, want exists", err)
	}
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "Intel"}, "admin"); !errors.Is(err, service.ErrMachineTypeInvalid) {
		t.Fatalf("bad name = %v, want invalid", err)
	}

	moved, err := svc.BindMachineTypeNodes(ctx, "intel-k66", []string{"n3"}, "admin")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if len(moved.Results) != 1 || !moved.Results[0].OK {
		t.Fatalf("bind result = %+v", moved.Results)
	}
	view, err = svc.GetMachineType(ctx, "intel-k66")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.NodeCount != 3 {
		t.Fatalf("node count = %d, want 3", view.NodeCount)
	}
	if !containsWarning(view.Warnings, model.MachineTypeWarningMultipleCPUID) {
		t.Fatalf("warnings = %v, want multiple_cpuid", view.Warnings)
	}

	if err := svc.DeleteMachineType(ctx, "intel-k66", false, "admin"); !errors.Is(err, service.ErrMachineTypeInUse) {
		t.Fatalf("delete in use = %v, want in use", err)
	}
	if err := svc.DeleteMachineType(ctx, "intel-k66", true, "admin"); err != nil {
		t.Fatalf("delete unbind: %v", err)
	}
	snap, err := svc.GetNode(ctx, "n1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if model.MachineTypeValue(snap.Labels) != "" {
		t.Fatalf("n1 still bound to %q", model.MachineTypeValue(snap.Labels))
	}
	if _, err := svc.GetMachineType(ctx, "intel-k66"); !errors.Is(err, service.ErrMachineTypeNotFound) {
		t.Fatalf("get deleted = %v, want not found", err)
	}
}

func TestMachineTypeReplaceMembersAndMissingFacts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "a", "cpu-a", "6.6.1")
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "b", HostIP: "b"}); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "pool", NodeIDs: []string{"a"}}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	result, err := svc.ReplaceMachineTypeNodes(ctx, "pool", []string{"b"}, "admin")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %d, want add+remove", len(result.Results))
	}
	view, err := svc.GetMachineType(ctx, "pool")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.NodeCount != 1 || view.Nodes[0].NodeID != "b" {
		t.Fatalf("members = %+v, want only b", view.Nodes)
	}
	if !containsWarning(view.Warnings, model.MachineTypeWarningMissingHostFacts) {
		t.Fatalf("warnings = %v, want missing facts", view.Warnings)
	}
	if err := svc.SetNodeMachineType(ctx, "a", "missing", "admin"); !errors.Is(err, service.ErrMachineTypeNotFound) {
		t.Fatalf("bind unknown type = %v", err)
	}
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "other", NodeIDs: []string{"a"}}, "admin"); err != nil {
		t.Fatalf("create other: %v", err)
	}
	cleared, err := svc.UnbindMachineTypeNodes(ctx, "pool", []string{"b", "a"}, "admin")
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if !cleared.Results[0].OK || cleared.Results[1].OK {
		t.Fatalf("unbind results = %+v", cleared.Results)
	}
}

func TestMachineTypeSetAuditsAndSurvivesReregister(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "intel-k66", DisplayName: "Intel"}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "intel-k66", "alice"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "intel-k66", "alice"); err != nil {
		t.Fatalf("idempotent set: %v", err)
	}
	ops, err := fs.ListOperations(ctx, "n1", 20)
	if err != nil {
		t.Fatalf("ops: %v", err)
	}
	sets := 0
	for _, op := range ops {
		if op.Type == model.OpSetMachineType {
			sets++
			if !strings.Contains(op.Detail, "intel-k66") {
				t.Fatalf("detail = %q", op.Detail)
			}
		}
	}
	if sets != 1 {
		t.Fatalf("set audits = %d, want 1", sets)
	}
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "n1",
		Labels: map[string]string{model.LabelMachineType: "other"},
	}); err != nil {
		t.Fatalf("reregister: %v", err)
	}
	snap, _ := svc.GetNode(ctx, "n1")
	if model.MachineTypeValue(snap.Labels) != "intel-k66" {
		t.Fatalf("machine type = %q", model.MachineTypeValue(snap.Labels))
	}
	updated, err := svc.UpdateMachineType(ctx, "intel-k66", &model.UpdateMachineTypeRequest{
		DisplayName: strPtr("Intel refreshed"),
	}, "bob")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DisplayName != "Intel refreshed" || updated.UpdatedBy != "bob" {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestRebindSurvivesClearOfPreviousMachineType(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	for _, name := range []string{"old-type", "other"} {
		if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: name}, "admin"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "old-type", "admin"); err != nil {
		t.Fatalf("bind old: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "other", "admin"); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	unbind, err := svc.UnbindMachineTypeNodes(ctx, "old-type", []string{"n1"}, "admin")
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if unbind.Results[0].OK || !strings.Contains(unbind.Results[0].Error, "other") {
		t.Fatalf("unbind result = %+v, want mismatch against other", unbind.Results[0])
	}
	if err := svc.DeleteMachineType(ctx, "old-type", true, "admin"); err != nil {
		t.Fatalf("delete old with unbind: %v", err)
	}
	snap, err := svc.GetNode(ctx, "n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if model.MachineTypeValue(snap.Labels) != "other" {
		t.Fatalf("machine type = %q, want other", model.MachineTypeValue(snap.Labels))
	}
}

func TestStaleRedisSnapshotDoesNotHideMachineType(t *testing.T) {
	svc, _ := newTestService(t)
	ms := installMemSnapshots(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "intel-k66"}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "intel-k66", "admin"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	stale, err := ms.read("n1")
	if err != nil || stale == nil {
		t.Fatalf("redis snapshot = %v err=%v", stale, err)
	}
	stale.Labels = map[string]string{}
	ms.write(stale)

	got, err := svc.GetNode(ctx, "n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if model.MachineTypeValue(got.Labels) != "intel-k66" {
		t.Fatalf("GetNode machine type = %q", model.MachineTypeValue(got.Labels))
	}
	listed, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || model.MachineTypeValue(listed[0].Labels) != "intel-k66" {
		t.Fatalf("ListNodes = %+v", listed)
	}
	err = svc.DeleteMachineType(ctx, "intel-k66", false, "admin")
	if !errors.Is(err, service.ErrMachineTypeInUse) {
		t.Fatalf("delete = %v, want in use", err)
	}
}

func TestListNodesUsesDBLabelsWhenRedisScanMisses(t *testing.T) {
	svc, _ := newTestService(t)
	ms := installMemSnapshots(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "intel-k66"}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "intel-k66", "admin"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	stale, err := ms.read("n1")
	if err != nil || stale == nil {
		t.Fatalf("redis snapshot = %v err=%v", stale, err)
	}
	stale.Labels = map[string]string{}
	ms.write(stale)
	t.Cleanup(nodemetric.SetScanNodeSnapshotsHook(func() ([]*model.NodeSnapshot, error) {
		return nil, errors.New("scan down")
	}))

	listed, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || model.MachineTypeValue(listed[0].Labels) != "intel-k66" {
		t.Fatalf("ListNodes = %+v", listed)
	}
	if listed[0].SchedulingDisabled {
		t.Fatal("scheduling should follow the registration labels")
	}
	err = svc.DeleteMachineType(ctx, "intel-k66", false, "admin")
	if !errors.Is(err, service.ErrMachineTypeInUse) {
		t.Fatalf("delete = %v, want in use", err)
	}
}

func TestConcurrentLabelWritersPreserveIsolationUserAndMachineType(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "intel-k66"}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = svc.SetNodeSchedulingDisabled(ctx, "n1", true, "admin", "")
		}()
		go func() {
			defer wg.Done()
			_ = svc.UpdateNodeLabels(ctx, "n1", map[string]string{"zone": "gz"}, "admin")
		}()
		go func() {
			defer wg.Done()
			_ = svc.SetNodeMachineType(ctx, "n1", "intel-k66", "admin")
		}()
	}
	wg.Wait()

	reg, err := fs.GetRegistration(ctx, "n1")
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	labels, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if labels["zone"] != "gz" {
		t.Fatalf("zone = %q", labels["zone"])
	}
	if labels[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue {
		t.Fatalf("isolation = %q", labels[model.LabelSchedulingDisabled])
	}
	if labels[model.LabelMachineType] != "intel-k66" {
		t.Fatalf("machine type = %q", labels[model.LabelMachineType])
	}
}

func TestCreateMachineTypeRollsBackOnBindFailure(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	registerFactNode(t, svc, "n2", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{Name: "e2e-label-prev"}, "admin"); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	if err := svc.SetNodeMachineType(ctx, "n1", "e2e-label-prev", "admin"); err != nil {
		t.Fatalf("seed n1 binding: %v", err)
	}

	fs.failMutateFor = map[string]error{"n2": errors.New("db down during bind")}
	_, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{
		Name:    "e2e-label-half",
		NodeIDs: []string{"n1", "n2"},
	}, "admin")
	if err == nil {
		t.Fatal("bind failure must surface")
	}

	// The catalog row must not survive, otherwise a retry hits 409.
	if _, err := fs.GetMachineType(ctx, "e2e-label-half"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("catalog row survived the failed create: %v", err)
	}
	// n1 was bound before the failure and must be back on its previous type.
	snap, err := svc.GetNode(ctx, "n1")
	if err != nil {
		t.Fatalf("get n1: %v", err)
	}
	if got := model.MachineTypeValue(snap.Labels); got != "e2e-label-prev" {
		t.Fatalf("n1 machine type = %q, want e2e-label-prev", got)
	}

	// With the failure cleared, the same create must succeed.
	delete(fs.failMutateFor, "n2")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{
		Name:    "e2e-label-half",
		NodeIDs: []string{"n1", "n2"},
	}, "admin"); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	view, err := svc.GetMachineType(ctx, "e2e-label-half")
	if err != nil {
		t.Fatalf("get created type: %v", err)
	}
	if view.NodeCount != 2 {
		t.Fatalf("members = %d, want 2", view.NodeCount)
	}
}

func TestDeleteMachineTypeRestoresLabelsOnRowFailure(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	registerFactNode(t, svc, "n1", "cpu-a", "6.6.1")
	registerFactNode(t, svc, "n2", "cpu-a", "6.6.1")
	if _, err := svc.CreateMachineType(ctx, &model.CreateMachineTypeRequest{
		Name:    "e2e-label-del",
		NodeIDs: []string{"n1", "n2"},
	}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}

	fs.failOnDeleteMachineType = errors.New("db down during delete")
	err := svc.DeleteMachineType(ctx, "e2e-label-del", true, "admin")
	if err == nil {
		t.Fatal("catalog delete failure must surface")
	}
	// The row is still there, so its members must not have been silently unbound.
	if _, err := fs.GetMachineType(ctx, "e2e-label-del"); err != nil {
		t.Fatalf("catalog row should survive: %v", err)
	}
	for _, id := range []string{"n1", "n2"} {
		snap, err := svc.GetNode(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := model.MachineTypeValue(snap.Labels); got != "e2e-label-del" {
			t.Fatalf("%s machine type = %q, want e2e-label-del", id, got)
		}
	}

	// Once the store recovers the delete must go through.
	fs.failOnDeleteMachineType = nil
	if err := svc.DeleteMachineType(ctx, "e2e-label-del", true, "admin"); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, err := fs.GetMachineType(ctx, "e2e-label-del"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("catalog row should be gone: %v", err)
	}
}

func containsWarning(warnings []string, want string) bool {
	for _, w := range warnings {
		if w == want {
			return true
		}
	}
	return false
}

func strPtr(s string) *string { return &s }
