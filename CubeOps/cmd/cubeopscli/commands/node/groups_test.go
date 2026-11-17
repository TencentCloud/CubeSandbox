// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

func hostFactsNode(id, ip, cpuid, kernel string, healthy bool) *model.SchedulerNode {
	if cpuid == "" && kernel == "" {
		// No facts at all, as an old cubelet that never reported them.
		return &model.SchedulerNode{InsID: id, IP: ip, Healthy: healthy}
	}
	return &model.SchedulerNode{
		InsID: id, IP: ip, Healthy: healthy,
		HostFacts: &model.HostFacts{CPUIDHash: cpuid, HostKernelRelease: kernel},
	}
}

func sampleHostFactsNodes() []*model.SchedulerNode {
	return []*model.SchedulerNode{
		hostFactsNode("node-b", "10.0.0.2", "sha256:aaa", "5.15.0", true),
		hostFactsNode("node-a", "10.0.0.1", "sha256:aaa", "5.15.0", true),
		hostFactsNode("node-c", "10.0.0.3", "sha256:bbb", "5.15.0", true),
		hostFactsNode("node-d", "10.0.0.4", "sha256:aaa", "6.1.0", true),
		// Missing both facts (old cubelet never reported).
		{InsID: "node-e", IP: "10.0.0.5", Healthy: true},
		// Missing only the kernel release: still unclassifiable.
		hostFactsNode("node-f", "10.0.0.6", "sha256:aaa", "", true),
	}
}

func TestGroupNodesByHostFacts(t *testing.T) {
	result := groupNodesByHostFacts(sampleHostFactsNodes())

	assert.Equal(t, 6, result.TotalNodes)
	assert.Equal(t, 3, result.GroupCount, "three known classes: (aaa,5.15) (bbb,5.15) (aaa,6.1)")
	assert.Len(t, result.Groups, 4, "three known groups plus the unknown bucket")

	// Deterministic order: sorted by (cpuid_hash, kernel release), named type-N.
	assert.Equal(t, "type-1", result.Groups[0].Name)
	assert.Equal(t, "sha256:aaa", result.Groups[0].CPUIDHash)
	assert.Equal(t, "5.15.0", result.Groups[0].HostKernelRelease)
	// Members sorted by node id within a group.
	assert.Equal(t, []string{"node-a", "node-b"}, groupMemberIDs(result.Groups[0]))

	assert.Equal(t, "type-2", result.Groups[1].Name)
	assert.Equal(t, "sha256:aaa", result.Groups[1].CPUIDHash)
	assert.Equal(t, "6.1.0", result.Groups[1].HostKernelRelease)
	assert.Equal(t, []string{"node-d"}, groupMemberIDs(result.Groups[1]))

	assert.Equal(t, "type-3", result.Groups[2].Name)
	assert.Equal(t, "sha256:bbb", result.Groups[2].CPUIDHash)
	assert.Equal(t, []string{"node-c"}, groupMemberIDs(result.Groups[2]))

	// Unknown bucket is last and holds every node missing a required fact.
	last := result.Groups[len(result.Groups)-1]
	assert.Equal(t, hostFactsUnknownGroup, last.Name)
	assert.Equal(t, "", last.CPUIDHash)
	assert.Equal(t, []string{"node-e", "node-f"}, groupMemberIDs(last))
}

func groupMemberIDs(group hostFactsGroup) []string {
	ids := make([]string, 0, len(group.Nodes))
	for _, m := range group.Nodes {
		ids = append(ids, m.NodeID)
	}
	return ids
}

