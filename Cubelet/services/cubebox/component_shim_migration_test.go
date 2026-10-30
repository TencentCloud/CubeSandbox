// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/components"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func migrationTemplate() *templatetypes.LocalRunTemplate {
	local := &templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{}}
	for _, name := range []string{
		templatetypes.CubeComponentCubeShim, templatetypes.CubeComponentCubeKernel,
		templatetypes.CubeComponentCubeImage, templatetypes.CubeComponentCubeAgent,
	} {
		local.Componts[name] = templatetypes.LocalComponent{Component: templatetypes.MachineComponent{
			Name: name, Version: "old", Path: filepath.Join("/inventory", name, "old", templatetypes.DefaultRelativePath(name)),
		}}
	}
	return local
}

func TestExplicitShimBindingOverridesOnlyHostRuntimePin(t *testing.T) {
	source := migrationTemplate()
	cb := &cubeboxstore.CubeBox{LocalRunTemplate: source.Clone()}
	seedCubeBoxComponentVersionsFromRequest(cb, &workflow.CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationComponentCubeShimVersion:   "new",
			constants.MasterAnnotationComponentCubeKernelVersion: "different-kernel",
			constants.MasterAnnotationComponentCubeImageVersion:  "different-guest",
			constants.MasterAnnotationComponentCubeAgentVersion:  "different-agent",
		},
	}})
	require.Equal(t, "new", cb.ComponentVersions[templatetypes.CubeComponentCubeShim])
	for _, name := range []string{templatetypes.CubeComponentCubeKernel, templatetypes.CubeComponentCubeImage, templatetypes.CubeComponentCubeAgent} {
		require.Equal(t, "old", cb.ComponentVersions[name])
	}
	base := t.TempDir()
	for name, version := range cb.ComponentVersions {
		file := filepath.Join(base, name, version, templatetypes.DefaultRelativePath(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte(name), 0o644))
	}
	previous := componentManagerInst
	t.Cleanup(func() { SetComponentManagerForTest(previous) })
	SetComponentManagerForTest(components.NewComponentManager(&components.ComponentManagerConfig{VersionedBaseDir: base}))
	require.NoError(t, EnsureCubeBoxComponents(context.Background(), cb))
	shim := cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim].Component
	require.Equal(t, "new", shim.Version)
	require.Equal(t, filepath.Join(base, templatetypes.CubeComponentCubeShim, "new", templatetypes.RelativePathCubeShim), shim.Path)
	require.Equal(t, "old", source.Componts[templatetypes.CubeComponentCubeShim].Component.Version)
	require.Equal(t, filepath.Join("/inventory", templatetypes.CubeComponentCubeShim, "old", templatetypes.RelativePathCubeShim), source.Componts[templatetypes.CubeComponentCubeShim].Component.Path)
}

func TestAbsentShimBindingKeepsSnapshotVersion(t *testing.T) {
	cb := &cubeboxstore.CubeBox{LocalRunTemplate: migrationTemplate()}
	seedCubeBoxComponentVersionsFromRequest(cb, &workflow.CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{}})
	require.Equal(t, "old", cb.ComponentVersions[templatetypes.CubeComponentCubeShim])
}

func TestInvalidExplicitShimBindingDoesNotChangeSnapshotPin(t *testing.T) {
	for _, version := range []string{"sha256:short", ".", "/", "@@@"} {
		t.Run(version, func(t *testing.T) {
			cb := &cubeboxstore.CubeBox{LocalRunTemplate: migrationTemplate()}
			err := seedCubeBoxComponentVersionsFromRequest(cb, &workflow.CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{
				Annotations: map[string]string{constants.MasterAnnotationComponentCubeShimVersion: version},
			}})
			require.ErrorContains(t, err, "invalid explicit shim version")
			require.Equal(t, "old", cb.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim].Component.Version)
		})
	}
}

func TestMissingMigratedShimDoesNotFallBackToOldVersion(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, templatetypes.CubeComponentCubeShim, "old", templatetypes.RelativePathCubeShim)
	require.NoError(t, os.MkdirAll(filepath.Dir(old), 0o755))
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o755))
	previous := componentManagerInst
	t.Cleanup(func() { SetComponentManagerForTest(previous) })
	SetComponentManagerForTest(components.NewComponentManager(&components.ComponentManagerConfig{VersionedBaseDir: base}))
	cb := &cubeboxstore.CubeBox{LocalRunTemplate: &templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{Name: templatetypes.CubeComponentCubeShim, Version: "old"}},
	}}}
	seedCubeBoxComponentVersionsFromRequest(cb, &workflow.CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{constants.MasterAnnotationComponentCubeShimVersion: "missing"},
	}})
	require.ErrorIs(t, EnsureCubeBoxComponents(context.Background(), cb), components.ErrComponentVersionMissing)
	require.Equal(t, "missing", cb.ComponentVersions[templatetypes.CubeComponentCubeShim])
}
