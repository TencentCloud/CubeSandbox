// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestCubeRuntimeBesideShimPath(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "cube-runtime")
	shim := filepath.Join(dir, "containerd-shim-cube-rs")
	require.NoError(t, os.WriteFile(runtime, []byte("rt"), 0o755))
	require.NoError(t, os.WriteFile(shim, []byte("shim"), 0o755))

	cb := &cubeboxstore.CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {
					Component: templatetypes.MachineComponent{
						Name: templatetypes.CubeComponentCubeShim,
						Path: shim,
					},
				},
			},
		},
	}
	require.Equal(t, runtime, cubeRuntimeBesideShimPath(cb))
}

func TestCubeRuntimeBesideShimPathMissing(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeShim: {
					Component: templatetypes.MachineComponent{
						Path: "/no/such/bin/containerd-shim-cube-rs",
					},
				},
			},
		},
	}
	require.Empty(t, cubeRuntimeBesideShimPath(cb))
}
