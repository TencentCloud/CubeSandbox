// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"fmt"
	"path/filepath"
)

const DefaultToolBaseDir = "/usr/local/services/cubetoolbox"

// Paths separates installed kernel sources from node-local rootfs artifacts.
// ImageBasePath is the final cubebox artifact directory, without another suffix.
type Paths struct {
	ToolBaseDir      string
	ImageBasePath    string
	SharedKernelPath string
}

var paths = Paths{
	ToolBaseDir:      DefaultToolBaseDir,
	ImageBasePath:    DefaultToolBaseDir + "/cubebox_os_image",
	SharedKernelPath: DefaultToolBaseDir + "/cube-kernel-scf/vmlinux",
}

func ResolvePaths(toolBaseDir, imageBasePath, sharedKernelPath string) (Paths, error) {
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

// GetRawImageFilePathByMedia is GetRawImageFilePath with a storage-media-aware
// suffix (.ext4 / .erofs). Use it at sites that know the artifact's
// ImageSpec.StorageMedia so the downloaded file name matches the format the
// microVM expects to mount.
func GetRawImageFilePathByMedia(instanceType, imageID, storageMedia string) string {
	return paths.ImageFileByMedia(instanceType, imageID, storageMedia)
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
	// Keep the historical per-type layout for instance types without an
	// explicitly configured artifact directory.
	return filepath.Join(p.ToolBaseDir, instanceType+"_os_image")
}

func (p Paths) ImageFile(instanceType, imageID string) string {
	return filepath.Join(p.ImageDir(instanceType), imageID, imageID+".ext4")
}

// ImageFileByMedia returns the artifact rootfs file path with a suffix chosen
// by the image storage media ("ext4" → .ext4, "erofs" → .erofs). An empty/unknown
// media defaults to the historical ".ext4" so callers without media info
// (e.g. legacy code paths) keep working. The media value is the
// ImageSpec.StorageMedia string CubeMaster fills (see
// ImageStorageMediaType_ext4/erofs .String()).
func (p Paths) ImageFileByMedia(instanceType, imageID, storageMedia string) string {
	return filepath.Join(p.ImageDir(instanceType), imageID, imageID+ArtifactSuffix(storageMedia))
}

// ArtifactSuffix maps an ImageSpec.StorageMedia string to the on-disk artifact
// file suffix. Unknown/empty values default to ".ext4" (the historical format)
// so a missing media never produces a suffixless file.
func ArtifactSuffix(storageMedia string) string {
	switch storageMedia {
	case "erofs":
		return ".erofs"
	default:
		return ".ext4"
	}
}

// ArtifactFsType maps an ImageSpec.StorageMedia string to the filesystem type
// the microVM must mount the pmem backing file as ("ext4" / "erofs"). It is the
// pmem config counterpart of ArtifactSuffix: the suffix picks the file name,
// this picks CubePmem.FsType handed to the hypervisor. Unknown/empty defaults
// to "ext4".
func ArtifactFsType(storageMedia string) string {
	switch storageMedia {
	case "erofs":
		return "erofs"
	default:
		return "ext4"
	}
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
