// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const testShareDir = "/data/cubelet/hostdir/sandbox/rw"

func TestPlanHostDirMounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		backends     map[string]*HostDirBackendInfo
		volumeMounts []*cubebox.VolumeMounts
		want         []HostDirMount
		wantErr      string
	}{
		{
			name:     "empty storage has no plan",
			backends: nil,
		},
		{
			name: "parent destination is planned before its nested child",
			backends: map[string]*HostDirBackendInfo{
				"child":  {VolumeName: "member-output", ShareDir: testShareDir, BindPath: testShareDir + "/member-output"},
				"parent": {VolumeName: "team-share", ShareDir: testShareDir, BindPath: testShareDir + "/team-share", ReadOnly: true},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "member-output", ContainerPath: "/workspace/team-share/members/agent-a"},
				{Name: "team-share", ContainerPath: "/workspace/./team-share/", Readonly: true},
			},
			want: []HostDirMount{
				{VolumeName: "team-share", SourceSubdir: "team-share", ReadOnly: true, ContainerPath: "/workspace/./team-share/"},
				{VolumeName: "member-output", SourceSubdir: "member-output", ContainerPath: "/workspace/team-share/members/agent-a"},
			},
		},
		{
			name: "equal destinations keep request order so the later mount wins",
			backends: map[string]*HostDirBackendInfo{
				"writable":  {VolumeName: "writable", ShareDir: testShareDir, BindPath: testShareDir + "/writable"},
				"read-only": {VolumeName: "read-only", ShareDir: testShareDir, BindPath: testShareDir + "/read-only", ReadOnly: true},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "read-only", ContainerPath: "/workspace/data", Readonly: true},
				{Name: "writable", ContainerPath: "/workspace/data"},
			},
			want: []HostDirMount{
				{VolumeName: "read-only", SourceSubdir: "read-only", ReadOnly: true, ContainerPath: "/workspace/data"},
				{VolumeName: "writable", SourceSubdir: "writable", ContainerPath: "/workspace/data"},
			},
		},
		{
			name: "backend key orders the sources of one multi-source volume",
			backends: map[string]*HostDirBackendInfo{
				"hostdir-0/source-b": {VolumeName: "hostdir-0", ShareDir: testShareDir, BindPath: testShareDir + "/source-b"},
				"hostdir-0/source-a": {VolumeName: "hostdir-0", ShareDir: testShareDir, BindPath: testShareDir + "/source-a"},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "hostdir-0", ContainerPath: "/workspace/data"},
			},
			want: []HostDirMount{
				{VolumeName: "hostdir-0", SourceSubdir: "source-a", ContainerPath: "/workspace/data"},
				{VolumeName: "hostdir-0", SourceSubdir: "source-b", ContainerPath: "/workspace/data"},
			},
		},
		{
			name: "a nested bind path is exposed by its base name",
			backends: map[string]*HostDirBackendInfo{
				"nested": {VolumeName: "nested", ShareDir: testShareDir, BindPath: testShareDir + "/team/member"},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "nested", ContainerPath: "/workspace/data"},
			},
			want: []HostDirMount{
				{VolumeName: "nested", SourceSubdir: "member", ContainerPath: "/workspace/data"},
			},
		},
		{
			name: "backends without a matching container mount are skipped",
			backends: map[string]*HostDirBackendInfo{
				"matched":   {VolumeName: "matched", ShareDir: testShareDir, BindPath: testShareDir + "/matched"},
				"unmatched": {VolumeName: "missing", ShareDir: testShareDir, BindPath: testShareDir + "/missing"},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "matched", ContainerPath: "/workspace/matched"},
			},
			want: []HostDirMount{
				{VolumeName: "matched", SourceSubdir: "matched", ContainerPath: "/workspace/matched"},
			},
		},
		{
			name: "bind path outside the share dir fails the plan",
			backends: map[string]*HostDirBackendInfo{
				"outside": {VolumeName: "outside", ShareDir: testShareDir, BindPath: "/data/elsewhere/outside"},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "outside", ContainerPath: "/workspace/outside"},
			},
			wantErr: "unusable bind path",
		},
		{
			name: "bind path equal to the share dir fails the plan",
			backends: map[string]*HostDirBackendInfo{
				"root": {VolumeName: "root", ShareDir: testShareDir, BindPath: testShareDir},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "root", ContainerPath: "/workspace/root"},
			},
			wantErr: "unusable bind path",
		},
		{
			name: "a declared volume with an empty container path fails the plan",
			backends: map[string]*HostDirBackendInfo{
				"no-path": {VolumeName: "no-path", ShareDir: testShareDir, BindPath: testShareDir + "/no-path"},
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "no-path"},
			},
			wantErr: "empty container path",
		},
		{
			name: "a backend without a record fails the plan",
			backends: map[string]*HostDirBackendInfo{
				"broken": nil,
			},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "broken", ContainerPath: "/workspace/broken"},
			},
			wantErr: "has no volume record",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info := &StorageInfo{HostDirBackendInfos: tt.backends}
			plan, err := info.PlanHostDirMounts(context.Background(), tt.volumeMounts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Nil(t, plan)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, plan)
		})
	}
}

func TestPlanHostDirMountsNilStorage(t *testing.T) {
	t.Parallel()

	var info *StorageInfo
	plan, err := info.PlanHostDirMounts(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, plan)
}
