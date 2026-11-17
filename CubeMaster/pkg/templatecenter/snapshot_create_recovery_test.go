// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestRecoverSnapshotCreateReservationGuards(t *testing.T) {
	for _, scenario := range []string{"fenced", "unconfirmed", "reason_missing", "wrong_node", "young", "not_retired", "healthy", "master_exists", "active_job"} {
		t.Run(scenario, func(t *testing.T) {
			db := snapshotReservationTestDB(t)
			oldExists, oldNodes := sandboxExistsOnMasterFn, getSnapshotReconcilerNodes
			t.Cleanup(func() { sandboxExistsOnMasterFn, getSnapshotReconcilerNodes = oldExists, oldNodes })
			sandboxExistsOnMasterFn = func(context.Context, string) bool { return scenario == "master_exists" }
			getSnapshotReconcilerNodes = func(int, string) node.NodeList {
				if scenario == "healthy" {
					return node.NodeList{&node.Node{IP: "192.0.2.10"}}
				}
				return nil
			}
			ctx := context.Background()
			require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "source", Status: StatusReady}).Error)
			require.NoError(t, ReserveSnapshotCreate(ctx, "source", "reserved", "node", "192.0.2.10"))
			if scenario != "not_retired" {
				require.NoError(t, tombstoneSnapshotRecord(ctx, "source"))
			}
			if scenario != "young" {
				require.NoError(t, db.Model(&models.SnapshotRuntimeActive{}).Where("sandbox_id = ?", "reserved").Update("attached_at", time.Now().Add(-25*time.Hour)).Error)
			}
			// A recovery must leave unrelated live bindings intact.
			require.NoError(t, AttachSnapshotRuntimeBinding(ctx, SnapshotRuntimeRefInfo{SnapshotID: "source", SandboxID: "live", NodeID: "other"}, "running"))
			if scenario == "active_job" {
				require.NoError(t, db.Create(&models.TemplateImageJob{JobID: "rollback", ResourceID: "source", Operation: JobOperationSnapshotRollback, Status: JobStatusRunning}).Error)
			}
			req := SnapshotCreateRecovery{SandboxID: "reserved", NodeID: "node", NodeIP: "192.0.2.10", ConfirmNodeFenced: scenario != "unconfirmed", Reason: "host permanently powered off"}
			if scenario == "wrong_node" {
				req.NodeID = "other"
			}
			if scenario == "reason_missing" {
				req.Reason = ""
			}
			err := RecoverSnapshotCreateReservation(ctx, "source", req)
			if scenario != "fenced" {
				require.Error(t, err)
				refs, err := CountActiveSnapshotRuntimeRefs(ctx, "source")
				require.NoError(t, err)
				require.EqualValues(t, 2, refs)
				return
			}
			require.NoError(t, err)
			require.NoError(t, RecoverSnapshotCreateReservation(ctx, "source", req), "repeating recovery must be idempotent")
			var active []models.SnapshotRuntimeActive
			require.NoError(t, db.Find(&active).Error)
			require.Len(t, active, 1)
			require.Equal(t, "live", active[0].SandboxID)
			var history []models.SnapshotRuntimeRef
			require.NoError(t, db.Where("sandbox_id = ?", "reserved").Find(&history).Error)
			require.Len(t, history, 1)
			require.Equal(t, SnapshotRuntimeRefStatusReleased, history[0].Status)
			require.Contains(t, history[0].LastError, req.Reason)
		})
	}
}
