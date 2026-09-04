// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCreateRequestReferencesPluginVolume(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{
			"plugin-volume-mounts": `[{"name":"data","container_path":"/mnt/data"}]`,
		},
		Volumes: []*sandboxtypes.Volume{{Name: "data"}},
	}

	referenced, err := createRequestReferencesPluginVolume(req, "data")
	require.NoError(t, err)
	assert.True(t, referenced)

	referenced, err = createRequestReferencesPluginVolume(req, "other")
	require.NoError(t, err)
	assert.False(t, referenced)
}

func TestCreateRequestReferencesPluginVolumeRejectsMalformedMetadata(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{"plugin-volume-mounts": `{bad`},
	}
	_, err := createRequestReferencesPluginVolume(req, "data")
	require.Error(t, err)
}
