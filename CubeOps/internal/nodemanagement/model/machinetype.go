// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"time"
)

// MachineTypeUnassigned is the list filter for nodes with no machine type.
const MachineTypeUnassigned = "__unassigned__"

const (
	MachineTypeWarningMultipleCPUID    = "multiple_cpuid"
	MachineTypeWarningMissingHostFacts = "missing_host_facts"
)

// MachineTypeValue returns the trimmed machine-type label, or empty.
func MachineTypeValue(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	return strings.TrimSpace(labels[LabelMachineType])
}

// MachineTypeMatches reports whether a node's machine-type value satisfies a
// list filter. An empty filter matches everything. MachineTypeUnassigned
// matches only nodes that have no machine type.
func MachineTypeMatches(value, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	if filter == MachineTypeUnassigned {
		return strings.TrimSpace(value) == ""
	}
	return strings.TrimSpace(value) == filter
}

// CreateMachineTypeRequest creates a catalog row and optionally binds nodes.
type CreateMachineTypeRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIDs"`
}

// UpdateMachineTypeRequest patches display fields. Nil leaves the stored value.
type UpdateMachineTypeRequest struct {
	DisplayName *string `json:"displayName"`
	Description *string `json:"description"`
}

// MachineTypeNodesRequest is the membership payload for bind, unbind and replace.
type MachineTypeNodesRequest struct {
	NodeIDs []string `json:"nodeIDs"`
}

// SetNodeMachineTypeRequest binds one node to a catalog machine type.
type SetNodeMachineTypeRequest struct {
	MachineType string `json:"machineType"`
}

// MachineTypeNode is one member of a machine type, with the host facts the
// operator needs to judge restore compatibility.
type MachineTypeNode struct {
	NodeID             string `json:"nodeID"`
	HostIP             string `json:"hostIP"`
	Healthy            bool   `json:"healthy"`
	SchedulingDisabled bool   `json:"schedulingDisabled"`
	CPUVendor          string `json:"cpuVendor"`
	CPUModel           string `json:"cpuModel"`
	CPUIDHash          string `json:"cpuidHash"`
	HostKernelRelease  string `json:"hostKernelRelease"`
}

// MachineTypeCPUIDGroup aggregates members that share one CPU identity.
// Unknown is true when the member reported no cpuid_hash.
type MachineTypeCPUIDGroup struct {
	CPUIDHash string   `json:"cpuidHash"`
	Unknown   bool     `json:"unknown"`
	CPUVendor string   `json:"cpuVendor"`
	CPUModel  string   `json:"cpuModel"`
	NodeCount int      `json:"nodeCount"`
	NodeIDs   []string `json:"nodeIDs"`
}

// MachineTypeKernelGroup counts members on one host kernel release.
type MachineTypeKernelGroup struct {
	Release   string `json:"release"`
	NodeCount int    `json:"nodeCount"`
}

// MachineTypeView is the catalog row plus the live membership and hardware mix.
type MachineTypeView struct {
	Name             string                   `json:"name"`
	DisplayName      string                   `json:"displayName"`
	Description      string                   `json:"description"`
	CreatedBy        string                   `json:"createdBy"`
	UpdatedBy        string                   `json:"updatedBy"`
	CreatedAt        time.Time                `json:"createdAt"`
	UpdatedAt        time.Time                `json:"updatedAt"`
	NodeCount        int                      `json:"nodeCount"`
	HealthyNodeCount int                      `json:"healthyNodeCount"`
	Nodes            []MachineTypeNode        `json:"nodes"`
	CPUIDGroups      []MachineTypeCPUIDGroup  `json:"cpuidGroups"`
	KernelReleases   []MachineTypeKernelGroup `json:"kernelReleases"`
	Warnings         []string                 `json:"warnings"`
}

// MachineTypeNodeResult is one node's outcome in a batch membership change.
type MachineTypeNodeResult struct {
	NodeID string `json:"nodeID"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// MachineTypeBindResult is the per-node outcome of a membership write.
type MachineTypeBindResult struct {
	Name    string                  `json:"name"`
	Results []MachineTypeNodeResult `json:"results"`
}
