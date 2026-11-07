// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// Guest-affecting cube.master.* request annotations must reach the shim's
// OCI spec annotations; platform-internal cube.master.* keys must not.
func TestCreateSandboxGuestAnnotationPassthrough(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationCgroupMode:           "legacy",
				constants.MasterAnnotationEnableIvshmem:        "true",
				constants.MasterAnnotationInstanceType:         "cubebox",
				constants.MasterAnnotationComponentEnvdVersion: "0.0.1",
			},
		},
	}

	specOpts, err := plugin.CreateSandbox(context.Background(), flowOpts)
	require.NoError(t, err)

	spec := applySpecOpts(t, context.Background(), specOpts)
	require.Equal(t, "legacy", spec.Annotations[constants.MasterAnnotationCgroupMode])
	require.Equal(t, "true", spec.Annotations[constants.MasterAnnotationEnableIvshmem])
	require.NotContains(t, spec.Annotations, constants.MasterAnnotationInstanceType)
	require.NotContains(t, spec.Annotations, constants.MasterAnnotationComponentEnvdVersion)
}
