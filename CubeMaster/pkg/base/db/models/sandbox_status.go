// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

// SandboxStatus is the control-plane view of a sandbox's restart state.
// It is written by node reports and is not required for the node to restart.
type SandboxStatus struct {
	gorm.Model
	SandboxID               string     `json:"sandbox_id" gorm:"column:sandbox_id"`
	HostID                  string     `json:"host_id" gorm:"column:host_id"`
	Phase                   string     `json:"phase" gorm:"column:phase"`
	RestartPolicy           string     `json:"restart_policy" gorm:"column:restart_policy"`
	RestartState            string     `json:"restart_state" gorm:"column:restart_state"`
	RestartCount            int32      `json:"restart_count" gorm:"column:restart_count"`
	LastExitCode            *int32     `json:"last_exit_code" gorm:"column:last_exit_code"`
	LastExitReason          string     `json:"last_exit_reason" gorm:"column:last_exit_reason"`
	LastFinishedAt          *time.Time `json:"last_finished_at" gorm:"column:last_finished_at"`
	LastRestartAt           *time.Time `json:"last_restart_at" gorm:"column:last_restart_at"`
	LastSuccessfulRestartAt *time.Time `json:"last_successful_restart_at" gorm:"column:last_successful_restart_at"`
	LastFailedRestartAt     *time.Time `json:"last_failed_restart_at" gorm:"column:last_failed_restart_at"`
	NextRestartAt           *time.Time `json:"next_restart_at" gorm:"column:next_restart_at"`
	StatusSeq               int64      `json:"status_seq" gorm:"column:status_seq"`
	ReportedAt              time.Time  `json:"reported_at" gorm:"column:reported_at"`
}

func (SandboxStatus) TableName() string {
	return constants.SandboxStatusTableName
}