func TestGroupNodesByHostFacts_RepresentativeFacts(t *testing.T) {
	nodes := []*model.SchedulerNode{
		{
			InsID: "node-a", IP: "10.0.0.1", Healthy: true,
			HostFacts: &model.HostFacts{
				CPUVendor: "GenuineIntel", CPUModel: "Intel(R) Xeon(R)",
				CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0",
				HostKernelFingerprint: "sha256:fp-aaa",
			},
		},
		{
			InsID: "node-b", IP: "10.0.0.2", Healthy: true,
			HostFacts: &model.HostFacts{
				CPUVendor: "GenuineIntel", CPUModel: "Intel(R) Xeon(R)",
				CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0",
			},
		},
	}
	result := groupNodesByHostFacts(nodes)
	assert.Equal(t, 1, result.GroupCount)
	group := result.Groups[0]
	assert.Equal(t, "GenuineIntel", group.CPUVendor)
	assert.Equal(t, "Intel(R) Xeon(R)", group.CPUModel)
	assert.Equal(t, "sha256:fp-aaa", group.HostKernelFingerprint)
}

func TestGroupNodesByHostFacts_Empty(t *testing.T) {
	result := groupNodesByHostFacts(nil)
	assert.Equal(t, 0, result.TotalNodes)
	assert.Equal(t, 0, result.GroupCount)
	assert.Empty(t, result.Groups)

	result = groupNodesByHostFacts([]*model.SchedulerNode{nil})
	assert.Equal(t, 0, result.TotalNodes, "nil entries are skipped, not counted")
}

func TestMarkHostFactsGroupNode(t *testing.T) {
	assert.Equal(t, "node-1", markHostFactsGroupNode(hostFactsGroupNode{NodeID: "node-1", Healthy: true}))
	assert.Equal(t, "node-2(down)", markHostFactsGroupNode(hostFactsGroupNode{NodeID: "node-2", Healthy: false}))
	assert.Equal(t, "node-3(cordoned)", markHostFactsGroupNode(hostFactsGroupNode{NodeID: "node-3", Healthy: true, SchedulingDisabled: true}))
	assert.Equal(t, "node-4(down)(cordoned)(tainted)",
		markHostFactsGroupNode(hostFactsGroupNode{NodeID: "node-4", Healthy: false, SchedulingDisabled: true, KVMModuleTaint: "OE"}))
}

func TestPreviewHash(t *testing.T) {
	short := "sha256:abc"
	assert.Equal(t, short, previewHash(short))
	long := "sha256:" + strings.Repeat("a", 64)
	assert.Equal(t, "sha256:"+strings.Repeat("a", hostFactsHashPreviewLen-7)+"...", previewHash(long))
}

func TestPrintHostFactsGroups_CompactByDefault(t *testing.T) {
	out := captureStdout(t, func() {
		printHostFactsGroups(groupNodesByHostFacts(sampleHostFactsNodes()), false)
	})
	for _, want := range []string{
		"GROUP", "CPU_VENDOR", "CPU_MODEL", "COUNT", "NODES",
		"type-1", "node-a,node-b",
		"type-2", "node-d",
		"type-3", "node-c",
		"unknown", "node-e,node-f",
		"6 node(s) in 3 host-fact group(s) (2 unclassified)",
		"warning: 2 node(s) reported no cpuid_hash/host_kernel_release",
	} {
		assert.Contains(t, out, want)
	}
	// Compact default hides the raw fact values.
	for _, hidden := range []string{"CPUID_HASH", "KERNEL_RELEASE", "sha256:aaa", "5.15.0", "6.1.0"} {
		assert.NotContains(t, out, hidden, "compact table must not show %q", hidden)
	}
}

func TestPrintHostFactsGroups_DetailShowsFactColumns(t *testing.T) {
	out := captureStdout(t, func() {
		printHostFactsGroups(groupNodesByHostFacts(sampleHostFactsNodes()), true)
	})
	for _, want := range []string{
		"CPUID_HASH", "KERNEL_RELEASE",
		"type-1", "sha256:aaa", "5.15.0", "node-a,node-b",
		"type-2", "6.1.0", "node-d",
		"type-3", "sha256:bbb", "node-c",
		"unknown", "node-e,node-f",
	} {
		assert.Contains(t, out, want)
	}
}

