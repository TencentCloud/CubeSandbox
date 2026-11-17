// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// hostFactsUnknownGroup is the bucket for nodes whose cpuid_hash or
// host_kernel_release is missing: their compatibility class cannot be
// verified, so cross-node restore fails closed for them.
const hostFactsUnknownGroup = "unknown"

// hostFactsGroupThreshold truncates the cpuid hash in the table view so one
// value does not push the NODES column off screen; --json carries the full
// value.
const hostFactsHashPreviewLen = 24

// groupsCommand groups cluster nodes by the two host facts that
// cross-node memory restore (pause/resume, from-snapshot) requires to be
// strictly equal: cpuid_hash and host_kernel_release. Nodes sharing both
// values form one restore-compatibility class — a snapshot taken on one
// member can be restored on every other member. Nodes missing either fact
// fall into the "unknown" group.
var groupsCommand = cli.Command{
	Name:    "groups",
	Aliases: []string{"group", "hostfacts"},
	Usage:   "group nodes by host facts (cpuid_hash + host_kernel_release) into restore-compatibility classes",
	Description: "Nodes in the same group share cpuid_hash and host_kernel_release, " +
		"the two dimensions cross-node memory restore (pause/resume, from-snapshot) requires. " +
		"Use this to see how many node types a heterogeneous cluster mixes and which " +
		"nodes can restore each other's snapshots. Nodes missing either fact fall into " +
		"the 'unknown' group: their class cannot be verified and cross-node restore " +
		"fails closed for them.",
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "json", Usage: "print structured json instead of a table"},
		cli.BoolFlag{Name: "detail", Usage: "also show the CPUID_HASH and KERNEL_RELEASE columns in the table (json always carries the full values)"},
	},
	Action: groupsAction,
}

// hostFactsGroupNode is one node's identity inside a group.
type hostFactsGroupNode struct {
	NodeID             string `json:"node_id"`
	NodeIP             string `json:"node_ip,omitempty"`
	Healthy            bool   `json:"healthy"`
	SchedulingDisabled bool   `json:"scheduling_disabled"`
	// KVMModuleTaint is informational: a non-empty taint marks a node whose
	// live kvm module is suspicious, which blocks restores onto it.
	KVMModuleTaint string `json:"kvm_module_taint,omitempty"`
}

// hostFactsGroup is one restore-compatibility class. The fact fields come
// from the group's first member; within a group cpuid_hash and
// host_kernel_release are equal by construction, and vendor/model are
// reported for human readability only.
type hostFactsGroup struct {
	Name                  string               `json:"group"`
	CPUVendor             string               `json:"cpu_vendor,omitempty"`
	CPUModel              string               `json:"cpu_model,omitempty"`
	CPUIDHash             string               `json:"cpuid_hash,omitempty"`
	HostKernelRelease     string               `json:"host_kernel_release,omitempty"`
	HostKernelFingerprint string               `json:"host_kernel_fingerprint,omitempty"`
	Nodes                 []hostFactsGroupNode `json:"nodes"`
}

// hostFactsGroups is the command result. GroupCount counts the known classes
// only; the unknown bucket, when present, is the last entry of Groups.
type hostFactsGroups struct {
	TotalNodes int              `json:"total_nodes"`
	GroupCount int              `json:"group_count"`
	Groups     []hostFactsGroup `json:"groups"`
}

func groupsAction(c *cli.Context) error {
	nodes, err := fetchSchedulerNodes(c)
	if err != nil {
		return err
	}
	result := groupNodesByHostFacts(nodes)
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	printHostFactsGroups(result, c.Bool("detail"))
	return nil
}

// fetchSchedulerNodes wires the server list and fetches every node from the
// CubeOps internal endpoint, sorted by node id.
func fetchSchedulerNodes(c *cli.Context) ([]*model.SchedulerNode, error) {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return nil, errors.New("no server addr")
	}
	port = c.GlobalString("port")
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return nil, err
	}
	reqURL := buildURL(host, fmt.Sprintf("/internal/v1/nodes?requestID=%s", requestID))
	var rsp []*model.SchedulerNode
	if err := doHttpReq(c, reqURL, http.MethodGet, requestID, nil, &rsp); err != nil {
		return nil, err
	}
	sort.Slice(rsp, func(i, j int) bool { return rsp[i].ID() < rsp[j].ID() })
	return rsp, nil
}

