// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxstatus"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func enrichSandboxListRestart(ctx context.Context, items []*types.SandboxBriefData) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil && item.SandboxID != "" {
			ids = append(ids, item.SandboxID)
		}
	}
	rows := loadRestartStatus(ctx, ids)
	for _, item := range items {
		if item == nil {
			continue
		}
		if st, ok := rows[item.SandboxID]; ok {
			item.RestartStatus = st
		}
	}
}

func enrichSandboxInfoRestart(ctx context.Context, items []*types.SandboxData) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil && item.SandboxID != "" {
			ids = append(ids, item.SandboxID)
		}
	}
	rows := loadRestartStatus(ctx, ids)
	for _, item := range items {
		if item == nil {
			continue
		}
		if st, ok := rows[item.SandboxID]; ok {
			item.RestartStatus = st
		}
	}
}

func loadRestartStatus(ctx context.Context, ids []string) map[string]*types.RestartStatus {
	rows, err := sandboxstatus.BatchGet(ctx, ids)
	if err != nil {
		if err != sandboxstatus.ErrNotReady {
			log.G(ctx).Warnf("sandbox restart status: %v", err)
		}
		return nil
	}
	out := make(map[string]*types.RestartStatus, len(rows))
	for id, row := range rows {
		out[id] = restartStatusFromRow(row)
	}
	return out
}

func restartStatusFromRow(row *models.SandboxStatus) *types.RestartStatus {
	if row == nil {
		return nil
	}
	return &types.RestartStatus{
		RestartPolicy:           sandboxstatus.DisplayPolicy(row.RestartPolicy),
		RestartState:            row.RestartState,
		RestartCount:            row.RestartCount,
		LastExitCode:            row.LastExitCode,
		LastExitReason:          row.LastExitReason,
		LastRestartAt:           row.LastRestartAt,
		LastSuccessfulRestartAt: row.LastSuccessfulRestartAt,
		LastFailedRestartAt:     row.LastFailedRestartAt,
		NextRestartAt:           row.NextRestartAt,
	}
}
