// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplyTemplateToContainerCreateTimeEnvOverridesTemplateDefaults(t *testing.T) {
	template := &types.Container{
		Name:    "main",
		Command: []string{"/bin/sh", "-c", "sleep infinity"},
		Args:    []string{"ignored"},
		Envs: []*types.KeyValue{
			{Key: "BASE_ENV", Value: "from-template"},
			{Key: "OVERRIDE_ME", Value: "template-value"},
		},
		Image: &types.ImageSpec{Image: "tpl-image"},
	}

	request := &types.Container{
		Envs: []*types.KeyValue{
			{Key: "OVERRIDE_ME", Value: "create-value"},
			{Key: "CREATE_ONLY", Value: "create-only"},
		},
	}

	require.NoError(t, applyTemplateToContainer(request, template, 0))

	assert.Equal(t, []string{"/bin/sh", "-c", "sleep infinity"}, request.Command)
	assert.Equal(t, []string{"ignored"}, request.Args)
	require.Len(t, request.Envs, 4)
	assert.Equal(t, &types.KeyValue{Key: "BASE_ENV", Value: "from-template"}, request.Envs[0])
	assert.Equal(t, &types.KeyValue{Key: "OVERRIDE_ME", Value: "template-value"}, request.Envs[1])
	assert.Equal(t, &types.KeyValue{Key: "OVERRIDE_ME", Value: "create-value"}, request.Envs[2])
	assert.Equal(t, &types.KeyValue{Key: "CREATE_ONLY", Value: "create-only"}, request.Envs[3])
}

func TestApplyTemplateToContainerPreservesExplicitCommandAndArgs(t *testing.T) {
	template := &types.Container{
		Command: []string{"/entrypoint.sh"},
		Args:    []string{"template-arg"},
		Image:   &types.ImageSpec{Image: "tpl-image"},
	}
	request := &types.Container{
		Command: []string{"/custom-entrypoint"},
		Args:    []string{"request-arg"},
	}

	require.NoError(t, applyTemplateToContainer(request, template, 0))

	assert.Equal(t, []string{"/custom-entrypoint"}, request.Command)
	assert.Equal(t, []string{"request-arg"}, request.Args)
}
