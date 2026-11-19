// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

const (
	NodeMetaRegistrationTable = "t_cube_node_registration"
	NodeMetaStatusTable       = "t_cube_node_status"
	NodeComponentVersionTable = "t_cube_node_component_version"
	NodeOperationTable        = "t_cube_node_operation"
)

const (
	LabelSchedulingDisabled      = "cube.cloud.tencentcloud.com/scheduling-disabled"
	LabelSchedulingDisabledValue = "true"

	// LabelMachineType groups nodes into an operator-declared machine type.
	// Writable only through the machine type API; Cubelet registration and a
	// bulk replace must leave it intact. Cross-node restore scopes candidates
	// by it: same value + same cpuid_hash.
	LabelMachineType = "cube.cubesandbox.io/machine-type"
)

const (
	DefaultInstanceTypeName = "default"
	HostStatusRunning       = "RUNNING"
)

const (
	ReasonHeartbeatExpired = "HeartbeatExpired"
	ReasonReportedNotReady = "ReportedNotReady"
)

const (
	OpIsolate          = "isolate"
	OpUnisolate        = "unisolate"
	OpSetLabels        = "set-labels"
	OpReplaceLabels    = "replace-labels"
	OpDelLabel         = "delete-label"
	OpSetMachineType   = "set-machine-type"
	OpClearMachineType = "clear-machine-type"
	OpSetQuota         = "set-quota"
	OpSetClusterQuota  = "set-cluster-quota"
	OpDelete           = "delete"
)
