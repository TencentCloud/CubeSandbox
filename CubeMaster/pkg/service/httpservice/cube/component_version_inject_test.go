// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"testing"

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

func TestReplicaShimBindingReplacesCallerAnnotation(t *testing.T) {
	for _, version := range []string{"selected-shim", ""} {
		annotations := map[string]string{constants.CubeAnnotationComponentCubeShimVersion: "caller-shim"}
		injectReplicaComponentVersionAnnotations(annotations, templatecenter.ReplicaStatus{ShimVersion: version})
		require.Equal(t, version, annotations[constants.CubeAnnotationComponentCubeShimVersion])
		if version == "" {
			require.NotContains(t, annotations, constants.CubeAnnotationComponentCubeShimVersion)
		}
	}
}

func TestTemplateMergeCannotReplaceSelectedReplicaShim(t *testing.T) {
	for _, selected := range []string{"selected", ""} {
		t.Run(selected, func(t *testing.T) {
			request := &types.CreateCubeSandboxReq{Annotations: map[string]string{constants.CubeAnnotationComponentCubeShimVersion: "caller"}}
			snapshot := &types.CreateCubeSandboxReq{Annotations: map[string]string{constants.CubeAnnotationComponentCubeShimVersion: "snapshot", "custom": "retained"}}
			injectReplicaComponentVersionAnnotations(request.Annotations, templatecenter.ReplicaStatus{ShimVersion: selected})
			applyTemplateAnnotationsAndLabels(snapshot, request)
			require.Equal(t, selected, request.Annotations[constants.CubeAnnotationComponentCubeShimVersion])
			require.Equal(t, "retained", request.Annotations["custom"])
		})
	}
}
