// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package components

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// ErrComponentVersionMissing means the requested version is not in local inventory.
var ErrComponentVersionMissing = errors.New("component version missing on node")

type ComponentManagerConfig struct {
	VersionedBaseDir string `toml:"versioned_base_dir"`
}

func DefaultConfig() *ComponentManagerConfig {
	return &ComponentManagerConfig{
		VersionedBaseDir: templatetypes.DefaultVersionedBaseDir,
	}
}

type ComponentManager struct {
	config *ComponentManagerConfig
}

func NewComponentManager(config *ComponentManagerConfig) *ComponentManager {
	if config == nil {
		config = DefaultConfig()
	}
	if strings.TrimSpace(config.VersionedBaseDir) == "" {
		config.VersionedBaseDir = templatetypes.DefaultVersionedBaseDir
	}
	cm := &ComponentManager{
		config: config,
	}
	distribution.RegisterHandler(distribution.ResourceTaskTypeComponent, cm)
	return cm
}

// Ensure resolves name/version/relativePath to an absolute LocalPath in inventory.
// Missing directory or file returns ErrComponentVersionMissing.
func (c *ComponentManager) Ensure(ctx context.Context, name, version, relativePath string) (string, error) {
	_ = ctx
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	relativePath = strings.TrimSpace(relativePath)
	if name == "" {
		return "", fmt.Errorf("component name is empty")
	}
	if version == "" {
		return "", fmt.Errorf("component %s version is empty", name)
	}
	version = templatetypes.InventoryVersionKey(version)
	if version == "" {
		return "", fmt.Errorf("component %s version is empty after inventory-key normalize", name)
	}
	if relativePath == "" {
		relativePath = templatetypes.DefaultRelativePath(name)
	}
	if relativePath == "" {
		return "", fmt.Errorf("component %s relative path is empty", name)
	}

	versionedDir := templatetypes.VersionedComponentDir(c.config.VersionedBaseDir, name, version)
	ok, err := utils.DenExist(versionedDir)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: dir %s (component=%s version=%s)", ErrComponentVersionMissing, versionedDir, name, version)
	}

	localPath := path.Join(versionedDir, relativePath)
	if _, err := os.Stat(localPath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: file %s (component=%s version=%s path=%s)", ErrComponentVersionMissing, localPath, name, version, relativePath)
		}
		return "", err
	}
	return localPath, nil
}

func (c *ComponentManager) Handle(ctx context.Context, task *distribution.SubTaskDefine) (status distribution.TaskStatus, err error) {
	baseStatus := newComponentTaskStatus(task)
	status = baseStatus
	component := baseStatus.Component

	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"mod":       "component_manager",
		"task_id":   task.Name,
		"component": component.Name,
		"version":   component.Version,
		"template":  task.TemplateID,
	})
	defer func() {
		if err != nil {
			logEntry.Errorf("handle component task failed: %v", err)
			baseStatus.AddError(ctx, err)
		} else {
			baseStatus.SetStatus(distribution.TaskStatus_SUCCESS, "")
			logEntry.Infof("handle component task success local_path=%s", baseStatus.LocalComponent.Component.Path)
		}
	}()

	relativePath := strings.TrimSpace(component.Path)
	localPath, ensureErr := c.Ensure(ctx, component.Name, component.Version, relativePath)
	if ensureErr != nil {
		err = ensureErr
		return
	}
	baseStatus.LocalComponent.Component.Path = localPath
	return
}

func (c *ComponentManager) IsReady() bool {
	ok, _ := utils.DenExist(c.config.VersionedBaseDir)
	return ok
}

func (c *ComponentManager) Config() *ComponentManagerConfig {
	return c.config
}

var _ distribution.TaskHandler = &ComponentManager{}

type ComponentTaskStatus struct {
	*distribution.BaseSubTaskStatus
	*templatetypes.LocalComponent
}

func newComponentTaskStatus(task *distribution.SubTaskDefine) *ComponentTaskStatus {
	return &ComponentTaskStatus{
		BaseSubTaskStatus: task.NewRunningStatus(),
		LocalComponent: &templatetypes.LocalComponent{
			DistributionReference: *task.GenDistributionReference(),
			Component:             *task.Object.(*templatetypes.MachineComponent),
		},
	}
}
