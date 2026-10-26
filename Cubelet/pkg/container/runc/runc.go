// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

var runcFilePath string

func Init(dataDir string) {
	runcFilePath = filepath.Join(dataDir, "runc")
	_ = os.MkdirAll(runcFilePath, os.ModeDir|0755)
}

// joinFileDir validates container/sandbox name and returns the safe path
// under runcFilePath, preventing path traversal attacks.
func joinFileDir(container string) (string, error) {
	return utils.SafeJoinPath(runcFilePath, container)
}

func GenMount(ctx context.Context, opts *workflow.CreateContext) ([]specs.Mount, error) {
	if userData, ok := opts.ReqInfo.Annotations[constants.MasterAnnotationsUserData]; ok && userData != "" {
		ud, err := cubeboxstore.ParseUserData(userData)
		if err != nil {
			return nil, fmt.Errorf("parse user data error: %v", err)
		}

		if ud == nil || ud.DataBytes == nil {
			return nil, fmt.Errorf("must provider user data")
		}

		containerDir, err := joinFileDir(opts.SandboxID)
		if err != nil {
			return nil, fmt.Errorf("invalid sandbox path: %w", err)
		}
		if err := os.Mkdir(containerDir, 0755); err != nil && !os.IsExist(err) {
			return nil, err
		}
		file := filepath.Join(containerDir, "user_data")
		if err := os.WriteFile(file, ud.DataBytes, 0644); err != nil {
			return nil, fmt.Errorf("write user data failed: %v", err)
		}
		mounts := []specs.Mount{
			{
				Type:        constants.MountTypeBind,
				Source:      file,
				Options:     []string{constants.MountOptBindRO, constants.MountPropagationRprivate},
				Destination: "/mnt/openstack/latest/user_data",
			},
		}
		return mounts, nil
	}
	return []specs.Mount{}, nil
}

func Clean(ctx context.Context, sandbox string) error {
	if sandbox == "" {
		return nil
	}
	sandboxDir, err := joinFileDir(sandbox)
	if err != nil {
		return fmt.Errorf("Clean: %w", err)
	}
	if exist, _ := utils.DenExist(sandboxDir); !exist {
		return nil
	}
	return os.RemoveAll(sandboxDir)
}
