// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// collectVolumeMountsFromContainers gathers mounts from every reported
// container. Template sandboxes often only have a type=sandbox container, and
// hostdir mounts are injected onto that container as well.
func collectVolumeMountsFromContainers(containers []*cubeboxv1.Container) []*cubeboxv1.VolumeMounts {
	if len(containers) == 0 {
		return nil
	}
	out := make([]*cubeboxv1.VolumeMounts, 0)
	for _, container := range containers {
		if container == nil {
			continue
		}
		for _, mount := range container.GetVolumeMounts() {
			if mount == nil {
				continue
			}
			out = append(out, mount)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isInternalRootfsMount reports mounts that are Cube runtime plumbing
// (writable rootfs at "/") and must not appear on public info/list.
func isInternalRootfsMount(mount *cubeboxv1.VolumeMounts) bool {
	if mount == nil {
		return false
	}
	if mount.GetContainerPath() == "/" {
		return true
	}
	return strings.HasPrefix(mount.GetName(), "cube_rootfs_")
}

// volumeMountsToContainerInfo converts cubelet mounts into API-facing JSON structs.
// HostPath is intentionally omitted: node-local paths are not part of the public
// sandbox info/list contract.
func volumeMountsToContainerInfo(mounts []*cubeboxv1.VolumeMounts) []*types.VolumeMountInfo {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]*types.VolumeMountInfo, 0, len(mounts))
	// Hostdir mounts can be injected into multiple containers; dedupe by the
	// public mount identity so info/list does not repeat the same entry.
	// Caveat: this key is intentional for hostdir (identical copies across
	// containers). Other mount types that share name/path/readonly but differ
	// elsewhere would also collapse — keep hostdir-oriented.
	seen := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		if mount == nil || isInternalRootfsMount(mount) {
			continue
		}
		key := mount.GetName() + "\x1f" +
			mount.GetContainerPath() + "\x1f" +
			strconv.FormatBool(mount.GetReadonly())
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, &types.VolumeMountInfo{
			Name:          mount.GetName(),
			ContainerPath: mount.GetContainerPath(),
			Readonly:      mount.GetReadonly(),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
