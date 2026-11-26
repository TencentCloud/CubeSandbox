// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestSnapshotRechecksRuntimeMountsBeforeWritingJob(t *testing.T) {
	for _, pending := range []bool{true, false} {
		name := "pending attach"
		want := sandboxspec.ErrRuntimeAttachPending
		if !pending {
			name = "settled attach"
			want = sandboxspec.ErrRuntimeMountsChanged
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			stubSnapshotLifecycleLock(t)
			db := snapshotReservationTestDB(t)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, sandboxspec.Init(db))
			const sandboxID = "sb-before-snapshot-job"
			spec := models.SandboxSpec{SandboxID: sandboxID, RequestJSON: `{"instance_type":"cubebox","backend":"xfs"}`}
			require.NoError(t, db.Create(&spec).Error)

			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
			patches.ApplyFunc(ensureSnapshotNodeWritable, func(context.Context, string, string, bool) error {
				// An Attach reaches its durable step after the initial reads but before capture claims the spec row.
				if pending {
					return sandboxspec.ClaimRuntimeAttachInTx(db, sandboxID, spec.ID, "attach-token")
				}
				mount := sandboxspec.RuntimeVolumeMount{VolumeID: "data", VolumeRecordID: 100, ContainerPath: "/data", Readonly: true}
				return sandboxspec.RecordRuntimeVolumeMount(ctx, sandboxID, spec.ID, mount)
			})
			patches.ApplyFunc(originHostFactsJSON, func(context.Context, string) string { return "" })
			patches.ApplyFunc(cubelet.CommitSandbox, func(context.Context, string, *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
				t.Fatal("a stale capture must stop before dispatch")
				return nil, nil
			})

			info, err := SubmitSandboxSnapshot(ctx, "req-late", sandboxID, "node", "127.0.0.1", "capture", "", snapshotmode.Full)
			require.Nil(t, info)
			require.ErrorIs(t, err, ErrTemplateAttemptInProgress)
			require.ErrorIs(t, err, want)
			var jobs, snapshots int64
			require.NoError(t, db.Model(&models.TemplateImageJob{}).Count(&jobs).Error)
			require.NoError(t, db.Model(&models.SnapshotRecord{}).Count(&snapshots).Error)
			require.Zero(t, jobs)
			require.Zero(t, snapshots)
		})
	}
}

func TestRuntimeVolumeRollbackStopsBeforeNodeSelection(t *testing.T) {
	for _, tc := range []struct {
		name                string
		current, pending    bool
		target, existingJob bool
	}{
		{name: "current sandbox changed", current: true},
		{name: "target contains runtime mounts", target: true},
		{name: "attach outcome unknown", pending: true},
		{name: "pending rollback rechecks current mounts", current: true, existingJob: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			stubSnapshotLifecycleLock(t)
			db := snapshotReservationTestDB(t)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&models.SandboxSpec{}))
			require.NoError(t, sandboxspec.Init(db))
			const sandboxID, snapshotID, reqID = "sb-runtime", "snap-runtime", "rollback-runtime"
			spec := models.SandboxSpec{SandboxID: sandboxID, RequestJSON: "{}"}
			require.NoError(t, db.Create(&spec).Error)
			mount := sandboxspec.RuntimeVolumeMount{VolumeID: "data", VolumeRecordID: 100, ContainerPath: "/data", Readonly: true}
			if tc.current {
				require.NoError(t, sandboxspec.RecordRuntimeVolumeMount(ctx, sandboxID, spec.ID, mount))
			}
			if tc.pending {
				require.NoError(t, sandboxspec.ClaimRuntimeAttachInTx(db, sandboxID, spec.ID, "test-token"))
			}
			record := models.SnapshotRecord{SnapshotID: snapshotID, OriginSandboxID: sandboxID, Status: StatusReady, RequestJSON: "{}"}
			if tc.target {
				mounts, err := json.Marshal([]sandboxspec.RuntimeVolumeMount{mount})
				require.NoError(t, err)
				frozen, err := json.Marshal(&sandboxtypes.CreateCubeSandboxReq{
					Annotations: map[string]string{constants.CubeAnnotationSnapshotRuntimeVolumeMounts: string(mounts)},
				})
				require.NoError(t, err)
				record.RequestJSON = string(frozen)
			}
			require.NoError(t, db.Create(&record).Error)
			if tc.existingJob {
				raw, err := marshalSnapshotRollbackRequest(reqID, sandboxID, snapshotID, "node-1", "127.0.0.1", 2, 0, "xfs")
				require.NoError(t, err)
				require.NoError(t, db.Create(&models.TemplateImageJob{
					JobID: "old-rollback", RequestID: reqID, TemplateID: snapshotID, SandboxID: sandboxID,
					Operation: JobOperationSnapshotRollback, Status: JobStatusPending, RequestJSON: raw,
				}).Error)
			}

			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(getSandboxData, func(context.Context, string, string) (*sandboxtypes.SandboxData, error) {
				t.Fatal("runtime mount rejection must precede node selection")
				return nil, nil
			})
			patches.ApplyFunc(getSnapshotReadyReplica, func(context.Context, string, string) (ReplicaStatus, error) {
				t.Fatal("runtime mount rejection must precede restore dispatch")
				return ReplicaStatus{}, nil
			})
			info, err := RollbackSandboxToSnapshot(ctx, reqID, sandboxID, snapshotID, "cubebox", "")
			require.Nil(t, info)
			require.ErrorIs(t, err, ErrTemplateAttemptInProgress)
			if tc.pending {
				require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
			}
			var jobs int64
			require.NoError(t, db.Model(&models.TemplateImageJob{}).Count(&jobs).Error)
			if tc.existingJob {
				require.EqualValues(t, 1, jobs)
			} else {
				require.Zero(t, jobs)
			}
		})
	}
}
