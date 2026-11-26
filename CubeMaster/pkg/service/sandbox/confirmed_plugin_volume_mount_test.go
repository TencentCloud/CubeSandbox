// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
)

func TestGetEffectiveSandboxSpecReloadsConfirmedMount(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "spec.db")
	database, err := gorm.Open(sqlite.Open(filename), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, database.AutoMigrate(&models.SandboxSpec{}))
	require.NoError(t, database.Exec("CREATE UNIQUE INDEX idx_sandbox_spec_id ON t_cube_sandbox_spec(sandbox_id)").Error)
	require.NoError(t, sandboxspec.Init(database))

	const sandboxID = "sb-confirmed-mount"
	base := &types.CreateCubeSandboxReq{
		Volumes:     []*types.Volume{{Name: "work"}},
		Containers:  []*types.Container{{Name: "main"}},
		Annotations: map[string]string{AnnotationPluginVolumeMounts: `[{"name":"work","container_path":"/work","readonly":true}]`},
	}
	require.NoError(t, injectPluginVolumeMounts(ctx, base))
	require.NoError(t, sandboxspec.Put(ctx, sandboxID, base, sandboxspec.PutOptions{}))
	before, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.Empty(t, before.Mounts)
	mount := sandboxspec.RuntimeVolumeMount{VolumeID: "dataset", VolumeRecordID: 17, ContainerPath: "/data", Readonly: true}
	require.NoError(t, sandboxspec.RecordRuntimeVolumeMount(ctx, sandboxID, before.RowID, mount))
	require.NoError(t, sqlDB.Close())

	reopened, err := gorm.Open(sqlite.Open(filename), &gorm.Config{})
	require.NoError(t, err)
	reopenedSQL, err := reopened.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopenedSQL.Close()) })
	require.NoError(t, sandboxspec.Init(reopened))

	effective, mounts, err := GetEffectiveSandboxSpec(ctx, sandboxID)
	require.NoError(t, err)
	require.Equal(t, []sandboxspec.RuntimeVolumeMount{mount}, mounts)
	require.Equal(t, []*types.Volume{{Name: "work"}, {Name: "dataset"}}, effective.Volumes)
	require.Equal(t, []*cubeboxv1.VolumeMounts{
		{Name: "work", ContainerPath: "/work", Readonly: true},
		{Name: "dataset", ContainerPath: "/data", Readonly: true},
	}, effective.Containers[0].VolumeMounts)
	persistedBase, err := sandboxspec.Get(ctx, sandboxID)
	require.NoError(t, err)
	require.Equal(t, before.Request, persistedBase)
}

