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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
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
