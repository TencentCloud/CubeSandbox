// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

// SandboxSpec persists the canonical create-time spec of a sandbox so that
// downstream control-plane flows (snapshot create, debug inspection, etc.)
// can recover the full request without forcing the original caller to
// re-supply it. The record is written on sandbox-create success and removed
// on sandbox-destroy success.
type SandboxSpec struct {
	gorm.Model
	SandboxID    string `json:"sandbox_id" gorm:"column:sandbox_id"`
	TemplateID   string `json:"template_id" gorm:"column:template_id"`
	InstanceType string `json:"instance_type" gorm:"column:instance_type"`
	NetworkType  string `json:"network_type" gorm:"column:network_type"`
	HostID       string `json:"host_id" gorm:"column:host_id"`
	HostIP       string `json:"host_ip" gorm:"column:host_ip"`
	RequestJSON  string `json:"request_json" gorm:"column:request_json"`
	Backfilled   bool   `json:"backfilled" gorm:"column:backfilled"`
}

func (SandboxSpec) TableName() string {
	return constants.SandboxSpecTableName
}
