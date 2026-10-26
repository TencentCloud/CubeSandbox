// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestFillResumeRecreateFieldsFromCreateReq(t *testing.T) {
	cfg := config.GetConfig()
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.CubeletConf)
	prevEnable := cfg.CubeletConf.EnableExposedPort
	cfg.CubeletConf.EnableExposedPort = false
	defer func() { cfg.CubeletConf.EnableExposedPort = prevEnable }()

	allow := false
	out := &cubebox.RunCubeSandboxRequest{
		RequestID: "req-1",
		Annotations: map[string]string{
			"pause": "binding",
		},
	}
	fillResumeRecreateFields(context.Background(), "sb-net", out, &types.CreateCubeSandboxReq{
		InstanceType:   "cubebox",
		NetworkType:    "tap",
		RuntimeHandler: "cube",
		Annotations:    map[string]string{},
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowInternetAccess: &allow,
			AllowOut:            []string{"10.0.0.0/8"},
			DenyOut:             []string{"0.0.0.0/0"},
		},
	})

	require.NotNil(t, out.CubeNetworkConfig)
	require.NotNil(t, out.CubeNetworkConfig.AllowInternetAccess)
	assert.False(t, *out.CubeNetworkConfig.AllowInternetAccess)
	assert.Equal(t, []string{"10.0.0.0/8"}, out.CubeNetworkConfig.AllowOut)
	assert.Equal(t, []string{"0.0.0.0/0"}, out.CubeNetworkConfig.DenyOut)
	assert.Equal(t, "tap", out.NetworkType)
	assert.Equal(t, "cube", out.RuntimeHandler)
	assert.Equal(t, "binding", out.Annotations["pause"])
}
