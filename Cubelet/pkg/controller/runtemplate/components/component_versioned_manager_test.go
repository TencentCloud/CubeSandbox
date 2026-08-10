// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package components

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

func setupTestManager(t *testing.T) (*ComponentManager, *ComponentManagerConfig, string) {
	t.Helper()
	tempDir := t.TempDir()
	config := &ComponentManagerConfig{
		VersionedBaseDir: path.Join(tempDir, "versioned"),
	}
	require.NoError(t, os.MkdirAll(config.VersionedBaseDir, 0755))

	manager := NewComponentManager(config)
	return manager, config, tempDir
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	assert.Equal(t, templatetypes.DefaultVersionedBaseDir, config.VersionedBaseDir)
}

func TestNewComponentManagerDefaultsEmptyBaseDir(t *testing.T) {
	manager := NewComponentManager(&ComponentManagerConfig{})
	assert.Equal(t, templatetypes.DefaultVersionedBaseDir, manager.Config().VersionedBaseDir)
}

func TestComponentManager_Ensure(t *testing.T) {
	manager, config, _ := setupTestManager(t)

	shimDir := path.Join(config.VersionedBaseDir, "cube-shim", "1.0.0", "bin")
	require.NoError(t, os.MkdirAll(shimDir, 0755))
	shimFile := path.Join(shimDir, "containerd-shim-cube-rs")
	require.NoError(t, os.WriteFile(shimFile, []byte("shim"), 0755))

	localPath, err := manager.Ensure(context.Background(), "cube-shim", "1.0.0", "")
	require.NoError(t, err)
	assert.Equal(t, shimFile, localPath)

	_, err = manager.Ensure(context.Background(), "cube-shim", "9.9.9", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrComponentVersionMissing))
	assert.Contains(t, err.Error(), "cube-shim")
	assert.Contains(t, err.Error(), "9.9.9")

	_, err = manager.Ensure(context.Background(), "cube-shim", "1.0.0", "bin/missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrComponentVersionMissing))
}

func TestComponentManager_Handle(t *testing.T) {
	manager, config, _ := setupTestManager(t)

	shimDir := path.Join(config.VersionedBaseDir, "comp-a", "1.0", "bin")
	require.NoError(t, os.MkdirAll(shimDir, 0755))
	shimFile := path.Join(shimDir, "tool")
	require.NoError(t, os.WriteFile(shimFile, []byte("x"), 0644))

	testCases := []struct {
		name         string
		component    templatetypes.MachineComponent
		expectErr    bool
		expectStatus distribution.TaskStatusCode
		expectPath   string
	}{
		{
			name: "Success - file LocalPath",
			component: templatetypes.MachineComponent{
				Name:    "comp-a",
				Version: "1.0",
				Path:    "bin/tool",
			},
			expectErr:    false,
			expectStatus: distribution.TaskStatus_SUCCESS,
			expectPath:   shimFile,
		},
		{
			name: "Failure - Component name is empty",
			component: templatetypes.MachineComponent{
				Name:    "",
				Version: "1.0",
			},
			expectErr:    true,
			expectStatus: distribution.TaskStatus_FAILED,
		},
		{
			name: "Failure - version missing",
			component: templatetypes.MachineComponent{
				Name:    "comp-missing",
				Version: "1.0",
				Path:    "bin/tool",
			},
			expectErr:    true,
			expectStatus: distribution.TaskStatus_FAILED,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			task := &distribution.SubTaskDefine{
				TaskCommon: distribution.TaskCommon{
					Name:       "test-task",
					TemplateID: "test-template",
				},
				Object: &tc.component,
			}

			status, err := manager.Handle(context.Background(), task)
			if tc.expectErr {
				require.Error(t, err)
				assert.Equal(t, tc.expectStatus, status.GetStatus())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expectStatus, status.GetStatus())
			cs := status.(*ComponentTaskStatus)
			assert.Equal(t, tc.expectPath, cs.Component.Path)
			assert.True(t, filepath.IsAbs(cs.Component.Path))
		})
	}
}

func TestComponentManager_IsReady(t *testing.T) {
	tempDir := t.TempDir()
	versionedDir := path.Join(tempDir, "versioned")

	testCases := []struct {
		name     string
		prepare  func()
		config   *ComponentManagerConfig
		expected bool
	}{
		{
			name: "Ready when VersionedBaseDir exists",
			prepare: func() {
				require.NoError(t, os.MkdirAll(versionedDir, 0755))
			},
			config: &ComponentManagerConfig{
				VersionedBaseDir: versionedDir,
			},
			expected: true,
		},
		{
			name:    "Not Ready when VersionedBaseDir does not exist",
			prepare: func() {},
			config: &ComponentManagerConfig{
				VersionedBaseDir: path.Join(tempDir, "missing"),
			},
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.prepare()
			manager := NewComponentManager(tc.config)
			assert.Equal(t, tc.expected, manager.IsReady())
		})
	}
}
