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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
)

const DefaultSandboxCPUshares = 2

func WithRelativeRoot(root string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		if s.Root == nil {
			s.Root = &runtimespec.Root{}
		}
		s.Root.Path = root
		return nil
	}
}

func WithoutRoot(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
	s.Root = nil
	return nil
}

func WithProcessArgs(config *runtime.ContainerConfig, image *imagespec.ImageConfig) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		command, args := config.GetCommand(), config.GetArgs()

		if len(command) == 0 {

			if len(args) == 0 {
				args = append([]string{}, image.Cmd...)
			}
			if command == nil {
				if !(len(image.Entrypoint) == 1 && image.Entrypoint[0] == "") {
					command = append([]string{}, image.Entrypoint...)
				}
			}
		}
		if len(command) == 0 && len(args) == 0 {
			return errors.New("no command specified")
		}
		return oci.WithProcessArgs(append(command, args...)...)(ctx, client, c, s)
	}
}

type orderedMounts []*runtime.Mount

func (m orderedMounts) Len() int {
	return len(m)
}

func (m orderedMounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

func (m orderedMounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m orderedMounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].ContainerPath), string(os.PathSeparator))
}

func WithAnnotation(k, v string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations[k] = v
		return nil
	}
}

func WithAdditionalGIDs(userstr string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		if s.Process == nil {
			s.Process = &runtimespec.Process{}
		}
		gids := s.Process.User.AdditionalGids
		if err := oci.WithAdditionalGIDs(userstr)(ctx, client, c, s); err != nil {
			return err
		}

		s.Process.User.AdditionalGids = mergeGids(s.Process.User.AdditionalGids, gids)
		return nil
	}
}

func mergeGids(gids1, gids2 []uint32) []uint32 {
	gidsMap := make(map[uint32]struct{})
	for _, gid1 := range gids1 {
		gidsMap[gid1] = struct{}{}
	}
	for _, gid2 := range gids2 {
		gidsMap[gid2] = struct{}{}
	}
	var gids []uint32
	for gid := range gidsMap {
		gids = append(gids, gid)
	}
	sort.Slice(gids, func(i, j int) bool { return gids[i] < gids[j] })
	return gids
}

func WithoutDefaultSecuritySettings(_ context.Context, _ oci.Client, c *containers.Container, s *runtimespec.Spec) error {
	if s.Process == nil {
		s.Process = &runtimespec.Process{}
	}

	s.Process.ApparmorProfile = ""
	if s.Linux != nil {
		s.Linux.Seccomp = nil
	}

	s.Process.Rlimits = nil
	return nil
}

func WithCapabilities(sc *runtime.LinuxContainerSecurityContext, allCaps []string) oci.SpecOpts {
	capabilities := sc.GetCapabilities()
	if capabilities == nil {
		return nullOpt
	}

	var opts []oci.SpecOpts

	if util.InStringSlice(capabilities.GetAddCapabilities(), "ALL") {
		opts = append(opts, oci.WithCapabilities(allCaps))
	}
	if util.InStringSlice(capabilities.GetDropCapabilities(), "ALL") {
		opts = append(opts, oci.WithCapabilities(nil))
	}

	var caps []string
	for _, c := range capabilities.GetAddCapabilities() {
		if strings.ToUpper(c) == "ALL" {
			continue
		}

		caps = append(caps, "CAP_"+strings.ToUpper(c))
	}
	opts = append(opts, oci.WithAddedCapabilities(caps))

	caps = []string{}
	for _, c := range capabilities.GetDropCapabilities() {
		if strings.ToUpper(c) == "ALL" {
			continue
		}
		caps = append(caps, "CAP_"+strings.ToUpper(c))
	}
	opts = append(opts, oci.WithDroppedCapabilities(caps))
	return oci.Compose(opts...)
}

func nullOpt(_ context.Context, _ oci.Client, _ *containers.Container, _ *runtimespec.Spec) error {
	return nil
}

func WithoutAmbientCaps(_ context.Context, _ oci.Client, c *containers.Container, s *runtimespec.Spec) error {
	if s.Process == nil {
		s.Process = &runtimespec.Process{}
	}
	if s.Process.Capabilities == nil {
		s.Process.Capabilities = &runtimespec.LinuxCapabilities{}
	}
	s.Process.Capabilities.Ambient = nil
	return nil
}

