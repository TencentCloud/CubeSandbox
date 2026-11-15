// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !windows && !openbsd

package mount

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/moby/sys/mountinfo"
)

func UnmountRecursive(target string, flags int) error {
	if target == "" {
		return nil
	}

	target, err := CanonicalizePath(target)
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}

	prefix := target
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	mounts, err := mountinfo.GetMounts(func(m *mountinfo.Info) (bool, bool) {
		if m == nil {
			return true, false
		}
		match := m.Mountpoint == target || strings.HasPrefix(m.Mountpoint, prefix)
		return !match, false
	})
	if err != nil {
		return err
	}

	targetSet := make(map[string]struct{})
	for _, m := range mounts {
		targetSet[m.Mountpoint] = struct{}{}
	}

	var targets []string
	for m := range targetSet {
		targets = append(targets, m)
	}

	sort.SliceStable(targets, func(i, j int) bool {
		return len(targets[i]) > len(targets[j])
	})

	for i, target := range targets {
		if err := mount.UnmountAll(target, flags); err != nil {
			if i == len(targets)-1 {
				return err
			}
		}
	}
	return nil
}
