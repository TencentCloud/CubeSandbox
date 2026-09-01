// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package opts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/mount"
	cdispec "github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/oci"
	osinterface "github.com/containerd/containerd/v2/pkg/os"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux/label"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func WithMounts(osi osinterface.OS, config *runtime.ContainerConfig, extra []*runtime.Mount, mountLabel string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, _ *containers.Container, s *runtimespec.Spec) (err error) {

		var (
			criMounts = config.GetMounts()
			mounts    = append([]*runtime.Mount{}, criMounts...)
		)

		for _, e := range extra {
			found := false
			for _, c := range criMounts {
				if filepath.Clean(e.ContainerPath) == filepath.Clean(c.ContainerPath) {
					found = true
					break
				}
			}
			if !found {
				mounts = append(mounts, e)
			}
		}

		sort.Stable(orderedMounts(mounts))

		s.Mounts = append(s.Mounts, runtimespec.Mount{
			Source:      "cgroup",
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
		})

		mountSet := make(map[string]struct{})
		for _, m := range mounts {
			mountSet[filepath.Clean(m.ContainerPath)] = struct{}{}
		}

		defaultMounts := s.Mounts
		s.Mounts = nil

		for _, m := range defaultMounts {
			dst := filepath.Clean(m.Destination)
			if _, ok := mountSet[dst]; ok {

				continue
			}
			if _, mountDev := mountSet["/dev"]; mountDev && strings.HasPrefix(dst, "/dev/") {

				continue
			}
			s.Mounts = append(s.Mounts, m)
		}

		for _, mount := range mounts {
			var (
				dst = mount.GetContainerPath()
				src = mount.GetHostPath()
			)

			if _, err := osi.Stat(src); err != nil {
				if !os.IsNotExist(err) {
					return fmt.Errorf("failed to stat %q: %w", src, err)
				}
				if err := osi.MkdirAll(src, 0755); err != nil {
					return fmt.Errorf("failed to mkdir %q: %w", src, err)
				}
			}

			src, err := osi.ResolveSymbolicLink(src)
			if err != nil {
				return fmt.Errorf("failed to resolve symlink %q: %w", src, err)
			}
			if s.Linux == nil {
				s.Linux = &runtimespec.Linux{}
			}
			options := []string{"rbind"}
			switch mount.GetPropagation() {
			case runtime.MountPropagation_PROPAGATION_PRIVATE:
				options = append(options, "rprivate")

			case runtime.MountPropagation_PROPAGATION_BIDIRECTIONAL:
				if err := ensureShared(src, osi.LookupMount); err != nil {
					return err
				}
				options = append(options, "rshared")
				s.Linux.RootfsPropagation = "rshared"
			case runtime.MountPropagation_PROPAGATION_HOST_TO_CONTAINER:
				if err := ensureSharedOrSlave(src, osi.LookupMount); err != nil {
					return err
				}
				options = append(options, "rslave")
				if s.Linux.RootfsPropagation != "rshared" &&
					s.Linux.RootfsPropagation != "rslave" {
					s.Linux.RootfsPropagation = "rslave"
				}
			default:
				log.G(ctx).Warnf("Unknown propagation mode for hostPath %q", mount.HostPath)
				options = append(options, "rprivate")
			}

			if mount.GetReadonly() {
				options = append(options, "ro")
			} else {
				options = append(options, "rw")
			}

			if mount.GetSelinuxRelabel() {
				ENOTSUP := syscall.Errno(0x5f)
				if err := label.Relabel(src, mountLabel, false); err != nil && err != ENOTSUP {
					return fmt.Errorf("relabel %q with %q failed: %w", src, mountLabel, err)
				}
			}

			var uidMapping []runtimespec.LinuxIDMapping

			var gidMapping []runtimespec.LinuxIDMapping

			s.Mounts = append(s.Mounts, runtimespec.Mount{
				Source:      src,
				Destination: dst,
				Type:        "bind",
				Options:     options,
				UIDMappings: uidMapping,
				GIDMappings: gidMapping,
			})
		}
		return nil
	}
}

func ensureShared(path string, lookupMount func(string) (mount.Info, error)) error {
	mountInfo, err := lookupMount(path)
	if err != nil {
		return err
	}

	optsSplit := strings.Split(mountInfo.Optional, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		}
	}

	return fmt.Errorf("path %q is mounted on %q but it is not a shared mount", path, mountInfo.Mountpoint)
}

