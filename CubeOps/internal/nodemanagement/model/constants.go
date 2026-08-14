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
	OpIsolate   = "isolate"
	OpUnisolate = "unisolate"
	OpSetLabels = "set-labels"
	OpDelLabel  = "delete-label"
	OpDelete    = "delete"
)
