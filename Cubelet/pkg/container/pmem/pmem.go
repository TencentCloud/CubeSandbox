// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"fmt"
	"path/filepath"
)

const DefaultToolBaseDir = "/usr/local/services/cubetoolbox"
const DefaultImageBasePath = "/data/cubelet/cubebox_os_image"

// Paths separates installed kernel sources from node-local rootfs artifacts.
// ImageBasePath is the final cubebox artifact directory, without another suffix.
type Paths struct {
	ToolBaseDir      string
	ImageBasePath    string
	SharedKernelPath string
}

var paths = Paths{
	ToolBaseDir:      DefaultToolBaseDir,
	ImageBasePath:    DefaultImageBasePath,
	SharedKernelPath: DefaultToolBaseDir + "/cube-kernel-scf/vmlinux",
}

func ResolvePaths(toolBaseDir, imageBasePath, sharedKernelPath string) (Paths, error) {
	// Preserve the old layout only when the legacy base was explicitly set.
	if imageBasePath == "" && toolBaseDir == "" {
		imageBasePath = DefaultImageBasePath
	}
	if toolBaseDir == "" {
		toolBaseDir = DefaultToolBaseDir
	}
	if imageBasePath == "" {
		imageBasePath = filepath.Join(toolBaseDir, "cubebox_os_image")
	}
	if sharedKernelPath == "" {
		sharedKernelPath = filepath.Join(toolBaseDir, "cube-kernel-scf", "vmlinux")
	}
	for _, entry := range []struct{ key, value string }{
		{"cubetool_base_dir", toolBaseDir},
		{"image_base_path", imageBasePath},
		{"shared_kernel_path", sharedKernelPath},
	} {
		if !filepath.IsAbs(entry.value) {
			return Paths{}, fmt.Errorf("images.%s must be an absolute path: %q", entry.key, entry.value)
		}
	}
	return Paths{filepath.Clean(toolBaseDir), filepath.Clean(imageBasePath), filepath.Clean(sharedKernelPath)}, nil
}

// InitPaths is called once before plugins start. Path resolution does not create
// files; artifact writers create their destination directories when needed.
func InitPaths(resolved Paths) { paths = resolved }

func CurrentPaths() Paths { return paths }

func GetRawImageFilePath(instanceType, imageID string) string {
	return paths.ImageFile(instanceType, imageID)
}

func GetRawKernelFilePath(instanceType, imageID string) string {
	return paths.KernelFile(instanceType, imageID)
}

func GetKoFilePath(instanceType, imageID string) string {
	return filepath.Join(GetPmemBasePath(instanceType), imageID, imageID+".ko")
}

func GetSharedKernelFilePath() string {
	return paths.SharedKernelPath
}

func GetPmemBasePath(instanceType string) string {
	return paths.ImageDir(instanceType)
}

func (p Paths) ImageDir(instanceType string) string {
	if instanceType == "cubebox" {
		return p.ImageBasePath
	}
	return filepath.Join(p.ToolBaseDir, instanceType+"_os_image")
}

func (p Paths) ImageFile(instanceType, imageID string) string {
	return filepath.Join(p.ImageDir(instanceType), imageID, imageID+".ext4")
}

func (p Paths) KernelFile(instanceType, imageID string) string {
	return filepath.Join(p.ImageDir(instanceType), imageID, imageID+".vm")
}

type CubePmem struct {
	File          string `json:"file"`
	DiscardWrites bool   `json:"discard_writes"`
	SourceDir     string `json:"source_dir"`
	FsType        string `json:"fs_type"`
	Size          int64  `json:"size"`
	ID            string `json:"id"`
}
