// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestInjectReplicaComponentVersionAnnotationsIncludesShim(t *testing.T) {
	annotations := map[string]string{}
	injectReplicaComponentVersionAnnotations(annotations, templatecenter.ReplicaStatus{
		GuestImageVersion: "img-1",
		AgentVersion:      "agent-1",
		KernelVersion:     "kern-1",
		ShimVersion:       "shim-1",
	})
	require.Equal(t, "img-1", annotations[constants.CubeAnnotationComponentCubeImageVersion])
	require.Equal(t, "agent-1", annotations[constants.CubeAnnotationComponentCubeAgentVersion])
	require.Equal(t, "kern-1", annotations[constants.CubeAnnotationComponentCubeKernelVersion])
	require.Equal(t, "shim-1", annotations[constants.CubeAnnotationComponentCubeShimVersion])
}

func TestSnapshotCreateBindingWithCapturedRuntimeMount(t *testing.T) {
	for _, mode := range []snapshotmode.Mode{snapshotmode.Full, snapshotmode.FsOnly} {
		t.Run(mode.String(), func(t *testing.T) {
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			metadataKey := constants.CubeAnnotationSnapshotRuntimeVolumeMounts
			patches.ApplyFunc(templatecenter.GetTemplateRequest, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
				return &types.CreateCubeSandboxReq{Backend: "xfs", Annotations: map[string]string{
					metadataKey:                          `[{"volume_id":"data","volume_record_id":7,"container_path":"/data","readonly":true}]`,
					sandbox.AnnotationPluginVolumeMounts: `[{"name":"data","container_path":"/data","readonly":true}]`,
				}, Volumes: []*types.Volume{{Name: "data"}}}, nil
			})
			patches.ApplyFunc(templatecenter.GetTemplateKind, func(context.Context, string) (string, error) {
				return templatecenter.TemplateKindSnapshot, nil
			})
			patches.ApplyFunc(templatecenter.EnsureTemplateLocalityReady, func(context.Context, string, string) error { return nil })
			oldSource, oldReady, oldReplica := getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn, resolveSnapshotReadyReplicaFn
			t.Cleanup(func() {
				getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn, resolveSnapshotReadyReplicaFn = oldSource, oldReady, oldReplica
			})
			getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
				return &templatecenter.RestoreSource{Mode: mode, Backend: "xfs", OriginNodeID: "node-a", OriginNodeIP: "127.0.0.1"}, nil
			}
			ensureSnapshotReadyForNewUseFn = func(context.Context, string) error { return nil }
			resolveSnapshotReadyReplicaFn = func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
				return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
			}
			req := &types.CreateCubeSandboxReq{Annotations: map[string]string{metadataKey: "user value"}, Volumes: []*types.Volume{{Name: "caller-volume"}}}
			err := dealCubeboxCreateReqWithTemplateCenter(t.Context(), "snap-data", req)
			if mode.HasMemory() {
				require.ErrorIs(t, err, templatecenter.ErrTemplateAttemptInProgress)
				return
			}
			require.NoError(t, err)
			require.NotContains(t, req.Annotations, metadataKey)
			require.NotContains(t, req.Annotations, sandbox.AnnotationPluginVolumeMounts)
			require.Len(t, req.Volumes, 1)
			require.Equal(t, "caller-volume", req.Volumes[0].Name)
		})
	}
}

func TestLocalTemplateShimPinOverridesCaller(t *testing.T) {
	key := constants.CubeAnnotationComponentCubeShimVersion
	configured := &types.CreateCubeSandboxReq{Annotations: map[string]string{key: "operator-shim"}}
	requested := &types.CreateCubeSandboxReq{Annotations: map[string]string{key: "caller-shim"}}
	applyTemplateAnnotationsAndLabels(configured, requested)
	injectReplicaComponentVersionAnnotations(requested.Annotations, templatecenter.ReplicaStatus{ShimVersion: "replica-shim"})
	require.Equal(t, "operator-shim", requested.Annotations[key])
}

// Exercise the actual kind dispatch, template merge and replica injection.
func TestMasterShimBindingCreateFlow(t *testing.T) {
	for _, tc := range []struct {
		name, kind, pin, replica, want string
	}{
		{"template_operator", templatecenter.TemplateKindTemplate, "operator-shim", "replica-shim", "operator-shim"},
		{"template_replica", templatecenter.TemplateKindTemplate, "", "replica-shim", "replica-shim"},
		{"snapshot_replica", templatecenter.TemplateKindSnapshot, "saved-shim", "replica-shim", "replica-shim"},
		{"snapshot_unbound", templatecenter.TemplateKindSnapshot, "saved-shim", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			key := constants.CubeAnnotationComponentCubeShimVersion
			patches.ApplyFunc(templatecenter.GetTemplateRequest, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
				return &types.CreateCubeSandboxReq{Backend: "xfs", Annotations: map[string]string{key: tc.pin}}, nil
			})
			patches.ApplyFunc(templatecenter.GetTemplateKind, func(context.Context, string) (string, error) { return tc.kind, nil })
			patches.ApplyFunc(templatecenter.EnsureTemplateLocalityReady, func(context.Context, string, string) error { return nil })
			oldSource, oldReady := getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn
			oldSnapshot, oldTemplate := resolveSnapshotReadyReplicaFn, resolveTemplateReadyReplicaFn
			defer func() {
				getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn = oldSource, oldReady
				resolveSnapshotReadyReplicaFn, resolveTemplateReadyReplicaFn = oldSnapshot, oldTemplate
			}()
			getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
				return &templatecenter.RestoreSource{Backend: "xfs", OriginNodeID: "node-a", OriginNodeIP: "127.0.0.1"}, nil
			}
			ensureSnapshotReadyForNewUseFn = func(context.Context, string) error { return nil }
			replica := func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
				return templatecenter.ReplicaStatus{NodeID: "node-a", ShimVersion: tc.replica}, nil
			}
			resolveSnapshotReadyReplicaFn = replica
			resolveTemplateReadyReplicaFn = replica
			req := &types.CreateCubeSandboxReq{Annotations: map[string]string{key: "caller-shim"}}
			require.NoError(t, dealCubeboxCreateReqWithTemplateCenter(context.Background(), "template-a", req))
			require.Equal(t, tc.want, req.Annotations[key])
		})
	}
}
