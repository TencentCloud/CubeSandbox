// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// HostDirMount is one external volume as the guest must present it: its name
// inside the virtio-fs channel, its access mode, and where it lands.
type HostDirMount struct {
	VolumeName string
	// SourceSubdir is the volume's name inside the channel.
	SourceSubdir string
	ReadOnly     bool
	// ContainerPath is the destination the caller asked for.
	ContainerPath string
}

// PlanHostDirMounts is the single decision for which external volume reaches
// which container path, and in what order. Callers only render the result.
// An undeclared backend is not mounted; a declared one that cannot be used
// fails the create.
func (i *StorageInfo) PlanHostDirMounts(ctx context.Context, volumeMounts []*cubebox.VolumeMounts) ([]HostDirMount, error) {
	if i == nil || len(i.HostDirBackendInfos) == 0 {
		return nil, nil
	}
	type indexedMount struct {
		mount *cubebox.VolumeMounts
		index int
	}
	byVolume := make(map[string]indexedMount, len(volumeMounts))
	for idx, mount := range volumeMounts {
		if mount == nil {
			continue
		}
		byVolume[mount.GetName()] = indexedMount{mount: mount, index: idx}
	}

	type candidate struct {
		plan         HostDirMount
		requestIndex int
		backendKey   string
	}
	candidates := make([]candidate, 0, len(i.HostDirBackendInfos))
	for backendKey, info := range i.HostDirBackendInfos {
		if info == nil {
			return nil, fmt.Errorf("host-dir backend %q has no volume record", backendKey)
		}
		indexed, ok := byVolume[info.VolumeName]
		if !ok {
			log.G(ctx).Debugf("[hostdir] volume %q is not mounted by this container", info.VolumeName)
			continue
		}
		if indexed.mount.GetContainerPath() == "" {
			return nil, fmt.Errorf("volume %q is declared with an empty container path", info.VolumeName)
		}
		subdir, ok := hostDirSourceSubdir(info)
		if !ok {
			return nil, fmt.Errorf("volume %q has an unusable bind path %q under share dir %q",
				info.VolumeName, info.BindPath, info.ShareDir)
		}
		candidates = append(candidates, candidate{
			plan: HostDirMount{
				VolumeName:    info.VolumeName,
				SourceSubdir:  subdir,
				ReadOnly:      info.ReadOnly,
				ContainerPath: indexed.mount.GetContainerPath(),
			},
			requestIndex: indexed.index,
			backendKey:   backendKey,
		})
	}
	sort.Slice(candidates, func(a, b int) bool {
		left := path.Clean(candidates[a].plan.ContainerPath)
		right := path.Clean(candidates[b].plan.ContainerPath)
		if left != right {
			// A parent path is a lexical prefix of its nested children.
			return left < right
		}
		if candidates[a].requestIndex != candidates[b].requestIndex {
			return candidates[a].requestIndex < candidates[b].requestIndex
		}
		return candidates[a].backendKey < candidates[b].backendKey
	})

	if len(candidates) == 0 {
		return nil, nil
	}
	plan := make([]HostDirMount, 0, len(candidates))
	for _, c := range candidates {
		log.G(ctx).Infof("[hostdir] plan %q -> %s (ro=%v, subdir=%s)",
			c.plan.VolumeName, c.plan.ContainerPath, c.plan.ReadOnly, c.plan.SourceSubdir)
		plan = append(plan, c.plan)
	}
	return plan, nil
}

// hostDirSourceSubdir is the volume's single-segment name inside the channel.
func hostDirSourceSubdir(info *HostDirBackendInfo) (string, bool) {
	if info.ShareDir == "" || info.BindPath == "" {
		return "", false
	}
	shareDir := filepath.Clean(info.ShareDir)
	bindPath := filepath.Clean(info.BindPath)
	rel, err := filepath.Rel(shareDir, bindPath)
	if err != nil || rel == "." || filepath.IsAbs(rel) ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	base := filepath.Base(bindPath)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", false
	}
	return base, true
}
