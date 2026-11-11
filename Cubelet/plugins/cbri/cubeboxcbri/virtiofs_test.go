// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenerateRestoreVirtiofsOptEqualDestinationsPreserveRequestOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		volumeMounts     []*cubebox.VolumeMounts
		wantSources      []string
		wantDestinations []string
	}{
		{
			name: "later writable mount wins",
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "read-only", ContainerPath: "/workspace/data", Readonly: true},
				{Name: "writable", ContainerPath: "/workspace/data"},
			},
			wantSources: []string{
				constants.PropagationContainerDirRo + "/read-only",
				constants.PropagationContainerDirRw + "/writable",
			},
			wantDestinations: []string{"/workspace/data", "/workspace/data"},
		},
		{
			name: "later read-only mount wins for equivalent cleaned destinations",
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "writable", ContainerPath: "/workspace/data/"},
				{Name: "read-only", ContainerPath: "/workspace/./data", Readonly: true},
			},
			wantSources: []string{
				constants.PropagationContainerDirRw + "/writable",
				constants.PropagationContainerDirRo + "/read-only",
			},
			wantDestinations: []string{"/workspace/data/", "/workspace/./data"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flowOpts := &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{
					HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
						"read-only": {
							VolumeName: "read-only",
							ShareDir:   "/data/cubelet/hostdir/sandbox",
							BindPath:   "/data/cubelet/hostdir/sandbox/read-only",
							ReadOnly:   true,
						},
						"writable": {
							VolumeName: "writable",
							ShareDir:   "/data/cubelet/hostdir/sandbox",
							BindPath:   "/data/cubelet/hostdir/sandbox/writable",
						},
					},
				},
			}
			containerReq := &cubebox.ContainerConfig{VolumeMounts: tt.volumeMounts}

			for range 32 {
				specOpts, err := generateRestoreVirtiofsOpt(context.Background(), flowOpts, containerReq)
				require.NoError(t, err)

				spec := applySpecOpts(t, context.Background(), specOpts)
				var mounts []virtiofs.VirtioMounts
				require.NoError(t, json.Unmarshal([]byte(spec.Annotations[constants.AnnotationPropagationExecMounts]), &mounts))
				require.Len(t, mounts, len(tt.wantSources))
				require.Equal(t, tt.wantSources, []string{mounts[0].VirtiofsSource, mounts[1].VirtiofsSource})
				require.Equal(t, tt.wantDestinations, []string{mounts[0].Destination, mounts[1].Destination})
			}
		})
	}
}

func TestGenerateRestoreVirtiofsOptSkipsUndeclaredVolumes(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		StorageInfo: &storage.StorageInfo{
			HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
				"matched-backend": {
					VolumeName: "matched",
					ShareDir:   "/data/cubelet/hostdir/sandbox",
					BindPath:   "/data/cubelet/hostdir/sandbox/matched",
				},
				"unmatched-backend": {
					VolumeName: "missing",
					ShareDir:   "/data/cubelet/hostdir/sandbox",
					BindPath:   "/data/cubelet/hostdir/sandbox/missing",
				},
			},
		},
	}
	tests := []struct {
		name         string
		containerReq *cubebox.ContainerConfig
		wantMounts   []virtiofs.VirtioMounts
	}{
		{
			name: "nil container request",
		},
		{
			name: "all backend volume names are unmatched",
			containerReq: &cubebox.ContainerConfig{VolumeMounts: []*cubebox.VolumeMounts{
				{Name: "other", ContainerPath: "/workspace/other"},
			}},
		},
		{
			name: "matched backend remains when another is unmatched",
			containerReq: &cubebox.ContainerConfig{VolumeMounts: []*cubebox.VolumeMounts{
				{Name: "matched", ContainerPath: "/workspace/matched"},
			}},
			wantMounts: []virtiofs.VirtioMounts{{
				VirtiofsSource: constants.PropagationContainerDirRw + "/matched",
				Destination:    "/workspace/matched",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			specOpts, err := generateRestoreVirtiofsOpt(context.Background(), flowOpts, tt.containerReq)
			require.NoError(t, err)
			if len(tt.wantMounts) == 0 {
				require.Empty(t, specOpts)
				return
			}

			spec := applySpecOpts(t, context.Background(), specOpts)
			var mounts []virtiofs.VirtioMounts
			require.NoError(t, json.Unmarshal([]byte(spec.Annotations[constants.AnnotationPropagationExecMounts]), &mounts))
			require.Equal(t, tt.wantMounts, mounts)
		})
	}
}