func TestPrintHostFactsGroups_Empty(t *testing.T) {
	out := captureStdout(t, func() {
		printHostFactsGroups(groupNodesByHostFacts(nil), false)
	})
	assert.Contains(t, out, "no nodes")
}

func TestPrintHostFactsGroups_NodeStateMarkers(t *testing.T) {
	nodes := []*model.SchedulerNode{
		{InsID: "node-ok", IP: "10.0.0.1", Healthy: true,
			HostFacts: &model.HostFacts{CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0"}},
		{InsID: "node-down", IP: "10.0.0.2", Healthy: false,
			HostFacts: &model.HostFacts{CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0"}},
		{InsID: "node-cordoned", IP: "10.0.0.3", Healthy: true, SchedulingDisabled: true,
			HostFacts: &model.HostFacts{CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0"}},
		{InsID: "node-tainted", IP: "10.0.0.4", Healthy: true,
			HostFacts: &model.HostFacts{CPUIDHash: "sha256:aaa", HostKernelRelease: "5.15.0", KVMModuleTaint: "OE"}},
	}
	out := captureStdout(t, func() {
		printHostFactsGroups(groupNodesByHostFacts(nodes), true)
	})
	assert.Contains(t, out, "node-cordoned(cordoned)")
	assert.Contains(t, out, "node-down(down)")
	assert.Contains(t, out, "node-tainted(tainted)")
	assert.NotContains(t, out, "node-ok(", "healthy uncordoned node has no marker")
}

func TestGroupsAction_JSON(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleHostFactsNodes())
	defer srv.Close()
	ctx := newCommandContext(t, "groups", groupsFlags, h, p, []string{"--json"})

	var decoded hostFactsGroups
	out := captureStdout(t, func() { assert.NoError(t, groupsAction(ctx)) })
	assert.NoError(t, json.Unmarshal([]byte(out), &decoded))
	assert.Equal(t, 6, decoded.TotalNodes)
	assert.Equal(t, 3, decoded.GroupCount)
	assert.Len(t, decoded.Groups, 4)
	assert.Equal(t, "type-1", decoded.Groups[0].Name)
	// JSON carries the full hash even when the table truncates it.
	assert.Equal(t, "sha256:aaa", decoded.Groups[0].CPUIDHash)
	assert.Equal(t, hostFactsUnknownGroup, decoded.Groups[3].Name)
}

func TestGroupsAction_Table(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleHostFactsNodes())
	defer srv.Close()
	ctx := newCommandContext(t, "groups", groupsFlags, h, p, nil)

	out := captureStdout(t, func() { assert.NoError(t, groupsAction(ctx)) })
	assert.Contains(t, out, "type-1")
	assert.Contains(t, out, "node-a,node-b")
	assert.NotContains(t, out, "CPUID_HASH", "compact table hides the fact columns by default")
}

func TestGroupsAction_DetailFlag(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleHostFactsNodes())
	defer srv.Close()
	ctx := newCommandContext(t, "groups", groupsFlags, h, p, []string{"--detail"})

	out := captureStdout(t, func() { assert.NoError(t, groupsAction(ctx)) })
	assert.Contains(t, out, "CPUID_HASH")
	assert.Contains(t, out, "KERNEL_RELEASE")
	assert.Contains(t, out, "sha256:aaa")
}

func TestGroupsAction_HTTPError(t *testing.T) {
	srv, h, p := setupStubServer(t, 500, "internal error")
	defer srv.Close()
	ctx := newCommandContext(t, "groups", groupsFlags, h, p, nil)
	err := groupsAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestGroupsAction_NoServerAddr(t *testing.T) {
	ctx := newCommandContext(t, "groups", groupsFlags, "", "3010", nil)
	err := groupsAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no server addr")
}

var groupsFlags = []cli.Flag{cli.BoolFlag{Name: "json"}, cli.BoolFlag{Name: "detail"}}