func ensureSharedOrSlave(path string, lookupMount func(string) (mount.Info, error)) error {
	mountInfo, err := lookupMount(path)
	if err != nil {
		return err
	}

	optsSplit := strings.Split(mountInfo.Optional, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		} else if strings.HasPrefix(opt, "master:") {
			return nil
		}
	}
	return fmt.Errorf("path %q is mounted on %q but it is not a shared or slave mount", path, mountInfo.Mountpoint)
}

func getDeviceUserGroupID(runAsVal *cubebox.Int64Value) uint32 {
	if runAsVal != nil {
		return uint32(runAsVal.GetValue())
	}
	return 0
}

func WithDevices(osi osinterface.OS, config *cubebox.ContainerConfig, enableDeviceOwnershipFromSecurityContext bool) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		if s.Linux == nil {
			s.Linux = &runtimespec.Linux{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &runtimespec.LinuxResources{}
		}

		oldDevices := len(s.Linux.Devices)

		if config.GetOciConfig() == nil {
			return nil
		}
		for _, device := range config.GetOciConfig().GetDevices() {
			path, err := osi.ResolveSymbolicLink(device.HostPath)
			if err != nil {
				return err
			}

			o := oci.WithDevices(path, device.ContainerPath, device.Permissions)
			if err := o(ctx, client, c, s); err != nil {
				return err
			}
		}

		if enableDeviceOwnershipFromSecurityContext {
			UID := getDeviceUserGroupID(config.GetSecurityContext().GetRunAsUser())
			GID := getDeviceUserGroupID(config.GetSecurityContext().GetRunAsGroup())

			for idx := oldDevices; idx < len(s.Linux.Devices); idx++ {
				if UID != 0 {
					*s.Linux.Devices[idx].UID = UID
				}
				if GID != 0 {
					*s.Linux.Devices[idx].GID = GID
				}
			}
		}
		return nil
	}
}

func WithOOMScoreAdj(config *runtime.ContainerConfig, restrict bool) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Process == nil {
			s.Process = &runtimespec.Process{}
		}

		resources := config.GetLinux().GetResources()
		if resources == nil {
			return nil
		}
		adj := int(resources.GetOomScoreAdj())
		if restrict {
			var err error
			adj, err = restrictOOMScoreAdj(adj)
			if err != nil {
				return err
			}
		}
		s.Process.OOMScoreAdj = &adj
		return nil
	}
}

func WithPodOOMScoreAdj(adj int, restrict bool) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Process == nil {
			s.Process = &runtimespec.Process{}
		}
		if restrict {
			var err error
			adj, err = restrictOOMScoreAdj(adj)
			if err != nil {
				return err
			}
		}
		s.Process.OOMScoreAdj = &adj
		return nil
	}
}

func getCurrentOOMScoreAdj() (int, error) {
	b, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		return 0, fmt.Errorf("could not get the daemon oom_score_adj: %w", err)
	}
	s := strings.TrimSpace(string(b))
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("could not get the daemon oom_score_adj: %w", err)
	}
	return i, nil
}

func restrictOOMScoreAdj(preferredOOMScoreAdj int) (int, error) {
	currentOOMScoreAdj, err := getCurrentOOMScoreAdj()
	if err != nil {
		return preferredOOMScoreAdj, err
	}
	if preferredOOMScoreAdj < currentOOMScoreAdj {
		return currentOOMScoreAdj, nil
	}
	return preferredOOMScoreAdj, nil
}

func WithCDI(CDIDevices []*cubebox.CDIDevice) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *oci.Spec) error {
		seen := make(map[string]bool)

		var devices []string
		for _, device := range CDIDevices {
			deviceName := device.Name
			if seen[deviceName] {
				log.G(ctx).Debugf("Skipping duplicated CDI device %s", deviceName)
				continue
			}
			devices = append(devices, deviceName)
			seen[deviceName] = true
		}
		log.G(ctx).Infof("Container %v: CDI devices from OCIConfig.CDIDevices: %v", c.ID, devices)

		return cdispec.WithCDIDevices(devices...)(ctx, client, c, s)
	}
}
