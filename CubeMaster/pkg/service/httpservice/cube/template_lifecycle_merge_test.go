// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestApplyTemplateToExistingContainerPreservesLifecycle(t *testing.T) {
	templateHooks := &cubeboxv1.TemplateLifecycleCommands{
		PostStart: []string{"/bin/init"}, PreStop: []string{"/bin/flush"},
	}
	for _, tc := range []struct {
		name  string
		hooks *cubeboxv1.TemplateLifecycleCommands
	}{
		{name: "inherit"},
		{name: "explicit override", hooks: &cubeboxv1.TemplateLifecycleCommands{PostStart: []string{"/bin/custom-init"}}},
		{name: "explicit empty override", hooks: &cubeboxv1.TemplateLifecycleCommands{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{Containers: []*types.Container{{Name: "override", Lifecycle: tc.hooks}}}
			require.NoError(t, applyTemplateToContainer(req.Containers[0], &types.Container{Lifecycle: templateHooks}, 0))
			want := tc.hooks
			if want == nil {
				want = templateHooks
			}
			require.Equal(t, want, req.Containers[0].Lifecycle)
			require.Equal(t, "override", req.Containers[0].Name)
		})
	}
}
