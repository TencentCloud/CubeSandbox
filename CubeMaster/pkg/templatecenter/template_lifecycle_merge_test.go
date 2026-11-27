// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestApplyTemplateRequestToExistingContainerPreservesLifecycle(t *testing.T) {
	templateHooks := &cubeboxv1.TemplateLifecycleCommands{
		PostStart: []string{"/bin/init"}, PreStop: []string{"/bin/flush"},
	}
	for _, tc := range []struct {
		name  string
		hooks *cubeboxv1.TemplateLifecycleCommands
	}{
		{name: "inherit"},
		{name: "explicit override", hooks: &cubeboxv1.TemplateLifecycleCommands{PreStop: []string{"/bin/custom-flush"}}},
		{name: "explicit empty override", hooks: &cubeboxv1.TemplateLifecycleCommands{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{Containers: []*types.Container{{Name: "override", Lifecycle: tc.hooks}}}
			template := &types.CreateCubeSandboxReq{Containers: []*types.Container{{Lifecycle: templateHooks}}}
			require.NoError(t, applyTemplateRequest(template, req))
			want := tc.hooks
			if want == nil {
				want = templateHooks
			}
			require.Equal(t, want, req.Containers[0].Lifecycle)
			require.Equal(t, "override", req.Containers[0].Name)
		})
	}
}