// groupNodesByHostFacts partitions nodes into restore-compatibility classes.
// The grouping key is exactly the pair of required equality dimensions of
// the restore-compat v1 policy (cpuid_hash, host_kernel_release), so two
// nodes land in the same group if and only if cross-node memory restore
// between them can pass the fact check. Known groups are named type-1,
// type-2, ... in deterministic (hash, release) order; nodes missing either
// fact share the trailing "unknown" group.
func groupNodesByHostFacts(nodes []*model.SchedulerNode) hostFactsGroups {
	type groupKey struct {
		cpuid  string
		kernel string
	}
	known := map[groupKey][]*model.SchedulerNode{}
	var unknown []*model.SchedulerNode
	for _, n := range nodes {
		if n == nil {
			continue
		}
		var cpuid, kernel string
		if facts := n.HostFacts; facts != nil {
			cpuid = strings.TrimSpace(facts.CPUIDHash)
			kernel = strings.TrimSpace(facts.HostKernelRelease)
		}
		if cpuid == "" || kernel == "" {
			unknown = append(unknown, n)
			continue
		}
		known[groupKey{cpuid, kernel}] = append(known[groupKey{cpuid, kernel}], n)
	}

	result := hostFactsGroups{TotalNodes: 0, GroupCount: len(known), Groups: nil}
	keys := make([]groupKey, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cpuid != keys[j].cpuid {
			return keys[i].cpuid < keys[j].cpuid
		}
		return keys[i].kernel < keys[j].kernel
	})
	for i, key := range keys {
		members := known[key]
		sort.Slice(members, func(a, b int) bool { return members[a].ID() < members[b].ID() })
		group := hostFactsGroup{
			Name:              fmt.Sprintf("type-%d", i+1),
			CPUIDHash:         key.cpuid,
			HostKernelRelease: key.kernel,
			Nodes:             make([]hostFactsGroupNode, 0, len(members)),
		}
		for _, n := range members {
			group.Nodes = append(group.Nodes, hostFactsGroupNodeFrom(n))
			// Representative facts come from the first member; only the
			// informational fields differ within a group in practice.
			if group.CPUVendor == "" && n.HostFacts != nil {
				group.CPUVendor = strings.TrimSpace(n.HostFacts.CPUVendor)
				group.CPUModel = strings.TrimSpace(n.HostFacts.CPUModel)
				group.HostKernelFingerprint = strings.TrimSpace(n.HostFacts.HostKernelFingerprint)
			}
		}
		result.Groups = append(result.Groups, group)
		result.TotalNodes += len(members)
	}
	if len(unknown) > 0 {
		sort.Slice(unknown, func(a, b int) bool { return unknown[a].ID() < unknown[b].ID() })
		group := hostFactsGroup{
			Name:  hostFactsUnknownGroup,
			Nodes: make([]hostFactsGroupNode, 0, len(unknown)),
		}
		for _, n := range unknown {
			group.Nodes = append(group.Nodes, hostFactsGroupNodeFrom(n))
		}
		result.Groups = append(result.Groups, group)
		result.TotalNodes += len(unknown)
	}
	return result
}

// hostFactsGroupNodeFrom projects a SchedulerNode into a group member.
func hostFactsGroupNodeFrom(n *model.SchedulerNode) hostFactsGroupNode {
	member := hostFactsGroupNode{
		NodeID:             n.ID(),
		NodeIP:             n.HostIP(),
		Healthy:            n.Healthy,
		SchedulingDisabled: n.SchedulingDisabled,
	}
	if n.HostFacts != nil {
		member.KVMModuleTaint = strings.TrimSpace(n.HostFacts.KVMModuleTaint)
	}
	return member
}

// printHostFactsGroups renders the group table plus a summary. The default
// table is compact — group, CPU identity, member count and nodes; pass
// detail=true to add the CPUID_HASH and KERNEL_RELEASE columns. Node ids
// carry inline state markers so an operator can spot at a glance which
// members are currently usable as a restore target.
func printHostFactsGroups(result hostFactsGroups, detail bool) {
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	if detail {
		fmt.Fprintln(w, "GROUP\tCPU_VENDOR\tCPU_MODEL\tCPUID_HASH\tKERNEL_RELEASE\tCOUNT\tNODES")
	} else {
		fmt.Fprintln(w, "GROUP\tCPU_VENDOR\tCPU_MODEL\tCOUNT\tNODES")
	}
	for _, group := range result.Groups {
		ids := make([]string, 0, len(group.Nodes))
		for _, member := range group.Nodes {
			ids = append(ids, markHostFactsGroupNode(member))
		}
		if detail {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				group.Name, group.CPUVendor, group.CPUModel,
				previewHash(group.CPUIDHash), group.HostKernelRelease,
				len(group.Nodes), strings.Join(ids, ","))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
				group.Name, group.CPUVendor, group.CPUModel,
				len(group.Nodes), strings.Join(ids, ","))
		}
	}
	_ = w.Flush()

	unknownCount := 0
	if len(result.Groups) > 0 && result.Groups[len(result.Groups)-1].Name == hostFactsUnknownGroup {
		unknownCount = len(result.Groups[len(result.Groups)-1].Nodes)
	}
	if result.TotalNodes == 0 {
		fmt.Println("no nodes")
		return
	}
	fmt.Printf("%d node(s) in %d host-fact group(s)", result.TotalNodes, result.GroupCount)
	if unknownCount > 0 {
		fmt.Printf(" (%d unclassified)", unknownCount)
	}
	fmt.Println()
	if unknownCount > 0 {
		fmt.Printf("warning: %d node(s) reported no cpuid_hash/host_kernel_release; their class is unknown and cross-node restore fails closed for them\n", unknownCount)
	}
}

// markHostFactsGroupNode renders one member as "node-id" plus state markers:
// (down) unhealthy, (cordoned) scheduling-disabled, (tainted) kvm module taint.
func markHostFactsGroupNode(member hostFactsGroupNode) string {
	id := member.NodeID
	if !member.Healthy {
		id += "(down)"
	}
	if member.SchedulingDisabled {
		id += "(cordoned)"
	}
	if member.KVMModuleTaint != "" {
		id += "(tainted)"
	}
	return id
}

// previewHash shortens a long hash for the table; the full value is in --json.
func previewHash(hash string) string {
	if len(hash) <= hostFactsHashPreviewLen {
		return hash
	}
	return hash[:hostFactsHashPreviewLen] + "..."
}
