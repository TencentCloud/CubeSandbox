// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestRestoreMatchFactsJSON_OnlyCPUIDAndKernel(t *testing.T) {
	raw := RestoreMatchFactsJSON(sampleHostFacts())
	if raw == "" {
		t.Fatal("expected slim json")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want only cpuid_hash and host_kernel_release, got %v", got)
	}
	if got["cpuid_hash"] != "sha256:cpu" {
		t.Errorf("cpuid_hash=%v", got["cpuid_hash"])
	}
	if got["host_kernel_release"] != "5.15.0" {
		t.Errorf("host_kernel_release=%v", got["host_kernel_release"])
	}
}

func TestRestoreMatchFactsJSON_Empty(t *testing.T) {
	if got := RestoreMatchFactsJSON(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := RestoreMatchFactsJSON(&HostFacts{CPUVendor: "GenuineIntel"}); got != "" {
		t.Errorf("vendor-only must not freeze, got %q", got)
	}
}

// NodeMachineType is the cross-package protocol: restoreplace reads the same
// label key, so a silent trim/empty-map regression here would drop resume back
// to the strict HostFacts path without any error.
func TestMachineTypeIfIdentityMatches(t *testing.T) {
	labels := map[string]string{constants.LabelMachineType: "intel-k66"}
	same := &node.Node{InsID: "node-1", NodeLabels: labels}
	if got := machineTypeIfIdentityMatches("node-1", same); got != "intel-k66" {
		t.Fatalf("same id = %q", got)
	}
	other := &node.Node{InsID: "node-2", NodeLabels: labels}
	if got := machineTypeIfIdentityMatches("node-1", other); got != "" {
		t.Fatalf("different id = %q, want empty", got)
	}
	if got := machineTypeIfIdentityMatches("", other); got != "intel-k66" {
		t.Fatalf("ip-only lookup = %q", got)
	}
	unknown := &node.Node{NodeLabels: labels}
	if got := machineTypeIfIdentityMatches("node-1", unknown); got != "intel-k66" {
		t.Fatalf("empty cached id = %q", got)
	}
	if machineTypeIfIdentityMatches("node-1", nil) != "" {
		t.Fatal("nil node should be empty")
	}
}

func TestMachineTypeOf(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"empty", map[string]string{}, ""},
		{"absent", map[string]string{"team": "infra"}, ""},
		{"value", map[string]string{constants.LabelMachineType: "intel-k66"}, "intel-k66"},
		{"trimmed", map[string]string{constants.LabelMachineType: "  intel-k66  "}, "intel-k66"},
		{"blank", map[string]string{constants.LabelMachineType: "   "}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := machineTypeOf(tc.labels); got != tc.want {
				t.Fatalf("machineTypeOf = %q, want %q", got, tc.want)
			}
		})
	}
}
