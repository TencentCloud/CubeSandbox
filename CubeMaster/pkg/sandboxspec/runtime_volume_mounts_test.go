// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandboxspec

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func TestGetRuntimeVolumeMountsMany(t *testing.T) {
	database := setupSpecStore(t)
	ctx := t.Context()
	previous := getManyBatchSize
	getManyBatchSize = 2
	t.Cleanup(func() { getManyBatchSize = previous })

	valid := `[{"volume_id":"dataset","volume_record_id":17,"container_path":"/data","readonly":true}]`
	corrupt := `[{"volume_id":"dataset","volume_record_id":17,"container_path":"/data"}]`
	empty := `[]`
	rows := []models.SandboxSpec{
		{SandboxID: "sb-first", RequestJSON: "{", RuntimeVolumeMounts: &valid},
		{SandboxID: "sb-corrupt", RequestJSON: "{}", RuntimeVolumeMounts: &corrupt},
		{SandboxID: "sb-second", RequestJSON: "{}", RuntimeVolumeMounts: &valid},
		{SandboxID: "sb-deleted", RequestJSON: "{}", RuntimeVolumeMounts: &valid},
		{SandboxID: "sb-legacy", RequestJSON: "{}"},
		{SandboxID: "sb-empty", RequestJSON: "{}", RuntimeVolumeMounts: &empty},
	}
	require.NoError(t, database.Create(&rows).Error)
	for _, row := range rows {
		if row.RuntimeVolumeMounts != nil {
			require.NoError(t, database.Model(&row).UpdateColumn("runtime_volume_mounts", *row.RuntimeVolumeMounts).Error)
		}
	}
	require.NoError(t, Delete(ctx, "sb-deleted"))
	var deleted models.SandboxSpec
	require.NoError(t, database.Unscoped().Where("sandbox_id = ?", "sb-deleted").Take(&deleted).Error)
	require.True(t, deleted.DeletedAt.Valid)
	require.Equal(t, &valid, deleted.RuntimeVolumeMounts)
	ids := []string{" sb-first ", "sb-corrupt", "sb-second", "sb-deleted", "sb-legacy", "sb-empty", "sb-missing", "sb-first", ""}
	mount := RuntimeVolumeMount{VolumeID: "dataset", VolumeRecordID: 17, ContainerPath: "/data", Readonly: true}
	want := map[string][]RuntimeVolumeMount{"sb-first": {mount}, "sb-second": {mount}}

	queries := 0
	failSecond := false
	batchErr := errors.New("second batch read failed")
	require.NoError(t, database.Callback().Query().Before("gorm:query").Register("test_runtime_mount_batch", func(tx *gorm.DB) {
		queries++
		require.Equal(t, []string{"sandbox_id", "runtime_volume_mounts"}, tx.Statement.Selects)
		if failSecond && queries == 2 {
			_ = tx.AddError(batchErr)
		}
	}))
	emptyResult, err := GetRuntimeVolumeMountsMany(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, emptyResult)
	require.Zero(t, queries)

	got, err := GetRuntimeVolumeMountsMany(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 4, queries)

	queries = 0
	failSecond = true
	got, err = GetRuntimeVolumeMountsMany(ctx, ids)
	require.ErrorIs(t, err, batchErr)
	require.Equal(t, map[string][]RuntimeVolumeMount{"sb-first": {mount}}, got)
	require.Equal(t, 2, queries)
}

func TestRuntimeVolumeMountsSurviveBaseSpecPut(t *testing.T) {
	database := setupSpecStore(t)
	ctx := t.Context()
	const sandboxID = "sb-runtime-volume"
	putSpec(t, sandboxID, "before")

	before, err := GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.NotZero(t, before.RowID)
	require.Nil(t, before.Mounts)

	dataset := RuntimeVolumeMount{VolumeID: "dataset", VolumeRecordID: 17, ContainerPath: "/data", Readonly: true}
	require.NoError(t, RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, dataset))
	require.NoError(t, RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, dataset))
	conflict := dataset
	conflict.VolumeRecordID++
	require.ErrorContains(t, RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, conflict), "conflicts")

	stale := before.Request
	host := "localhost:${PORT}"
	stale.CubeNetworkConfig = &sandboxtypes.CubeNetworkConfig{MaskRequestHost: &host}
	require.NoError(t, Put(ctx, sandboxID, stale, PutOptions{}))

	base, err := Get(ctx, sandboxID)
	require.NoError(t, err)
	require.Equal(t, stale, base)

	logs := RuntimeVolumeMount{VolumeID: "logs", VolumeRecordID: 18, ContainerPath: "/logs", Readonly: true}
	require.NoError(t, RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, logs))
	after, err := GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.Equal(t, before.RowID, after.RowID)
	require.Equal(t, base, after.Request)
	require.Equal(t, []RuntimeVolumeMount{dataset, logs}, after.Mounts)

	// Corrupt mount data must fail the explicit read and refuse further writes.
	for _, malformed := range []string{
		"{",
		"null",
		`[{"volume_id":"dataset","volume_record_id":17,"container_path":"/data"}]`,
		`[{"volume_id":"dataset","volume_record_id":17,"container_path":"/data","readonly":null}]`,
	} {
		require.NoError(t, database.Model(&models.SandboxSpec{}).Where("id = ?", before.RowID).
			UpdateColumn("runtime_volume_mounts", malformed).Error)
		_, err = GetWithRuntimeVolumeMounts(ctx, sandboxID)
		require.ErrorContains(t, err, "decode runtime volume mounts")
		require.ErrorContains(t, RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, logs), "decode runtime volume mounts")
	}
	base, err = Get(ctx, sandboxID)
	require.NoError(t, err)
	require.Equal(t, stale, base)
}

func TestRecordRuntimeVolumeMountRejectsObsoleteSpecRow(t *testing.T) {
	database := setupSpecStore(t)
	ctx := t.Context()
	const sandboxID = "sb-recreated"
	mount := RuntimeVolumeMount{VolumeID: "dataset", VolumeRecordID: 17, ContainerPath: "/data", Readonly: true}
	require.ErrorIs(t, RecordRuntimeVolumeMount(ctx, sandboxID, 1, mount), ErrSandboxSpecNotFound)

	putSpec(t, sandboxID, "old")
	old, err := GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.NoError(t, Delete(ctx, sandboxID))
	require.ErrorIs(t, RecordRuntimeVolumeMount(ctx, sandboxID, old.RowID, mount), ErrSandboxSpecNotFound)

	require.NoError(t, database.Unscoped().Delete(&models.SandboxSpec{}, old.RowID).Error)
	putSpec(t, sandboxID, "new")
	current, err := GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.NotEqual(t, old.RowID, current.RowID)
	require.ErrorIs(t, RecordRuntimeVolumeMount(ctx, sandboxID, old.RowID, mount), ErrSandboxSpecNotFound)
	current, err = GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.Empty(t, current.Mounts)
}
