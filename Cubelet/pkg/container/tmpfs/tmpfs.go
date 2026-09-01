// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package tmpfs

import (
	"context"
	"fmt"
	"strconv"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func GenMount(ctx context.Context, c *cubebox.ContainerConfig) []specs.Mount {
	var mounts []specs.Mount
	devShmSize := getShmSize(ctx, c)
	opts := []string{"nosuid", "noexec", "nodev", "mode=1777"}
	if devShmSize != 0 {
		opts = append(opts, fmt.Sprintf("size=%d", devShmSize))
	} else {
		opts = append(opts, "size=65536k")
	}

	mounts = append(mounts, specs.Mount{
		Type:        constants.MountTypeTmpfs,
		Destination: "/dev/shm",
		Source:      "shm",
		Options:     opts,
	})

	return mounts
}

func GenNscdMount() specs.Mount {
	return specs.Mount{
		Type:        constants.MountTypeTmpfs,
		Destination: "/var/run/nscd/",
		Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
	}
}

func getShmSize(ctx context.Context, c *cubebox.ContainerConfig) int64 {
	var devShmSize int64 = 0
	if devBytes, ok := c.Annotations["dev_shm_size_in_bytes"]; ok {
		devShmSize, _ = strconv.ParseInt(string(devBytes), 10, 64)
		return devShmSize
	}
	return devShmSize
}

func GenSizeMount(ctx context.Context, target string, size int64) []specs.Mount {
	return GenSizeMountWithVolumeMount(ctx, target, size, nil)
}

func GenSizeMountWithVolumeMount(ctx context.Context, target string, size int64, vm *cubebox.VolumeMounts) []specs.Mount {
	var mounts []specs.Mount
	opts := []string{"nosuid", "noexec", "nodev", "mode=1777"}

	if vm != nil {

		if vm.Exec {

			opts = []string{"nosuid", "nodev", "mode=1777"}
		}

		if vm.Uid != "" {
			opts = append(opts, fmt.Sprintf("uid=%s", vm.Uid))
		}

		if vm.Gid != "" {
			opts = append(opts, fmt.Sprintf("gid=%s", vm.Gid))
		}
	}

	if size != 0 {
		opts = append(opts, fmt.Sprintf("size=%d", size))
	} else {
		opts = append(opts, "size=65536k")
	}

	mounts = append(mounts, specs.Mount{
		Type:        constants.MountTypeTmpfs,
		Destination: target,
		Options:     opts,
	})

	return mounts
}
