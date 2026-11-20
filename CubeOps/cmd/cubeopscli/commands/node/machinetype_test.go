// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"flag"
	"strings"
	"testing"

	"github.com/urfave/cli"

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

func TestPrintMachineTypeResultsReportsFailures(t *testing.T) {
	// A partial failure must make the command exit non-zero in both output
	// modes; the unbind action relies on this return value and has no separate
	// failed check of its own.
	cases := []struct {
		name    string
		json    bool
		results []model.MachineTypeNodeResult
		wantErr bool
	}{
		{
			name: "table all ok",
			results: []model.MachineTypeNodeResult{
				{NodeID: "n1", OK: true},
				{NodeID: "n2", OK: true},
			},
		},
		{
			name: "table partial failure",
			results: []model.MachineTypeNodeResult{
				{NodeID: "n1", OK: true},
				{NodeID: "n2", Error: "node is bound to \"other\""},
			},
			wantErr: true,
		},
		{
			name: "json partial failure",
			json: true,
			results: []model.MachineTypeNodeResult{
				{NodeID: "n1", OK: true},
				{NodeID: "n2", Error: "node is bound to \"other\""},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := flag.NewFlagSet("machine-type", flag.ContinueOnError)
			set.Bool("json", tc.json, "")
			c := cli.NewContext(nil, set, nil)
			err := printMachineTypeResults(c, tc.results)
			if tc.wantErr && err == nil {
				t.Fatal("partial failure must return an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "1 of 2") {
				t.Fatalf("error = %v, want a per-node failure count", err)
			}
		})
	}
}

func idsOf(nodes []*model.SchedulerNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID())
	}
	return out
}