func TestWithConfirmedPluginVolumeMount(t *testing.T) {
	for _, tt := range []struct {
		name     string
		readonly bool
		writable []string
	}{
		{name: "readonly", readonly: true},
		{name: "writable", writable: []string{"dataset"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const work = `{"name":"work","container_path":"/work","readonly":true,"extra":{"tag":7}}`
			base := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{AnnotationPluginVolumeMounts: "[" + work + "]", AnnotationPluginVolumeSources: "node-only"},
				Volumes:     []*types.Volume{{Name: "work"}},
				Containers:  []*types.Container{{Name: "main"}, {Name: "sidecar"}},
				Labels:      map[string]string{"project": "original"},
			}
			ctx := context.Background()
			require.NoError(t, injectPluginVolumeMounts(ctx, base))
			before, err := json.Marshal(base)
			require.NoError(t, err)
			mount := pluginVolumeMountEntry{Name: "dataset", ContainerPath: "/data", Readonly: tt.readonly}
			projected, err := withConfirmedPluginVolumeMount(ctx, base, mount)
			require.NoError(t, err)
			require.Equal(t, []*types.Volume{{Name: "work"}, {Name: "dataset"}}, projected.Volumes)
			require.NotContains(t, projected.Annotations, AnnotationPluginVolumeSources)
			var entries []json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(projected.Annotations[AnnotationPluginVolumeMounts]), &entries))
			require.Len(t, entries, 2)
			require.JSONEq(t, work, string(entries[0]))
			var added pluginVolumeMountEntry
			require.NoError(t, json.Unmarshal(entries[1], &added))
			require.Equal(t, mount, added)
			require.Len(t, projected.Containers, 2)
			for _, container := range projected.Containers {
				require.Len(t, container.VolumeMounts, 2)
				require.Equal(t, "/work", container.VolumeMounts[0].GetContainerPath())
				require.Equal(t, mount.Name, container.VolumeMounts[1].GetName())
				require.Equal(t, mount.ContainerPath, container.VolumeMounts[1].GetContainerPath())
				require.Equal(t, mount.Readonly, container.VolumeMounts[1].GetReadonly())
			}
			writable, err := WritablePluginVolumeMounts(projected)
			require.NoError(t, err)
			require.Equal(t, tt.writable, writable)
			replay, err := withConfirmedPluginVolumeMount(ctx, projected, mount)
			require.NoError(t, err)
			require.Equal(t, projected, replay)

			// Capturing the new spec must not change an earlier snapshot.
			projected.Labels["project"] = "changed"
			projected.Containers[0].VolumeMounts[0].Readonly = false
			after, err := json.Marshal(base)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestWithConfirmedPluginVolumeMountFirstBinding(t *testing.T) {
	base := &types.CreateCubeSandboxReq{Containers: []*types.Container{{Name: "main"}}}
	projected, err := withConfirmedPluginVolumeMount(context.Background(), base, pluginVolumeMountEntry{Name: "dataset", ContainerPath: "/data"})
	require.NoError(t, err)
	require.Len(t, projected.Volumes, 1)
	require.Len(t, projected.Containers[0].VolumeMounts, 1)
	require.Nil(t, base.Annotations)
	require.Empty(t, base.Volumes)
	require.Empty(t, base.Containers[0].VolumeMounts)
}

func TestWithConfirmedPluginVolumeMountRejectsNonPluginReplay(t *testing.T) {
	base := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{AnnotationPluginVolumeMounts: `[{"name":"cache","container_path":"/cache"}]`},
		Volumes:     []*types.Volume{{Name: "cache", VolumeSource: &types.VolumeSource{EmptyDir: &types.EmptyDirVolumeSource{}}}},
		Containers:  []*types.Container{{Name: "main"}},
	}
	projected, err := withConfirmedPluginVolumeMount(context.Background(), base, pluginVolumeMountEntry{Name: "cache", ContainerPath: "/cache"})
	require.ErrorContains(t, err, "non-plugin source")
	require.Nil(t, projected)
	require.Empty(t, base.Containers[0].VolumeMounts)
}

func TestWithConfirmedPluginVolumeMountConflict(t *testing.T) {
	base := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{AnnotationPluginVolumeMounts: `[{"name":"work","container_path":"/work","readonly":true}]`},
		Volumes:     []*types.Volume{{Name: "work"}, {Name: "cache", VolumeSource: &types.VolumeSource{EmptyDir: &types.EmptyDirVolumeSource{}}}},
		Containers: []*types.Container{{Name: "main"}, {
			Name:         "sidecar",
			VolumeMounts: []*cubeboxv1.VolumeMounts{{Name: "cache", ContainerPath: "/cache"}},
		}},
	}
	ctx := context.Background()
	require.NoError(t, injectPluginVolumeMounts(ctx, base))
	before, err := json.Marshal(base)
	require.NoError(t, err)
	for _, tt := range []struct {
		name  string
		mount pluginVolumeMountEntry
	}{
		{"same volume different path", pluginVolumeMountEntry{Name: "work", ContainerPath: "/data", Readonly: true}},
		{"same volume different mode", pluginVolumeMountEntry{Name: "work", ContainerPath: "/work"}},
		{"same target different volume", pluginVolumeMountEntry{Name: "dataset", ContainerPath: "/work/."}},
		{"volume name belongs to emptyDir", pluginVolumeMountEntry{Name: "cache", ContainerPath: "/data"}},
		{"only the second container conflicts", pluginVolumeMountEntry{Name: "dataset", ContainerPath: "/cache"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			projected, err := withConfirmedPluginVolumeMount(ctx, base, tt.mount)
			require.Error(t, err)
			require.Nil(t, projected)
			after, err := json.Marshal(base)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
