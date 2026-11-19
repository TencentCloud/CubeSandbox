// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

func TestFilterSchedulerNodesByMachineType(t *testing.T) {
	nodes := []*model.SchedulerNode{
		{InsID: "a", NodeLabels: map[string]string{model.LabelMachineType: "intel-k66"}},
		{InsID: "b"},
		{InsID: "c", NodeLabels: map[string]string{model.LabelMachineType: "amd-k66"}},
	}
	got := filterSchedulerNodes(nodes, "intel-k66", false)
	if len(got) != 1 || got[0].ID() != "a" {
		t.Fatalf("filter = %v", idsOf(got))
	}
	unassigned := filterSchedulerNodes(nodes, "", true)
	if len(unassigned) != 1 || unassigned[0].ID() != "b" {
		t.Fatalf("unassigned = %v", idsOf(unassigned))
	}
}

func TestRejectMachineTypeLabel(t *testing.T) {
	if err := rejectMachineTypeLabel(map[string]string{model.LabelMachineType: "x"}, ""); err == nil {
		t.Fatal("set of machine-type must be rejected")
	}
	if err := rejectMachineTypeLabel(nil, model.LabelMachineType); err == nil {
		t.Fatal("delete of machine-type must be rejected")
	}
	if err := rejectMachineTypeLabel(map[string]string{"team": "infra"}, ""); err != nil {
		t.Fatal(err)
	}
}

func idsOf(nodes []*model.SchedulerNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID())
	}
	return out
}
