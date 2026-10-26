// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/components"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

func TestEnsureTemplateComponentsWritesLocalPath(t *testing.T) {
	base := t.TempDir()
	ver := "1.0.0"
	shimRel := templatetypes.RelativePathCubeShim
	shimFile := filepath.Join(base, templatetypes.CubeComponentCubeShim, ver, shimRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(shimFile), 0o755))
	require.NoError(t, os.WriteFile(shimFile, []byte("shim"), 0o755))

	imgRel := templatetypes.RelativePathCubeImage
	imgFile := filepath.Join(base, templatetypes.CubeComponentCubeImage, ver, imgRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(imgFile), 0o755))
	require.NoError(t, os.WriteFile(imgFile, []byte("img"), 0o644))

	cm := components.NewComponentManager(&components.ComponentManagerConfig{
		VersionedBaseDir: base,
	})
	SetComponentManagerForTest(cm)

	local := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{},
	}
	err := EnsureTemplateComponents(context.Background(), local, map[string]string{
		templatetypes.CubeComponentCubeShim:  ver,
		templatetypes.CubeComponentCubeImage: ver,
	})
	require.NoError(t, err)
	require.Equal(t, shimFile, local.Componts[templatetypes.CubeComponentCubeShim].Component.Path)
	require.Equal(t, ver, local.Componts[templatetypes.CubeComponentCubeShim].Component.Version)
	require.Equal(t, imgFile, local.Componts[templatetypes.CubeComponentCubeImage].Component.Path)
}

func TestEnsureTemplateComponentsMissingVersionErrors(t *testing.T) {
	base := t.TempDir()
	cm := components.NewComponentManager(&components.ComponentManagerConfig{
		VersionedBaseDir: base,
	})
	SetComponentManagerForTest(cm)

	local := &templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{}}
	err := EnsureTemplateComponents(context.Background(), local, map[string]string{
		templatetypes.CubeComponentCubeAgent: "missing-ver",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, components.ErrComponentVersionMissing)
}

func TestEnsureTemplateComponentsSkipsEmptyVersions(t *testing.T) {
	local := &templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{}}
	require.NoError(t, EnsureTemplateComponents(context.Background(), local, map[string]string{
		templatetypes.CubeComponentCubeShim: "",
	}))
	require.Empty(t, local.Componts)
}

func TestEnsureTemplateComponentsOnClonesConcurrent(t *testing.T) {
	base := t.TempDir()
	ver := "1.0.0"
	shimFile := filepath.Join(base, templatetypes.CubeComponentCubeShim, ver, templatetypes.RelativePathCubeShim)
	require.NoError(t, os.MkdirAll(filepath.Dir(shimFile), 0o755))
	require.NoError(t, os.WriteFile(shimFile, []byte("shim"), 0o755))
	SetComponentManagerForTest(components.NewComponentManager(&components.ComponentManagerConfig{
		VersionedBaseDir: base,
	}))

	src := &templatetypes.LocalRunTemplate{Componts: map[string]templatetypes.LocalComponent{}}
	versions := map[string]string{templatetypes.CubeComponentCubeShim: ver}
	const n = 16
	var start, done sync.WaitGroup
	start.Add(n)
	done.Add(n)
	for range n {
		go func() {
			defer done.Done()
			start.Done()
			start.Wait()
			local := src.Clone()
			if err := EnsureTemplateComponents(context.Background(), local, versions); err != nil {
				t.Errorf("ensure: %v", err)
			}
		}()
	}
	done.Wait()
	require.Empty(t, src.Componts)
}
