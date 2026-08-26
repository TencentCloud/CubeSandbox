// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestEnvdVersionFromCubeBoxUsesCreateAnnotation(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{Annotations: map[string]string{
		constants.MasterAnnotationComponentEnvdVersion: " 0.2.0 ",
	}}}
	require.Equal(t, "0.2.0", envdVersionFromCubeBox(cb))
	require.Empty(t, envdVersionFromCubeBox(&cubeboxstore.CubeBox{}))
	require.Empty(t, envdVersionFromCubeBox(nil))
}

func TestGuestEnvironmentVersionsFromComponentMapPrefersPinned(t *testing.T) {
	fallback := guestEnvironmentVersions{
		GuestImage: "live-img",
		Agent:      "live-agent",
		Kernel:     "live-kern",
		Shim:       "live-shim",
	}
	got := guestEnvironmentVersionsFromComponentMap(map[string]string{
		templatetypes.CubeComponentCubeImage:  "pin-img",
		templatetypes.CubeComponentCubeAgent:  "pin-agent",
		templatetypes.CubeComponentCubeKernel: "pin-kern",
		templatetypes.CubeComponentCubeShim:   "pin-shim",
	}, fallback)
	require.Equal(t, "pin-img", got.GuestImage)
	require.Equal(t, "pin-agent", got.Agent)
	require.Equal(t, "pin-kern", got.Kernel)
	require.Equal(t, "pin-shim", got.Shim)
}

func TestGuestEnvironmentVersionsFromComponentMapFallsBackWhenMissing(t *testing.T) {
	fallback := guestEnvironmentVersions{
		GuestImage: "live-img",
		Agent:      "live-agent",
		Kernel:     "live-kern",
		Shim:       "live-shim",
	}
	got := guestEnvironmentVersionsFromComponentMap(map[string]string{
		templatetypes.CubeComponentCubeKernel: "pin-kern",
	}, fallback)
	require.Equal(t, "live-img", got.GuestImage)
	require.Equal(t, "live-agent", got.Agent)
	require.Equal(t, "pin-kern", got.Kernel)
	require.Equal(t, "live-shim", got.Shim)

	got = guestEnvironmentVersionsFromComponentMap(nil, fallback)
	require.Equal(t, fallback, got)
}

func TestGuestEnvironmentVersionsFromCubeBoxUsesCapturedMap(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		ComponentVersions: map[string]string{
			templatetypes.CubeComponentCubeImage:  "pin-img",
			templatetypes.CubeComponentCubeAgent:  "pin-agent",
			templatetypes.CubeComponentCubeKernel: "pin-kern",
			templatetypes.CubeComponentCubeShim:   "pin-shim",
		},
	}
	got := guestEnvironmentVersionsFromCubeBox(cb)
	require.Equal(t, "pin-img", got.GuestImage)
	require.Equal(t, "pin-agent", got.Agent)
	require.Equal(t, "pin-kern", got.Kernel)
	require.Equal(t, "pin-shim", got.Shim)
}
