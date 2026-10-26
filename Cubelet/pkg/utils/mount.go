// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"golang.org/x/sys/unix"
)

var (
	errStatxNotSupport = errors.New("the statx syscall is not supported. At least Linux kernel 4.11 is needed")
)

func IsLikelyNotMountPoint(file string) (bool, error) {
	notMountPoint, err := isLikelyNotMountPointStatx(file)
	if errors.Is(err, errStatxNotSupport) {

		return isLikelyNotMountPointStat(file)
	}

	return notMountPoint, err
}

func IsMountPoint(dst string) (bool, error) {
	if mntInfo, err := mount.Lookup(dst); err == nil {
		if mntInfo.Mountpoint == dst {
			return true, nil
		}
		return false, nil
	} else {
		return false, err
	}
}

func isLikelyNotMountPointStatx(file string) (bool, error) {
	var stat, rootStat unix.Statx_t
	var err error

	if stat, err = statx(file); err != nil {
		return true, err
	}

	if stat.Attributes_mask != 0 {
		if stat.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 {
			if stat.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0 {

				return false, nil
			} else {

				return true, nil
			}
		}
	}

	root := filepath.Dir(strings.TrimSuffix(file, "/"))
	if rootStat, err = statx(root); err != nil {
		return true, err
	}

	return (stat.Dev_major == rootStat.Dev_major && stat.Dev_minor == rootStat.Dev_minor), nil
}
func statx(file string) (unix.Statx_t, error) {
	var stat unix.Statx_t
	if err := unix.Statx(0, file, unix.AT_STATX_DONT_SYNC, 0, &stat); err != nil {
		if err == unix.ENOSYS {
			return stat, errStatxNotSupport
		}

		return stat, err
	}

	return stat, nil
}

func isLikelyNotMountPointStat(file string) (bool, error) {
	stat, err := os.Stat(file)
	if err != nil {
		return true, err
	}
	rootStat, err := os.Stat(filepath.Dir(strings.TrimSuffix(file, "/")))
	if err != nil {
		return true, err
	}

	if stat.Sys().(*syscall.Stat_t).Dev != rootStat.Sys().(*syscall.Stat_t).Dev {

		return false, nil
	}

	return true, nil
}
