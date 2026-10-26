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

func writeToolboxShimVersion(t *testing.T, root, version string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cube-shim"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cube-shim", "version.json"), []byte(`{
  "schema_version": 1,
  "components": {
    "containerd-shim-cube-rs": {"version": "`+version+`"}
  }
}`), 0o644))
}

func TestCaptureForCubeBoxFromToolbox(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cube-image"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cube-image", "version"), []byte("img-1\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cube-agent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cube-agent", "version"), []byte("agent-1\n"), 0o644))
	writeToolboxShimVersion(t, root, "shim-1")

	cb := &cubeboxstore.CubeBox{
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeKernel: {
					Component: templatetypes.MachineComponent{
						Name:    templatetypes.CubeComponentCubeKernel,
						Version: "kern-1",
					},
				},
			},
		},
	}
	captureForCubeBox(cb, root)
	require.Equal(t, "kern-1", cb.ComponentVersions[templatetypes.CubeComponentCubeKernel])
	require.Equal(t, "img-1", cb.ComponentVersions[templatetypes.CubeComponentCubeImage])
	require.Equal(t, "agent-1", cb.ComponentVersions[templatetypes.CubeComponentCubeAgent])
	require.Equal(t, "shim-1", cb.ComponentVersions[templatetypes.CubeComponentCubeShim])

	// Second capture must keep already-recorded keys (merge, not overwrite).
	cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeKernel] = templatetypes.LocalComponent{
		Component: templatetypes.MachineComponent{Name: templatetypes.CubeComponentCubeKernel, Version: "kern-2"},
	}
	captureForCubeBox(cb, root)
	require.Equal(t, "kern-1", cb.ComponentVersions[templatetypes.CubeComponentCubeKernel])
}

func TestCaptureForCubeBoxMergesMissingShim(t *testing.T) {
	root := t.TempDir()
	writeToolboxShimVersion(t, root, "shim-live")

	cb := &cubeboxstore.CubeBox{
		ComponentVersions: map[string]string{
			templatetypes.CubeComponentCubeImage:  "img-1",
			templatetypes.CubeComponentCubeAgent:  "agent-1",
			templatetypes.CubeComponentCubeKernel: "kern-1",
		},
	}
	captureForCubeBox(cb, root)
	require.Equal(t, "img-1", cb.ComponentVersions[templatetypes.CubeComponentCubeImage])
	require.Equal(t, "agent-1", cb.ComponentVersions[templatetypes.CubeComponentCubeAgent])
	require.Equal(t, "kern-1", cb.ComponentVersions[templatetypes.CubeComponentCubeKernel])
	require.Equal(t, "shim-live", cb.ComponentVersions[templatetypes.CubeComponentCubeShim])
}

func TestInventoryNameForCollectorComponent(t *testing.T) {
	name, ok := inventoryNameForCollectorComponent("guest-image")
	require.True(t, ok)
	require.Equal(t, templatetypes.CubeComponentCubeImage, name)

	name, ok = inventoryNameForCollectorComponent("kernel")
	require.True(t, ok)
	require.Equal(t, templatetypes.CubeComponentCubeKernel, name)

	name, ok = inventoryNameForCollectorComponent("cube-agent")
	require.True(t, ok)
	require.Equal(t, templatetypes.CubeComponentCubeAgent, name)

	name, ok = inventoryNameForCollectorComponent(collectorComponentShim)
	require.True(t, ok)
	require.Equal(t, templatetypes.CubeComponentCubeShim, name)

	_, ok = inventoryNameForCollectorComponent("cubelet")
	require.False(t, ok)
}
