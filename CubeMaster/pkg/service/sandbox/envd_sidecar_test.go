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
	// The shell snippet must start envd in the background, run the original
	// command also in the background, and wait on envd so that the container
	// lifetime is governed by envd rather than by the original command.
	assert.Contains(t, req.Containers[0].Args[0], constants.CubeEnvdInImagePath)
	assert.Contains(t, req.Containers[0].Args[0], "ENVD_PID=$!")
	assert.Contains(t, req.Containers[0].Args[0], "wait $ENVD_PID")
	assert.Equal(t, []string{"--", "python3", "app.py"}, req.Containers[0].Args[1:])
	assert.Contains(t, out.ExposedPorts, int64(envdDefaultPort))
}

// TestInjectEnvdSidecarTTYImageSurvives verifies that a tty-type image whose
// default Cmd is a bare interactive shell (e.g. "bash") does not cause the
// container to exit before envd's health check can pass. The original command
// must be placed in the background ("$@" &) so its immediate exit does not
// terminate the container, and the shell must wait on envd instead.
func TestInjectEnvdSidecarTTYImageSurvives(t *testing.T) {
	for _, shell := range []string{"bash", "sh", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			container := &types.Container{
				Name: "main",
				// Simulate a tty-type image: no Entrypoint, Cmd is a bare shell.
				Command: nil,
				Args:    []string{shell},
			}
			wrapEntrypointForEnvd(container)

			snippet := container.Args[0]
			// envd must be backgrounded.
			assert.Contains(t, snippet, constants.CubeEnvdInImagePath)
			assert.Contains(t, snippet, "ENVD_PID=$!")
			// The original command must also run in the background so that its
			// immediate exit (no tty) does not kill the container.
			assert.Contains(t, snippet, `"$@" &`)
			// The container lifetime must be tied to envd, not the original cmd.
			assert.Contains(t, snippet, "wait $ENVD_PID")
			// The shell is still passed as the original command so it can be
			// exec'd when a user later attaches a tty.
			assert.Equal(t, shell, container.Args[len(container.Args)-1])
		})
	}
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
