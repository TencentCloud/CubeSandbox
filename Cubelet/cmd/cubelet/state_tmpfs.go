// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	stdlog "log"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

// mountTmpfsDir ensures stateDir is a tmpfs with the configured size ceiling.
//
// An existing mount is never umount+remounted (that would wipe state). Non-tmpfs
// mounts are left alone; genuine tmpfs resizes use MS_REMOUNT. Remount failure
// on an existing mount only warns — fresh mount failure fails startup.
func mountTmpfsDir(stateDir string, context *cli.Context) error {
	stateDir = filepath.Clean(stateDir)
	sizeMB := context.Int("state-tmpfs-size")
	if sizeMB <= 0 {
		return fmt.Errorf("invalid state-tmpfs-size: %d", sizeMB)
	}

	info, err := lookupExactMount(stateDir)
	if err != nil {
		return fmt.Errorf("lookup mount %s: %w", stateDir, err)
	}
	if info == nil {
		return mountFreshStateTmpfs(stateDir, sizeMB)
	}
	return maybeRemountStateTmpfs(stateDir, info, sizeMB)
}

func tmpfsSizeFromMB(sizeMB int) uint64 {
	return uint64(sizeMB) * 1024 * 1024
}

func stateTmpfsMount(sizeMB int, remount bool) *mount.Mount {
	opts := []string{fmt.Sprintf("size=%dm", sizeMB)}
	if remount {
		opts = []string{"remount", opts[0]}
	}
	return &mount.Mount{Type: "tmpfs", Source: "none", Options: opts}
}

func mountFreshStateTmpfs(stateDir string, sizeMB int) error {
	if err := stateTmpfsMount(sizeMB, false).Mount(stateDir); err != nil {
		return err
	}
	exist, err := mountinfo.Mounted(stateDir)
	if err != nil {
		return fmt.Errorf("verify tmpfs mount %s: %w", stateDir, err)
	}
	if !exist {
		return fmt.Errorf("mount tmpfs:%v fail", stateDir)
	}
	stdlog.Printf("mounted state tmpfs %s size=%dm", stateDir, sizeMB)
	return nil
}

func maybeRemountStateTmpfs(stateDir string, info *mountinfo.Info, sizeMB int) error {
	if info.FSType != "tmpfs" {
		stdlog.Printf("state dir %s already mounted as %s, skip tmpfs remount", stateDir, info.FSType)
		return nil
	}

	desiredBytes := tmpfsSizeFromMB(sizeMB)
	currentBytes, hasSize := parseTmpfsSizeBytes(info.VFSOptions)
	if hasSize && tmpfsSizeEqual(currentBytes, desiredBytes) {
		return nil
	}

	if hasSize && desiredBytes < currentBytes && refuseShrinkStateTmpfs(stateDir, currentBytes, desiredBytes) {
		return nil
	}

	prevSize := "unknown"
	if hasSize {
		prevSize = strconv.FormatUint(currentBytes, 10)
	}
	if err := stateTmpfsMount(sizeMB, true).Mount(stateDir); err != nil {
		stdlog.Printf("warn: remount state tmpfs %s size %s -> %d bytes failed: %v; keeping existing mount",
			stateDir, prevSize, desiredBytes, err)
		return nil
	}
	stdlog.Printf("remounted state tmpfs %s size %s -> %dm (%d bytes)", stateDir, prevSize, sizeMB, desiredBytes)
	return nil
}

func refuseShrinkStateTmpfs(stateDir string, currentBytes, desiredBytes uint64) bool {
	used, err := tmpfsUsedBytes(stateDir)
	if err != nil {
		stdlog.Printf("warn: cannot measure usage of state tmpfs %s (%v); refuse shrink %d -> %d bytes",
			stateDir, err, currentBytes, desiredBytes)
		return true
	}
	if used > desiredBytes {
		stdlog.Printf("warn: refuse shrink state tmpfs %s from %d to %d bytes: used %d bytes",
			stateDir, currentBytes, desiredBytes, used)
		return true
	}
	return false
}

func lookupExactMount(mountpoint string) (*mountinfo.Info, error) {
	mounts, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(mountpoint))
	if err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	return mounts[0], nil
}

// parseTmpfsSizeBytes parses the size= option from mountinfo VFSOptions.
// Kernel typically shows size in kilobytes (e.g. size=512000k for size=500m).
// Percent-of-RAM sizes are treated as unparseable.
func parseTmpfsSizeBytes(vfsOptions string) (uint64, bool) {
	for _, opt := range strings.Split(vfsOptions, ",") {
		opt = strings.TrimSpace(opt)
		if !strings.HasPrefix(opt, "size=") {
			continue
		}
		raw := strings.TrimPrefix(opt, "size=")
		if raw == "" || strings.HasSuffix(raw, "%") {
			return 0, false
		}
		return parseLinuxSizeBytes(raw)
	}
	return 0, false
}

func parseLinuxSizeBytes(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1024
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * mult, true
}

// tmpfsSizeEqual treats sizes within one page as equal to avoid remount churn
// from PAGE_SIZE rounding in the kernel.
func tmpfsSizeEqual(a, b uint64) bool {
	page := uint64(unix.Getpagesize())
	if a > b {
		return a-b < page
	}
	return b-a < page
}

func tmpfsUsedBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks < st.Bfree {
		return 0, fmt.Errorf("invalid statfs blocks=%d bfree=%d", st.Blocks, st.Bfree)
	}
	return (st.Blocks - st.Bfree) * uint64(st.Bsize), nil
}
