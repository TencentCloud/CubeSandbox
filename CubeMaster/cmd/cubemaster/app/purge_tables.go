// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"

// cubeMasterPurgeTables returns the tombstone tables CubeMaster purges.
//
// Precedence: common.disable_hard_delete takes precedence over
// soft_delete_purge for instance records. When disable_hard_delete is set,
// instance rows are intentionally retained (soft-deleted for audit/recovery),
// so t_cube_instance_info and t_cube_instance_userdata are EXEMPT from purge —
// otherwise the purger would hard-delete exactly the records the operator
// chose to keep. See docs/guide/soft-delete-purge.md.
//
// When disable_hard_delete is false (the default), t_cube_instance_info is
// hard-deleted on the delete path (no tombstone accumulates; included only
// defensively) and t_cube_instance_userdata is always soft-deleted and so is
// purged. t_cube_sandbox_spec and t_cube_template_replica are always purged
// (they are not instance records).
func cubeMasterPurgeTables(disableHardDelete bool) []string {
	tables := []string{
		constants.SandboxSpecTableName,
		constants.TemplateReplicaTableName,
	}
	if !disableHardDelete {
		tables = append(tables,
			constants.InstanceInfoTableName,
			constants.InstanceUserDataTableName,
		)
	}
	return tables
}