func TestGenerateRestoreVirtiofsOptFailsOnAnUnusableBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		backend      *storage.HostDirBackendInfo
		volumeMounts []*cubebox.VolumeMounts
		wantErr      string
	}{
		{
			name:    "declared volume with an empty container path",
			backend: &storage.HostDirBackendInfo{VolumeName: "broken", ShareDir: "/share", BindPath: "/share/broken"},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "broken"},
			},
			wantErr: "empty container path",
		},
		{
			name:    "declared volume bound outside the share dir",
			backend: &storage.HostDirBackendInfo{VolumeName: "outside", ShareDir: "/share", BindPath: "/elsewhere/outside"},
			volumeMounts: []*cubebox.VolumeMounts{
				{Name: "outside", ContainerPath: "/mnt/outside"},
			},
			wantErr: "unusable bind path",
		},
		{
			name:    "backend without a record",
			backend: nil,
			wantErr: "has no volume record",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flowOpts := &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{
					HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{"broken": tt.backend},
				},
			}
			specOpts, err := generateRestoreVirtiofsOpt(context.Background(), flowOpts,
				&cubebox.ContainerConfig{VolumeMounts: tt.volumeMounts})
			require.ErrorContains(t, err, tt.wantErr)
			require.Nil(t, specOpts)
		})
	}
}

func TestGenerateRestoreVirtiofsOptOrdersParentBeforeNestedChildForAllAccessModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		parentRO     bool
		childRO      bool
		parentSource string
		childSource  string
	}{
		{
			name:         "read-only parent and read-only child",
			parentRO:     true,
			childRO:      true,
			parentSource: constants.PropagationContainerDirRo + "/team-share",
			childSource:  constants.PropagationContainerDirRo + "/member-output",
		},
		{
			name:         "read-only parent and writable child",
			parentRO:     true,
			childRO:      false,
			parentSource: constants.PropagationContainerDirRo + "/team-share",
			childSource:  constants.PropagationContainerDirRw + "/member-output",
		},
		{
			name:         "writable parent and read-only child",
			parentRO:     false,
			childRO:      true,
			parentSource: constants.PropagationContainerDirRw + "/team-share",
			childSource:  constants.PropagationContainerDirRo + "/member-output",
		},
		{
			name:         "writable parent and writable child",
			parentRO:     false,
			childRO:      false,
			parentSource: constants.PropagationContainerDirRw + "/team-share",
			childSource:  constants.PropagationContainerDirRw + "/member-output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flowOpts := &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{
					HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
						"child": {
							VolumeName: "member-output",
							ShareDir:   "/data/cubelet/hostdir/sandbox",
							BindPath:   "/data/cubelet/hostdir/sandbox/member-output",
							ReadOnly:   tt.childRO,
						},
						"parent": {
							VolumeName: "team-share",
							ShareDir:   "/data/cubelet/hostdir/sandbox",
							BindPath:   "/data/cubelet/hostdir/sandbox/team-share",
							ReadOnly:   tt.parentRO,
						},
					},
				},
			}
			containerReq := &cubebox.ContainerConfig{
				VolumeMounts: []*cubebox.VolumeMounts{
					{Name: "member-output", ContainerPath: "/workspace/team-share/members/agent-a", Readonly: tt.childRO},
					{Name: "team-share", ContainerPath: "/workspace/team-share", Readonly: tt.parentRO},
				},
			}

			for range 32 {
				specOpts, err := generateRestoreVirtiofsOpt(context.Background(), flowOpts, containerReq)
				require.NoError(t, err)

				spec := applySpecOpts(t, context.Background(), specOpts)
				var mounts []virtiofs.VirtioMounts
				require.NoError(t, json.Unmarshal([]byte(spec.Annotations[constants.AnnotationPropagationExecMounts]), &mounts))
				require.Equal(t, []virtiofs.VirtioMounts{
					{VirtiofsSource: tt.parentSource, Destination: "/workspace/team-share"},
					{VirtiofsSource: tt.childSource, Destination: "/workspace/team-share/members/agent-a"},
				}, mounts)
			}
		})
	}
}

func TestGenerateRestoreVirtiofsOptSkipsGuestMountRestore(t *testing.T) {
	t.Parallel()

	storageInfo := &storage.StorageInfo{
		HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
			"writable": {
				VolumeName: "writable",
				ShareDir:   "/data/cubelet/hostdir/sandbox",
				BindPath:   "/data/cubelet/hostdir/sandbox/writable",
			},
		},
	}
	containerReq := &cubebox.ContainerConfig{
		VolumeMounts: []*cubebox.VolumeMounts{
			{Name: "writable", ContainerPath: "/mnt/data"},
		},
	}
	cases := []struct {
		name        string
		annotations map[string]string
	}{
		{
			name: "pause resume",
			annotations: map[string]string{
				constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
				constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
			},
		},
		{
			name: "fromsnap",
			annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID: "snap-runtime1",
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			flowOpts := &workflow.CreateContext{
				ReqInfo:     &cubebox.RunCubeSandboxRequest{Annotations: tt.annotations},
				StorageInfo: storageInfo,
			}
			specOpts, err := generateRestoreVirtiofsOpt(context.Background(), flowOpts, containerReq)
			require.NoError(t, err)
			require.Empty(t, specOpts)

			sandboxOpts, err := generateSandboxVirtiofsOpt(context.Background(), flowOpts, true)
			require.NoError(t, err)
			spec := applySpecOpts(t, context.Background(), sandboxOpts)
			require.NotEmpty(t, spec.Annotations[constants.AnnotationVirtiofs])
			require.Contains(t, spec.Annotations[constants.AnnotationVirtiofs], `"remap_filter":true`)
		})
	}
}
