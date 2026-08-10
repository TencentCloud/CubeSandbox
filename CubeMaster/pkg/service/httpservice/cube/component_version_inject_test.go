// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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
