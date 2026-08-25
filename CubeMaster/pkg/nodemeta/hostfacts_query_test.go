// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestFilterHostFactCandidates(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "hash-x", HostKernelRelease: "5.15.0"}}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "hash-y", HostKernelRelease: "5.15.0"}}
	armNode := &node.Node{InsID: "node-arm", IP: "10.0.0.3", Healthy: true, HostFacts: &node.HostFacts{CPUVendor: "ARM", CPUModel: "Neoverse-N2", CPUIDHash: "sha256:arm-a78", HostKernelRelease: "6.6.0"}}
	unhealthy := &node.Node{InsID: "node-c", IP: "10.0.0.4", Healthy: false, HostFacts: &node.HostFacts{CPUIDHash: "hash-x", HostKernelRelease: "5.15.0"}}
	noFacts := &node.Node{InsID: "node-d", IP: "10.0.0.5", Healthy: true}
	emptyFacts := &node.Node{InsID: "node-e", IP: "10.0.0.6", Healthy: true, HostFacts: &node.HostFacts{}}

	tests := []struct {
		name      string
		nodes     node.NodeList
		cpuidHash string
		kernelRel string
		matchAll  bool
		wantIDs   []string
	}{
		{
			name:     "matchAll returns all healthy nodes with facts",
			nodes:    node.NodeList{nodeA, nodeB, armNode, unhealthy, noFacts, emptyFacts},
			matchAll: true,
			wantIDs:  []string{"node-a", "node-arm", "node-b"},
		},
		{
			name:      "filtered by CPUIDHash and kernel",
			nodes:     node.NodeList{nodeA, nodeB},
			cpuidHash: "hash-x",
			kernelRel: "5.15.0",
			matchAll:  false,
			wantIDs:   []string{"node-a"},
		},
		{
			name: "same kernel different cpuid_hash is rejected",
			nodes: node.NodeList{
				nodeA,
				&node.Node{InsID: "node-other-cpu", IP: "10.0.0.7", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "hash-other", HostKernelRelease: "5.15.0"}},
			},
			cpuidHash: "hash-x",
			kernelRel: "5.15.0",
			matchAll:  false,
			wantIDs:   []string{"node-a"},
		},
		{
			name: "same cpuid_hash different kernel is rejected",
			nodes: node.NodeList{
				nodeA,
				&node.Node{InsID: "node-other-kernel", IP: "10.0.0.8", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "hash-x", HostKernelRelease: "6.1.0"}},
			},
			cpuidHash: "hash-x",
			kernelRel: "5.15.0",
			matchAll:  false,
			wantIDs:   []string{"node-a"},
		},
		{
			name:      "ARM node filtered by its own CPUIDHash",
			nodes:     node.NodeList{nodeA, armNode},
			cpuidHash: "sha256:arm-a78",
			kernelRel: "6.6.0",
			matchAll:  false,
			wantIDs:   []string{"node-arm"},
		},
		{
			name:      "no match returns empty",
			nodes:     node.NodeList{nodeA, nodeB},
			cpuidHash: "hash-z",
			kernelRel: "5.15.0",
			matchAll:  false,
			wantIDs:   []string{},
		},
		{
			name:     "skips unhealthy and nil-facts nodes",
			nodes:    node.NodeList{unhealthy, noFacts, emptyFacts},
			matchAll: true,
			wantIDs:  []string{},
		},
		{
			name:     "empty input returns empty",
			nodes:    node.NodeList{},
			matchAll: true,
			wantIDs:  []string{},
		},
		{
			name:     "nil nodes in list are skipped",
			nodes:    node.NodeList{nil, nodeA, nil},
			matchAll: true,
			wantIDs:  []string{"node-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterHostFactCandidates(tt.nodes, tt.cpuidHash, tt.kernelRel, tt.matchAll)
			gotIDs := make([]string, 0, len(got))
			for _, c := range got {
				gotIDs = append(gotIDs, c.NodeID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}
