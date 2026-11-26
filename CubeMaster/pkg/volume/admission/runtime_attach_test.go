// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"gorm.io/gorm"
)

func TestRuntimeAttachMarkerCommitsWithVolumeAdmissionAndSettlement(t *testing.T) {
	ctx := t.Context()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "attach.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.SandboxSpec{}, &models.VolumeRecord{}, &models.RuntimeVolumeAttach{}))
	require.NoError(t, sandboxspec.Init(db))

	volume := models.VolumeRecord{VolumeID: "shared", Name: "shared"}
	spec := models.SandboxSpec{SandboxID: "sb", RequestJSON: "{}"}
	require.NoError(t, db.Create(&volume).Error)
	require.NoError(t, db.Create(&spec).Error)
	require.NoError(t, sandboxspec.EnsureNoPendingRuntimeAttach(ctx, "legacy"))

	request := runtimeAttachRequestForTest(spec, volume, "first")
	wrongSpec := request
	wrongSpec.SpecRowID++
	_, _, err = BeginRuntimeAttach(ctx, db, wrongSpec, nil)
	require.ErrorIs(t, err, sandboxspec.ErrSandboxSpecNotFound)
	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.Zero(t, volume.InFlightCreates)

	guard, admitted, err := BeginRuntimeAttach(ctx, db, request, nil)
	require.NoError(t, err)
	require.Equal(t, volume.ID, admitted.ID)
	require.ErrorIs(t, sandboxspec.EnsureNoPendingRuntimeAttach(ctx, spec.SandboxID), sandboxspec.ErrRuntimeAttachPending)
	_, _, err = BeginRuntimeAttach(ctx, db, request, nil)
	require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.EqualValues(t, 1, volume.InFlightCreates)

	require.NoError(t, guard.Finish(ctx, nil, true))
	require.NoError(t, sandboxspec.EnsureNoPendingRuntimeAttach(ctx, spec.SandboxID))
	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.Zero(t, volume.InFlightCreates)
	require.Zero(t, volume.RefCount)

	request.RequestID = "second"
	guard, _, err = BeginRuntimeAttach(ctx, db, request, nil)
	require.NoError(t, err)
	mount := sandboxspec.RuntimeVolumeMount{VolumeID: volume.VolumeID, VolumeRecordID: volume.ID, ContainerPath: "/data", Readonly: true}
	report := map[string][]byte{refcount.ExtInfoKey: []byte(`[{"volume_id":"shared","referenced":1}]`)}
	unrelated := models.VolumeRecord{VolumeID: "unrelated", Name: "unrelated"}
	require.NoError(t, db.Create(&unrelated).Error)
	otherGuard, _, err := BeginCreate(ctx, db, []string{unrelated.VolumeID})
	require.NoError(t, err)
	var persisted models.RuntimeVolumeAttach
	require.NoError(t, db.Take(&persisted, "request_key = ?", models.RuntimeVolumeAttachKey(request.SandboxID, request.RequestID)).Error)
	changed := persisted
	changed.VolumeID, changed.VolumeRecordID = unrelated.VolumeID, unrelated.ID
	require.ErrorContains(t, SettleRuntimeAttach(ctx, db, changed, nil, models.RuntimeAttachPublished), "request changed")
	changed = persisted
	changed.ContainerPath = "/different"
	require.ErrorContains(t, SettleRuntimeAttach(ctx, db, changed, report, models.RuntimeAttachPublished), "request changed")
	require.NoError(t, db.Take(&unrelated, unrelated.ID).Error)
	require.EqualValues(t, 1, unrelated.InFlightCreates)
	require.Zero(t, unrelated.RefCount)
	require.NoError(t, otherGuard.Finish(ctx, nil, false))
	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.EqualValues(t, 1, volume.InFlightCreates)
	require.Zero(t, volume.RefCount)
	require.NoError(t, guard.FinishRuntimeAttach(ctx, report, spec.SandboxID, spec.ID, mount))
	require.NoError(t, sandboxspec.EnsureNoPendingRuntimeAttach(ctx, spec.SandboxID))
	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.Zero(t, volume.InFlightCreates)
	require.EqualValues(t, 1, volume.RefCount)
	confirmed, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, spec.SandboxID)
	require.NoError(t, err)
	require.Equal(t, []sandboxspec.RuntimeVolumeMount{mount}, confirmed.Mounts)

	other := models.SandboxSpec{SandboxID: "sb-replaced", RequestJSON: "{}"}
	require.NoError(t, db.Create(&other).Error)
	guard, _, err = BeginRuntimeAttach(ctx, db, runtimeAttachRequestForTest(other, volume, "third"), nil)
	require.NoError(t, err)
	replacement := "other-owner"
	require.NoError(t, db.Model(&other).UpdateColumn("pending_runtime_attach", replacement).Error)
	require.ErrorContains(t, guard.FinishRuntimeAttach(ctx, report, other.SandboxID, other.ID, mount), "marker changed")

	require.NoError(t, db.Take(&volume, volume.ID).Error)
	require.EqualValues(t, 1, volume.InFlightCreates)
	require.EqualValues(t, 1, volume.RefCount)
	require.NoError(t, db.Take(&other, other.ID).Error)
	require.Equal(t, &replacement, other.PendingRuntimeAttach)
	require.Nil(t, other.RuntimeVolumeMounts)
}

func runtimeAttachRequestForTest(spec models.SandboxSpec, volume models.VolumeRecord, requestID string) models.RuntimeVolumeAttach {
	return models.RuntimeVolumeAttach{
		SandboxID: spec.SandboxID, SpecRowID: spec.ID, RequestID: requestID, ExecutionID: "vm",
		VolumeID: volume.VolumeID, ContainerPath: "/data", Readonly: true,
	}
}
