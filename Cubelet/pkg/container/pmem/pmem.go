// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"os"
	"path/filepath"
)

var baseDirPath string

func Init(dataDir string) {
	baseDirPath = dataDir
	_ = os.MkdirAll(baseDirPath, os.ModeDir|0755)
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
	return filepath.Join(baseDirPath, "cube-kernel-scf", "vmlinux")
}

func GetPmemBasePath(instanceType string) string {
	return filepath.Join(baseDirPath, instanceType+"_os_image")
}

type CubePmem struct {
	File          string `json:"file"`
	DiscardWrites bool   `json:"discard_writes"`
	SourceDir     string `json:"source_dir"`
	FsType        string `json:"fs_type"`
	Size          int64  `json:"size"`
	ID            string `json:"id"`
}
