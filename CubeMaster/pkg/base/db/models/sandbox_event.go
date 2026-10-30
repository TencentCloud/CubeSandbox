// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

// SandboxEvent is one entry of a sandbox's lifecycle history. Rows are
// append-only and outlive the sandbox, so they carry no soft-delete column.
type SandboxEvent struct {
	ID         uint64    `json:"id" gorm:"column:id;primaryKey;autoIncrement"`
	SandboxID  string    `json:"sandbox_id" gorm:"column:sandbox_id"`
	Event      string    `json:"event" gorm:"column:event"`
	TemplateID string    `json:"template_id" gorm:"column:template_id"`
	Reason     string    `json:"reason" gorm:"column:reason"`
	CreatedAt  time.Time `json:"created_at" gorm:"column:created_at"`
}

func (SandboxEvent) TableName() string {
	return constants.SandboxEventTableName
}
