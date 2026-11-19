// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

func TestValidateLabelKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"app", true},
		{"example.com/app", true},
		{"", false},
		{"a/b/c", false},
		{"cube.cloud.tencentcloud.com/scheduling-disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			err := service.ValidateLabelKey(tc.key)
			got := err == nil
			if got != tc.want {
				t.Errorf("ValidateLabelKey(%q) = %v, want ok=%v", tc.key, err, tc.want)
			}
		})
	}
}

func TestValidateLabelsSkippingReserved(t *testing.T) {
	// Reserved control-plane label must be exempt, other labels still validated.
	ok := map[string]string{model.LabelSchedulingDisabled: "true", "app": "demo"}
	if err := service.ValidateLabelsSkippingReserved(ok); err != nil {
		t.Errorf("reserved label should be skipped, got %v", err)
	}
	bad := map[string]string{model.LabelSchedulingDisabled: "true", "": "v"}
	if err := service.ValidateLabelsSkippingReserved(bad); err == nil {
		t.Error("invalid user label should still be rejected")
	}
}

func TestStripAndPreserveSchedulingLabel(t *testing.T) {
	existing := map[string]string{"zone": "gz", model.LabelSchedulingDisabled: model.LabelSchedulingDisabledValue}
	cubelet := map[string]string{"rack": "r1", model.LabelSchedulingDisabled: "false"}
	got := service.StripAndPreserveSchedulingLabel(existing, cubelet)
	if got[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue {
		t.Errorf("scheduling label overwritten: %s", got[model.LabelSchedulingDisabled])
	}
	if got["rack"] != "r1" {
		t.Errorf("rack missing")
	}
}

func TestDecodeSchedulingDisabled(t *testing.T) {
	if !service.DecodeSchedulingDisabled(map[string]string{model.LabelSchedulingDisabled: "true"}) {
		t.Error("expected disabled")
	}
	if service.DecodeSchedulingDisabled(map[string]string{"zone": "gz"}) {
		t.Error("expected enabled")
	}
}

// Data-plane labels (reported by Cubelet, read by the scheduler) are read-only
// for the admin label API.
func TestIsSystemLabelKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"kubernetes.io/hostname", true},
		{"kubernetes.io/arch", true},
		{"beta.kubernetes.io/os", true},
		{"cube.cloud.tencentcloud.com/instance-type", true},
		{"cube.cubesandbox.io/machine-type", false},
		{"cube.cloud.tencentcloud.com", true},
		{"app", false},
		{"example.com/app", false},
	}
	for _, tc := range cases {
		if got := service.IsSystemLabelKey(tc.key); got != tc.want {
			t.Errorf("IsSystemLabelKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestValidateLabelKeyRejectsSystemLabels(t *testing.T) {
	for _, key := range []string{"kubernetes.io/hostname", "cube.cloud.tencentcloud.com/instance-type"} {
		if err := service.ValidateLabelKey(key); !errors.Is(err, service.ErrReservedLabel) {
			t.Errorf("ValidateLabelKey(%q) = %v, want ErrReservedLabel", key, err)
		}
	}
}

func TestCountUserLabelsIgnoresSystemLabels(t *testing.T) {
	labels := map[string]string{
		"kubernetes.io/arch":                        "amd64",
		"beta.kubernetes.io/os":                     "linux",
		"cube.cloud.tencentcloud.com/instance-type": "cubebox",
		model.LabelSchedulingDisabled:               "true",
		model.LabelMachineType:                      "intel-k66",
		"team":                                      "infra",
	}
	if got := service.CountUserLabels(labels); got != 1 {
		t.Errorf("CountUserLabels = %d, want 1 (team only; machine-type is reserved)", got)
	}
}

func TestValidateMachineTypeValueShape(t *testing.T) {
	for _, ok := range []string{"intel-k66", "a", "amd-epyc-9k84"} {
		if err := service.ValidateMachineTypeValue(ok); err != nil {
			t.Errorf("ValidateMachineTypeValue(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-leading", "Intel", "intel_k66", strings.Repeat("a", 64)} {
		if err := service.ValidateMachineTypeValue(bad); !errors.Is(err, service.ErrInvalidLabel) {
			t.Errorf("ValidateMachineTypeValue(%q) = %v, want ErrInvalidLabel", bad, err)
		}
	}
}

// DeleteNodeLabel mutates the map it is given in place. If mutateNodeLabels
// hands over the same map it later compares against, the audit entry reports
// "no label change" for a real delete.
func TestDeleteNodeLabelRecordsRemoval(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{"team": "infra", "zone": "gz"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.DeleteNodeLabel(ctx, "node-1", "team", "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	ops, err := fs.ListOperations(ctx, "node-1", 20)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	var detail string
	for _, op := range ops {
		if op.Type == model.OpDelLabel {
			detail = op.Detail
		}
	}
	if !strings.Contains(detail, "removed=1") || !strings.Contains(detail, "-team") {
		t.Fatalf("delete audit detail = %q, want removed=1 -team", detail)
	}
}

// machine-type is not a generic label: the label API refuses to write or
// delete it, replace keeps the stored value, and Cubelet cannot overwrite it.
func TestMachineTypeProtectionIsPerOperation(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.UpdateNodeLabels(ctx, "node-1", map[string]string{model.LabelMachineType: "intel-k66"}, "admin"); !errors.Is(err, service.ErrReservedLabel) {
		t.Fatalf("label API set machine-type = %v, want ErrReservedLabel", err)
	}
	seedMachineTypeLabel(t, fs, "node-1", "intel-k66")

	if err := svc.ReplaceNodeLabels(ctx, "node-1", map[string]string{"zone": "gz"}, "admin"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := machineType(t, svc, ctx, "node-1"); got != "intel-k66" {
		t.Fatalf("replace dropped machine-type, got %q", got)
	}
	if err := svc.DeleteNodeLabel(ctx, "node-1", model.LabelMachineType, "admin"); !errors.Is(err, service.ErrReservedLabel) {
		t.Fatalf("label API delete machine-type = %v, want ErrReservedLabel", err)
	}
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{model.LabelMachineType: "attacker", "zone": "sh"},
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := machineType(t, svc, ctx, "node-1"); got != "intel-k66" {
		t.Fatalf("cubelet overwrote machine-type, got %q", got)
	}
}

func machineType(t *testing.T, svc *service.NodeService, ctx context.Context, nodeID string) string {
	t.Helper()
	snap, err := svc.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	return snap.Labels[model.LabelMachineType]
}

// A nil PUT body is a 200 no-op; only replace tells "clear" from "nothing
// supplied".
func TestUpdateNodeLabels_NilBodyIsANoOp(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{
		NodeID: "node-1",
		Labels: map[string]string{"team": "infra"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.UpdateNodeLabels(ctx, "node-1", nil, "internal"); err != nil {
		t.Fatalf("nil merge must be a no-op, got %v", err)
	}
	snap, err := svc.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if snap.Labels["team"] != "infra" {
		t.Fatalf("labels = %v, want team preserved", snap.Labels)
	}
}
