// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"golang.org/x/sys/unix"
)

var hostDirBasePath = "/data/cubelet/hostdir"

const hostDirMountTimeout = 3 * time.Second

const hostDirMountBinary = "/usr/bin/mount"

var runHostDirCommand = defaultRunHostDirCommand

type HostDirBackendInfo struct {
	VolumeName string `json:"volume_name"`

	ShareDir string `json:"share_dir"`

	BindPath string `json:"bind_path"`

	ReadOnly bool `json:"read_only"`
}

func defaultRunHostDirCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s %v: %w", name, args, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		if stderr.Len() > 0 {
			return fmt.Errorf("%w: %s", err, stderr.String())
		}
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return ctx.Err()
	}
}

func bindHostDir(ctx context.Context, srcPath, bindDest string, readOnly bool) error {
	mountCtx, cancel := context.WithTimeout(ctx, hostDirMountTimeout)
	defer cancel()

	if err := runHostDirCommand(mountCtx, hostDirMountBinary, "--rbind", srcPath, bindDest); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("bind mount %s -> %s timed out after %s: %w", srcPath, bindDest, hostDirMountTimeout, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("bind mount %s -> %s canceled: %w", srcPath, bindDest, err)
		}
		return fmt.Errorf("bind mount %s -> %s: %w", srcPath, bindDest, err)
	}

	if !readOnly {
		return nil
	}

	remountCtx, remountCancel := context.WithTimeout(ctx, hostDirMountTimeout)
	defer remountCancel()

	if err := runHostDirCommand(remountCtx, hostDirMountBinary, "-o", "remount,bind,ro", bindDest); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("remount ro %s timed out after %s: %w", bindDest, hostDirMountTimeout, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("remount ro %s canceled: %w", bindDest, err)
		}
		return fmt.Errorf("remount ro %s: %w", bindDest, err)
	}
	return nil
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

		key := v.GetName() + "/" + src.GetName()
		result.HostDirBackendInfos[key] = &HostDirBackendInfo{
			VolumeName: v.GetName(),
			ShareDir:   shareDir,
			BindPath:   bindDest,
			ReadOnly:   readOnly,
		}

		log.G(ctx).Infof("[hostdir] binding %s -> %s (ro=%v)", src.GetHostPath(), bindDest, readOnly)
		if err := bindHostDir(ctx, src.GetHostPath(), bindDest, readOnly); err != nil {
			return fmt.Errorf("prepareHostDirVolume: %w", err)
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

	// Resolve symlinks on the parent (hostDirBasePath and its ancestors) only,
	// not on the per-sandbox leaf component. The kernel records fully resolved
	// mountpoints in /proc/self/mountinfo, so when any ancestor of
	// hostDirBasePath is a symlink (e.g. /data -> /mnt/ssd/data),
	// IsMountPoint's string comparison would otherwise miss the mounts, leaking
	// them and letting os.RemoveAll wipe the real backing directory.
	//
	// We deliberately do NOT EvalSymlinks the leaf <sandboxID>: if that
	// component were ever replaced by a symlink, resolving it would make
	// os.RemoveAll follow the link and delete the target's contents (a
	// link-following deletion hazard). Keeping the leaf unresolved means
	// RemoveAll only unlinks the symlink itself.
	resolvedBase, err := filepath.EvalSymlinks(hostDirBasePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: resolve %s: %w", hostDirBasePath, err)
	}
	sandboxDir := filepath.Join(resolvedBase, info.SandboxID)

	if _, err := os.Lstat(sandboxDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: stat %s: %w", sandboxDir, err)
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
