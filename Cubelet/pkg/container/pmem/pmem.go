// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultOsImageParentDir is the parent directory for per-instance OS image
// artifacts (<parent>/<instanceType>_os_image/...). Kept on /data so large
// template rootfs files do not fill the system disk under cubetoolbox.
const DefaultOsImageParentDir = "/data"

var (
	// toolboxBasePath holds the cubetoolbox install root (shared kernel, etc.).
	toolboxBasePath string
	// osImageParentPath holds the parent of <instanceType>_os_image directories.
	osImageParentPath string
)

// DefaultCubeboxOsImageDir returns the default directory for cubebox OS-image
// artifacts (/data/cubebox_os_image). Shared by images/pmem and CBRI defaults.
func DefaultCubeboxOsImageDir() string {
	return filepath.Join(DefaultOsImageParentDir, "cubebox_os_image")
}

// ResolveOsImageParentDir returns configured if non-empty, otherwise the
// default parent for OS-image artifacts.
func ResolveOsImageParentDir(configured string) string {
	if configured == "" {
		return DefaultOsImageParentDir
	}
	return configured
}

// Init configures toolbox paths and stores OS-image artifacts under the same
// root (backward compatible for tests). Prefer InitWithPaths in production.
func Init(dataDir string) {
	_ = InitWithPaths(dataDir, dataDir)
}

// InitWithPaths separates the toolbox install root from the OS-image parent dir.
// Returns an error if either directory cannot be created so callers fail early.
func InitWithPaths(toolboxDir, osImageParentDir string) error {
	toolboxBasePath = toolboxDir
	osImageParentPath = osImageParentDir
	if err := os.MkdirAll(toolboxBasePath, os.ModeDir|0755); err != nil {
		return fmt.Errorf("mkdir toolbox base %s: %w", toolboxBasePath, err)
	}
	osImageDir := filepath.Join(osImageParentPath, "cubebox_os_image")
	if err := os.MkdirAll(osImageDir, os.ModeDir|0755); err != nil {
		return fmt.Errorf("mkdir os image dir %s: %w", osImageDir, err)
	}
	return nil
}

func GetRawImageFilePath(instanceType, imageID string) string {
	return filepath.Join(GetPmemBasePath(instanceType), imageID, imageID+".ext4")
}

func GetRawKernelFilePath(instanceType, imageID string) string {
	return filepath.Join(GetPmemBasePath(instanceType), imageID, imageID+".vm")
}

func GetKoFilePath(instanceType, imageID string) string {
	return filepath.Join(GetPmemBasePath(instanceType), imageID, imageID+".ko")
}

func GetSharedKernelFilePath() string {
	return filepath.Join(toolboxBasePath, "cube-kernel-scf", "vmlinux")
}

func GetPmemBasePath(instanceType string) string {
	return filepath.Join(osImageParentPath, instanceType+"_os_image")
}

type CubePmem struct {
	File          string `json:"file"`
	DiscardWrites bool   `json:"discard_writes"`
	SourceDir     string `json:"source_dir"`
	FsType        string `json:"fs_type"`
	Size          int64  `json:"size"`
	ID            string `json:"id"`
}
