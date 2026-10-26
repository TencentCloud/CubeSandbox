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
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	osinterface "github.com/containerd/containerd/v2/pkg/os"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func namedPipePath(p string) bool {
	return strings.HasPrefix(p, `\\.\pipe\`)
}

func cleanMount(p string) string {
	if namedPipePath(p) {
		return p
	}
	return filepath.Clean(p)
}

func parseMount(osi osinterface.OS, mount *runtime.Mount) (*runtimespec.Mount, error) {
	var (
		dst = mount.GetContainerPath()
		src = mount.GetHostPath()
	)

	if !namedPipePath(src) {
		if _, err := osi.Stat(src); err != nil {

			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to stat %q: %w", src, err)
			}
			if err := osi.MkdirAll(src, 0755); err != nil {
				return nil, fmt.Errorf("failed to mkdir %q: %w", src, err)
			}
		}
		var err error
		originalSrc := src
		src, err = osi.ResolveSymbolicLink(src)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve symlink %q: %w", originalSrc, err)
		}

		src = filepath.Clean(src)

		if !(len(dst) == 2 && dst[1] == ':') {
			dst = filepath.Clean(dst)
			if dst[0] == '\\' {
				dst = "C:" + dst
			}
		} else if dst[0] == 'c' || dst[0] == 'C' {
			return nil, fmt.Errorf("destination path can not be C drive")
		}
	}

	var options []string

	if mount.GetReadonly() {
		options = append(options, "ro")
	} else {
		options = append(options, "rw")
	}
	return &runtimespec.Mount{Source: src, Destination: dst, Options: options}, nil
}

func WithWindowsMounts(osi osinterface.OS, config *runtime.ContainerConfig, extra []*runtime.Mount) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, _ *containers.Container, s *runtimespec.Spec) error {

		var (
			criMounts = config.GetMounts()
			mounts    = append([]*runtime.Mount{}, criMounts...)
		)

		for _, e := range extra {
			found := false
			for _, c := range criMounts {
				if cleanMount(e.ContainerPath) == cleanMount(c.ContainerPath) {
					found = true
					break
				}
			}
			if !found {
				mounts = append(mounts, e)
			}
		}

		sort.Stable(orderedMounts(mounts))

		mountSet := make(map[string]struct{})
		for _, m := range mounts {
			mountSet[cleanMount(m.ContainerPath)] = struct{}{}
		}

		defaultMounts := s.Mounts
		s.Mounts = nil

		for _, m := range defaultMounts {
			dst := cleanMount(m.Destination)
			if _, ok := mountSet[dst]; ok {

				continue
			}
			s.Mounts = append(s.Mounts, m)
		}

		for _, mount := range mounts {
			parsedMount, err := parseMount(osi, mount)
			if err != nil {
				return err
			}
			s.Mounts = append(s.Mounts, *parsedMount)
		}
		return nil
	}
}

func WithWindowsResources(resources *runtime.WindowsContainerResources) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if resources == nil {
			return nil
		}
		if s.Windows == nil {
			s.Windows = &runtimespec.Windows{}
		}
		if s.Windows.Resources == nil {
			s.Windows.Resources = &runtimespec.WindowsResources{}
		}
		if s.Windows.Resources.Memory == nil {
			s.Windows.Resources.Memory = &runtimespec.WindowsMemoryResources{}
		}

		var (
			count  = uint64(resources.GetCpuCount())
			shares = uint16(resources.GetCpuShares())
			max    = uint16(resources.GetCpuMaximum())
			limit  = uint64(resources.GetMemoryLimitInBytes())
		)
		if s.Windows.Resources.CPU == nil && (count != 0 || shares != 0 || max != 0) {
			s.Windows.Resources.CPU = &runtimespec.WindowsCPUResources{}
		}
		if count != 0 {
			s.Windows.Resources.CPU.Count = &count
		}
		if shares != 0 {
			s.Windows.Resources.CPU.Shares = &shares
		}
		if max != 0 {
			s.Windows.Resources.CPU.Maximum = &max
		}
		if limit != 0 {
			s.Windows.Resources.Memory.Limit = &limit
		}
		return nil
	}
}

func WithWindowsDefaultSandboxShares(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
	if s.Windows == nil {
		s.Windows = &runtimespec.Windows{}
	}
	if s.Windows.Resources == nil {
		s.Windows.Resources = &runtimespec.WindowsResources{}
	}
	if s.Windows.Resources.CPU == nil {
		s.Windows.Resources.CPU = &runtimespec.WindowsCPUResources{}
	}
	i := uint16(DefaultSandboxCPUshares)
	s.Windows.Resources.CPU.Shares = &i
	return nil
}

func WithWindowsCredentialSpec(credentialSpec string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Windows == nil {
			s.Windows = &runtimespec.Windows{}
		}
		s.Windows.CredentialSpec = credentialSpec
		return nil
	}
}

func WithWindowsDevices(config *runtime.ContainerConfig) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		for _, device := range config.GetDevices() {
			if device.ContainerPath != "" {
				return fmt.Errorf("unexpected ContainerPath %s, must be empty", device.ContainerPath)
			}

			if device.Permissions != "" {
				return fmt.Errorf("unexpected Permissions %s, must be empty", device.Permissions)
			}

			hostPath := device.HostPath
			if strings.HasPrefix(hostPath, "class/") {
				hostPath = "class://" + strings.TrimPrefix(hostPath, "class/")
			}

			idType, id, ok := strings.Cut(hostPath, "://")
			if !ok {
				return fmt.Errorf("unrecognised HostPath format %v, must match IDType://ID", device.HostPath)
			}

			o := oci.WithWindowsDevice(idType, id)
			if err := o(ctx, client, c, s); err != nil {
				return fmt.Errorf("failed adding device with HostPath %v: %w", device.HostPath, err)
			}
		}
		return nil
	}
}
