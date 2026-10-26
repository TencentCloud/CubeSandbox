// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestInjectEnvdSidecarWrapsEntrypoint(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations:  map[string]string{constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn},
		Containers: []*types.Container{{
			Name:    "main",
			Command: []string{"python3"},
			Args:    []string{"app.py"},
		}},
	}
	out := &cubebox.RunCubeSandboxRequest{}

	injectEnvdSidecar(context.Background(), req, out)
	assert.Equal(t, []string{"/bin/sh", "-c"}, req.Containers[0].Command)
	assert.Contains(t, req.Containers[0].Args[0], constants.CubeEnvdInImagePath)
	assert.Equal(t, []string{"--", "python3", "app.py"}, req.Containers[0].Args[1:])
	assert.Contains(t, out.ExposedPorts, int64(envdDefaultPort))
}

func TestInjectEnvdSidecarDoesNotDuplicateEnvdPort(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations:  map[string]string{constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn},
		Containers: []*types.Container{{
			Name:    "main",
			Command: []string{"python3"},
			Args:    []string{"app.py"},
		}},
	}
	out := &cubebox.RunCubeSandboxRequest{ExposedPorts: []int64{envdDefaultPort}}

	injectEnvdSidecar(context.Background(), req, out)
	assert.Equal(t, []int64{envdDefaultPort}, out.ExposedPorts)
}

func TestInjectEnvdSidecarIsIdempotent(t *testing.T) {
	container := &types.Container{Name: "main", Command: []string{"sleep"}, Args: []string{"infinity"}}
	wrapEntrypointForEnvd(container)
	originalArgs := append([]string(nil), container.Args...)
	req := &types.CreateCubeSandboxReq{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations:  map[string]string{constants.CubeAnnotationsInjectEnvd: constants.CubeAnnotationsInjectEnvdOptIn},
		Containers:   []*types.Container{container},
	}
	out := &cubebox.RunCubeSandboxRequest{}

	injectEnvdSidecar(context.Background(), req, out)
	assert.Equal(t, originalArgs, container.Args)
	assert.Empty(t, out.ExposedPorts)
}

func TestInjectEnvdSidecarSkipsWhenAnnotationMissing(t *testing.T) {
	container := &types.Container{Name: "main", Command: []string{"sleep"}, Args: []string{"infinity"}}
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{}, Containers: []*types.Container{container}}
	out := &cubebox.RunCubeSandboxRequest{}

	injectEnvdSidecar(context.Background(), req, out)
	assert.Equal(t, []string{"sleep"}, container.Command)
	assert.Equal(t, []string{"infinity"}, container.Args)
	assert.Empty(t, out.ExposedPorts)
}
