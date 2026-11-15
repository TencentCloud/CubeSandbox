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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"golang.org/x/sys/unix"
)

var hostDirBasePath = "/data/cubelet/hostdir"

const hostDirMountTimeout = 3 * time.Second

const hostDirMountBinary = "/usr/bin/mount"

var runHostDirCommand = defaultRunHostDirCommand

var isHostDirMountPoint = utils.IsMountPoint

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
		if err := validateHostDirSourceName(src.GetName()); err != nil {
			return fmt.Errorf("prepareHostDirVolume: %w", err)
		}
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

		mounted, err := isHostDirMountPoint(bindDest)
		if err != nil {
			return fmt.Errorf("prepareHostDirVolume: check mount point %s: %w", bindDest, err)
		}
		if mounted {
			return fmt.Errorf("prepareHostDirVolume: bind destination %s is already a mount point", bindDest)
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

// validateHostDirSourceName requires a single path segment, so a volume stays
// inside its own directory under the share dir.
func validateHostDirSourceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("volume source name %q must be a single path segment", name)
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
	// mountinfo prefix matching would otherwise miss the mounts.
	//
	// We deliberately do NOT EvalSymlinks the leaf <sandboxID>: if that
	// component were ever replaced by a symlink, we only unlink the symlink
	// itself without traversing into the target.
	resolvedBase, err := filepath.EvalSymlinks(hostDirBasePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: resolve %s: %w", hostDirBasePath, err)
	}
	sandboxDir := filepath.Join(resolvedBase, info.SandboxID)

	fi, err := os.Lstat(sandboxDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cleanupHostDirVolumes: stat %s: %w", sandboxDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		if err := os.Remove(sandboxDir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cleanupHostDirVolumes: remove %s: %w", sandboxDir, err)
		}
		return nil
	}

	if err := unmountHostDirTree(sandboxDir); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: unmount %s: %v", sandboxDir, err)
		return err
	}

	if err := removeEmptyHostDirTree(sandboxDir); err != nil {
		log.G(ctx).Warnf("cleanupHostDirVolumes: remove %s: %v", sandboxDir, err)
		return err
	}
	return nil
}

func matchesSandboxMountPrefix(mountpoint, sandboxDir string) bool {
	if mountpoint == "" || sandboxDir == "" {
		return false
	}
	if mountpoint == sandboxDir {
		return true
	}
	prefix := sandboxDir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(mountpoint, prefix)
}

// unmountHostDirTree detaches all mounts at or under sandboxDir in deepest-first
// order (including stacked mounts on the same mountpoint).
func unmountHostDirTree(sandboxDir string) error {
	mounts, err := mountinfo.GetMounts(func(m *mountinfo.Info) (bool, bool) {
		if m == nil {
			return true, false
		}
		return !matchesSandboxMountPrefix(m.Mountpoint, sandboxDir), false
	})
	if err != nil {
		return fmt.Errorf("list mounts under %s: %w", sandboxDir, err)
	}

	seen := make(map[string]struct{}, len(mounts))
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if m == nil || m.Mountpoint == "" {
			continue
		}
		if _, ok := seen[m.Mountpoint]; ok {
			continue
		}
		seen[m.Mountpoint] = struct{}{}
		targets = append(targets, m.Mountpoint)
	}

	sort.SliceStable(targets, func(i, j int) bool {
		return len(targets[i]) > len(targets[j])
	})

	for _, target := range targets {
		if err := mount.UnmountAll(target, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("unmount %s: %w", target, err)
		}
	}

	// Defensive second pass across the directory tree: mountinfo already returned
	// every mount at/under sandboxDir, so this skips symlinks (leaving
	// removeEmptyHostDirTree to refuse non-directory entries) and checks
	// utils.IsMountPoint in case a mount boundary appeared mid-teardown.
	return filepath.WalkDir(sandboxDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d != nil && d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mounted, err := isHostDirMountPoint(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("check mount point %s: %w", path, err)
		}
		if !mounted {
			return nil
		}
		if err := mount.UnmountAll(path, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("unmount %s: %w", path, err)
		}
		if d.IsDir() && path != sandboxDir {
			return filepath.SkipDir
		}
		return nil
	})
}

// removeEmptyHostDirTree removes the staging directory hierarchy bottom-up using
// os.Remove only (never os.RemoveAll). Before removing any directory, it
// verifies that the directory is neither a remaining mount point nor on a
// different device from its parent. If a mount remains or a directory is
// non-empty, removal fails closed without deleting user data.
func removeEmptyHostDirTree(sandboxDir string) error {
	parentDir := filepath.Dir(sandboxDir)
	var parentStat unix.Stat_t
	if err := unix.Lstat(parentDir, &parentStat); err != nil {
		return fmt.Errorf("stat parent %s: %w", parentDir, err)
	}
	expectedDev := parentStat.Dev

	var dirs []string
	err := filepath.WalkDir(sandboxDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", path, err)
		}
		if st.Dev != expectedDev {
			return fmt.Errorf("refusing to remove %s: device id (%d) differs from parent (%d), mount still active",
				path, st.Dev, expectedDev)
		}
		if !d.IsDir() {
			return fmt.Errorf("refusing to remove unexpected non-directory entry %s in hostdir staging tree", path)
		}
		mounted, err := isHostDirMountPoint(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("verify mount status %s: %w", path, err)
		}
		if mounted {
			return fmt.Errorf("refusing to remove active mount point %s", path)
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return err
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		mounted, err := isHostDirMountPoint(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("re-verify mount status %s: %w", dir, err)
		}
		if mounted {
			return fmt.Errorf("refusing to remove active mount point %s", dir)
		}
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rmdir %s: %w", dir, err)
		}
	}
	return nil
}