func WithSelinuxLabels(process, mount string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) (err error) {
		if s.Linux == nil {
			s.Linux = &runtimespec.Linux{}
		}
		if s.Process == nil {
			s.Process = &runtimespec.Process{}
		}
		s.Linux.MountLabel = mount
		s.Process.SelinuxLabel = process
		return nil
	}
}

func WithSysctls(sysctls map[string]string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Linux == nil {
			s.Linux = &runtimespec.Linux{}
		}
		if s.Linux.Sysctl == nil {
			s.Linux.Sysctl = make(map[string]string)
		}
		for k, v := range sysctls {
			s.Linux.Sysctl[k] = v
		}
		return nil
	}
}

func WithSupplementalGroups(groups []int64) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Process == nil {
			s.Process = &runtimespec.Process{}
		}
		var guids []uint32
		for _, g := range groups {
			guids = append(guids, uint32(g))
		}
		s.Process.User.AdditionalGids = mergeGids(s.Process.User.AdditionalGids, guids)
		return nil
	}
}

func WithDefaultSandboxShares(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
	if s.Linux == nil {
		s.Linux = &runtimespec.Linux{}
	}
	if s.Linux.Resources == nil {
		s.Linux.Resources = &runtimespec.LinuxResources{}
	}
	if s.Linux.Resources.CPU == nil {
		s.Linux.Resources.CPU = &runtimespec.LinuxCPU{}
	}
	i := uint64(DefaultSandboxCPUshares)
	s.Linux.Resources.CPU.Shares = &i
	return nil
}

func WithoutNamespace(t runtimespec.LinuxNamespaceType) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Linux == nil {
			return nil
		}
		var namespaces []runtimespec.LinuxNamespace
		for i, ns := range s.Linux.Namespaces {
			if ns.Type != t {
				namespaces = append(namespaces, s.Linux.Namespaces[i])
			}
		}
		s.Linux.Namespaces = namespaces
		return nil
	}
}

func WithNamespacePath(t runtimespec.LinuxNamespaceType, nsPath string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *runtimespec.Spec) error {
		if s.Linux == nil {
			return fmt.Errorf("Linux spec is required")
		}

		for i, ns := range s.Linux.Namespaces {
			if ns.Type == t {
				s.Linux.Namespaces[i].Path = nsPath
				return nil
			}
		}
		return fmt.Errorf("no such namespace %s", t)
	}
}

func WithPodNamespaces(config *runtime.LinuxContainerSecurityContext, sandboxPid uint32, targetPid uint32, uids, gids []runtimespec.LinuxIDMapping) oci.SpecOpts {
	namespaces := config.GetNamespaceOptions()

	opts := []oci.SpecOpts{
		oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.NetworkNamespace, Path: GetNetworkNamespace(sandboxPid)}),
		oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.IPCNamespace, Path: GetIPCNamespace(sandboxPid)}),
		oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.UTSNamespace, Path: GetUTSNamespace(sandboxPid)}),
	}
	if namespaces.GetPid() != runtime.NamespaceMode_CONTAINER {
		opts = append(opts, oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.PIDNamespace, Path: GetPIDNamespace(targetPid)}))
	}

	if namespaces.GetUsernsOptions() != nil {
		switch namespaces.GetUsernsOptions().GetMode() {
		case runtime.NamespaceMode_NODE:

		case runtime.NamespaceMode_POD:
			opts = append(opts, oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.UserNamespace, Path: GetUserNamespace(sandboxPid)}))
			opts = append(opts, oci.WithUserNamespace(uids, gids))
		}
	}

	return oci.Compose(opts...)
}

const (
	netNSFormat = "/proc/%v/ns/net"

	ipcNSFormat = "/proc/%v/ns/ipc"

	utsNSFormat = "/proc/%v/ns/uts"

	pidNSFormat = "/proc/%v/ns/pid"

	userNSFormat = "/proc/%v/ns/user"
)

func GetNetworkNamespace(pid uint32) string {
	return fmt.Sprintf(netNSFormat, pid)
}

func GetIPCNamespace(pid uint32) string {
	return fmt.Sprintf(ipcNSFormat, pid)
}

func GetUTSNamespace(pid uint32) string {
	return fmt.Sprintf(utsNSFormat, pid)
}

func GetPIDNamespace(pid uint32) string {
	return fmt.Sprintf(pidNSFormat, pid)
}

func GetUserNamespace(pid uint32) string {
	return fmt.Sprintf(userNSFormat, pid)
}
