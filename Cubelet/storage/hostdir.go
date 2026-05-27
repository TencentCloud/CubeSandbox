// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"golang.org/x/sys/unix"
)

var hostDirBasePath = "/data/cubelet/hostdir"

type HostDirBackendInfo struct {
	VolumeName string `json:"volume_name"`

	ShareDir string `json:"share_dir"`

	BindPath string `json:"bind_path"`

	ReadOnly bool `json:"read_only"`
}

func (l *local) prepareHostDirVolume(ctx context.Context, opts *workflow.CreateContext,
	v *cubebox.Volume, result *StorageInfo) error {

	hdv := v.GetVolumeSource().GetHostDirVolumes()
	if hdv == nil {
		return nil
	}

	sandboxID := opts.SandboxID
	if sandboxID == "" {
		return fmt.Errorf("prepareHostDirVolume: sandbox ID is empty")
	}

	if result.HostDirBackendInfos == nil {
		result.HostDirBackendInfos = make(map[string]*HostDirBackendInfo)
	}

	for _, src := range hdv.GetVolumeSources() {
		if src.GetHostPath() == "" {
			return fmt.Errorf("prepareHostDirVolume: volume %q has empty host_path", src.GetName())
		}

		roStr := "rw"
		readOnly := false
		for _, c := range opts.ReqInfo.GetContainers() {
			for _, vm := range c.GetVolumeMounts() {
				if vm.GetName() == v.GetName() && vm.GetReadonly() {
					roStr = "ro"
					readOnly = true
				}
			}
		}

		shareDir := filepath.Join(hostDirBasePath, sandboxID, roStr)

		bindDest := filepath.Join(shareDir, src.GetName())

		if err := os.MkdirAll(bindDest, 0755); err != nil {
			return fmt.Errorf("prepareHostDirVolume: mkdir %s: %w", bindDest, err)
		}

		flags := uintptr(unix.MS_BIND | unix.MS_REC)
		if err := unix.Mount(src.GetHostPath(), bindDest, "", flags, ""); err != nil {
			return fmt.Errorf("prepareHostDirVolume: bind mount %s -> %s: %w",
				src.GetHostPath(), bindDest, err)
		}
		if readOnly {
			roFlags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
			if err := unix.Mount("", bindDest, "", roFlags, ""); err != nil {
				return fmt.Errorf("prepareHostDirVolume: remount ro %s: %w", bindDest, err)
			}
		}

		key := v.GetName() + "/" + src.GetName()
		result.HostDirBackendInfos[key] = &HostDirBackendInfo{
			VolumeName: v.GetName(),
			ShareDir:   shareDir,
			BindPath:   bindDest,
			ReadOnly:   readOnly,
		}
		log.G(ctx).Infof("[hostdir] bound %s -> %s (ro=%v, shareDir=%s)",
			src.GetHostPath(), bindDest, readOnly, shareDir)
	}
	return nil
}

func (l *local) cleanupHostDirVolumes(ctx context.Context, info *StorageInfo) error {
	if info == nil || info.SandboxID == "" || len(info.HostDirBackendInfos) == 0 {
		return nil
	}

	sandboxDir := filepath.Join(hostDirBasePath, info.SandboxID)
	if _, err := os.Stat(sandboxDir); os.IsNotExist(err) {
		return nil
	}

	if err := filepath.WalkDir(sandboxDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == sandboxDir {
			return nil
		}
		mounted, err := utils.IsMountPoint(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("check mount point %s: %w", path, err)
		}
		if mounted {
			if err := unix.Unmount(path, unix.MNT_DETACH); err != nil &&
				!errors.Is(err, unix.EINVAL) && !os.IsNotExist(err) {
				return fmt.Errorf("unmount %s: %w", path, err)
			}
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: skip removeAll %s: %v", sandboxDir, err)
		return err
	}

	if err := os.RemoveAll(sandboxDir); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: removeAll %s: %v", sandboxDir, err)
		return err
	}
	return nil
}
